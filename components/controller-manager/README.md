# Controller Manager

Go Controller Manager 提供 Kubernetes 风格的 Controller 公共运行框架，包括共享事件源、限速工作队列、错误重试、健康检查和优雅退出。业务 Controller 通过 initializer 注册，框架本身不包含 Build、Snapshot 等对象的业务状态机。

当前只有 `Job` 和 `Runner` 使用 List/Watch；其他 EBS 资源必须使用 PollingSource。

## 构建与测试

```bash
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o controller-manager ./cmd/controller-manager
```

从仓库根目录构建镜像：

```bash
docker build -f components/controller-manager/Dockerfile -t eulermaker/controller-manager:dev .
```

## 运行

开发环境可以显式关闭服务端 TLS 校验：

```bash
./controller-manager \
  --apiserver=https://localhost:8443 \
  --insecure-skip-verify=true
```

生产环境应通过 `--apiserver-ca` 校验 ebs-apiserver 服务端证书。进程默认在 `:8080` 提供 `/healthz` 和 `/readyz`。

当前入口尚未注册具体业务 Controller，因此可用于验证框架启动、健康检查和退出。业务 Controller 实现后，应在 `cmd/controller-manager` 的 initializer map 中显式注册。
