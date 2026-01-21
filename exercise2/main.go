package main

import (
	"flag"
	"fmt"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

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

	// 2. 使用类型化的 SharedInformerFactory
	// ResyncPeriod 设置为 0，表示禁用定期 Resync，只在真正有事件时才触发。这是推荐的做法。
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0, informers.WithNamespace(corev1.NamespaceDefault))
	podInformer := factory.Core().V1().Pods().Informer()

	// 3. 注册处理器：AddFunc、UpdateFunc、DeleteFunc
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if pod, ok := obj.(*corev1.Pod); ok {
				fmt.Printf("发现新 Pod： [%s]!\n", pod.Name)

			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod, ok1 := oldObj.(*corev1.Pod)
			newPod, ok2 := newObj.(*corev1.Pod)
			// 有什么场景会发生以下 !ok1 || !ok2 的情况？
			// 1. Cache Tombstone
			// 2. 使用 Unstructured 或 泛型 Informer
			// 3. 内存损坏或 Upstream Bug (极度罕见)
			if !ok1 || !ok2 {
				// 为什么这里是return而不是panic?
				// 1. 容错性：Informer 是在长期运行的守护进程（Controller）中跑的。
				// 如果因为收到一个脏数据或者类型断言失败导致 Panic，整个程序（Controller）
				// 就会崩溃退出，导致所有其他正常的业务逻辑全部中断。
				//
				// 2. 脏数据处理：在极少数情况下（例如 Delete 事件传来的 DeletedFinalStateUnknown 对象），
				// 直接断言为 *corev1.Pod 可能会失败。正确的做法是像在 DeleteFunc 里写的那样，去检查它是不是 Tombstone 对象。
				// 如果在 UpdateFunc 里断言失败，说明对象类型不对，忽略这次事件比让程序崩溃更安全。
				//
				// 3. Go 语言惯例：在处理异步事件流时，单个事件的处理失败不应影响整体系统的稳定性。
				return
			}

			// 如果 ResourceVersion 没有变化，说明是 Resync 事件，忽略
			if oldPod.ResourceVersion == newPod.ResourceVersion {
				// 可以取消注释来观察 Resync
				fmt.Printf("Pod [%s] Resync\n", newPod.Name)
				return
			}

			// 确实有新的更新，打印出 ResourceVersion 的变化，并比较主要变化的属性
			fmt.Printf("Pod [%s] 发生了更新！(ResourceVersion：%s -> %s)\n", newPod.Name, oldPod.ResourceVersion, newPod.ResourceVersion)

			// 比较 Pod Phase 的变化
			if oldPod.Status.Phase != newPod.Status.Phase {
				fmt.Printf("  - Phase 变化: %s -> %s\n", oldPod.Status.Phase, newPod.Status.Phase)
			}

			// 比较 Pod IP 的变化
			if oldPod.Status.PodIP != newPod.Status.PodIP {
				fmt.Printf("  - Pod IP 变化: %s -> %s\n", oldPod.Status.PodIP, newPod.Status.PodIP)
			}

			// 比较 Ready Condition 的变化
			// fmt.Printf("oldPod [%s] condition的内容：%v\n", oldPod.Name, oldPod.Status.Conditions)
			// fmt.Printf("newPod [%s] condition的内容：%v\n", newPod.Name, newPod.Status.Conditions)

			oldReadyStatus := getConditionStatus(oldPod.Status.Conditions, corev1.PodReady)
			newReadyStatus := getConditionStatus(newPod.Status.Conditions, corev1.PodReady)
			if oldReadyStatus != newReadyStatus {
				fmt.Printf("  - Ready 状态变化: %s -> %s\n", oldReadyStatus, newReadyStatus)
			}

		},
		DeleteFunc: func(obj interface{}) {
			//在某些情况下（比如网络问题导致 watch 中断），Informer
			// 可能没有收到确切的删除事件，但通过 List 操作发现了某个
			// 对象消失了。此时，它会向 DeleteFunc 传递一个
			// cache.DeletedFinalStateUnknown   类型的对象，而被
			// 删除的对象包裹在里面。

			// 需要处理 DeletedFinalStateUnknown
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					fmt.Println("删除事件处理错误: 对象既不是 Pod 也不是 Tombstone")
					return
				}
				pod, ok = tombstone.Obj.(*corev1.Pod)
				if !ok {
					fmt.Println("删除事件处理错误: Tombstone 中的对象不是 Pod")
					return
				}
			}
			fmt.Printf("Pod [%s] 被删除!\n", pod.Name)
		},
	})

	// 4. 正确启动 informer，并等待缓存同步

	// 创建一个停止 channel
	stopCh := make(chan struct{})
	defer close(stopCh)

	// 启动 factory，它会启动所有通过它创建的 inforer
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
