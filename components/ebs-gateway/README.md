# ebs-gateway

`ebs-gateway` 是 EulerMaker 对外的 HTTP 入口，负责认证、Project 权限校验、限流、审计，并将合法请求代理到 `ebs-apiserver`。

## 构建与测试

在当前目录执行：

```bash
go test ./...
CGO_ENABLED=0 go build -o ebs-gateway ./cmd/server
```

构建容器镜像：

```bash
docker build -t eulermaker/ebs-gateway:dev .
```

## 本地运行

Gateway 需要一个 Base64 编码的 HMAC 密钥。以下命令生成仅供本地开发使用的临时密钥：

```bash
openssl rand -base64 32 > /tmp/ebs-jwt-secret
chmod 600 /tmp/ebs-jwt-secret
```

先启动 `ebs-apiserver`，然后执行：

```bash
go run ./cmd/server \
  --jwt-secret-file=/tmp/ebs-jwt-secret \
  --apiserver-addr=https://localhost:8443 \
  --insecure-skip-verify
```

`--insecure-skip-verify` 仅用于本地测试。生产环境应使用 HTTPS，并通过 `--apiserver-ca` 配置信任的 CA。

服务启动后可以检查健康状态：

```bash
curl http://localhost:8080/healthz
```

完整参数请执行：

```bash
go run ./cmd/server --help
```

## Docker Compose

在仓库根目录创建 Compose 使用的本地密钥，然后构建并启动完整开发环境：

```bash
openssl rand -base64 32 > hacks/ebs-gateway-jwt-secret
docker compose -f hacks/docker-compose.yml up -d --build
```
