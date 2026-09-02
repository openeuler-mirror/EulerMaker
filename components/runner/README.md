# ebs-runner

`ebs-runner` 是 EulerMaker 的任务执行代理。它通过 `ebs-gateway` 注册执行机、接收已分配的 Job，并将日志和产物上传到 Artifact Manager。

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

Runner 启动前需要可访问的 Gateway、Artifact Manager，以及包含 MachineAccount 凭据的 JSON 文件：

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
  --artifact-manager=http://localhost:8081 \
  --machine-credential-file=./runner-machine-credential.json \
  --name=runner-ct-x86-01 \
  --type=ct \
  --insecure-skip-verify
```

`--insecure-skip-verify` 仅用于本地测试。生产环境应使用 HTTPS，并通过 `--gateway-ca` 和 `--artifact-manager-ca` 配置信任的 CA。

Runner 在启动时读取一次凭据文件；替换凭据后需要重启。凭据和签发的 Token 不应写入日志或 Runner 状态。

完整参数请执行：

```bash
go run ./cmd/runner --help
```

也可以在仓库根目录构建并启动完整开发环境：

```bash
docker compose -f hacks/docker-compose.yml up -d --build
```
