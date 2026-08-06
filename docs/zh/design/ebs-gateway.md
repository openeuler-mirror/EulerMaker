# ebs-gateway 设计

## 一、定位

`ebs-gateway` 是 EulerMaker 对外请求入口，位于客户端和 `ebs-apiserver` 之间，负责令牌认证、用户状态检查、Project 权限校验、审计、限流和请求转发。

```text
用户 / 外部系统 / runner
        |
        v
ebs-gateway
        |
        v
ebs-apiserver
        |
        v
etcd / Elasticsearch
```

`ebs-gateway` 保持无状态，User 由 `ebs-apiserver` 的用户管理插件提供。gateway 通过 User API 获取用户状态，并基于认证身份限制用户只能访问自己有权限的 Project。

## 二、设计目标

| 目标 | 说明 |
|------|------|
| 统一入口 | 用户、外部系统和 runner 统一通过 gateway 访问 `ebs/v1` API |
| 统一认证 | gateway 调用 apiserver IAM 模块验证账号密码，确认 User 状态后由 gateway 签发 JWT；业务请求进入 apiserver 前由 gateway 完成 token 校验 |
| 用户校验 | 通过 apiserver 的 User API 检查用户是否存在并启用 |
| 请求转发 | 将合法请求反向代理到 `ebs-apiserver` |
| Watch 透传 | 为 apiserver 支持 watch 的 Job、Runner 透传长连接和流式响应 |
| 审计记录 | 记录请求路径、方法、状态码、耗时和调用方信息 |
| 限流保护 | 对调用方进行基础限流，保护 apiserver |
| 用户隔离 | 基于用户身份和 Project labels 限制访问范围，支持 Project 在用户间共享 |
| 无业务状态 | 不保存资源对象，不直接访问主存储 |

## 三、资源归属边界

### 3.1 用户模型

用户以集群级 `User` 对象表示。`User.metadata.name` 是全局唯一的稳定用户标识，与普通用户 JWT 的 `sub` 一致。

User 由 `ebs-apiserver` 的用户管理插件提供，建议使用独立 API group，避免与构建资源耦合：

```text
/apis/iam.ebs/v1/users
/apis/iam.ebs/v1/users/{name}
```

`ebs-apiserver` 将 `ebs.io/owner-user` 和 `ebs.io/member-user.*` 视为普通 labels，只负责存储、查询和 label selector，不解释用户权限语义，也不执行 Project 用户权限校验。权限 label 的格式校验、注入、修改保护和访问控制全部由 gateway 完成。

最小 User 模型：

```yaml
apiVersion: iam.ebs/v1
kind: User
metadata:
  name: alice
spec:
  enabled: true
  displayName: Alice
  email: alice@example.com
```

约束如下：

- `metadata.name` 是稳定用户标识，与用户 JWT 的 `sub` 一致。
- `spec.enabled` 控制用户是否可以访问业务 API。
- apiserver IAM 模块验证密码，gateway 签发 JWT；User API 保存 gateway 鉴权需要的用户状态。
- 同时包含 `ebs:system` 和 `ebs:user-admin` 的管理调用方负责 User 的创建、删除和启停。
- gateway 可以短期缓存 User 查询结果，但 User API 是用户状态的权威来源。

### 3.2 业务资源归属

| 资源 | 归属规则 |
|------|----------|
| User | 集群级资源，由 apiserver 用户管理插件提供；`metadata.name` 是用户唯一标识 |
| Project | 集群级资源，`metadata.name` 是项目唯一标识，访问权限由 Project labels 表达 |
| Snapshot | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| Build | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| BuildInfo | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| RpmRepo | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| Job | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| Runner | 集群级资源，不归属于 Project |

Project 级资源通过 Project API 创建：

```text
/apis/ebs/v1/projects/{project}/snapshots
/apis/ebs/v1/projects/{project}/builds
/apis/ebs/v1/projects/{project}/buildinfos
/apis/ebs/v1/projects/{project}/rpmrepos
/apis/ebs/v1/projects/{project}/jobs
```

`{project}` 会由 apiserver 映射到对象的 `metadata.namespace`。gateway 不给 Snapshot、Build、BuildInfo、RpmRepo、Job 注入用户字段，这些对象通过 Project 命名空间继承 Project 访问权限。

Project 创建和更新时，gateway 使用 labels 表达 Project 的 owner user 和 member users：

```yaml
metadata:
  labels:
    ebs.io/owner-user: alice
    ebs.io/member-user.bob: "true"
    ebs.io/member-user.carol: "true"
```

label 语义：

| Label | 说明 |
|------|------|
| `ebs.io/owner-user` | Project 所有者用户名，单值 |
| `ebs.io/member-user.<username>` | 允许操作该 Project 的成员用户名，值固定为 `"true"` |

`<username>` 必须是已存在的 User `metadata.name`。User 名称满足 DNS1123 label 约束，因此可以同时用作 `owner-user` 的 label value 和 `member-user.<username>` 的 label key name 片段。

普通用户创建 Project 时，gateway 必须写入或覆盖 `ebs.io/owner-user=<jwt.sub>`，客户端传入的 owner user 不可信。system token 创建 Project 时必须显式提供 `ebs.io/owner-user`，gateway 校验该 User 存在且 `spec.enabled=true`，否则返回 403。

更新 Project 时，gateway 必须保护 `ebs.io/owner-user` 不被普通用户伪造或篡改。成员用户 label 用于表达共享权限，只有 owner user 和 system token 可以增删；新增的 member user 必须对应已存在且启用的 User。

