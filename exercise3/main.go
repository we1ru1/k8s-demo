package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath" // 新增导入
	"syscall"

	corev1 "k8s.io/api/core/v1"
	// 提供一个全局的 panic/error 处理器，防止 worker 中的错误搞垮整个程序。
	// 用于 wait.Until 和 ResyncPeriod。
	// 提供 Until 函数，可以优雅地启动一个循环运行的 worker。
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	// 这次练习的主角——工作队列。
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

	// 使用typed client更加简单理解（clientset）
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// 使用typed SharedInformerFactory，Resync 是指 Informer 会将缓存中的所有对象重新放入队列一次。
	// ResyncPeriod 设置为 0，表示禁用定期 Resync，只在真正有事件时才触发。这是推荐的做法。
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0, informers.WithNamespace(corev1.NamespaceDefault))
	podInformer := factory.Core().V1().Pods().Informer()

	// 创建Controller实例，将所有的必要组件转入
	controller := NewController(clientset, podInformer)

	// 创建一个停止 channel
	stopCh := make(chan struct{})

	// 监听 OS 信号，实现优雅关闭
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP) // 监听中断、终止和挂起信号

	go func() {
		<-sigChan // 阻塞直到接收到信号
		fmt.Println("\n收到终止信号，正在关闭控制器...")
		close(stopCh) // 调用安全关闭函数
	}()

	// 启动 factory工厂，它会启动所有通过它创建的 informer
	// 必须在 goroutine 中运行，因为它会一直阻塞直到 stopCh 关闭
	go factory.Start(stopCh)

	// Reason: 调用 Controller 的 Run 方法，启动控制器的主循环。
	if err := controller.Run(stopCh); err != nil {
		panic(fmt.Sprintf("运行 Controller 失败: %s", err.Error()))
	}

	fmt.Println("控制器已完全关闭。") // 确保主 goroutine 也能显示最终关闭信息

}
