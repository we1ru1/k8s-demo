# Exercise4: StaticSite CRD Controller

这个目录给出第四阶段的完整最小实现：

- 定义 CRD `StaticSite`
- 提供 `clientset/lister/informer`（当前为手写的 code-generator 风格输出）
- 使用 `workqueue` 做标准 controller
- 当 `StaticSite` 变更时，调和 `Deployment + Service`
- 更新 `status.availableReplicas`

## 目录结构

- `main.go`: Controller 主程序
- `pkg/apis/staticsite/v1alpha1`: API 类型定义
- `pkg/generated/...`: client/lister/informer
- `config/crd/staticsites.demo.k8s.io.yaml`: CRD 定义
- `config/sample-staticsite.yaml`: 示例对象
- `hack/update-codegen.sh`: 真实项目中用于重新生成代码的脚本

## 运行步骤

1. 安装 CRD

```bash
kubectl apply -f exercise4/config/crd/staticsites.demo.k8s.io.yaml
```

2. 启动 controller

```bash
go run ./exercise4 --namespace default
```

3. 创建自定义资源

```bash
kubectl apply -f exercise4/config/sample-staticsite.yaml
```

4. 验证

```bash
kubectl get deploy,svc,staticsite -n default
kubectl get staticsite my-site -n default -o yaml
```

5. 修改副本数并观察 Deployment 自动收敛

```bash
kubectl patch staticsite my-site -n default --type merge -p '{"spec":{"replicas":3}}'
kubectl get deploy my-site -n default
```

## 关于 code-generator

当前 `pkg/generated` 已经按 code-generator 输出风格组织，便于你先理解控制器主流程。
你后续可以用 `hack/update-codegen.sh` 在本地替换为真正自动生成的版本。
