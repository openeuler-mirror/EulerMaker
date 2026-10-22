# Controller Manager

Go Controller Manager 提供 Kubernetes 风格的 Controller 公共运行框架，包括共享事件源、限速工作队列、错误重试、健康检查和优雅退出。业务 Controller 通过 initializer 注册。

当前只有 `Job` 和 `Runner` 使用 List/Watch；`Build`、`Snapshot`、`BuildInfo` 和 `RpmRepo` 使用 PollingSource，其中 `Build`、`Snapshot` 和 `RpmRepo` 的列表带状态过滤条件。入口已注册这六个 Controller。Snapshot Controller 固化代码仓库 commit，并通过 git-server 驱动仓库同步和 ref 解析。

Job Watch 仅缓存非终态对象。历史 Job 清理在启动时执行一次，之后每轮结束等待 24 小时再分页扫描，每页 500 个摘要对象；默认清理结束超过 30 天的 Job，失败对象留待下一轮扫描。

`--job-history-action=delete` 为默认策略；设置为 `archive` 时，先通过 apiserver 将完整 Job 保存至 ES `ebs-oldjobs`，确认同一版本后才从 etcd 删除。归档失败保留源 Job，下次扫描重试。历史查询使用 `/apis/ebs/v1/projects/{project}/oldjobs`，不参与 Job Watch；归档不包含产物和日志文件，ES 历史暂不自动过期。

## 构建与测试

```bash
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o controller-manager ./cmd/controller-manager
```

从仓库根目录构建镜像：

```bash
docker build -f backend/controller-manager/Dockerfile -t eulermaker/controller-manager:dev .
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

生产环境应通过 `--apiserver-ca` 校验 ebs-apiserver 服务端证书。启用 apiserver 内部证书认证时，还需设置 `--apiserver-client-cert` 和 `--apiserver-client-key`，证书的 URI SAN 为 `spiffe://eulermaker/internal/controller-manager`。进程默认在 `:8080` 提供 `/healthz`、`/readyz` 和 `/metrics`。

BuildInfo Controller 固定使用 `rpmspec -P` 展开 spec；运行环境必须安装 `rpm-build`，解析失败不会回退到文本解析。RPM 宏可执行 shell/Lua 代码，当前解析未隔离，包仓库来源必须可信。仓库提供的 Dockerfile 已安装 `rpm-build`。

默认 `--controllers=*` 会选择全部六个 Controller；可用 `--controllers=-<名称>` 单独关闭。

Snapshot、BuildInfo 和 RpmRepo Controller 通过公共服务配置提供者读取 `Config/service-config`：`gitServer.internalURL` 提供 Git Server 管理地址，`artifactManagers` 提供各 Artifact Manager 实例的内部地址。每轮 reconcile 按需读取一次并固定本轮配置，下一轮读取最新值；启动时不依赖配置读取成功。配置读取失败交由框架重试或记录永久错误，不计入包级失败预算，也不写业务失败终态。Git Server 的请求超时、重试和缓存时长仍由命令行参数配置。

RpmRepo 根据 Project 的 `ebs.io/artifacts` annotation（缺省 `default`）选择实例；BuildInfo 按 `artifact://{instance}/repositories/.../` 选择内部地址下载元数据。Job `repo` 保留逻辑地址，由 Runner 根据其网络模式解析。Git 克隆地址仍采用 Git Server 返回的 `clone_url`。

Snapshot 解析并发数、单轮总预算、同步重入延迟及仓库失败上限分别由 `--snapshot-resolve-workers`、`--snapshot-resolve-budget`、`--snapshot-sync-requeue-delay` 和 `--snapshot-failure-retry-limit` 配置。Build Controller 复用全局 `--poll-period` 与 `--workers`；它的指标包括 `build_controller_status_update_conflicts_total`、`build_controller_status_update_unknown_total`、`build_controller_ensure_conflicts_total` 和 `build_controller_ensure_terminating_total`。

BuildInfo Controller 从 `Project.spec.buildPayload` 固化到 BuildInfo 的 `rpmbuild_script` 选择脚本；未配置时使用全局 `rpmbuild` 脚本。同一轮首次创建 Job 前读取 Script，并将观察到的 name、UID、resourceVersion 写入本轮新 Job 的单元素 `spec.scriptRefs`；Runner 拉取并执行该脚本。