必须经过 gateway 的受信任自动化调用方使用 system token 访问全局 API，Runner 使用独立的 runner token。普通用户 token 只有在对应 User 存在且已启用时才有效；gateway 直接使用 JWT `sub` 作为用户身份，并限制用户只能访问自己拥有或被授权为 member 的 Project 和 Project 子资源。

## 四、请求处理流程

业务请求的处理链：

```text
Request
  -> Audit
  -> Auth
  -> UserResolve
  -> RateLimit
  -> InjectHeaders
  -> ProjectAuthorize
  -> AccessLabels
  -> ReverseProxy
  -> Response
```

### 4.1 Audit

gateway 记录结构化审计日志，建议包含：

| 字段 | 说明 |
|------|------|
| `method` | HTTP 方法 |
| `path` | 请求路径 |
| `query` | 查询参数 |
| `status` | 响应状态码 |
| `latency_ms` | 请求耗时 |
| `client_ip` | 客户端地址 |
| `user` | 认证后的调用方标识 |
| `token_type` | `user`、`runner`、`system` 或 `user-admin` |
| `jti` | JWT 唯一标识，不记录完整 token |
| `user_agent` | 客户端 User-Agent |

### 4.2 Auth

客户端通过 Bearer Token 访问 gateway：

```text
Authorization: Bearer <token>
```

gateway 提供账号密码登录入口。Gateway 将账号密码提交给 apiserver IAM 模块验证，认证成功后再读取 User，确认用户存在且 `spec.enabled=true`；以上检查全部通过后，由 gateway 使用 HMAC 密钥签发 JWT。Apiserver IAM 模块只负责密码验证，不签发、解析或刷新 JWT。后续业务请求由 gateway 验证 JWT，并根据 token 类型决定是否执行 UserResolve。

普通用户 JWT claims：

```json
{
  "sub": "alice",
  "scopes": ["ebs:user"],
  "iss": "ebs-gateway",
  "aud": "ebs-api",
  "iat": 1790000000,
  "nbf": 1790000000,
  "exp": 1790003600,
  "jti": "01JEXAMPLETOKENID"
}
```

Runner JWT 额外包含与 Runner 对象名称一致的 `runner` claim：

```json
{
  "sub": "runner-001",
  "runner": "runner-001",
  "scopes": ["ebs:runner"],
  "iss": "ebs-gateway",
  "aud": "ebs-api",
  "iat": 1790000000,
  "nbf": 1790000000,
  "exp": 1790003600,
  "jti": "01JEXAMPLERUNNERID"
}
```

| Claim | 说明 |
|------|------|
| `sub` | 稳定调用方标识；普通用户与 User `metadata.name` 一致，Runner 与 `runner` claim 一致 |
| `runner` | Runner 对象名称，只允许 `ebs:runner` token 携带且必须提供 |
| `scopes` | 固定权限范围，必须满足下述合法组合 |
| `iss` | 固定为 `ebs-gateway`，不提供外部配置 |
| `aud` | 字符串，固定为 `ebs-api`，不提供外部配置 |
| `iat` | NumericDate 整数，签发时间 |
| `nbf` | NumericDate 整数，token 生效时间 |
| `exp` | NumericDate 整数，过期时间，且 `exp-iat` 不得超过配置的最大有效期 |
| `jti` | 非空字符串，token 唯一标识，用于审计和后续撤销扩展，不得作为权限来源 |

JWT header 必须包含：

```json
{"alg":"HS256","typ":"JWT"}
```

Gateway 只接受 `HS256`，使用 `--jwt-secret-file` 指定的单一 HMAC 密钥签发 token，并使用常量时间比较验证签名。非 `HS256` 算法、header 不合法或签名错误均返回 401。密钥文件内容是单个 base64 字符串，解码后不得少于 32 字节；密钥文件缺失、无法解码或密钥过短时 gateway 拒绝启动。

Gateway 必须验证 `sub`、`scopes`、`iss`、`aud`、`iat`、`nbf`、`exp` 和 `jti`。`sub` 必须是非空字符串，`scopes` 必须是非空字符串数组，`iss` 和 `aud` 必须分别精确等于固定值 `ebs-gateway` 和 `ebs-api`，不接受多 audience 数组。普通用户 token 有效期在代码中固定为 1 小时，允许的时钟偏差固定为 30 秒，接受的 token 最大有效期固定为 24 小时；`iat` 和 `nbf` 均不得晚于“当前时间 + 时钟偏差”，`exp` 必须晚于“当前时间 - 时钟偏差”、`iat` 和 `nbf`，且 `exp-iat` 不得超过最大有效期。缺失必需 claim、claim 类型错误或校验失败均返回 401。

固定 scope 及合法组合：

| Token 类型 | 合法 scopes | 签发方式 | 是否 UserResolve |
|------------|-------------|----------|------------------|
| 普通用户 | 仅 `ebs:user` | 只由 `POST /auth/login` 签发 | 是 |
| Runner | 仅 `ebs:runner` | 由部署管理员控制的受信任签发流程签发 | 否 |
| 受信任系统调用方 | 仅 `ebs:system` | 由部署管理员控制的受信任签发流程签发 | 否 |
| User 管理员 | `ebs:system` 和 `ebs:user-admin` | 由部署管理员控制的受信任签发流程签发 | 否 |

