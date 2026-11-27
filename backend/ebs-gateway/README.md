# ebs-gateway

EulerMaker 的对外 API 入口。Gateway 使用 Gin 注册显式路由，处理 JWT 认证、User 状态确认、Project 权限、限流和审计；业务对象仍由 `ebs-apiserver` 持久化。完整的资源和权限契约见[设计文档](../../docs/zh/design/ebs-gateway.md)。

## 目录

| 目录 | 职责 |
| --- | --- |
| `cmd/main.go` | 执行 Gateway 命令 |
| `cmd/app/server.go` | 定义 Cobra 参数、配置上游 TLS、启动 HTTP 服务 |
| `internal/gateway` | 注册 Gin 路由，组装中间件，按资源处理和转发请求 |
| `internal/middleware` | 身份认证、账号核验、路由身份准入、请求限流和审计 |
| `internal/identity` | JWT 签发、验证和身份类型 |
| `internal/iamclient` | 调用 apiserver 的 IAM 接口 |
| `internal/auth` | 密码注册/登录、机器凭据换 Token、Token 检查及密码修改；持有认证专用限流器 |
| `internal/oauth` | OAuth/OIDC 提供方认证、加密 Cookie、首次注册及结果确认 |
| `internal/policy` | 工程归属、Service 授权范围及资源字段写入保护 |
| `internal/mutation` | 严格解析 PUT/PATCH 并生成完整候选对象 |
| `internal/upstream` | 可信内部读取和流式反向代理 |
| `internal/limit` | 请求令牌桶 |

从 [router.go](internal/gateway/router.go) 查看路径、HTTP 方法和中间件顺序；从 [Authorize](internal/middleware/access.go) 和 [Policy](internal/policy/authorize.go) 查看鉴权处理。全局 [PrepareQuery](internal/middleware/query.go) 按 GET 方法和 Gin 路由模板仅允许 Project Job 列表、Runner 任务列表请求 Watch；其他已匹配路由在认证前返回 403，未匹配路由沿用 NoRoute。允许的 Watch 仍须通过后续认证、授权和查询检查。非 Watch 请求仅在配置了 `ListLimit` 的 GET 列表上规范化 `limit`；Project 及其公开子资源列表上限为 100，其余查询参数、对象读取和 Watch 参数保持原样。
全局 [Authenticate](internal/middleware/auth.go) 按方法和路由模板处理无需 Bearer 认证、可选认证和必须认证三类请求，默认必须认证。登录等凭据入口和 OpenAPI 不解析 Bearer Token；现有公开 GET 可匿名，但带 Token 时必须验证，Watch 必须认证。各路由不重复认证或通用限流，只负责业务授权；未触发限流时，未知路径匿名返回 401，认证成功后返回 404。
认证、身份准入及查询选项与路径、方法、额外中间件、Handler 一起声明在 [router.go](internal/gateway/router.go)，按认证、OAuth、透传及其他接口分别注册；透传路由统一设置 `upstream.Forward`。注册时生成同一份规则表，拒绝重复路由、空 Handler、空中间件及矛盾的认证或查询配置；显式 `AuthenticationNone` 自动补齐匿名准入，其余路由必须声明允许身份。`NewRouter` 在提供服务前发布 Router 独有的 `RoutePolicies` 只读副本，全局认证、准入和查询中间件共用该副本。工程归属与 Service 的 projects/permissions 由全局 `Authorize` 调用 Policy 校验，字段修改权限仍由业务 Handler 校验。
`GET /healthz` 在业务中间件挂载前直接注册 Handler，返回固定文本，不挂载 Recovery，也不经过审计、Watch 检查、认证和限流。业务链保持 Audit 在外、Recovery 在内，后续处理 panic 恢复后仍记录最终状态；认证和限流不包含健康检查特判。
通用限流由 [limit.go](internal/middleware/limit.go) 在业务链全局执行：认证后按身份类型/名称/IP 或匿名 IP 限流。登录、注册、机器换 Token、OAuth 和 Watch 并发限制继续保留，通用限流返回 429 和 `Retry-After: 1`。
普通公开 GET 完成全局认证、身份准入和查询检查后直接交给 `upstream.Forward`。工程 Job 列表的 Watch 保留 owner/member 校验；指定 Job 的 GET 由 `AssignedJob` 规则检查 Runner 分配关系，只投影读取名称、命名空间和 `status.runner`；状态更新读取并复用完整旧对象。Runner 创建与任务列表仅允许 Runner 身份，列表与 `/status` 读取仅允许 Ops/Admin，由全局准入判断。Runner 路径名称由全局 `RunnerSelf` 规则校验；路由内保留注册正文、普通对象更新字段、查询参数和 Watch 并发检查；所有业务路由都有准入规则的测试保障。
`/auth/check` 在全局业务鉴权前注册，仅执行认证、身份限流和 `AdmitIdentity(AllowAuthenticated)`，不要求 Service permission；所有允许 Service 的业务路由必须配置有效 permission。
Config 读写要求 Ops/Admin；`build-targets` 仅允许 User 身份，`service-endpoints` 仅允许 Runner 身份，均由统一规则表声明。公共写请求处理放在 `request.go`。
Policy 使用显式 `Rule` 声明允许身份、对象归属、Watch 归属和 Service permission，不再按资源字符串分派授权。全局 `Authorize` 适配请求参数、错误、Service 审计和请求级旧对象；Project owner/member 与 Service 工程范围校验由 Policy 完成。Project 用户更新在授权时读取完整旧对象，后续 PUT/PATCH 校验复用该对象，Service 更新仍按授权后的写入准备流程读取；仅校验归属的请求使用字段投影。
测试与业务文件同名，每个业务文件只对应一个 `*_test.go`，不额外拆分 internal 或 integration 测试文件。
认证接口及专用限流测试放在 [auth/handler_test.go](internal/auth/handler_test.go)，其余 Handler 测试与业务文件对应；全局请求链、路由规则和 OpenAPI 路由契约测试统一放在 [router_test.go](internal/gateway/router_test.go)。

