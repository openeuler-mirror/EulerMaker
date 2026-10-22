# Git Server

Git Server 为 EulerMaker 内部组件维护远端源码仓库的本地只读镜像。服务异步执行 mirror clone、fetch 和删除，通过 Smart HTTP 提供只读 clone/fetch，并提供有限的内部 Git 查询接口。

详细行为和并发契约见 `docs/zh/design/git-server.md`。

## 本地运行

```bash
go run ./cmd/git-server \
  --listen-address=:8080 \
  --data-dir=/tmp/eulermaker-git
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

健康检查为 `/healthz`，就绪检查为 `/readyz`。默认 HTTP 端口是 `8080`，管理接口与 `/git/` 克隆入口共用该端口。运行环境必须安装 Git 及 `git-http-backend`。

同步成功后返回 `clone_path`，例如 `/git/gitee.com/src-openeuler/gcc.git`。手动克隆时，将可访问的 HTTP(S) 服务入口与该转义路径拼接：

```bash
git clone http://localhost:8080/git/gitee.com/src-openeuler/gcc.git
```

`--clone-workers` 默认为 100，限制同时处理的 Smart HTTP 请求数；`--operation-timeout` 同时限制单个克隆请求，默认 10 分钟。入口只接受 Git clone/fetch，不提供 push、裸仓库文件浏览或 LFS 服务。需要集群外访问时，通过反向代理仅暴露 `/git/` 并配置 HTTPS，内部管理接口保持受限。

Compose 使用 `8084:8080`，管理面通过容器端口 `8080` 访问；Kubernetes 模板使用 Service 的 `8080` 端口。Git Server 不配置克隆基础地址，返回路径与部署入口无关。

Controller Manager 客户端将路径转换为 `git-server:///git/...`，存入 Snapshot 和 Job 的克隆地址字段。Runner 解析尚未实现，当前默认脚本无法直接克隆该逻辑地址；本阶段不能作为完整构建链路启用。

`Config/service-config.gitServer` 的 `runnerInternalURL`、`runnerExternalURL` 用于配置 Runner 的内外网克隆服务入口，均可留空；地址不附加 `/git`。默认内网入口为 `http://127.0.0.1:8084`，仅适用于同机宿主机网络的 Compose Runner。本阶段只保存这些配置，不改变 Runner 行为。

## 认证配置

通过 `--auth-config=/path/auth.toml` 加载 HTTPS 或 SSH 凭证。配置文件应由 Secret 只读挂载；服务不会把凭证写入仓库 origin。格式示例见设计文档。

## 测试

```bash
go test ./...
```
