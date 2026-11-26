# ebs-gateway

EulerMaker 的对外 API 入口。Gateway 使用 Gin 注册显式路由，处理 JWT 认证、User 状态确认、Project 权限、限流和审计；业务对象仍由 `ebs-apiserver` 持久化。完整的资源和权限契约见[设计文档](../../docs/zh/design/ebs-gateway.md)。

## 目录

| 目录 | 职责 |
| --- | --- |
| `cmd/main.go` | 执行 Gateway 命令 |
| `cmd/app/server.go` | 定义 Cobra 参数、配置上游 TLS、启动 HTTP 服务 |
| `internal/gateway` | 注册 Gin 路由，组装中间件，按资源处理和转发请求 |
| `internal/middleware` | 身份认证、账号核验、Admin 门卫、请求限流和审计 |
| `internal/identity` | JWT 签发、验证和身份类型 |
| `internal/iam` | 调用 apiserver 的 IAM 接口 |
| `internal/oauth` | OAuth/OIDC 提供方认证、加密 Cookie、首次注册及结果确认 |
| `internal/policy` | Project、Runner、Config、Script 等授权及写入保护 |
| `internal/mutation` | 严格解析 PUT/PATCH 并生成完整候选对象 |
| `internal/upstream` | 可信内部读取和流式反向代理 |
| `internal/limit` | 请求令牌桶 |

从 [router.go](internal/gateway/router.go) 查看路径、HTTP 方法和中间件顺序；从 [resources.go](internal/gateway/resources.go) 查看资源请求的授权和公开读取处理。全局 [ValidateWatchRoute](internal/middleware/watch.go) 按 GET 方法和 Gin 路由模板仅允许 Project Job 列表、Runner 任务列表请求 Watch；其他已匹配路由在认证前返回 403，未匹配路由沿用 NoRoute。允许的 Watch 仍须通过后续认证、授权和查询检查。
全局 [AuthenticateRequest](internal/middleware/auth.go) 按方法和路由模板处理无需 Bearer 认证、可选认证和必须认证三类请求，默认必须认证。登录等凭据入口和 OpenAPI 不解析 Bearer Token；现有公开 GET 可匿名，但带 Token 时必须验证，Watch 必须认证。各路由不重复认证或通用限流，只负责业务授权；未触发限流时，未知路径匿名返回 401，认证成功后返回 404。
认证与 Watch 路由规则集中在 [routes.go](internal/middleware/routes.go)：`unauthenticatedRoutes`、`optionalAuthenticationRoutes`、`watchRoutes` 分别列出无需认证、可选认证和允许 Watch 的路由，每行一个方法/路径组合。未列出的业务路由默认必须认证、默认不支持 Watch；`auth.go` 和 `watch.go` 仅执行规则。
`GET /healthz` 在业务中间件挂载前直接注册 Handler，返回固定文本，不挂载 Recovery，也不经过审计、Watch 检查、认证和限流。业务链保持 Audit 在外、Recovery 在内，后续处理 panic 恢复后仍记录最终状态；认证和限流不包含健康检查特判。
通用限流由 [limit.go](internal/middleware/limit.go) 在业务链全局执行：认证前按 IP 限流，认证后按身份类型/名称/IP 或匿名 IP 限流。登录、注册、机器换 Token、OAuth 和 Watch 并发限制继续保留，通用限流返回 429 和 `Retry-After: 1`。
指定 Job 的 GET 路由显式挂载 `runnerJobReadGuard` 检查 Runner 归属，漏挂时拒绝转发。Runner 各路由使用独立的业务 Guard，在公共认证和限流之后完成该操作的全部检查，再交给 `forward`；任务列表的 `runnerJobsGuard` 同时检查授权、查询参数和 Watch 并发。Runner 路由不使用检查完成标记，Guard 挂载由路由权限测试覆盖。
Config 的读写统一要求 Ops/Admin，`GET /apis/ebs/v1/build-targets` 向已认证用户单独投影发行版及架构，与 `service-endpoints` 共用认证和限流中间件，各自的身份限制由 Handler 校验；公共写请求处理放在 `request.go`。
Policy 的公共入口负责分派，Project、Runner 的授权及写入规则集中到 `project.go`、`runner.go`。
测试与业务文件同名，每个业务文件只对应一个 `*_test.go`，不额外拆分 internal 或 integration 测试文件。
Handler 私有逻辑测试放在对应文件中，真实 HTTP 请求链、路由注册和 OpenAPI 路由契约测试放在 [router_test.go](internal/gateway/router_test.go)。

## 本地运行

先准备仅包含一个 Base64 编码、解码后至少 32 字节的 HMAC 密钥文件，然后运行：

```bash
go run ./cmd \
  --apiserver-addr https://localhost:8443 \
  --jwt-secret-file /path/to/jwt-secret
```

开发环境连接自签名 apiserver 时，可以配置 `--apiserver-ca`；仅在隔离测试环境中使用 `--insecure-skip-verify`。启用 apiserver 内部证书认证时，还需设置 `--apiserver-client-cert` 和 `--apiserver-client-key`，证书的 URI SAN 为 `spiffe://eulermaker/internal/ebs-gateway`。默认监听 `:8080`，`GET /healthz` 返回健康状态。构建镜像时 Dockerfile 使用同一个 `cmd` 入口。

