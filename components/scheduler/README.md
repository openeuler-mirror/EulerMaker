# Scheduler

Scheduler 通过 ebs-apiserver List/Watch `Job` 和 `Runner`，为 Pending 且尚未绑定的 Job 选择满足运行时、标签、污点和 CPU/内存容量要求的 Runner，并通过 Job status 完成绑定。

## 构建与测试

```bash
go test ./...
go test -race ./...
CGO_ENABLED=0 go build -o scheduler ./cmd/scheduler
```

从仓库根目录构建镜像：

```bash
docker build -f components/scheduler/Dockerfile -t eulermaker/scheduler:dev .
```

## 运行

Scheduler 通过 HTTPS 直连 ebs-apiserver：

```bash
./scheduler \
  --apiserver=https://ebs-apiserver:8443 \
  --apiserver-ca=/etc/ebs/certs/ca.crt
```

开发环境可显式设置 `--insecure-skip-verify` 跳过服务端证书校验，生产环境不应启用。当前 Scheduler 不提供客户端证书配置；待 ebs-apiserver 支持客户端证书认证后再补充。进程在 `:8080` 暴露 `/healthz`、`/readyz` 和 `/metrics`。

完整设计、并发约束和错误恢复语义见 `docs/zh/design/scheduler.md`。
