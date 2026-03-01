package main

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// 1. 定义 Controller 结构体
// 包含了控制器需要的所有组件（客户端、Lister、Informer同步状态、工作队列）
type Controller struct {
	// kubernetes.Interface 是 k8s.io/client-go/kubernetes 包中定义的一个 Go 接口（interface）。
	// 是一个聚合接口，它包含了所有核心 Kubernetes API 组（例如 CoreV1(), AppsV1(), NetworkingV1()
	// 等）的客户端接口。
	clientset kubernetes.Interface
	podLister v1.PodLister
	podSynced cache.InformerSynced
	queue     workqueue.TypedRateLimitingInterface[string] // key是string类型
}

// Controller的构造函数（核心：生产者逻辑）。作用：
//  1. 使用构造函数来封装 Controller 的初始化逻辑，使 main 函数更整洁；
//  2. 负责创建和设置 Controller 的所有内部组件；
func NewController(clientset kubernetes.Interface, podInformer cache.SharedIndexInformer) *Controller {
	// 使用 NewTypedRateLimitingQueue 来创建一个类型安全的队列实例，替代了旧的 NewRateLimitingQueue。
	// 队列现在明确知道它处理的是 `string` 类型的数据。
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())

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
			// cache.MetaNamespaceKeyFunc 是官方提供的工具函数，用于安全取出key。
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err != nil {
				// runtime.HandleError 是一个全局错误处理器，即使发生错误也不会使整个程序 panic。
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
		// cache.DeletionHandlingMetaNamespaceKeyFunc 是专门为删除事件设计的 key 提取函数。
		// 因为删除事件可能传递的是一个 DeletedFinalStateUnknown 对象（当Informer在watch中断后重新同步时），
		// 这个函数能正确处理这种情况。
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
	// defer runtime.HandleCrash() 用于捕获任何可能发生的 panic，防止整个 controller 进程崩溃。
	defer runtime.HandleCrash()
	// defer c.queue.ShutDown() 确保在 Run 方法退出时，工作队列被优雅地关闭。
	// 这会使得正在从队列中读取的 worker (c.queue.Get()) 收到一个“关闭”信号，从而安全退出。
	defer c.queue.ShutDown()

	fmt.Println("等待缓存同步...")
	// cache.WaitForCacheSync 是一个至关重要的步骤。
	// 它会阻塞程序，直到 Informer 的本地缓存与 API Server 完全同步。
	// 只有同步完成后，我们才能保证 Lister 返回的数据是准确的。
	if !cache.WaitForCacheSync(stopCh, c.podSynced) {
		return fmt.Errorf("等待缓存同步失败")
	}
	fmt.Println("缓存已同步，worker 开始运行...")

	// wait.Until 会在一个新的 goroutine 中，以固定的时间间隔（这里是每秒）重复调用 c.runWorker 函数。
	// stopCh 用于通知这个循环何时停止。这是启动消费者 (worker) 的标准方式。
	go wait.Until(c.runWorker, time.Second, stopCh)

	// 等待 stopCh 关闭的信号，这意味着 Controller 需要停止了。
	<-stopCh
	fmt.Println("正在关闭 worker...")
	return nil
}

// runWorker 启动一个无限循环来消费队列中的任务。
func (c *Controller) runWorker() {
	// for c.processNextWorkItem() 是 worker 的主循环。
	// 它会不断地调用 processNextWorkItem 来处理队列中的下一个任务。
	// processNextWorkItem 会在队列关闭时返回 false，从而使这个循环自然退出。
	for c.processNextWorkItem() {

	}

}

// Consumer逻辑：
//
//	processNextWorkItem() 从workqueue中取出一个key，然后交给业务逻辑函数处理。
func (c *Controller) processNextWorkItem() bool {
	// 由于队列是类型化的string，c.queue.Get()现在直接返回一个string类型的key
	key, shutdown := c.queue.Get()
	if shutdown {
		// 如果队列被关闭了（通过 c.queue.Shutdown()），Get 会立即返回true，
		// 此时应该退出 worker 循环，所以返回false。
		return false
	}

	// 在此处调用Done，以便workqueue知道我们已经完成了对此项的处理。同时，如果
	// 不想让这个work item作重新入队，必须记得调用Forget。例如，当发生暂时性错误时，
	// 不会调用Forget，而是将该项重新放回workqueue，并在退避期后再次尝试。
	defer c.queue.Done(key)

	// 真正的业务逻辑，包含在syncPod()中
	if err := c.syncPod(key); err != nil {
		// 如果业务逻辑 syncPod 返回错误，说明处理失败，需要重试。
		// c.queue.AddRateLimited(key) 会根据队列的速率限制策略（指数退避）
		// 在未来的某个时间点将这个 key 重新放回队列。
		c.queue.AddRateLimited(key)
		runtime.HandleError(fmt.Errorf("同步 Pod [%s] 失败： %v", key, err))

		// 虽然有错误，但是我们仍然想继续处理队列中的下一个任务，所以返回true。
		return true
	}

	// syncPod()执行成功，c.queue.Forget(key) 会将这个 key 从队列中彻底移除。
	c.queue.Forget(key)
	fmt.Printf("成功处理 Pod [%s]\n", key)
	return true
}

// syncPod()：具体的业务逻辑
func (c *Controller) syncPod(key string) error {
	// cache
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("无效的 key: %s", key))
		// 如果 key 本身格式就有问题，重试也没有意义，所以返回nil，不再入队
		return nil
	}

	// 模拟处理失败，重新入队
	if name == "error-pod" {
		fmt.Printf(" -> 模拟 Pod [%s] 处理失败\n", key)
		return fmt.Errorf("模拟业务逻辑处理错误，key将重新入队")
	}

	// 从Lister（本地缓存）中获取 Pod 对象
	// 这一步的性能极高，因为完全是内存操作，没有向 API Server 请求
	// 是 Informer 模式性能优势的核心体现。
	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil {
		// 如果从 cache 中获取key失败，最常见的原因就是Pod被删除了。
		// 在这种情况下，我们通常不需要做任何事情，因为删除事件已经（或将要）被处理。
		// 所以我们直接返回 `nil`，表示处理完成，不需要重试。
		fmt.Printf("  -> Pod [%s] 在缓存中不存在，可能已被删除\n", key)
		return nil
	}
	// 真实业务逻辑
	fmt.Printf("  -> 业务逻辑: 正在处理 Pod [%s]，当前状态为 %s\n", pod.Name, pod.Status.Phase)

	// 如果需要写回到API Server，则使用c.clientset
	// c.clientset.CoreV1().Pods(namespace).Update(...)
	return nil
}