日志统一使用 klog 标准参数，包括 `--v`（默认 `0`）、`--vmodule` 和 `--logtostderr`（默认 `true`）。启动、正常退出及请求审计使用 `InfoS`，启动或退出失败使用 `ErrorS`，进程退出前执行 `Flush`。verbosity 不等同于最低严重级别，默认保留请求审计。User 身份逐请求读取 apiserver，不缓存。

`--ip-rate-limit-per-sec` / `--ip-rate-limit-burst` 控制认证前的每 IP 额度，默认 `1000/2000`；`--rate-limit-per-sec` / `--rate-limit-burst` 统一控制身份/IP 与匿名 IP 的额度，默认 `100/200`。匿名和已认证请求复用同一个限流器，通过 `anonymous/{clientIP}` 与 `{type}/{sub}/{clientIP}` 隔离计数；已认证用户读取公开资源也使用身份 key。限流按副本独立计算，当前不信任转发头；经 Nginx 或 NAT 接入的请求可能共用一个客户端 IP，应按汇总流量调整前置 IP 额度。

进程收到 SIGINT/SIGTERM 后最多等待 15 秒排空在途请求，再退出；超时会强制关闭连接并返回错误。部署的终止宽限期应留出排空及后台任务退出时间。

Gateway 依赖仓库中的公共 `api` module，镜像构建上下文使用仓库根目录：

```sh
docker build -f backend/ebs-gateway/Dockerfile -t ebs-gateway:dev .
```

## 路由与安全约束

OAuth 默认关闭，使用 `--oauth-config-file=/path/to/oauth.yaml` 启用，配置示例和注册恢复规则见 [OAuth 设计](../../docs/zh/design/ebs-gateway.md#108-配置与部署)。支持 OIDC discovery 或固定端点；普通 OAuth 的 `userinfo-sub` adapter 要求固定用户接口返回稳定且不复用的 `sub`，不能直接套用仅返回用户名的平台。

指定 `--oauth-config-file` 即启用 OAuth，自动关闭本地密码登录和注册（`/auth/login`、`/auth/register` 返回 403），不提供独立开关；不指定配置文件时使用本地登录和注册。无效配置导致启动失败，提供方暂不可用不会自动回退密码登录。首次 OAuth 登录未关联账号时统一进入补全注册流程，既有 JWT 和 Runner 认证不受影响。

OAuth 配置中的密钥直接内嵌，不再引用其他密钥文件（以下为配置片段）：

```yaml
contextKeys:
  activeKeyID: key-1
  keys:
    key-1: <独立生成的32字节密钥的Base64值>
provider:
  clientSecret: <第三方应用密钥>
  # 其他 provider 配置见设计文档
```

Cookie 密钥可用 `openssl rand -base64 32` 生成。将完整 `oauth.yaml` 保存到一个 Kubernetes Secret，再只读挂载给 `--oauth-config-file`；所有 Gateway 副本共享配置及密钥环，更新后重启加载。配置含密钥，不放入 ConfigMap、不提交真实配置、不写入日志；JWT 密钥仍保持独立。不新增 Gateway→ES 连接。OAuth IAM 接口要求 Gateway 的内部 mTLS 身份，不能使用匿名 apiserver 连接。

Gateway 端已提供 `/auth/providers` 和 `/auth/oauth/*`，前端已接入回调、首次注册及结果确认。提供方回调登记为 `https://<页面域名>/auth/callback`，前端收到 code/state 后同源 POST 到 `/auth/oauth/{provider}/callback`；不能将提供方配置成直接 GET 该 API。社区基本注册和登录已验证，安全及异常场景仍需验收。Kubernetes 操作步骤见 [OAuth 部署](../../deploy/kubernetes/README.md#71-启用社区-oauth-登录)。

- `/auth/*` 负责注册、登录、Runner token 交换、token 校验和本人密码修改；只有 Admin 能创建 MachineAccount。
- `/apis/iam.ebs/v1/*` 仅允许 Admin 管理非管理员 User 和 MachineAccount。
- `/apis/ebs/v1/*` 只注册设计文档列出的资源、对象和子资源路径；匿名只读白名单业务对象，原始 Config 仅限运维读写。Runner 可通过 `/apis/ebs/v1/service-endpoints` 读取 `Config/services` 中的 Runner 地址投影；响应为 `artifactManagers` 和 `gitServer`，均不包含管理地址，不直接开放 原始 Config。
- Service Token 每次请求读取机器账号的 `spec.projects` 和 `spec.permissions`，支持 `projects:update`、`builds:create`、`builds:abort` 三项独立权限；任一列表为空时拒绝资源操作。Admin 通过 `/auth/machineaccounts` 注册时传入授权字段，前端授权表单尚未接入。公开读取使用匿名请求，不携带 Service Token。
- Snapshot、BuildInfo、RpmRepo 的集合、对象及对象 `/status` 仅注册 GET。
- Gateway 不向上游注入用户或 Runner 身份头；转发时仍移除外部 Bearer Token。PUT/PATCH 先读取服务端对象、生成完整候选对象并校验受保护字段，然后以 PUT 转发。
- Build 的 PUT/PATCH 不开放；Job `/abort` 仅 Project owner/member 用户可调用；Runner 只可管理自身对象和已分配 Job。

验证代码：

```bash
go test ./...
go vet ./...
```
