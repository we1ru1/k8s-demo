package main

import (
	"context"
	"fmt"
	staticsitev1alpha1 "k8s-demo/exercise4/pkg/apis/staticsite/v1alpha1"
	generatedclientset "k8s-demo/exercise4/pkg/client/clientset/versioned"
	staticsitelister "k8s-demo/exercise4/pkg/client/listers/staticsite/v1alpha1"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type Controller struct {
	kubeClient kubernetes.Interface
	siteClient generatedclientset.Interface

	siteLister       staticsitelister.StaticSiteLister
	siteSynced       cache.InformerSynced
	deploymentSynced cache.InformerSynced

	queue workqueue.TypedRateLimitingInterface[string]
}

func NewController(
	kubeClient kubernetes.Interface,
	siteClient generatedclientset.Interface,
	siteInformer cache.SharedIndexInformer,
	deploymentInformer cache.SharedIndexInformer,
) *Controller {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())

	c := &Controller{
		kubeClient:       kubeClient,
		siteClient:       siteClient,
		siteLister:       staticsitelister.NewStaticSiteLister(siteInformer.GetIndexer()),
		siteSynced:       siteInformer.HasSynced,
		deploymentSynced: deploymentInformer.HasSynced,
		queue:            queue,
	}

	// Reason: 事件回调里只做入队，避免阻塞 informer 的事件分发线程。
	siteInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueue,
		UpdateFunc: func(_, newObj interface{}) { c.enqueue(newObj) },
		DeleteFunc: c.enqueue,
	})
	// Reason: 仅监听 CR 不足以感知 Pod/Deployment 状态推进，这里把 Deployment 事件反向映射回 StaticSite 入队。
	deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueueByDeploymentOwner,
		UpdateFunc: func(_, newObj interface{}) { c.enqueueByDeploymentOwner(newObj) },
		DeleteFunc: c.enqueueByDeploymentOwner,
	})

	return c
}

func (c *Controller) enqueue(obj interface{}) {
	// Reason: 删除事件可能是 tombstone，DeletionHandlingMetaNamespaceKeyFunc 更安全。
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(fmt.Errorf("生成 key 失败: %v", err))
		return
	}
	c.queue.Add(key)
}

func (c *Controller) enqueueByDeploymentOwner(obj interface{}) {
	var deployment *appsv1.Deployment
	switch t := obj.(type) {
	case *appsv1.Deployment:
		deployment = t
	case cache.DeletedFinalStateUnknown:
		var ok bool
		deployment, ok = t.Obj.(*appsv1.Deployment)
		if !ok {
			return
		}
	default:
		return
	}

	owner := metav1.GetControllerOf(deployment)
	if owner == nil || owner.Kind != "StaticSite" {
		return
	}
	key := deployment.Namespace + "/" + owner.Name
	fmt.Printf("Deployment 事件触发，入队 StaticSite: %s\n", key)
	c.queue.Add(key)
}

func (c *Controller) Run(stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.queue.ShutDown()

	fmt.Println("等待 StaticSite informer 缓存同步...")
	if !cache.WaitForCacheSync(stopCh, c.siteSynced, c.deploymentSynced) {
		return fmt.Errorf("informer 缓存同步失败")
	}
	fmt.Println("缓存同步完成，controller 开始运行...")

	go wait.Until(c.runWorker, time.Second, stopCh)
	<-stopCh
	return nil
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func (c *Controller) processNextItem() bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.sync(context.Background(), key); err != nil {
		// Reason: 失败后用 AddRateLimited 进行指数退避重试，避免打爆 API Server。
		c.queue.AddRateLimited(key)
		runtime.HandleError(fmt.Errorf("同步 %q 失败: %v", key, err))
		return true
	}

	c.queue.Forget(key)
	return true
}

func (c *Controller) sync(ctx context.Context, key string) error {
	// workqueue 里存的是 "namespace/name"，先拆出来用于后续查缓存和调用 API。
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		// Reason: key 格式不合法通常是脏数据，重试也无意义，直接丢弃即可。
		return nil
	}

	// 先从 informer 本地缓存读 CR，避免每次都请求 apiserver。
	site, err := c.siteLister.StaticSites(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Reason: CR 删除后通常由 OwnerReference 触发级联回收，这里补充删除观测日志。
			c.logOwnedResourceDeletion(ctx, namespace, name)
			return nil
		}
		// 临时错误（网络抖动等）交给上层重试队列处理。
		//TODO: workqueue会自动处理error情况吗
		return err
	}

	// image 是我们创建 Deployment 的必要字段，缺失时无法继续调和。
	if site.Spec.Image == "" {
		// 对明显无效的 Spec 不做重试，避免 controller 空转。
		runtime.HandleError(fmt.Errorf("StaticSite %s/%s spec.image 为空", namespace, name))
		return nil
	}

	// replicas 允许为空，这里统一给默认值，保证后续调和逻辑总有目标副本数。
	replicas := int32(1)
	if site.Spec.Replicas != nil {
		replicas = *site.Spec.Replicas
	}
	// Reason: 用户未指定时默认 IfNotPresent；指定后严格按用户期望收敛。
	imagePullPolicy := desiredImagePullPolicy(site.Spec.ImagePullPolicy)

	// Service selector 和 Deployment template labels 必须一致，流量才能正确路由到 Pod。
	labels := map[string]string{
		"app":                     site.Name,
		"demo.k8s.io/static-site": site.Name,
	}

	// 先确保 Service 存在并匹配期望，保证网络入口稳定。
	if err := c.reconcileService(ctx, site, labels); err != nil {
		return err
	}

	// 再调和 Deployment，把镜像和副本数收敛到期望状态。
	dep, err := c.reconcileDeployment(ctx, site, labels, replicas, imagePullPolicy)
	if err != nil {
		return err
	}
	// Reason: 这里输出 Deployment 运行态变化，便于观察 Pod 是否逐步进入 Ready/Available。
	c.logDeploymentStatusChange(site, dep)

	// 最后回写 Status，让用户看到 controller 观测到的实际状态。
	return c.updateStatusIfNeeded(ctx, site, dep.Status.AvailableReplicas)
}