`POST /auth/login` 永远只签发 `ebs:user`，请求不能指定或提升 scope。`ebs:runner`、`ebs:system` 和 `ebs:user-admin` 不通过账号密码登录签发。`ebs:user-admin` 不能单独使用，必须同时包含 `ebs:system`。`ebs:user`、`ebs:runner` 和 `ebs:system` 互斥；重复、未知或不满足合法组合的 scopes 返回 401。

JWT 只有两种签发路径：普通用户 token 由 gateway 登录接口在线签发；Runner、system 和 User 管理员 token 的签发暂不属于当前阶段实现范围。gateway 当前使用单一 HMAC 密钥和固定 issuer `ebs-gateway`，不提供公网通用签发接口，也不接受登录调用方自选 scope、`sub`、有效期或 `runner` claim。所有后续签发方必须生成不可预测且不重复的 `jti`，并遵守固定的 24 小时最大有效期。

普通用户 JWT 不携带额外的资源归属信息，`sub` 就是唯一用户身份。`ebs:system` 仅保留给必须经过 gateway 的受信任自动化调用方，Runner 必须使用独立的 `ebs:runner` scope。

认证失败返回：

| 场景 | HTTP 状态码 |
|------|-------------|
| 未携带 token | 401 |
| header、签名或算法不合法 | 401 |
| issuer、audience 或时间 claims 不合法 | 401 |
| scope 未知、冲突或组合不合法 | 401 |
| Runner token 缺少合法 `runner` claim | 401 |

### 4.3 PasswordLogin

登录请求：

```text
POST /auth/login
```

```json
{"username":"alice","password":"user supplied password"}
```

gateway 使用内部凭据调用 apiserver：

```text
POST /internal/iam/v1/authenticate
```

认证成功后，gateway 读取 User，确认 `spec.enabled=true`，然后按 4.2 节规则签发仅包含 `ebs:user` scope 的完整 JWT。登录请求不能指定 scope、有效期或其他权限 claim。登录失败统一返回 401，不区分用户不存在、密码错误或账号锁定。登录请求体和密码不得写入访问日志、审计日志或错误响应。

登录成功返回：

```json
{
  "token": "<jwt>",
  "tokenType": "Bearer",
  "expiresIn": 3600
}
```

响应必须包含 `Cache-Control: no-store`；`expiresIn` 为从签发时间开始计算的秒数。

密码设置和修改通过 `PUT /auth/users/{name}/password` 完成：

- 同时包含 `ebs:system` 和 `ebs:user-admin` 的管理调用方为用户设置初始密码或重置密码时提交 `newPassword`。
- 用户修改自己的密码时提交 `currentPassword` 和 `newPassword`；gateway 先调用认证接口验证当前密码，再调用内部密码设置接口。
- gateway 将 `newPassword` 转换为 apiserver 内部接口要求的 `password` 字段并转发，不在本地保存密码。

### 4.4 UserResolve

仅 `ebs:user` token 在完成 4.2 节全部 JWT 校验后执行 UserResolve。`ebs:runner`、`ebs:system` 以及 `ebs:system` + `ebs:user-admin` token 不映射为 User，也不执行 UserResolve。对于普通用户，gateway 使用受信任的内部凭据读取：

```text
GET /apis/iam.ebs/v1/users/{jwt.sub}
```

检查规则：

| 场景 | 结果 |
|------|------|
| User 不存在 | 返回 403 |
| `spec.enabled=false` | 返回 403 |
| User API 不可用且无有效缓存 | 返回 503 |

gateway 必须确认返回对象的 `metadata.name` 与 JWT `sub` 完全一致，并可以按用户名缓存 `enabled`。缓存时间不超过 JWT 剩余有效期，JWT 和缓存使用较短有效期，使用户禁用能够及时生效。

User API 的内部查询不能使用正在校验的普通用户 token，避免递归鉴权和权限提升。gateway 应使用仅具备 User 读取权限的内部凭据，且不得将该凭据转发给客户端。

### 4.5 RateLimit

gateway 按调用方和客户端地址限流：

```text
{sub}/{clientIP}
```

如果请求未通过认证，不进入业务反向代理。

建议配置：

| 配置 | 默认值 | 说明 |
|------|--------|------|
| `--rate-limit-per-sec` | `100` | 每秒补充令牌数 |
| `--rate-limit-burst` | `200` | 突发桶容量 |

超过限流返回：

```text
HTTP 429 Too Many Requests
```

### 4.6 InjectHeaders

gateway 在转发前删除客户端伪造的内部身份头，再写入可信身份头。

删除客户端传入的所有内部身份头：

```text
X-EBS-*
```

注入：

```text
X-EBS-User: <jwt.sub>
X-EBS-Scopes: <jwt.scopes>
```

这些 header 只来自 gateway，客户端传入值一律丢弃。Runner 身份由 `X-EBS-User` 和 `ebs:runner` scope 表达，受信任自动化调用方权限由 `ebs:system` scope 表达。

### 4.7 ProjectAuthorize

gateway 在转发前进行用户级 Project 鉴权。

普通用户 token：

- 必须对应一个已启用且 `metadata.name` 等于 JWT `sub` 的 User。
- 可以创建 Project，Project owner user 强制为 JWT `sub`。
- 可以访问 owner user 为自己的 Project。
- 可以访问 member users 包含自己的 Project。
- 可以访问上述 Project 下的 Snapshot、Build、BuildInfo、RpmRepo、Job。
- 不能访问 Snapshot、Build、BuildInfo、RpmRepo、Job 的全局 API。
- 不能访问 Runner API。

Runner token：

