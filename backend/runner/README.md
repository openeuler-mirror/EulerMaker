# ebs-runner

`ebs-runner` 是 EulerMaker 的任务执行代理。它通过 `ebs-gateway` 注册执行机、接收已分配的 Job，并将日志和产物上传到 Artifact Manager。

CT Job 的 `spec.scriptRefs` 非空时，Runner 按顺序经 Gateway 拉取所有 Script，写入 `/workspace/scripts/{name}`，并将第一项作为容器 ENTRYPOINT。Runner 进程内按 Script 名称、UID、resourceVersion 缓存正文；Job 指定的 UID 或 resourceVersion 变化时重新拉取。数组为空时使用镜像入口。

## 代码结构

```text
cmd/runner/     命令入口
pkg/options/    顶层配置装配、默认值、命令行解析和校验
pkg/runner/     Agent 装配、注册心跳、Job 生命周期和工作目录清理
pkg/gateway/    Gateway 客户端、资源传输类型、机器凭据与 Token 刷新
pkg/executor/   执行器、脚本缓存、payload 落盘和日志时间戳
pkg/container/ 容器运行时接口、Docker CLI 与 containerd 原生 Go Client 后端
pkg/artifact/  Artifact 地址路由、产物与日志上传、上传回执和保留期清理
```

测试随所属模块放置。`runner` 装配各模块；底层包不反向依赖 `runner`。CT 后端默认使用 Docker，也可选择 containerd。

Go 配置按 `Gateway`、`Container.Containerd`、`Artifact.Log` 和 `Artifact.Upload` 分组，业务配置类型分别定义在 `pkg/gateway`、`pkg/container`、`pkg/artifact` 的 `config.go` 中，由 `pkg/options` 组合；命令行参数保持扁平命名。

## 构建与测试

在当前目录执行：

```bash
go test ./...
CGO_ENABLED=0 go build -o ebs-runner ./cmd/runner
```

构建容器镜像：

```bash
docker build -t eulermaker/ebs-runner:dev .
```

## 本地运行

Runner 启动前需要可访问的 Gateway，以及包含 MachineAccount 凭据的 JSON 文件。Artifact Manager 地址由 Runner 启动时从 Gateway 获取：

```json
{
  "clientID": "runner-site-a",
  "clientSecret": "base64url-encoded-random-secret"
}
```

启动 Runner：

```bash
go run ./cmd/runner \
  --gateway=http://localhost:8080 \
  --machine-credential-file=./runner-machine-credential.json \
  --name=runner-ct-x86-01 \
  --type=ct \
  --insecure-skip-verify
```

`--insecure-skip-verify` 仅用于本地测试。生产环境应使用 HTTPS，并通过 `--gateway-ca` 和 `--artifact-manager-ca` 配置信任的 CA。

Runner 会在写入 `payload.json` 前，将 `repo[].url` 中的 `artifact://{instance}/repositories/...` 转换为该实例的 Runner 可访问 HTTP(S) 地址；普通仓库 URL 不变。Job 对象中的原始 payload 不会被修改。

Runner 在启动时读取一次凭据文件；替换凭据后需要重启。凭据和签发的 Token 不应写入日志或 Runner 状态。

完整参数请执行：

```bash
go run ./cmd/runner --help
```

也可以在仓库根目录先启动控制面，再独立启动 Runner。示例中的 `EXPORT_SERVER_ADDR` 需替换为控制面容器及构建节点均可访问的实际内网地址。Runner 使用宿主机网络；异机部署时需设置从 Runner 节点可访问的 `RUNNER_GATEWAY_URL` 和唯一的 `RUNNER_NAME`，并在 `Config/artifact-manager-conf` 中配置 Runner 可访问的实例地址：

```bash
EXPORT_SERVER_ADDR=192.0.2.10 docker compose -f hacks/docker-compose.yml up -d --build
DOCKER_GID="$(stat -c %g /var/run/docker.sock)" \
  docker compose -p eulermaker-runner -f hacks/docker-compose.runner.yml up -d --build
```

## containerd 后端

在 Linux 构建节点上安装并启动 containerd、`containerd-shim-runc-v2` 和 runc，然后以有权访问 socket、快照及挂载的用户运行 Runner（建议直接在宿主机以 root 运行）：

```bash
./ebs-runner \
  --gateway=https://gateway.example:8443 \
  --gateway-ca=/etc/ebs/gateway-ca.crt \
  --machine-credential-file=/etc/ebs/runner-machine-credential.json \
  --name=runner-containerd-01 \
  --container-runtime=containerd
```

| 参数 | 默认值 |
|---|---|
| `--container-runtime` | `docker` |
| `--containerd-address` | `/run/containerd/containerd.sock` |
| `--containerd-namespace` | `eulermaker` |
| `--containerd-snapshotter` | `overlayfs` |

containerd 后端直接调用 Go Client，不依赖 Docker、ctr、nerdctl 或 CRI。使用独立 namespace，不使用 kubelet 的 `k8s.io`；namespace 只用于对象分组，不是安全边界。

当前只支持 `runtimeSpec.networkMode=host`，BuildInfo Controller 生成的 Job 已使用该值；其他网络模式会明确报错。镜像入口、命令、环境变量、绑定挂载和 privileged 配置沿用 CT Job 契约。镜像通过原生 resolver 拉取，当前支持公开仓库，不读取 Docker 登录凭据，也不自动继承 containerd CRI 插件的镜像认证或 mirror 配置。

日志由 shim 写入 `${rootDir}/containerd/{namespace}/{containerID}.log`，Runner 实时读取并上传；退出后删除 task、排空日志，再清理容器元数据、可写快照及运行时日志。镜像缓存保留。Runner 重启后按容器标签核对归属并停止、移除旧任务，再处理服务端分配的 Job。切换运行时或 namespace 前应先排空原后端的任务。

容器化部署时，除 containerd socket 外，`${rootDir}` 与 containerd 存储目录（通常 `/var/lib/containerd`）都必须以相同绝对路径挂载，保证宿主机 shim 的日志路径、Job bind mount 和镜像用户解析能访问同一文件；Runner 还需要相应的宿主机文件访问和挂载权限，不能直接沿用仅挂载 Docker socket 的 Compose 配置。运行时日志文件不做轮转，需监控构建节点磁盘空间。
