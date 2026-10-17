# ebs-runner

`ebs-runner` 是 EulerMaker 的任务执行代理。它通过 `ebs-gateway` 注册执行机、接收已分配的 Job，并将日志和产物上传到 Artifact Manager。

CT Job 的 `spec.scriptRefs` 非空时，Runner 按顺序经 Gateway 拉取所有 Script，写入 `/workspace/scripts/{name}`，并将第一项作为容器 ENTRYPOINT。Runner 进程内按 Script 名称、UID、resourceVersion 缓存正文；Job 指定的 UID 或 resourceVersion 变化时重新拉取。数组为空时使用镜像入口。

## 代码结构

```text
cmd/runner/     命令入口
pkg/runner/     Agent 装配、注册心跳、Job 生命周期和工作目录清理
pkg/gateway/    Gateway 客户端、资源传输类型、机器凭据与 Token 刷新
pkg/executor/   执行器、脚本缓存、payload 落盘和日志时间戳
pkg/container/ 容器运行时接口与 Docker CLI 实现
pkg/artifact/  Artifact 地址路由、产物与日志上传、上传回执和保留期清理
```

测试随所属模块放置。`runner` 装配各模块；底层包不反向依赖 `runner`。当前 CT 执行后端仍为 Docker。

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