- scopes 必须仅包含 `ebs:runner`，且 `sub`、`runner` claim 和路径中的 Runner 名称必须一致。
- 可以读取和更新自己的 Runner 对象；写请求仅允许 `PUT`、`PATCH` 自身 `/status`，不得修改 Runner spec 或其他 Runner。
- 可以对全局 Job API 执行 `GET`、`list` 和 `watch`，用于发现分配给自己的 Job；当前版本明确接受所有 Runner 都能通过 list/watch 读取全部 Job 的安全边界，gateway 不对响应做对象级过滤。Runner 客户端仍必须只处理 `status.runner` 等于自身名称的 Job。
- 对单个 Job 执行 `get` 时，只允许读取 `status.runner` 等于自身名称的 Job；也只能通过该 Job 的 `/status` 子资源更新执行状态。不得修改 Job spec、Runner 绑定关系或其他 Runner 的 Job。
- 不得访问 Project、Snapshot、Build、BuildInfo、RpmRepo 和 User API。
- 禁止对所有资源执行 `DELETE`。

受信任 system token：

- 必须包含 `ebs:system` scope。
- 可以访问各资源的全局 API；watch 仅适用于 Job、Runner。
- 可以访问 Runner API。
- 可以跨 Project 访问业务资源，但不能访问 User API；User 管理还必须具有 `ebs:user-admin`。

Project 级鉴权规则：

| 请求 | 普通用户 | Runner | System |
|------|----------|--------|--------|
| `POST /apis/ebs/v1/projects` | 允许，强制写入 `owner-user=<jwt.sub>` | 禁止 | 允许，但必须提供合法且已启用的 `owner-user` |
| `GET /apis/ebs/v1/projects` | 只返回自己拥有或被授权的 Project | 禁止 | 允许全量 |
| `GET /apis/ebs/v1/projects/{project}` | 有 owner/member 权限才允许 | 禁止 | 允许 |
| `/apis/ebs/v1/projects/{project}/...` | 有 owner/member 权限才允许 | 仅允许读取已分配 Job、更新其 `/status` | 允许 |
| `/apis/ebs/v1/{snapshots,builds,buildinfos,rpmrepos}` | 禁止 | 禁止 | 允许跨 Project list/查询，不支持 watch |
| `/apis/ebs/v1/jobs`（含 `watch=true`） | 禁止 | 只允许 `GET`/list/watch；对象读取和状态写入须匹配 `status.runner` | 允许跨 Project list/watch |
| `/apis/ebs/v1/runners...` | 禁止 | 只允许读取自身及更新自身 `/status` | 允许 |

User API 鉴权规则：

| 请求 | 普通用户 | Runner | 仅 System | System + UserAdmin |
|------|----------|--------|-------------|--------------------|
| `/apis/iam.ebs/v1/users...` | 禁止 | 禁止 | 禁止 | 允许管理 |

User 管理使用附加的 `ebs:user-admin` scope。它不能独立授权，只有和 `ebs:system` 同时出现时才允许访问 User API。

以下资源—verb 矩阵是授权实现的规范；未列出或标为“禁止”的组合默认拒绝。`get/list/watch` 统称读操作，`create/update/patch/delete` 统称写操作；资源实际不支持的 verb 即使矩阵允许也由 apiserver 拒绝。

| 资源范围 | Owner 用户 | Member 用户 | Runner | System | System + UserAdmin |
|----------|------------|-------------|--------|--------|--------------------|
| Project | `get/list/create/update/patch/delete` | `get/list`，禁止所有写操作 | 禁止 | 全部支持的 verb | 同 System |
| Project 子资源：Snapshot、Build、BuildInfo、RpmRepo | 全部支持的 verb | `get/list/create/update/patch`，禁止 `delete` | 禁止 | 全部支持的 verb | 同 System |
| Project 子资源：Job | 全部支持的 verb，watch 仅在 apiserver 支持时允许 | `get/list/create/update/patch`，禁止 `delete`；watch 仅在 apiserver 支持时允许 | 仅已分配 Job 的 `get` 和 `/status` 的 `update/patch` | 全部支持的 verb | 同 System |
| 全局 Job | 禁止 | 禁止 | `get/list/watch`，单对象 get 后仍须核对 `status.runner` | 全部支持的 verb | 同 System |
| Runner | 禁止 | 禁止 | 自身 `get` 和自身 `/status` 的 `update/patch` | 全部支持的 verb | 同 System |
| User 与用户密码 | 仅本人修改密码，禁止 User API | 仅本人修改密码，禁止 User API | 禁止 | 禁止 | 全部支持的 User verb 和密码管理 |

Owner 用户只有在修改 Project access labels 时受 4.8 节的额外字段约束。Member 用户禁止对任何资源执行 `DELETE`，也不能修改 Project 本身或 Project access labels。Runner 禁止所有 `create` 和 `delete`，其 Job `/status` 更新不得改变 `status.runner`。

Runner 对全部 Job 的可见性只代表读取权限，不产生执行或写入权限。单对象 Job `get` 和 `/status` 写入仍必须读取对象并校验 `status.runner` 等于 token 的 `runner` claim，不匹配时返回 403。由于任一受信任 Runner 都能看到 Job 的 spec、status 和 metadata，Job 对象不得保存密码、访问令牌、私钥或其他明文敏感信息；执行所需凭据必须通过独立的受控凭据交付机制提供。若未来需要隔离 Job 可见性，应由 apiserver 提供可信的服务端过滤，或由 gateway 对 list 和 watch 事件执行流式过滤，不能依赖 Runner 客户端自行隐藏。