func (c *Controller) reconcileDeployment(
	ctx context.Context,
	site *staticsitev1alpha1.StaticSite,
	labels map[string]string,
	replicas int32,
	imagePullPolicy corev1.PullPolicy,
) (*appsv1.Deployment, error) {
	// 这里开始调和内置资源 Deployment，让“实际状态”逼近 CR 描述的“期望状态”。
	deployClient := c.kubeClient.AppsV1().Deployments(site.Namespace)
	deployName := site.Name

	existing, err := deployClient.Get(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}

		// 不存在就创建，包含 OwnerReference 以便 CR 删除时触发级联回收。
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployName,
				Namespace: site.Namespace,
				Labels:    labels,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(site, staticsitev1alpha1.SchemeGroupVersion.WithKind("StaticSite")),
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:            "web",
								Image:           site.Spec.Image,
								ImagePullPolicy: imagePullPolicy,
								Ports:           []corev1.ContainerPort{{ContainerPort: 80}},
							},
						},
					},
				},
			},
		}

		fmt.Printf("创建 Deployment: %s/%s\n", site.Namespace, deployName)
		return deployClient.Create(ctx, deployment, metav1.CreateOptions{})
	}

	// 已存在时只更新发生漂移的字段，避免无意义 Update 导致额外滚动和事件噪音。
	needUpdate := false
	deploymentCopy := existing.DeepCopy()

	// 副本数来自 CR 的 spec.replicas（或默认值），不一致就收敛。
	if deploymentCopy.Spec.Replicas == nil || *deploymentCopy.Spec.Replicas != replicas {
		deploymentCopy.Spec.Replicas = &replicas
		needUpdate = true
	}

	// 镜像来自 CR 的 spec.image，确保 Deployment 始终使用期望镜像。
	if len(deploymentCopy.Spec.Template.Spec.Containers) == 0 {
		deploymentCopy.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:            "web",
			Image:           site.Spec.Image,
			ImagePullPolicy: imagePullPolicy,
			Ports:           []corev1.ContainerPort{{ContainerPort: 80}},
		}}
		needUpdate = true
	} else {
		if deploymentCopy.Spec.Template.Spec.Containers[0].Image != site.Spec.Image {
			deploymentCopy.Spec.Template.Spec.Containers[0].Image = site.Spec.Image
			needUpdate = true
		}
		if deploymentCopy.Spec.Template.Spec.Containers[0].ImagePullPolicy != imagePullPolicy {
			deploymentCopy.Spec.Template.Spec.Containers[0].ImagePullPolicy = imagePullPolicy
			needUpdate = true
		}
	}

	if needUpdate {
		fmt.Printf("更新 Deployment: %s/%s\n", site.Namespace, deployName)
		return deployClient.Update(ctx, deploymentCopy, metav1.UpdateOptions{})
	}

	fmt.Printf("Deployment 无需更新: %s/%s（已与期望状态一致）\n", site.Namespace, deployName)
	return existing, nil
}

