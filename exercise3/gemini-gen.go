package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/client-go/util/workqueue"
)

// ===================================================================================
// 1. Controller 结构体定义
// ===================================================================================
// Reason:
// 这是构建控制器模式的最佳实践。
// 它将所有控制器运行所需的组件（客户端、Lister、Informer同步状态、工作队列）聚合在一起。
// 这样做使得组件的管理和传递更加清晰、方便。
// type Controller struct {
// 	clientset   kubernetes.Interface
// 	podLister   v1.PodLister
// 	podSynced   cache.InformerSynced
// 	// REASON FOR CHANGE:
// 	// 使用类型化的 `TypedRateLimitingInterface[string]` 替代了已废弃的 `RateLimitingInterface`。
// 	// 这利用了 Go 的泛型，使得队列中的 key 被明确为 string 类型，从而增加了代码的类型安全性和可读性。
// 	queue       workqueue.TypedRateLimitingInterface[string]
// }

// ===================================================================================
// 2. NewController 构造函数 (核心：生产者逻辑)
// ===================================================================================
// Reason:
// 使用构造函数来封装 Controller 的初始化逻辑，使 main 函数更整洁。
// 它负责创建和设置 Controller 的所有内部组件。
func NewController(clientset kubernetes.Interface, podInformer cache.SharedIndexInformer) *Controller {

	// REASON FOR CHANGE:
	// 使用 `NewTypedRateLimitingQueue` 来创建一个类型安全的队列实例，替代了旧的 `NewRateLimitingQueue`。
	// 队列现在明确知道它处理的是 `string` 类型的数据。
	queue := workqueue.NewTypedRateLimitingQueue[string](workqueue.DefaultControllerRateLimiter())

	c := &Controller{
		clientset: clientset,
		// Reason: 从 Informer 中获取 Lister。Lister 提供了从本地缓存（而不是API Server）高效读取资源的能力。
		podLister: v1.NewPodLister(podInformer.GetIndexer()),
		// Reason: 这是 Informer 的缓存同步状态函数，后续会用它来确保在处理任何业务逻辑前，本地缓存已经与API Server同步。
		podSynced: podInformer.HasSynced,
		queue:     queue,
	}

	// ===================================================================================
	// 生产者 (Producer) 逻辑
	// ===================================================================================
	// Reason:
	// 在这里注册事件回调函数，但它们现在只做一件事：将变更对象的 key (格式通常是 "namespace/name") 放入工作队列。
	// 这个过程必须非常迅速，不能执行任何耗时操作，从而将“事件发现”与“事件处理”彻底解耦。
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// Reason: `cache.MetaNamespaceKeyFunc` 是一个官方提供的工具函数，用于从对象中安全地提取出 key。
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err != nil {
				// Reason: `runtime.HandleError` 是一个全局错误处理器，即使发生错误也不会使整个程序 panic。
				runtime.HandleError(err)
				return
			}
			fmt.Printf("Event -> Add Pod: %s\n", key)
			// Reason: 将 key 添加到工作队列。
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
		DeleteFunc: func(obj interface{}) {
			// Reason: `cache.DeletionHandlingMetaNamespaceKeyFunc` 是专门为删除事件设计的 key 提取函数。
			// 因为删除事件可能传递的是一个 `DeletedFinalStateUnknown` 对象（当Informer在watch中断后重新同步时），这个函数能正确处理这种情况。
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

// ===================================================================================
// 3. Controller 的 Run 方法 (生命周期管理)
// ===================================================================================
// Reason:
// Run 方法是 Controller 的主入口。它负责启动和管理 Controller 的整个生命周期。
func (c *Controller) Run(stopCh <-chan struct{}) error {
	// Reason: `defer runtime.HandleCrash()` 用于捕获任何可能发生的 panic，防止整个 controller 进程崩溃。
	defer runtime.HandleCrash()
	// Reason: `defer c.queue.ShutDown()` 确保在 Run 方法退出时，工作队列被优雅地关闭。
	// 这会使得正在从队列中读取的 worker (c.queue.Get()) 收到一个“关闭”信号，从而安全退出。
	defer c.queue.ShutDown()

	fmt.Println("等待缓存同步...")
	// Reason: `cache.WaitForCacheSync` 是一个至关重要的步骤。
	// 它会阻塞程序，直到 Informer 的本地缓存与 API Server 完全同步。
	// 只有同步完成后，我们才能保证 Lister 返回的数据是准确的。
	if !cache.WaitForCacheSync(stopCh, c.podSynced) {
		return fmt.Errorf("等待缓存同步失败")
	}
	fmt.Println("缓存已同步, worker 开始运行...")

	// Reason: `wait.Until` 会在一个新的 goroutine 中，以固定的时间间隔（这里是每秒）重复调用 `c.runWorker` 函数。
	// `stopCh` 用于通知这个循环何时停止。这是启动消费者 (worker) 的标准方式。
	go wait.Until(c.runWorker, time.Second, stopCh)

	// 等待 stopCh 关闭的信号，这意味着 Controller 需要停止了
	<-stopCh
	fmt.Println("正在关闭 worker...")
	return nil
}

// runWorker 启动一个无限循环来消费队列中的任务
func (c *Controller) runWorker() {
	// Reason: `for c.processNextWorkItem()` 是 worker 的主循环。
	// 它会不断地调用 `processNextWorkItem` 来处理队列中的下一个任务。
	// `processNextWorkItem` 会在队列关闭时返回 `false`，从而使这个循环自然退出。
	for c.processNextWorkItem() {
	}
}

// ===================================================================================
// 4. 核心处理逻辑 (消费者 Consumer)
// ===================================================================================
// processNextWorkItem 从工作队列中取出一个 key，然后交给业务逻辑函数处理。
func (c *Controller) processNextWorkItem() bool {
	// REASON FOR CHANGE:
	// 由于队列是类型化的 (`string`)，c.queue.Get() 现在直接返回一个 `string` 类型的 key。
	key, shutdown := c.queue.Get()
	if shutdown {
		// Reason: 如果队列被关闭了（通过 `c.queue.ShutDown()`），`Get` 会立即返回 `true`，此时应该退出 worker 循环。
		return false
	}

	// Reason: `defer c.queue.Done(key)` 是一个关键步骤。
	// 它必须在 `defer` 中调用，以确保无论处理成功还是失败，最终都会通知队列这个 key 已经处理完毕。
	// 如果不调用 `Done`，这个 key 会被认为“正在处理中”，队列的速率限制器将无法正常工作。
	defer c.queue.Done(key)

	// REASON FOR CHANGE:
	// 由于 key 已经是 string 类型，不再需要进行类型断言。
	// keyStr, ok := key.(string)

	// 调用真正的业务逻辑
	if err := c.syncPod(key); err != nil {
		// Reason: 如果业务逻辑 `syncPod` 返回错误，说明处理失败，需要重试。
		// `c.queue.AddRateLimited(key)` 会根据队列的速率限制策略（指数退避）在未来的某个时间点将这个 key 重新放回队列。
		c.queue.AddRateLimited(key)
		runtime.HandleError(fmt.Errorf("同步 Pod [%s] 失败: %v", key, err))
		return true // 虽然有错误，但我们还想继续处理队列中的下一个任务，所以返回 true。
	}

	// Reason: 如果 `syncPod` 处理成功，`c.queue.Forget(key)` 会将这个 key 从队列中彻底移除，
	// 并重置它的失败重试计数器。
	c.queue.Forget(key)
	fmt.Printf("成功处理 Pod [%s]\n", key)
	return true
}

// syncPod 包含具体的业务逻辑
func (c *Controller) syncPod(key string) error {
	// Reason: `cache.SplitMetaNamespaceKey` 是 `MetaNamespaceKeyFunc` 的逆操作，将 "namespace/name" 格式的 key 解析回来。
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("无效的 key: %s", key))
		// Reason: 如果 key 本身格式就有问题，重试也没有意义，所以直接返回 nil，不再重新入队。
		return nil
	}

	// ===================================================================================
	// 练习要求：模拟处理失败
	// ===================================================================================
	if name == "error-pod" {
		fmt.Printf("  -> 模拟 Pod [%s] 处理失败\n", key)
		return fmt.Errorf("模拟的业务逻辑处理错误")
	}

	// Reason: 从 Lister (本地缓存) 中获取 Pod 对象。
	// 这一步的性能极高，因为它完全是内存操作，没有对 API Server 的网络请求。
	// 这是 Informer 模式性能优势的核心体现。
	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil {
		// Reason: 如果从缓存中获取 Pod 失败，最常见的原因是这个 Pod 已经被删除了。
		// 在这种情况下，我们通常不需要做任何事情，因为删除事件已经（或将要）被处理。
		// 所以我们直接返回 `nil`，表示处理完成，不需要重试。
		fmt.Printf("  -> Pod [%s] 在缓存中不存在，可能已被删除\n", key)
		return nil
	}

	// ===================================================================================
	// 在这里编写你的真实业务逻辑
	// ===================================================================================
	fmt.Printf("  -> 业务逻辑: 正在处理 Pod [%s]，当前状态为 %s\n", pod.Name, pod.Status.Phase)

	// 如果需要写回到API Server，则使用 c.clientset
	// c.clientset.CoreV1().Pods(namespace).Update(...)

	return nil
}

// ===================================================================================
// 5. Main 函数 (程序入口)
// ===================================================================================
func main() {
	var kubeconfig *string
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

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// Reason: 创建 Informer 工厂。我们这里使用了默认的 `NewSharedInformerFactory`，
	// 并设置了一个 30 秒的 ResyncPeriod。这意味着每 30 秒，Informer 会将缓存中的所有对象重新放入队列一次。
	// 在现代控制器中，更推荐的做法是将其设为 0，完全依赖事件驱动。
	factory := informers.NewSharedInformerFactory(clientset, 30*time.Second)
	podInformer := factory.Core().V1().Pods().Informer()

	// Reason: 创建我们的 Controller 实例，将所有必要的组件传入。
	controller := NewController(clientset, podInformer)

	stopCh := make(chan struct{})
	defer close(stopCh)
	// Reason: 启动 Informer 工厂。这会启动所有通过该工厂创建的 Informer。它必须在一个单独的 goroutine 中运行。
	go factory.Start(stopCh)

	// Reason: 调用 Controller 的 Run 方法，启动控制器的主循环。
	if err := controller.Run(stopCh); err != nil {
		panic(fmt.Sprintf("运行 Controller 失败: %s", err.Error()))
	}
}