如果 gateway 无法确认 Project 归属，应返回 403，不能放行。

实现建议：

- Project 列表请求：普通用户请求 `GET /apis/ebs/v1/projects` 时，gateway 查询并合并两类 Project：
  - `ebs.io/owner-user=<jwt.sub>`
  - `ebs.io/member-user.<jwt.sub>=true`
- Project 详情请求：gateway 先读取 Project，确认 JWT `sub` 是 owner 或 member，再转发原请求。
- Project 子资源请求：gateway 先根据路径中的 `{project}` 读取 Project 并校验 owner/member 权限，再转发原请求。
- Project 写请求：gateway 在转发前注入或保护 Project owner/member labels。
- Runner 请求：先校验 Runner 路径身份或读取 Job 并核对 `status.runner`，状态写入还必须拒绝对 Job 绑定字段的修改。
- System 请求：包含 `ebs:system` scope 时不做 Project 用户过滤，但 User API 仍要求额外的 `ebs:user-admin`。

由于 Kubernetes label selector 不支持 OR，Project 列表请求由 gateway 发起两次查询并合并结果：一次查询 owner user，一次查询 member user。

### 4.8 AccessLabels

#### 4.8.1 PUT/PATCH 完整对象比较

gateway 不能只检查请求体中显式出现的字段。所有 `PUT`、`PATCH` 请求必须先构造本次请求将产生的完整候选对象，再对旧对象和候选对象执行授权比较；只有比较通过的候选对象可以写入 apiserver。

处理流程：

1. 根据规范化后的路由确定资源、对象名称和 subresource；路径中不存在对象名称的 collection 路由不接受 `PUT`、`PATCH`。gateway 先执行不依赖对象内容的 scope、verb、Project 和路径权限检查，调用方对该类请求必然无权时直接返回 403，不使用内部凭据探测对象。
2. gateway 使用内部读取凭据从相同资源路径读取当前对象，记为 `oldObject`。上游返回 404 时向客户端返回 404，读取失败时不执行写入。需要依据对象内容授权的 Runner Job 请求在读取后继续核对 `status.runner`。
3. `PUT` 只接受 `application/json`，请求体必须是单个完整 JSON 对象，以请求体作为 `candidateObject`。请求必须携带 `metadata.resourceVersion`，并且与 `oldObject.metadata.resourceVersion` 完全一致，否则返回 409。
4. `PATCH` 只接受 `application/merge-patch+json` 和 `application/json-patch+json`。gateway 将 patch 应用到 `oldObject` 的规范 JSON 表示，得到 `candidateObject`。JSON 语法错误、patch 操作失败或结果不是对象时返回 400。
5. PATCH 中如果显式提供或修改 `metadata.resourceVersion`，其结果必须等于旧对象版本；如果没有提供，gateway 将旧对象的 `metadata.resourceVersion` 写入候选对象。这样 PATCH 同样受乐观并发控制。
6. gateway 对完整的 `oldObject` 和 `candidateObject` 执行身份字段、subresource、角色权限和受保护字段比较。任何一项不通过均返回 403，不向上游发送写请求。
7. 比较通过后，gateway 不转发原始 PATCH，而是将 `candidateObject` 以 `PUT application/json` 转发到原对象或原 `/status` 路径。上游必须使用候选对象中的 `resourceVersion` 执行原子更新；对象在步骤 2 后发生变化时返回 409，gateway 不自动重放写请求。

外部接口不接受 `application/strategic-merge-patch+json`、`application/apply-patch+yaml`、YAML、缺失 `Content-Type` 或其他 PATCH 类型，统一返回 415。gateway 必须限制更新请求体和 patch 后候选对象的大小；超过 `--max-request-body-bytes` 返回 413。

JSON Patch 处理要求：

- 依次执行 RFC 6902 操作，支持 `add`、`remove`、`replace`、`move`、`copy` 和 `test`。
- JSON Pointer 必须完成 `~0`、`~1` 解码后再判断路径，不能用原始字符串前缀判断受保护字段。
- `move`、`copy` 必须同时检查 `from` 和 `path`，但最终授权仍以完整候选对象比较结果为准。
- `test` 失败返回 409，其他无法应用的操作返回 400。
- 重复 JSON object key、非法 UTF-8、非有限数字以及 trailing content 返回 400。

比较使用解析后的 JSON 值，不比较原始字节、字段顺序或空白。对象字段缺失与 `null` 是不同值；数组按顺序比较；数字按 JSON 数值比较；map 按完整 key/value 集合比较。对于受保护的 label，label 缺失、值变为 `null`、值变为空字符串和删除 label 都视为发生修改。gateway 不得通过丢弃未知字段使请求通过校验；无法识别或无法无损保留的字段返回 400。

所有角色均不得通过普通对象或 `/status` 更新修改以下身份和服务端字段：

```text
apiVersion
kind
metadata.name
metadata.namespace
metadata.uid
metadata.creationTimestamp
metadata.deletionTimestamp
metadata.deletionGracePeriodSeconds
metadata.generation
metadata.managedFields
```

`metadata.resourceVersion` 只能保持为 `oldObject` 的值。路径中的资源类型、namespace、name 必须分别与候选对象的 `apiVersion/kind`、`metadata.namespace`、`metadata.name` 一致；缺失、冲突或试图跨 Project 移动对象均返回 400。普通对象路径只允许修改 `metadata` 和 `spec` 中角色有权修改的字段，候选对象的 `status` 必须与旧对象完全相同；`/status` 路径只允许修改授权的 `status` 字段，`metadata`（除保持原值的 `resourceVersion` 外）和 `spec` 必须与旧对象完全相同。