Gateway 不开放 Job 创建、删除和本体 PUT/PATCH。用户中止任务使用 `POST /projects/{project}/jobs/{name}/abort`（前缀 `/apis/ebs/v1`），Runner 通过 `PATCH /projects/{project}/jobs/{name}/status` 更新执行状态（不开放 PUT），并继续校验分配归属和允许写入的字段。内部组件直连 apiserver 的接口不变。

ebsctl 在本地拒绝 Job 创建和删除；使用 Build 发起构建，删除所属 Build 触发 Controller 清理派生 Job。

Project 仅开放集合及对象路由，不开放 `/projects/{project}/status`。Script 读取仅允许已认证 User（含 Ops/Admin）和 Runner，Service Token 不可读取；Script 写入仍要求 Ops/Admin。

## 本地运行

先准备仅包含一个 Base64 编码、解码后至少 32 字节的 HMAC 密钥文件，然后运行：

```bash
go run ./cmd \
  --apiserver-addr https://localhost:8443 \
  --jwt-secret-file /path/to/jwt-secret
```

开发环境连接自签名 apiserver 时，可以配置 `--apiserver-ca`；仅在隔离测试环境中使用 `--insecure-skip-verify`。启用 apiserver 内部证书认证时，还需设置 `--apiserver-client-cert` 和 `--apiserver-client-key`，证书的 URI SAN 为 `spiffe://eulermaker/internal/ebs-gateway`。默认监听 `:8080`，`GET /healthz` 返回健康状态。构建镜像时 Dockerfile 使用同一个 `cmd` 入口。

日志统一使用 klog 标准参数，包括 `--v`（默认 `0`）、`--vmodule` 和 `--logtostderr`（默认 `true`）。启动、正常退出及请求审计使用 `InfoS`，启动或退出失败使用 `ErrorS`，进程退出前执行 `Flush`。verbosity 不等同于最低严重级别，默认保留请求审计。User 身份逐请求读取 apiserver，不缓存。

