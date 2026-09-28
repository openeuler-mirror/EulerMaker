# ebs-gateway

EulerMaker 的对外 API 入口。Gateway 使用 Gin 注册显式路由，处理 JWT 认证、User 状态确认、Project 权限、限流和审计；业务对象仍由 `ebs-apiserver` 持久化。完整的资源和权限契约见[设计文档](../../docs/zh/design/ebs-gateway.md)。

## 目录

| 目录 | 职责 |
| --- | --- |
| `cmd/main.go` | 执行 Gateway 命令 |
| `cmd/app/server.go` | 定义 Cobra 参数、配置上游 TLS、启动 HTTP 服务 |
| `internal/route` | 注册 Gin 路由、组织中间件 |
| `internal/handler` | 处理 Auth、IAM 与资源请求，执行鉴权和转发 |
| `internal/identity` | JWT 签发、验证和身份类型 |
| `internal/iam` | 调用 apiserver 的 IAM 接口 |
| `internal/policy` | Project、Runner、Config、Script 等授权及写入保护 |
| `internal/mutation` | 严格解析 PUT/PATCH 并生成完整候选对象 |
| `internal/upstream` | 可信内部读取和流式反向代理 |
| `internal/limit` | 请求令牌桶 |

## 本地运行

先准备仅包含一个 Base64 编码、解码后至少 32 字节的 HMAC 密钥文件，然后运行：

```bash
go run ./cmd \
  --apiserver-addr https://localhost:8443 \
  --jwt-secret-file /path/to/jwt-secret
```

开发环境连接自签名 apiserver 时，可以配置 `--apiserver-ca`；仅在隔离测试环境中使用 `--insecure-skip-verify`。默认监听 `:8080`，`GET /healthz` 返回健康状态。构建镜像时 Dockerfile 使用同一个 `cmd` 入口。

## 路由与安全约束

- `/auth/*` 负责注册、登录、Runner token 交换、token 校验和本人密码修改；只有 Admin 能创建 MachineAccount。
- `/apis/iam.ebs/v1/*` 仅允许 Admin 管理非管理员 User 和 MachineAccount。
- `/apis/ebs/v1/*` 只注册设计文档列出的资源、对象和子资源路径；匿名只读白名单业务对象，Config 按可见性读取。
- Snapshot、BuildInfo、RpmRepo 的集合、对象及对象 `/status` 仅注册 GET。
- Gateway 丢弃客户端传入的所有 `X-EBS-*` 头，只有授权成功后才向上游注入可信身份头。PUT/PATCH 先读取服务端对象、生成完整候选对象并校验受保护字段，然后以 PUT 转发。
- Build 的 PUT/PATCH 不开放；Job `/abort` 仅 Project owner/member 用户可调用；Runner 只可管理自身对象和已分配 Job。

验证代码：

```bash
go test ./...
go vet ./...
```