字段比较后的角色规则：

| 调用方和路径 | 允许发生变化的字段 | 额外约束 |
|--------------|--------------------|----------|
| Owner 更新 Project | `metadata.labels`、`metadata.annotations`、`spec` | `ebs.io/owner-user` 必须保持不变；member labels 按本节后续规则校验 |
| Member 更新 Project | 无 | 4.7 节禁止 member 修改 Project，请求返回 403 |
| Owner/Member 更新 Project 子资源普通路径 | `metadata.labels`、`metadata.annotations`、`spec` | member 禁止 DELETE，但可按 4.7 节执行 update/patch |
| Owner/Member 更新 Project 子资源 `/status` | `status` | 仅当对应资源暴露 `/status` 且 4.7 节允许该调用方更新时允许 |
| Runner 更新自身 `/status` | `status.phase`、`status.conditions`、`status.capacity`、`status.allocatable`、`status.addresses`、`status.info`、`status.heartbeat` | 路径名称必须等于 token 的 `runner` claim；`spec` 和全部 metadata 保持不变 |
| Runner 更新已分配 Job `/status` | `status.phase`、`status.stage`、`status.startTime`、`status.endTime`、`status.resultRoot`、`status.message` | 旧对象和候选对象的 `status.runner` 均必须等于 token 的 `runner` claim；Runner 不得修改 `status.runner` 或 `status.restartCount` |
| System 更新业务资源 | 普通路径为 `metadata`、`spec`，`/status` 路径为 `status` | 仍受身份字段、subresource 隔离和 `resourceVersion` 规则约束 |
| System + UserAdmin 更新 User | `metadata.labels`、`metadata.annotations`、`spec` | User labels 是普通扩展元数据，不参与 Project 权限判定 |

表中的允许字段是上限；状态值、不可变 spec 字段和资源自身校验规则仍由 apiserver 执行。没有列入允许集合的任何差异都返回 403。gateway 应在审计日志中记录请求方法、原始 patch 类型、旧/新 `resourceVersion`、被拒绝的 JSON Pointer 路径和拒绝原因，但不得记录密码、完整对象或完整 patch。

#### 4.8.2 User 和 Project access labels

只有同时包含 `ebs:system` 和 `ebs:user-admin` 的管理请求可以创建或修改 User。User labels 是普通扩展元数据，不作为身份或 Project 权限来源。

普通用户创建 Project 时，gateway 对 `POST` JSON 请求注入或覆盖：

```text
metadata.labels["ebs.io/owner-user"] = jwt.sub
```

system token 创建 Project 时不覆盖客户端提供的 `owner-user`，但必须读取该 User 并确认存在且已启用。

gateway 对 Project 的 `PUT`、`PATCH` 请求需要保护以下 labels：

```text
metadata.labels["ebs.io/owner-user"]
metadata.labels["ebs.io/member-user.<username>"]
```

访问规则：

- 普通 member user 不能修改 Project access labels。
- owner user 可以增删 `ebs.io/member-user.<username>` labels；新增成员前必须确认对应 User 存在且已启用。
- owner user 不能把 `ebs.io/owner-user` 改成其他用户。
- system token 可以修改 owner/member labels。

Project access labels 的保护逻辑只应用于 Project 对象。Snapshot、Build、BuildInfo、RpmRepo、Job 通过所属 Project 继承权限。

### 4.9 ReverseProxy

gateway 使用反向代理将请求转发到 `ebs-apiserver`。

代理要求：

- 除 4.8.1 节规定的 PUT/PATCH 规范化外，保持原始 HTTP 方法。
- 保持查询参数。
- 除 access label 注入和 4.8.1 节生成完整候选对象外，保持请求体。
- 支持 watch 长连接，不缓冲完整响应。
- 不为不支持 watch 的 Project、Snapshot、Build、BuildInfo、RpmRepo 模拟轮询；相关错误由 apiserver 原样返回。
- 透传 apiserver 返回的状态码和响应体。
- 使用 `--apiserver-addr` 设置上游地址。

## 五、路由设计

### 5.1 对外路由

gateway 暴露业务 API 和用户管理插件 API：

| 路由 | 鉴权 | 说明 |
|------|------|------|
| `GET /healthz` | 否 | 健康检查 |
| `POST /auth/login` | 否 | 账号密码登录，成功后签发 JWT |
| `PUT /auth/users/{name}/password` | 是 | 设置或修改密码，仅允许本人，或同时包含 `ebs:system` 和 `ebs:user-admin` 的管理调用方 |
| `ANY /apis/ebs/v1/*` | 是 | `ebs/v1` API 代理，需要 Project 用户权限校验 |
| `ANY /apis/iam.ebs/v1/users*` | 是 | User 管理 API，仅允许同时包含 `ebs:system` 和 `ebs:user-admin` 的管理 token |

不提供 `/api/*` 简化别名。客户端统一使用 Kubernetes-like API 路径，避免出现两套路由标准。

### 5.2 API 透传示例

