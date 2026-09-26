# Controller Manager

Go Controller Manager 提供 Kubernetes 风格的 Controller 公共运行框架，包括共享事件源、限速工作队列、错误重试、健康检查和优雅退出。业务 Controller 通过 initializer 注册。

当前只有 `Job` 和 `Runner` 使用 List/Watch；`Snapshot` 与 `Build` 使用带 phase 过滤的 PollingSource。入口已注册 Job、Runner、Snapshot 和 Build Controller。Snapshot Controller 固化代码仓库 commit，并通过 git-server 驱动仓库同步和 ref 解析。

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
  --git-server-addr=http://localhost:8084 \
  --runner-heartbeat-timeout=2m \
  --runner-startup-grace-period=5m \
  --job-runner-lost-grace-period=5m \
  --job-history-gc-enabled=true \
  --job-history-retention=720h
```

生产环境应通过 `--apiserver-ca` 校验 ebs-apiserver 服务端证书。进程默认在 `:8080` 提供 `/healthz`、`/readyz` 和 `/metrics`。

默认 `--controllers=*` 会启用 Job、Runner、Snapshot 和 Build Controller；可以使用 `--controllers=-job`、`--controllers=-runner`、`--controllers=-snapshot` 或 `--controllers=-build` 单独关闭。Snapshot 解析并发数、单轮总预算、同步重入延迟及仓库失败上限分别由 `--snapshot-resolve-workers`、`--snapshot-resolve-budget`、`--snapshot-sync-requeue-delay` 和 `--snapshot-failure-retry-limit` 配置。Build Controller 复用全局 `--poll-period` 与 `--workers`，不需要额外开关；它的指标包括 `build_controller_status_update_conflicts_total`、`build_controller_status_update_unknown_total`、`build_controller_ensure_conflicts_total` 和 `build_controller_ensure_terminating_total`。

BuildInfo Controller 从 `Project.spec.buildPayload` 固化到 BuildInfo 的 `rpmbuild_script` 选择脚本；未配置时使用全局 `rpmbuild` 脚本。同一轮首次创建 Job 前读取 Script，并将观察到的 name、UID、resourceVersion 写入本轮新 Job 的单元素 `spec.scriptRefs`；Runner 拉取并执行该脚本。