`--rate-limit-per-sec` / `--rate-limit-burst` 统一控制身份/IP 与匿名 IP 的额度，默认 `100/200`。匿名和已认证请求复用同一个限流器，通过 `anonymous/{clientIP}` 与 `{type}/{sub}/{clientIP}` 隔离计数；已认证用户读取公开资源也使用身份 key。限流按副本独立计算，当前不信任转发头；经 Nginx 或 NAT 接入的匿名请求及认证专用 IP 限制可能共用一个客户端 IP。无效 Token 在认证阶段被拒绝，不消耗身份限流额度。

进程收到 SIGINT/SIGTERM 后最多等待 15 秒排空在途请求，再退出；超时会强制关闭连接并返回错误。部署的终止宽限期应留出请求排空和连接清理时间。

Gateway 依赖仓库中的公共 `api` module，镜像构建上下文使用仓库根目录：

```sh
docker build -f backend/ebs-gateway/Dockerfile -t ebs-gateway:dev .
```

## 路由与安全约束

OAuth 按登录和回调请求初始化 Provider，不启动后台预热任务；初始化失败后，后续请求在 30 秒重试间隔结束后可再次尝试，成功后复用缓存。配置修改通过重启加载。

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
- `/apis/iam.ebs/v1/*` 仅允许 Admin 读取所有 User、修改/删除非管理员 User，以及管理 MachineAccount。
- `/apis/ebs/v1/*` 只注册设计文档列出的资源、对象和子资源路径；匿名只读白名单业务对象，原始 Config 仅限运维读写。Runner 可通过 `/apis/ebs/v1/service-endpoints` 读取 `Config/services` 中的 Runner 地址投影；响应为 `artifactManagers` 和 `gitServer`，均不包含管理地址，不直接开放 原始 Config。
- Service Token 每次请求读取机器账号的 `spec.projects` 和 `spec.permissions`，支持 `projects:update`、`builds:create`、`builds:abort` 三项独立权限；任一列表为空时拒绝资源操作。Admin 通过 `/auth/machineaccounts` 注册时传入授权字段；前端支持创建和编辑 service 账号的工程范围及权限。公开读取使用匿名请求，不携带 Service Token。
- Snapshot、BuildInfo、RpmRepo 的集合、对象及对象 `/status` 仅注册 GET。
- Gateway 不向上游注入用户或 Runner 身份头；转发时仍移除外部 Bearer Token。Config、Script 和 MachineAccount 的 PUT/PATCH 保留原始方法和正文，由 apiserver 校验；Runner `/status` 同样原样转发，apiserver 保留旧 spec 和受保护 metadata；其他更新先读取服务端对象、生成完整候选对象并校验受保护字段，然后以 PUT 转发。
- Build 创建、中止及工程 Job Watch 要求用户是工程 owner/member，Ops/Admin 不享有跨工程特权；普通公开读取不校验成员关系。Service Token 的 Build 创建和中止仍分别校验工程范围及 `builds:create`、`builds:abort` 权限。
- Build 创建、中止、删除、Job 中止和 Runner 删除通过授权后直接代理，不预读或重写正文；输入与状态校验由 apiserver 负责。Project 创建/更新及 Job 状态更新在路由中显式挂载正文处理，保留身份和字段保护。
- Build 的 PUT/PATCH 不开放；Job `/abort` 仅 Project owner/member 用户可调用；Runner 只可管理自身对象和已分配 Job。

上游 Transport 的响应头超时为 30 秒，从请求正文发送完成后开始计时；透传请求等待响应头超时返回 502。收到响应头后的 Watch 流不受该超时限制，仍持续转发，并随客户端取消关闭上游请求。内部 `Do` 调用另有包含正文读取的 30 秒总超时。

验证代码：

```bash
go test ./...
go vet ./...
```
