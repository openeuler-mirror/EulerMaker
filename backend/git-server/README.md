# Git Server

Git Server 为 EulerMaker 内部组件维护远端源码仓库的本地只读镜像。服务异步执行 mirror clone、fetch 和删除，通过 git daemon 提供只读 clone，并提供有限的内部 Git 查询接口。

详细行为和并发契约见 `docs/zh/design/git-server.md`。

## 本地运行

```bash
go run ./cmd/git-server \
  --listen-address=:8080 \
  --data-dir=/tmp/eulermaker-git \
  --git-daemon-address=:9418 \
  --clone-base-url=git://localhost:9418
```

同步公开仓库：

```bash
curl -sS -X POST http://localhost:8080/api/v1/repo/sync \
  -H 'Content-Type: application/json' \
  -d '{"origin_url":"https://gitee.com/src-openeuler/gcc.git"}'
```

查询最近一次成功同步结果：

```bash
curl -sS -X POST http://localhost:8080/api/v1/repo/status \
  -H 'Content-Type: application/json' \
  -d '{"origin_url":"https://gitee.com/src-openeuler/gcc.git"}'
```

删除仓库：

```bash
curl -i -X POST http://localhost:8080/api/v1/repo/delete \
  -H 'Content-Type: application/json' \
  -d '{"origin_url":"https://gitee.com/src-openeuler/gcc.git"}'
```

健康检查为 `/healthz`，就绪检查为 `/readyz`。默认 HTTP 端口是 `8080`，只读 Git 协议端口是 `9418`。

## 认证配置

通过 `--auth-config=/path/auth.toml` 加载 HTTPS 或 SSH 凭证。配置文件应由 Secret 只读挂载；服务不会把凭证写入仓库 origin。格式示例见设计文档。

## 测试

```bash
go test ./...
```