| 客户端请求 | 转发到 apiserver |
|------------|------------------|
| `GET /apis/ebs/v1/projects` | `GET /apis/ebs/v1/projects` |
| `POST /apis/ebs/v1/projects` | `POST /apis/ebs/v1/projects` |
| `GET /apis/ebs/v1/projects/{project}/buildinfos` | `GET /apis/ebs/v1/projects/{project}/buildinfos` |
| `GET /apis/ebs/v1/projects/{project}/rpmrepos` | `GET /apis/ebs/v1/projects/{project}/rpmrepos` |
| `GET /apis/ebs/v1/projects/{project}/jobs` | `GET /apis/ebs/v1/projects/{project}/jobs` |
| `PUT /apis/ebs/v1/projects/{project}/jobs/{name}/status` | `PUT /apis/ebs/v1/projects/{project}/jobs/{name}/status` |
| `GET /apis/ebs/v1/jobs?watch=true` | `GET /apis/ebs/v1/jobs?watch=true` |
| `PUT /apis/ebs/v1/runners/{name}/status` | `PUT /apis/ebs/v1/runners/{name}/status` |

### 5.3 Watch 透传

受信任 system 调用方和 Runner 可能通过 gateway 对支持 watch 的资源建立连接：

```text
GET /apis/ebs/v1/jobs?watch=true
GET /apis/ebs/v1/runners?watch=true
```

当前只有 etcd 主存储的 Job、Runner 支持 watch；Project、Snapshot、Build、BuildInfo、RpmRepo 使用 Elasticsearch 主存储，不支持 watch。Runner token 只允许全局 Job watch，不允许 Runner watch；system token 可以 watch Job 和 Runner。普通用户通过 Project 范围的 API 访问资源，Project 范围的 Job watch 在权限校验后透传。

gateway 需要支持流式响应：

- 不读取完整 upstream response 后再返回。
- 不为 watch 请求设置过短超时。
- 客户端断开时及时关闭 upstream 请求。
- 审计日志记录连接建立和最终关闭状态。

## 六、配置

建议组件目录：

```text
components/ebs-gateway/
```

启动配置：

| 参数 | 默认值 | 必填 | 说明 |
|------|--------|------|------|
| `--port` | `8080` | 否 | gateway 监听端口 |
| `--apiserver-addr` | `https://ebs-apiserver:8443` | 是 | 上游 apiserver 地址 |
| `--jwt-secret-file` | 空 | 是 | 保存单个 base64 HMAC 密钥的文件路径，解码后至少 32 字节 |
| `--user-cache-ttl` | `30s` | 否 | User 状态缓存时间，实际缓存不能超过 JWT 剩余有效期 |
| `--max-request-body-bytes` | `1048576` | 否 | 登录和代理写请求的最大请求体，以及 PATCH 后候选对象的最大字节数 |
| `--insecure-skip-verify` | `false` | 否 | 是否跳过 apiserver TLS 校验，仅测试环境使用 |
| `--apiserver-ca` | 空 | 否 | apiserver CA 文件路径 |
| `--rate-limit-per-sec` | `100` | 否 | 每秒令牌数 |
| `--rate-limit-burst` | `200` | 否 | 令牌桶容量 |
| `--log-level` | `info` | 否 | 日志级别 |

密钥文件由 Secret 以只读方式挂载，文件权限应限制为 `0600`，内容为单个 base64 字符串，例如：

```text
MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=
```

当前版本不支持新旧密钥并行验证。轮换密钥需要更新 Secret 并重启所有 gateway 实例，所有旧 token 随即失效，用户必须重新登录。多实例滚动更新期间，不同密钥的实例不能互相验证对方签发的 token，因此应在维护窗口内同步重启、临时缩容为单实例，或明确接受轮换窗口内的 401。密钥文件、解码后的密钥以及完整 token 不得写入日志；配置缺失时不得像旧系统一样动态生成随机密钥。

## 七、调用示例

### 7.1 健康检查

```bash
curl http://localhost:8080/healthz
```

### 7.2 创建测试 User

User 由 apiserver 插件管理，只有同时包含 `ebs:system` 和 `ebs:user-admin` 的管理 token 可以创建：

```bash
curl -X POST http://localhost:8080/apis/iam.ebs/v1/users \
  -H "Authorization: Bearer ${ADMIN_SYSTEM_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "iam.ebs/v1",
    "kind": "User",
    "metadata": {
      "name": "alice"
    },
    "spec": {
      "enabled": true,
      "displayName": "Alice",
      "email": "alice@example.com"
    }
  }'
```

为 User 设置初始密码：

```bash
curl -X PUT http://localhost:8080/auth/users/alice/password \
  -H "Authorization: Bearer ${ADMIN_SYSTEM_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"newPassword":"example password"}'
```

### 7.3 登录

```bash
curl -X POST http://localhost:8080/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"alice","password":"example password"}'
```

登录成功响应包含 JWT。token 中的 `sub` 与 User 的 `metadata.name` 一致，并直接作为 Project 权限判定的用户身份。

### 7.4 创建 Project

```bash
TOKEN=<token>

curl -X POST http://localhost:8080/apis/ebs/v1/projects \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "ebs/v1",
    "kind": "Project",
    "metadata": {
      "name": "openeuler-22-03-lts"
    },
    "spec": {
      "displayName": "openEuler 22.03 LTS",
      "description": "openEuler 22.03 LTS",
      "specBranch": "master",
      "buildTargets": [{
        "osVariant": "openEuler-22.03-LTS",
        "architecture": "aarch64"
      }]
    }
  }'
```

### 7.5 Runner Watch Job

Runner 使用受信任签发流程获得仅包含 `ebs:runner` 的 token，并通过全局 Job watch 发现分配给自己的 Job。以下签发代码仅用于说明 claims 和 header，生产环境的密钥来自受控的单一密钥文件：