func (c *Controller) reconcileService(ctx context.Context, site *staticsitev1alpha1.StaticSite, labels map[string]string) error {
	// Service 是流量入口，调和目标是 selector/ports 与 Deployment 模板保持一致。
	svcClient := c.kubeClient.CoreV1().Services(site.Namespace)
	svcName := site.Name

	existing, err := svcClient.Get(ctx, svcName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}

		// Service 不存在就创建；OwnerReference 让生命周期绑定到 StaticSite。
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svcName,
				Namespace: site.Namespace,
				Labels:    labels,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(site, staticsitev1alpha1.SchemeGroupVersion.WithKind("StaticSite")),
				},
			},
			Spec: corev1.ServiceSpec{
				Selector: labels,
				Ports: []corev1.ServicePort{
					{
						Name:       "http",
						Port:       80,
						TargetPort: intstrFromInt(80),
					},
				},
			},
		}

		fmt.Printf("创建 Service: %s/%s\n", site.Namespace, svcName)
		_, err = svcClient.Create(ctx, svc, metav1.CreateOptions{})
		return err
	}

	// 已存在时做最小变更，避免不必要的写操作。
	needUpdate := false
	svcCopy := existing.DeepCopy()

	// selector 决定 Service 能选中哪些 Pod，必须与预期 labels 对齐。
	if !labelsEquals(svcCopy.Spec.Selector, labels) {
		svcCopy.Spec.Selector = labels
		needUpdate = true
	}

	// 这个练习固定暴露 80 端口，发现偏差就收敛回标准配置。
	if len(svcCopy.Spec.Ports) != 1 || svcCopy.Spec.Ports[0].Port != 80 {
		svcCopy.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstrFromInt(80)}}
		needUpdate = true
	}

	if needUpdate {
		fmt.Printf("更新 Service: %s/%s\n", site.Namespace, svcName)
		_, err = svcClient.Update(ctx, svcCopy, metav1.UpdateOptions{})
		return err
	}

	fmt.Printf("Service 无需更新: %s/%s（已与期望状态一致）\n", site.Namespace, svcName)
	return nil
}

func (c *Controller) updateStatusIfNeeded(ctx context.Context, site *staticsitev1alpha1.StaticSite, availableReplicas int32) error {
	// status 没变化就不写回，减少 APIServer 压力并避免产生多余事件。
	if site.Status.AvailableReplicas == availableReplicas && site.Status.ObservedGeneration == site.Generation {
		return nil
	}

	// 使用副本对象更新 status，避免直接修改 informer 缓存对象。
	siteCopy := site.DeepCopy()
	siteCopy.Status.AvailableReplicas = availableReplicas
	siteCopy.Status.ObservedGeneration = site.Generation

	// Reason: status 与 spec 分离更新，符合 Kubernetes API 约定，也更接近真实 operator 写法。
	_, err := c.siteClient.DemoV1alpha1().StaticSites(site.Namespace).UpdateStatus(ctx, siteCopy, metav1.UpdateOptions{})
	if err == nil {
		fmt.Printf("更新 StaticSite Status: %s/%s (availableReplicas=%d)\n", site.Namespace, site.Name, availableReplicas)
	}
	return err
}

func intstrFromInt(v int32) intstr.IntOrString {
	// Reason: ServicePort.TargetPort 类型是 IntOrString，这个辅助函数用于显式构造整数端口。
	return intstr.IntOrString{Type: intstr.Int, IntVal: v}
}

func labelsEquals(a, b map[string]string) bool {
	// Reason: 通过 selector 字符串做标准化比较，避免 map 遍历顺序导致的误判。
	return labels.Set(a).AsSelectorPreValidated().String() == labels.Set(b).AsSelectorPreValidated().String()
}

func desiredImagePullPolicy(policy corev1.PullPolicy) corev1.PullPolicy {
	if policy == "" {
		return corev1.PullIfNotPresent
	}
	return policy
}

func (c *Controller) logOwnedResourceDeletion(ctx context.Context, namespace, name string) {
	if _, err := c.kubeClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			fmt.Printf("Deployment 已删除: %s/%s\n", namespace, name)
		} else {
			fmt.Printf("查询 Deployment 删除状态失败: %s/%s, err=%v\n", namespace, name, err)
		}
	} else {
		fmt.Printf("Deployment 仍存在，等待 GC 级联删除: %s/%s\n", namespace, name)
	}

	if _, err := c.kubeClient.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			fmt.Printf("Service 已删除: %s/%s\n", namespace, name)
		} else {
			fmt.Printf("查询 Service 删除状态失败: %s/%s, err=%v\n", namespace, name, err)
		}
	} else {
		fmt.Printf("Service 仍存在，等待 GC 级联删除: %s/%s\n", namespace, name)
	}
}

func (c *Controller) logDeploymentStatusChange(site *staticsitev1alpha1.StaticSite, dep *appsv1.Deployment) {
	oldAvailable := site.Status.AvailableReplicas
	newAvailable := dep.Status.AvailableReplicas
	if oldAvailable != newAvailable {
		fmt.Printf(
			"Deployment 状态变化: %s/%s availableReplicas %d -> %d (ready=%d, updated=%d, unavailable=%d)\n",
			dep.Namespace,
			dep.Name,
			oldAvailable,
			newAvailable,
			dep.Status.ReadyReplicas,
			dep.Status.UpdatedReplicas,
			dep.Status.UnavailableReplicas,
		)
		return
	}
	fmt.Printf(
		"Deployment 当前状态: %s/%s available=%d ready=%d updated=%d unavailable=%d\n",
		dep.Namespace,
		dep.Name,
		dep.Status.AvailableReplicas,
		dep.Status.ReadyReplicas,
		dep.Status.UpdatedReplicas,
		dep.Status.UnavailableReplicas,
	)
}
