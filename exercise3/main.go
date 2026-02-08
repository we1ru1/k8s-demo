package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"time"

	// 用于 wait.Until 和 ResyncPeriod。
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime" // 提供一个全局的 panic/error 处理器，防止 worker 中的错误搞垮整个程序。
	"k8s.io/apimachinery/pkg/util/wait"

	// 提供 Until 函数，可以优雅地启动一个循环运行的 worker。
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/client-go/util/workqueue" // 这次练习的主角——工作队列。
)

// 1. 定义 Controller 结构体
// 包含了控制器需要的所有组件（客户端、Lister、Informer同步状态、工作队列）
type Controller struct {
	//TODO: kubernetes.Interface 是个什么类型
	clientset kubernetes.Interface
	podLister v1.PodLister
	podSynced cache.InformerSynced
	queue     workqueue.TypedRateLimitingInterface[string] // key是string类型
}

// Controller的构造函数（核心：生产者逻辑）。作用：
//  1. 使用构造函数来封装 Controller 的初始化逻辑，使 main 函数更整洁；
//  2. 负责创建和设置 Controller 的所有内部组件；
func NewController(clientset kubernetes.Interface, podInformer cache.SharedIndexInformer) *Controller {
	// 使用 `NewTypedRateLimitingQueue` 来创建一个类型安全的队列实例，替代了旧的 `NewRateLimitingQueue`。
	// 队列现在明确知道它处理的是 `string` 类型的数据。
	queue := workqueue.NewTypedRateLimitingQueue[string](workqueue.DefaultTypedControllerRateLimiter[string]())

	c := &Controller{
		clientset: clientset,
		// 从 Informer 中获取 Lister。Lister 提供了从本地缓存（而不是API Server）高效读取资源的能力。
		podLister: v1.NewPodLister(podInformer.GetIndexer()),
		// Informer 的缓存同步状态函数，后续会用它来确保在处理任何业务逻辑前，本地缓存已经与API Server同步。
		podSynced: podInformer.HasSynced,
		queue:     queue,
	}

	// Producer 逻辑
	// 在这里注册事件回调函数，但它们现在只做一件事：将变更对象的 key (格式通常是 "namespace/name") 放入工作队列。
	// 这个过程必须非常迅速，不能执行任何耗时操作，从而将“事件发现”与“事件处理”彻底解耦。
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// `cache.MetaNamespaceKeyFunc`
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err != nil {
				runtime.HandleError(err)
				return
			}
			fmt.Printf("Event -> Add Pod: %s\n", key)
			// 将 key 添加到 workqueue
			c.queue.Add(key)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(newObj)
			if err != nil {
				runtime.HandleError(err)
				return
			}
			fmt.Printf("Event -> Update Pod: %s\n", key)
			c.queue.Add(key)
		},
		// `cache.DeletionHandlingMetaNamespaceKeyFunc` 是专门为删除事件设计的 key 提取函数。
		// 因为删除事件可能传递的是一个 `DeletedFinalStateUnknown` 对象（当Informer在watch中断后重新同步时），这个函数能正确处理这种情况
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err != nil {
				runtime.HandleError(err)
				return
			}
			fmt.Printf("Event -> Delete Pod: %s\n", key)
			c.queue.Add(key)
		},
	})

	return c
}

// Controller 的 Run 方法（生命周期管理）。作用：
//  1. Run 方法是 Controller 的主入口。它负责启动和管理 Controller 的整个生命周期。
func (c *Controller) Run(stopCh <-chan struct{}) error {
	// `defer runtime.HandleCrash()` 用于捕获任何可能发生的 panic，防止整个 controller 进程崩溃。
	defer runtime.HandleCrash()
	// `defer c.queue.ShutDown()` 确保在 Run 方法退出时，工作队列被优雅地关闭。
	// 这会使得正在从队列中读取的 worker (c.queue.Get()) 收到一个“关闭”信号，从而安全退出。
	defer c.queue.ShutDown()

	fmt.Println("等待缓存同步...")
	// `cache.WaitForCacheSync` 是一个至关重要的步骤。
	// 它会阻塞程序，直到 Informer 的本地缓存与 API Server 完全同步。
	// 只有同步完成后，我们才能保证 Lister 返回的数据是准确的。
	if !cache.WaitForCacheSync(stopCh, c.podSynced) {
		return fmt.Errorf("等待缓存同步失败")
	}
	fmt.Println("缓存已同步，worker 开始运行...")

	// `wait.Until` 会在一个新的 goroutine 中，以固定的时间间隔（这里是每秒）重复调用 `c.runWorker` 函数。
	// `stopCh` 用于通知这个循环何时停止。这是启动消费者 (worker) 的标准方式。
	go wait.Until(c.runWorker, time.Second, stopCh)

	// 等待 stopCh 关闭的信号，这意味着 Controller 需要停止了
	<-stopCh
	fmt.Println("正在关闭 worker...")
	return nil
}

// runWorker 启动一个无限循环来消费队列中的任务
func (c *Controller) runWorker() {
	// `for c.processNextWorkItem()` 是 worker 的主循环。
	// 它会不断地调用 `processNextWorkItem` 来处理队列中的下一个任务。
	// `processNextWorkItem` 会在队列关闭时返回 `false`，从而使这个循环自然退出。
	for c.processNextWorkItem() {

	}

}

// Consumer逻辑：
//
//	processNextWorkItem() 从workqueue中取出一个key，然后交给业务逻辑函数处理
func (c *Controller) processNextWorkItem() bool {

}
func main() {
	var kubeconfig *string

	// flag.String(arg1, arg2, arg3)的三个参数意思分别是：标志名，默认值，说明。
	// 其返回的不是一个 string，而是一个 *string (字符串指针)

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}

	// 1. 使用typed client更加简单理解（clientset）
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// 2. 使用typed SharedInformerFactory
	// ResyncPeriod 设置为 0，表示禁用定期 Resync，只在真正有事件时才触发。这是推荐的做法。
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0, informers.WithNamespace(corev1.NamespaceDefault))
	podInformer := factory.Core().V1().Pods().Informer()

	// 创建一个停止 channel
	stopCh := make(chan struct{})
	defer close(stopCh)

	// 启动 factory，它会启动所有通过它创建的 informer
	// 这必须在 goroutine 中运行，因为它会一直阻塞直到 stopCh 关闭
	go factory.Start(stopCh)

	// 等待缓存同步至关重要的
	// 在 informer 第一次将 apiserver 的全量资源加载到本地缓存之前，下面的调用会一直阻塞
	// 如果同步失败，程序就应该退出
	fmt.Println("等待缓存同步...")
	if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced) {
		panic("等待缓存同步失败!")
	}
	fmt.Println("缓存已同步, watcher 开始运行...")
	<-stopCh
}

// getConditionStatus 是一个辅助函数，用于获取指定 ConditionType 的状态
func getConditionStatus(conditions []corev1.PodCondition, conditionType corev1.PodConditionType) corev1.ConditionStatus {
	for _, cond := range conditions {
		if cond.Type == conditionType {
			return cond.Status
		}
	}
	return "" // 如果未找到该 Condition，返回空字符串
}