```bash
python3 - <<'PY'
import jwt
import time

token = jwt.encode(
    {
        "sub": "runner-001",
        "runner": "runner-001",
        "scopes": ["ebs:runner"],
        "iss": "ebs-gateway",
        "aud": "ebs-api",
        "iat": int(time.time()),
        "nbf": int(time.time()),
        "exp": int(time.time()) + 3600,
        "jti": "example-runner-token-id",
    },
    b"example-only-key-with-at-least-32-bytes",
    algorithm="HS256",
    headers={"typ": "JWT"},
)
print(token)
PY
```

```bash
curl -N http://localhost:8080/apis/ebs/v1/jobs?watch=true \
  -H "Authorization: Bearer ${RUNNER_TOKEN}"
```

## 八、安全边界

| 风险 | 处理 |
|------|------|
| 未认证请求访问业务 API | 返回 401，不转发到 apiserver |
| 登录接口泄漏密码 | 登录请求体不记录日志，gateway 与 apiserver 之间使用 TLS，错误响应不回显输入 |
| 密码暴力尝试 | gateway 按账号和客户端地址限流，IAM 模块维护失败次数和临时锁定状态 |
| 已删除或禁用的用户访问 | UserResolve 返回 403，不进入业务鉴权 |
| User API 不可用 | 仅使用仍有效的短期缓存；无有效缓存时返回 503，不默认放行 |
| 普通用户管理 User | `/apis/iam.ebs/v1/users*` 返回 403 |
| User 内部查询凭据泄漏 | 使用最小只读权限、独立密钥和轮换机制，不写入审计日志或转发请求头 |
| 客户端伪造内部身份头 | 转发前删除所有客户端 `X-EBS-*`，只重建 `X-EBS-User` 和 `X-EBS-Scopes` |
| 客户端伪造 Project owner | 普通用户创建 Project 时强制覆盖 `metadata.labels["ebs.io/owner-user"]`；system 创建时校验指定 owner User |
| 客户端越权修改 Project members | 只有 owner user 或 system token 可以修改 member user labels，新增成员必须是已启用 User |
| 用户访问未授权 Project | gateway 查询 Project owner/member labels，不匹配则返回 403 |
| 用户访问全局 API | 普通用户禁止；Runner 仅能发现 Job 并操作自身 Runner 和已分配 Job；system token 可访问业务资源全局 API |
| Runner 读取其他 Runner 的 Job | 当前版本接受 Runner 对全部 Job 的 list/watch 可见性；Job 不保存明文敏感信息，执行和状态写入仍按 `status.runner` 隔离 |
| token 泄漏 | 使用短有效期、固定 issuer/audience 和最小 scope；轮换单一密钥会使全部旧 token 失效，`jti` 进入审计记录以支持后续撤销扩展 |
| 请求风暴 | 基于调用方限流 |
| 上游 TLS 风险 | 生产环境启用 TLS 校验或配置 CA |
| watch 长连接占用 | 限制单调用方并发 watch 数，保留合理超时 |

## 九、测试设计

测试覆盖：

| 模块 | 场景 |
|------|------|
| Auth | 缺失 token、非法签名、非 HS256、非法 header、issuer/audience 错误、时间 claim 越界、缺失 `jti`、非法 scope 组合以及合法的 user/runner/system/admin token；密钥文件缺失、非法或过短时启动失败 |
| PasswordLogin | 登录成功且只能获得 `ebs:user`、请求 scope 不产生提权、密码错误、用户不存在、用户禁用、账号锁定、认证接口不可用和登录限流 |
| UserResolve | User 不存在、名称与 JWT `sub` 不匹配、禁用、缓存命中、User API 不可用，以及 runner/system/admin token 跳过 User 查询 |
| Header | 删除伪造 `X-EBS-*` 并注入可信身份 |
| ProjectAuthz | 普通用户只能访问自己拥有或作为 member 的 Project；Runner 可以 list/watch 全部 Job，但只能读取自身 Runner、更新自身 Runner status，并对 `status.runner` 匹配的单个 Job 执行 get 和 status 写入；system token 可访问业务全局 API |
| UserAdmin | user、runner、仅 `ebs:system` 和仅 `ebs:user-admin` 均不能访问 User API，只有 `ebs:system` + `ebs:user-admin` 可以管理 User 和重置密码 |
| ObjectCompare | PUT 缺失/过期 `resourceVersion`；Merge Patch 和 JSON Patch 构造完整候选对象；拒绝不支持的 patch 类型、非法 JSON Pointer、重复 key、超大对象和跨 subresource 修改；检查后并发更新返回 409且不自动重放 |
| AccessLabels | 普通用户创建 Project 时强制写入 owner user label；system 创建时校验 owner User；PUT/PATCH 不能通过 `null`、删除父 map、`move` 或 `copy` 绕过 owner/member user label 保护 |
| RunnerStatus | Runner 只能修改自身 Runner status 白名单字段和已分配 Job status 白名单字段，不能修改 Job runner、restartCount、spec 或 metadata |
| RateLimit | 超过令牌桶容量后返回 429 |
| Proxy | `/apis/ebs/v1/*` 原样透传 |
| Watch | system 的 Job/Runner watch 和 Runner 的 Job watch 能够流式转发；Runner 的 Runner watch、普通用户全局 watch 返回 403；ES-only 资源不支持 watch |
| Audit | 请求结束后记录 method/path/status/latency/user |
