# Controller Manager

Go Controller Manager 提供 Kubernetes 风格的 Controller 公共运行框架，包括共享事件源、限速工作队列、错误重试、健康检查和优雅退出。业务 Controller 通过 initializer 注册，框架本身不包含 Build、Snapshot 等对象的业务状态机。

当前只有 `Job` 和 `Runner` 使用 List/Watch；其他 EBS 资源必须使用 PollingSource。入口已注册 Job Controller 和 Runner Controller：Runner Controller 根据持久化心跳把失联 Runner 收敛为 `Offline`，Job Controller 随后处理绑定到不可用 Runner 的悬挂 Job，并按保留期清理终态 Job。

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
  --insecure-skip-verify=true \
  --runner-heartbeat-timeout=2m \
  --runner-startup-grace-period=5m \
  --job-runner-lost-grace-period=5m \
  --job-history-gc-enabled=true \
  --job-history-retention=720h
```

生产环境应通过 `--apiserver-ca` 校验 ebs-apiserver 服务端证书。进程默认在 `:8080` 提供 `/healthz`、`/readyz` 和 `/metrics`。

默认 `--controllers=*` 会启用 Job Controller 和 Runner Controller；可以使用 `--controllers=-job` 或 `--controllers=-runner` 分别关闭。Runner Controller 负责根据心跳把 Runner 标记为 `Offline`，Job Controller 只读取该状态并执行后续 Job 收敛。
