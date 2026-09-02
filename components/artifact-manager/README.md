# Artifact Manager

Artifact Manager 接收 Runner 上传的构建产物和实时容器日志，并把正文、元数据、幂等记录和日志 JSON Lines 索引保存到独立的本地持久化目录。

## 构建与测试

在当前目录执行：

```bash
go test ./...
CGO_ENABLED=0 go build -o artifact-manager ./cmd/server
```

构建容器镜像：

```bash
docker build -t eulermaker/artifact-manager:dev .
```

## 本地运行

先启动 Gateway，然后执行：

```bash
go run ./cmd/server \
  --listen=:8081 \
  --data-dir=/var/lib/ebs-artifacts \
  --gateway-url=http://localhost:8080
```

上述 HTTP Gateway 地址只适用于本地开发。生产环境应使用 HTTPS，并通过 `--gateway-ca` 配置信任的 CA。

服务启动后可以检查健康状态：

```bash
curl http://localhost:8081/healthz
curl http://localhost:8081/readyz
```

完整参数请执行：

```bash
go run ./cmd/server --help
```

## Docker Compose

在仓库根目录构建并启动完整开发环境：

```bash
docker compose -f hacks/docker-compose.yml up -d --build
```

Compose 环境中，Artifact Manager 在 `8081` 端口提供服务，并使用 `http://ebs-gateway:8080` 访问 Gateway。
