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
pkg/container/ 容器运行时接口、Docker 与 containerd 原生 Go Client 后端
pkg/service/   服务地址快照、Job 选址、Artifact 与 Git 逻辑地址解析
pkg/artifact/  产物与日志上传、上传回执和保留期清理
```

测试随所属模块放置。`runner` 装配各模块；底层包不反向依赖 `runner`。CT 后端默认使用 Docker，也可选择 containerd。

Go 配置按 `Gateway`、`Container.Containerd`、`Artifact.Log` 和 `Artifact.Upload` 分组，业务配置类型分别定义在 `pkg/gateway`、`pkg/container`、`pkg/artifact` 的 `config.go` 中，由 `pkg/options` 组合；命令行参数保持扁平命名。

## 构建与测试

在当前目录执行：

```bash
go test ./...
CGO_ENABLED=0 go build -ldflags="-X ebs-runner/pkg/runner.agentVersion=$(git rev-parse HEAD)" -o ebs-runner ./cmd/runner
```

构建容器镜像：

```bash
docker build --build-arg GIT_COMMIT="$(git rev-parse HEAD)" -t eulermaker/ebs-runner:dev .
```

`Runner.status.info.agentVersion` 上报构建时注入的完整 Git commit；未注入时为 `unknown`。

## 本地运行

Runner 启动前需要可访问的 Gateway，以及包含 MachineAccount 凭据的 JSON 文件。Artifact Manager 和 Git Server 地址通过 Gateway 的 `GET /apis/ebs/v1/service-endpoints` 获取：

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

Runner 启动时读取服务地址，此后每 600 秒加 ±30 秒抖动刷新，刷新失败继续使用最后一次成功的地址表。`--service-access-mode=internal`（默认）使用两类服务的 `runnerInternalURL`；`external` 使用 Artifact Manager 的 `runnerExternalURL`，源码直接访问 Job payload 的 `origin_url`，不依赖 Git Server 外网地址。配置来自 `Config/service-config`，Gateway 仅投影 Runner 地址，不返回管理面 `internalURL`。

Runner 写入本地 `payload.json` 前，按访问模式解析 `repo[].url` 中的 `artifact://{instance}/repositories/...`。源码处理规则如下：

- `internal`：将 `clone_url` 中的 `git-server:///git/...` 转换为 Git Server 内网 HTTP(S) 地址；普通克隆 URL 保持不变。服务基础地址不附加 `/git`，可以包含代理前缀；地址缺失或逻辑路径非法时明确报错。
- `external`：存在 `clone_url` 时，用 `origin_url` 替换它，包括原值为普通 URL 的情况；`origin_url` 必须为非空字符串，否则不启动构建容器。没有 `clone_url` 的自定义任务不受影响。

两种模式均保留 `origin_url`、`commit_id`，只修改本地 payload，不修改 Job 对象。上传与 Git 服务地址在 Job 启动时固定，刷新只影响新 Job。脚本仍检出固定提交；上游不可达、私有仓库认证缺失或 PR 提交无法获取时明确失败，不回退到 Git Server、默认分支或其他提交。Git 克隆及 HTTPS 信任由构建镜像负责；`use_git_lfs` 的固定 AtomGit 地址规则不变。

Runner 在启动时读取一次凭据文件；替换凭据后需要重启。凭据和签发的 Token 不应写入日志或 Runner 状态。

完整参数请执行：

```bash
go run ./cmd/runner --help
```

也可以在仓库根目录先启动控制面，再独立启动 Runner。Compose Runner 使用宿主机网络，同机部署可使用初始化配置中的回环地址。异机部署时设置可达的 `RUNNER_GATEWAY_URL` 和唯一的 `RUNNER_NAME`，并在 `Config/service-config` 中填写 Runner 与 Job 容器可访问的服务地址。`RUNNER_SERVICE_ACCESS_MODE` 映射到 `--service-access-mode`，默认 `internal`；设为 `external` 时配置制品实例的 `runnerExternalURL`，并确保构建容器可以访问上游源码仓库。

```bash
docker compose -f hacks/docker-compose.yml up -d --build
DOCKER_GID="$(stat -c %g /var/run/docker.sock)" \
  GIT_COMMIT="$(git rev-parse HEAD)" \
  docker compose -p eulermaker-runner -f hacks/docker-compose.runner.yml up -d --build
```

## Docker 后端

默认通过 Docker Engine Go SDK 连接 `/var/run/docker.sock`，支持 `DOCKER_HOST`、`DOCKER_TLS_VERIFY`、`DOCKER_CERT_PATH` 等 SDK 环境变量，自动协商 API 版本（最低 1.40）。不读取 Docker CLI 的 context 配置；非默认端点请显式设置 `DOCKER_HOST`。

Runner 无需安装或挂载 `docker` 可执行文件；容器化运行仍需挂载 Docker socket 和同路径的 Runner 工作目录，并保证 socket 访问权限。拉取私有镜像时读取 Runner 用户的 Docker 登录配置（`DOCKER_CONFIG` 或 `~/.docker/config.json`）；若配置了 credential helper，需单独提供对应 helper 可执行文件。

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

可在独立测试节点验证真实运行时（测试会拉取 BusyBox，并在独立测试 namespace 中创建、删除容器）：

```bash
EBS_TEST_CONTAINERD_ADDRESS=/run/containerd/containerd.sock \
  go test ./pkg/container -run TestContainerdIntegration -v
```
