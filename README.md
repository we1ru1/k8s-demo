# 实战练习计划

为了掌握 Client-go 和 Informer，LLM设计了**四个阶段**的练习。按顺序执行，每个阶段都解决了前一个阶段的痛点。

## Hello Client-go (建立连接与基础 CRUD)

**目标：** 理解 `kubeconfig` 加载机制和 `Clientset` 的基本操作。 **理论：** K8s API 是 RESTful 的，Clientset 是对 HTTP 请求的封装。

- **练习任务：** 编写一个 Go 程序 `pod-lister`。
    
    1. 在集群外（你的笔记本上）运行。
        
    2. 读取 `~/.kube/config` 文件。
        
    3. 列出 `default` 命名空间下所有的 Pod 名称和状态。
        
    4. **进阶挑战：** 修改代码，使其能够接受命令行参数，删除指定名称的 Pod。
        

**关键代码片段提示：**
```Go
config, _ := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
clientset, _ := kubernetes.NewForConfig(config)
pods, _ := clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
```

## 初识 Informer (从轮询到监听)

**目标：** 理解 List-Watch 机制和本地缓存。 **痛点解决：** 阶段一的代码每次运行都要发起 HTTP 请求，如果每秒运行一次，效率极低。

- **练习任务：** 编写一个 `pod-watcher`。
    
    1. 使用 `SharedInformerFactory` 创建一个监听 Pod 变化的 Informer。
        
    2. 注册 `AddFunc`, `UpdateFunc`, `DeleteFunc` 三个事件处理函数。
        
    3. 程序启动后，在终端打印：“发现新 Pod: `[Name]`”、“Pod `[Name]` 发生了更新”、“Pod `[Name]` 被删除”。
        
    4. **观察：** 手动用 `kubectl run nginx --image=nginx`，观察你的程序控制台输出。
        

**核心概念：** `ResyncPeriod`（同步周期）和 `DeltaFIFO`（增量队列）。

## 生产级模式 (引入 Workqueue)

**目标：** 解决并发竞争和事件丢失问题。 **痛点解决：** 在阶段二中，如果在 `UpdateFunc` 里执行耗时操作（比如写数据库），会阻塞整个 Informer，导致后续事件处理延迟。**这是生产环境的大忌。**

- **练习任务：** 构建一个标准的 "Controller" 结构。
    
    1. **引入队列：** 使用 `client-go/util/workqueue`。
        
    2. **生产者：** 在 `Add/Update/Delete` 回调中，**只做一件事**：把 Pod 的 Key (namespace/name) 放入 Workqueue。
        
    3. **消费者：** 编写一个 `worker` 协程，死循环从 Workqueue 中取 Key。
        
    4. **业务逻辑：** 拿到 Key 后，从 Informer 的 **Lister (本地缓存)** 中获取 Pod 对象，打印其详细信息。
        
    5. **重试机制：** 模拟处理失败（例如 `if name == "error-pod"`），调用 `queue.AddRateLimited()` 进行指数退避重试。
        

**这是 K8s 官方控制器的标准写法，必须掌握。**
## 实战 CRD Controller (从监听原生资源到自定义资源)

**目标：** 模拟写一个简单的 Operator。 **场景：** 假设我们要管理一个静态网站。

- **练习任务：**
    
    1. 定义一个 CRD `StaticSite`，包含字段 `image` (镜像) 和 `replicas` (副本数)。
        
    2. 使用 `code-generator` 生成 `clientset`, `listers`, `informers` 代码（这一步很有挑战性，涉及 Go Tags 和脚本）。
        
    3. 编写 Controller 逻辑：
        
        - 当用户创建 `StaticSite` 时，你的代码自动创建一个对应的 `Deployment` 和 `Service`。
            
        - 当用户修改 `StaticSite` 的 `replicas` 时，你的代码自动更新 `Deployment` 的副本数。
            
    4. **验证：** 这种模式就是所谓的 "Level Driven"（水平驱动），你的目标是将**实际状态 (Status)** 调整为 **期望状态 (Spec)**。
        

# 建议的学习资源路径

1. **阅读代码：** 直接看 Kubernetes 源码中的 `staging/src/k8s.io/client-go/examples/workqueue/main.go`。这是官方提供的最佳实践模板，我上面的阶段三就是基于此。
    
2. **调试：** 在 `ListAndWatch` 处打断点，理解数据是如何从 API Server 流向本地 Cache 的。
    

