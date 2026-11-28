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

`cmd/app` 分别创建资源、认证和 OAuth Handler，再交给 Gateway 注册路由；`auth` 和 `oauth` 不反向依赖 `gateway`。建议按以下顺序阅读代码：

1. [router.go](internal/gateway/router.go)：全局中间件顺序，以及各路由的方法、路径、认证模式、授权规则、查询选项和业务中间件。
2. [auth.go](internal/middleware/auth.go)、[access.go](internal/middleware/access.go)、[query.go](internal/middleware/query.go)：身份解析、鉴权的 HTTP 适配、Watch 检查和列表分页。
3. [Policy](internal/policy/authorize.go)：普通 Go 参数和结果承载的身份准入、Service 工程/操作权限及对象归属判断。
4. [request.go](internal/gateway/request.go) 和各资源 Handler：正文准备、完整对象 PATCH、字段权限校验；[upstream](internal/upstream/client.go) 负责内部读取和流式代理。

路由定义在注册时生成同一份规则表并发布 Router 独有的只读副本，拒绝重复路由、空处理函数和矛盾规则。显式 `AuthenticationNone` 且授权规则完全为空时自动补齐匿名准入；其余路由必须声明允许身份。用户 Build 创建、中止和工程 Job Watch 均要求 owner/member，Ops/Admin 不例外；Service 业务路由必须声明有效的 permission。

业务链为审计 → Recovery → 查询检查 → 认证 → 身份限流 → 授权 → 业务处理。公开 GET 可匿名，但带 Token 时必须验证，失败不得降级；未知路径匿名返回 401、认证后返回 404。健康检查在业务链之前注册，不审计、认证或限流；`/auth/check` 仅做认证、身份准入和限流，不要求 Service 工程授权。

`PrepareQuery` 仅允许工程 Job 列表和 Runner 任务列表 Watch，其他已匹配路由的 Watch 在认证前返回 403。非 Watch 请求仅对配置 `ListLimit` 的 GET 列表规范化分页，公开工程及其子资源列表上限为 100。通用限流按身份类型/名称/IP 或匿名 IP 隔离；登录、机器换 Token、OAuth 和 Watch 并发限制另行保留。

本地注册只使用全局匿名限流。密码登录的专用限流仅按用户名计数，每分钟补充 5 次额度，桶容量为 5；机器换 Token 仅按 clientID 计数，每分钟补充 10 次额度，桶容量为 10。两个专用桶均不按 IP 区分。全局匿名 IP 限流及 OAuth IP 限流仍然生效；代理后的请求可能共享这些额度，各 Gateway 副本独立计数。

通用及密码登录、机器换 Token 的令牌桶各自最多保存 4096 个 key。容量满时先回收额度已恢复的桶，否则淘汰最久未访问的桶，为新 key 腾出位置；被淘汰的账号再次访问会获得完整额度。OAuth 的独立限流实现不受此策略影响。

`internal/auth` 读取的 JSON 请求正文固定限制为 64 KiB，覆盖本地注册、密码登录、机器换 Token、密码修改和机器账号注册。超过上限返回 400，不调用 IAM；不受业务资源参数 `--max-request-body-bytes` 影响。

Project 用户更新在授权时读取完整对象并供正文校验复用；Job 状态更新同样复用 `AssignedJob` 已读取的对象。仅校验归属的 Project/Job 请求和 IAM 身份查询使用字段投影。Runner 路径名称统一由 `RunnerSelf` 校验，路由内保留注册正文、普通对象字段、查询参数及 Watch 并发检查。普通公开读取和无需正文改写的写入直接转发，具体处理链见[设计文档第五章](../../docs/zh/design/ebs-gateway.md#五路由设计)。

Config 仅 Ops/Admin 可读写；`build-targets` 仅 User 可读，`service-endpoints` 仅 Runner 可读。Script 读取允许 User（含 Ops/Admin）和 Runner，不允许 Service；写入要求 Ops/Admin。Project 不开放 `/status`，状态通过普通 GET 的 `includeFields=status` 获取。

Gateway 不开放 Job 创建、删除、本体 PUT/PATCH 或 `/status` PUT。用户通过 Build 发起构建或触发清理，通过 Job `/abort` 中止任务；Runner 使用 PATCH 更新已分配 Job 状态，Gateway 校验后以 PUT 转发。ebsctl 在本地拒绝 Job 创建和删除，内部组件直连 apiserver 的接口不变。

测试按业务文件归类：认证接口及专用限流在 [auth/handler_test.go](internal/auth/handler_test.go)，全局链路、路由规则和 OpenAPI 路由契约在 [router_test.go](internal/gateway/router_test.go)。

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
