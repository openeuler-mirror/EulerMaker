# Artifact Manager

Artifact Manager 接收 Runner 上传的构建产物和实时容器日志，并把正文、元数据、幂等记录和日志 JSON Lines 索引保存到独立的本地持久化目录。

## 启动

```bash
go run ./cmd/server \
  --listen=:8080 \
  --data-dir=/var/lib/ebs-artifacts \
  --gateway-url=http://localhost:8080
```

Compose 环境中，Artifact Manager 使用 `http://ebs-gateway:8080` 访问 Gateway，并通过宿主机端口 `8081` 提供服务。

协议、数据结构、提交顺序和恢复规则见 [设计文档](../../docs/zh/design/artifact-manager.md)。

## 验证

```bash
go test ./...
```
