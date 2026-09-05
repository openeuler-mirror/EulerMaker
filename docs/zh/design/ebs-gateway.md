# ebs-gateway 设计

## 一、定位

`ebs-gateway` 是 EulerMaker 对外请求入口，位于客户端和 `ebs-apiserver` 之间，负责公开只读访问、令牌认证、用户状态检查、Project 权限校验、审计、限流和请求转发。

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

`ebs-gateway` 保持无状态，User 和 MachineAccount 由 `ebs-apiserver` 的 IAM 模块提供。匿名调用方可以读取公开业务资源的完整对象，但不能 watch、写入或访问 Runner/IAM；认证用户仍按身份获得对应写权限。gateway 通过 User API 获取用户状态，并基于认证身份限制用户写入自己有权限的 Project；通过 MachineAccount 凭据认证为请求指定的 Runner 签发短期 token。

## 二、设计目标

| 目标 | 说明 |
|------|------|
| 统一入口 | 用户、外部系统和 runner 统一通过 gateway 访问 `ebs/v1` API |
| 公开读取 | 匿名和已认证调用方均可 get/list Project、Snapshot、Build、BuildInfo、RpmRepo 和 Job 的完整对象，不能匿名 watch |
| 统一认证 | gateway 提供用户自助注册和登录入口，调用 apiserver IAM 模块管理账号凭据；登录成功后由 gateway 签发 JWT，业务请求进入 apiserver 前由 gateway 完成 token 校验 |
| 用户校验 | 通过 apiserver 的 User API 检查用户是否存在并启用 |
| 机机认证 | 验证 MachineAccount 长期凭据和请求中的 Runner 名称格式，签发短期 Runner token |
| 请求转发 | 将合法请求反向代理到 `ebs-apiserver` |
| Watch 透传 | 为 apiserver 支持 watch 的 Job、Runner 透传长连接和流式响应 |
| 审计记录 | 记录请求路径、方法、状态码、耗时和调用方信息 |
| 限流保护 | 对调用方进行基础限流，保护 apiserver |
| 写入隔离 | 基于用户身份和 Project labels 限制写入范围，支持 Project 在用户间共享 |
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
  scopes:
    - ebs:user
  displayName: Alice
  email: alice@example.com
```

约束如下：

- `metadata.name` 是稳定用户标识，与用户 JWT 的 `sub` 一致。
- `spec.enabled` 控制用户是否可以访问业务 API。
- `spec.scopes` 必须恰好包含 `ebs:user`、`ebs:ops` 或 `ebs:admin` 中的一项；自助注册固定为 `["ebs:user"]`。
- 管理员标记由受信任内部流程通过 apiserver User API 设置，gateway 不提供对外提升管理员的接口。
- apiserver IAM 模块验证用户密码或 MachineAccount client secret，gateway 签发 JWT；IAM资源保存gateway鉴权需要的主体状态和token TTL。
- 普通用户通过公开注册接口创建自己的 User 和初始密码。
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

匿名调用方可以读取第 4.1 节定义的公开资源，认证用户通过 Project API 访问 Project 级资源。Runner 使用独立的 runner token。普通用户 token 只有在对应 User 存在且已启用时才有效；gateway 直接使用 JWT `sub` 作为用户身份，并限制用户只能写入自己拥有或被授权为 member 的 Project 和 Project 子资源。

## 四、请求处理流程

业务请求的处理链：

```text
Request
  -> Audit
  -> PublicRead / Auth
  -> UserResolve（认证用户）
  -> RateLimit
  -> InjectHeaders
  -> ProjectAuthorize
  -> AccessLabels
  -> ReverseProxy
  -> Response
```

### 4.1 公开读取

Gateway 在要求 Bearer Token 前先识别匿名公开读取。只有不携带 Authorization header 的 GET/HEAD 请求可以直接进入该分支；携带 Token 时必须先完成 Token 和 User 状态校验，非法、过期、已禁用或身份不一致时返回 401/403，不能降级为匿名访问。校验通过后，公开 `GET/HEAD` 使用与匿名调用方相同的路由、完整对象、分页和响应头规则，不执行 Project owner/member 读取过滤。

对匿名和已认证调用方开放以下资源的 collection、单对象及单对象 `/status` 读取：

| 资源 | 对外公开 API |
|------|--------------|
| Project | `/apis/ebs/v1/projects`、`/projects/{name}`、`/projects/{name}/status` |
| Snapshot | `/apis/ebs/v1/projects/{project}/snapshots[/{name}]`、`/projects/{project}/snapshots/{name}/status` |
| Build | `/apis/ebs/v1/projects/{project}/builds[/{name}]`、`/projects/{project}/builds/{name}/status` |
| BuildInfo | `/apis/ebs/v1/projects/{project}/buildinfos[/{name}]`、`/projects/{project}/buildinfos/{name}/status` |
| RpmRepo | `/apis/ebs/v1/projects/{project}/rpmrepos[/{name}]`、`/projects/{project}/rpmrepos/{name}/status` |
| Job | `/apis/ebs/v1/projects/{project}/jobs[/{name}]`、`/projects/{project}/jobs/{name}/status` |

匿名请求不开放 Runner、Runner 子资源、User、MachineAccount、非白名单资源的 `/status` 或其他未列入白名单的新资源。公开对象的 `/status` 仅允许单对象 `GET/HEAD` 并返回完整对象；collection 不存在 `/status`。所有 POST、PUT、PATCH、DELETE 均先认证，因此匿名调用方不能借公开 `/status` 修改状态。只要查询参数中出现非 `false` 的 `watch` 值就返回 401；匿名请求不能用 `watch=1`、重复 query 参数或其他等价值绕过。Project、Snapshot、Build、BuildInfo 和 RpmRepo 本身不支持 watch，也不能由 Gateway 模拟轮询。

公开读取直接透传 apiserver 返回的完整单对象或 List，不解码、删除或重写对象字段。公开范围内的 `metadata`、`spec` 和 `status` 均可被匿名或已认证调用方读取，包括对象后续新增的字段。因此 Project、Snapshot、Build、BuildInfo、RpmRepo 和 Job 的 API 数据结构不得保存密码、Token、私钥或其他不应公开的信息；需要保密的数据必须存放在非公开资源或独立的受控存储中。

匿名 HEAD 与对应 GET 使用相同的路由、query 和限流校验，但响应不包含正文，也不得透传可能泄露内部版本或存储实现的 header。公开 GET 可以保留 `Content-Type`、缓存策略和 requestID；不得透传内部 ETag、resourceVersion 或上游身份 header。

匿名请求按客户端 IP 使用独立令牌桶，额度应低于认证调用方；collection 必须设置服务端允许的 `limit` 上限，禁止匿名调用方请求无界列表。Gateway 不注入 `X-EBS-User` 或 `X-EBS-Scopes`，而是使用受信任的内部身份读取 apiserver 并原样转发对象响应。审计日志使用固定身份 `anonymous`，记录资源、verb、客户端地址、响应数量和 requestID。

### 4.2 Audit

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
| `token_type` | `user`、`runner`、`system` 或 `admin` |
| `jti` | JWT 唯一标识，不记录完整 token |
| `user_agent` | 客户端 User-Agent |

### 4.3 Auth

客户端通过 Bearer Token 访问 gateway：

```text
Authorization: Bearer <token>
```

gateway 提供用户密码登录和 MachineAccount 换取 Runner token 两类认证入口。用户登录时，Gateway 将账号密码提交给 apiserver IAM 模块验证，认证成功后再读取 User，确认用户存在且 `spec.enabled=true`。Runner 换取 token 时，Gateway 将 MachineAccount client ID 和 client secret 提交给 apiserver IAM 模块验证，并独立校验请求中的 Runner 名称。以上检查通过后，由 gateway 使用同一 HMAC 密钥签发对应的短期 JWT。Apiserver IAM 模块只负责凭据验证，不签发、解析或刷新 JWT。后续业务请求由 gateway 验证 JWT，并根据 token 类型决定是否执行 UserResolve。

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

Gateway 必须验证 `sub`、`scopes`、`iss`、`aud`、`iat`、`nbf`、`exp` 和 `jti`。`sub` 必须是非空字符串，`scopes` 必须是非空字符串数组，`iss` 和 `aud` 必须分别精确等于固定值 `ebs-gateway` 和 `ebs-api`，不接受多 audience 数组。普通用户 token 有效期在代码中固定为 24 小时，允许的时钟偏差固定为 30 秒，接受的 token 最大有效期固定为 24 小时；`iat` 和 `nbf` 均不得晚于“当前时间 + 时钟偏差”，`exp` 必须晚于“当前时间 - 时钟偏差”、`iat` 和 `nbf`，且 `exp-iat` 不得超过最大有效期。缺失必需 claim、claim 类型错误或校验失败均返回 401。

固定 scope 及合法组合：

| Token 类型 | 合法 scopes | 签发方式 | 是否 UserResolve |
|------------|-------------|----------|------------------|
| 普通用户 | 仅 `ebs:user` | 只由 `POST /auth/login` 签发 | 是 |
| 运维用户 | 仅 `ebs:ops` | `spec.scopes=["ebs:ops"]` 的 User 通过 `POST /auth/login` 签发 | 是 |
| Runner | 仅 `ebs:runner` | 只由 `POST /auth/runner-token` 使用 MachineAccount 凭据签发 | 否 |
| 受信任系统调用方 | 仅 `ebs:system` | 由部署管理员控制的受信任签发流程签发 | 否 |
| 管理员 | 仅 `ebs:admin` | `spec.scopes=["ebs:admin"]` 的 User 通过 `POST /auth/login` 签发 | 是 |

JWT 在线签发只有两条路径：`POST /auth/login` 根据 User 唯一的 `spec.scopes` 项签发仅包含 `ebs:user`、`ebs:ops` 或 `ebs:admin` 的 token；`POST /auth/runner-token` 使用 MachineAccount 凭据签发仅包含 `ebs:runner` 的 token。`ebs:system` 由部署管理员控制的受信任流程签发，不通过 User 或 MachineAccount 凭据在线签发。调用方不能指定 scope、`sub`、有效期或 `runner` claim。`ebs:user`、`ebs:ops`、`ebs:admin`、`ebs:runner` 和 `ebs:system` 互斥；重复、未知或多 scope 组合均返回 401。所有签发路径使用同一 HMAC 密钥和固定 issuer `ebs-gateway`，生成不可预测且不重复的 `jti`，并遵守固定的 24 小时验证上限；user、ops 和 admin token 固定为 24 小时，Runner token 由 MachineAccount 的 `tokenTTLSeconds` 决定且最长 24 小时。

普通用户 JWT 不携带额外的资源归属信息，`sub` 就是唯一用户身份。`ebs:system` 仅保留给必须经过 gateway 的受信任自动化调用方，Runner 必须使用独立的 `ebs:runner` scope。

认证失败返回：

| 场景 | HTTP 状态码 |
|------|-------------|
| 未携带 token | 401 |
| header、签名或算法不合法 | 401 |
| issuer、audience 或时间 claims 不合法 | 401 |
| scope 未知、冲突或组合不合法 | 401 |
| Runner token 缺少合法 `runner` claim | 401 |

#### 4.3.1 MachineAccountAndRunnerTokenExchange

Runner 使用 HTTP Basic 提交 MachineAccount 名称和 client secret，并在 JSON 请求体中声明 Runner 名称：

```text
POST /auth/runner-token
Authorization: Basic base64(<client-id>:<client-secret>)
Content-Type: application/json
```

```json
{"runner":"runner-001"}
```

Gateway 限制请求体大小并拒绝未知字段。`runner` 必须原样满足 DNS1123 label；Gateway 调用 `/internal/iam/v1/machineaccounts/{client-id}/authenticate` 验证凭据。认证成功后签发 `sub` 和 `runner` 均等于请求名称、scopes 仅为 `ebs:runner` 的 JWT。MachineAccount 名称不写入 `sub`，只作为审计字段 `actor` 记录，不能据此访问业务资源。

成功响应为：

```json
{
  "accessToken": "<jwt>",
  "tokenType": "Bearer",
  "expiresIn": 3600
}
```

响应必须包含 `Cache-Control: no-store`。账号不存在、凭据错误和账号锁定统一返回 401，不能泄漏账号状态；请求或 Runner 名称非法返回 400；超过独立的 client ID/IP 令牌桶限制返回 429 并包含 `Retry-After`；IAM 不可用返回 503。client secret、Basic header、完整 JWT 和凭据验证细节不得进入访问日志或审计日志。每次成功和失败交换都记录 client ID、目标 Runner、结果、客户端地址；成功记录签发 token 的 `jti`。

Runner不使用 refresh token。短期 token到期前，Runner使用 MachineAccount 凭据重新调用交换接口。删除账号后不能再交换新 token；已签发 token在最长24小时后自然失效，需要即时撤销属于后续 `jti` denylist或签名密钥轮换能力。

管理员通过 `POST /auth/machineaccounts` 原子创建 MachineAccount 和初始凭据：

```json
{
  "name": "runner-site-a",
  "clientSecret": "base64url-encoded-random-secret",
  "tokenTTLSeconds": 3600
}
```

`name` 必填，必须原样满足 DNS1123 label 约束且不超过 63 个字符。`clientSecret` 必须是无填充的 Base64URL 字符串，解码后至少包含 32 字节，编码后长度不得超过 256 字节。`tokenTTLSeconds` 可选，默认为 3600，取值范围为 300 至 86400。Gateway 按原值校验和转发 `name` 与 `clientSecret`，不执行 trim、大小写转换或其他规范化。请求只接受上述三个字段，未知字段或任一字段非法时返回 400。

Gateway要求 admin 权限，校验请求后调用`POST /internal/iam/v1/machineaccounts/register`。成功返回201和`{"name":"runner-site-a"}`，不回显secret；名称已存在返回409，非法请求返回400。管理端将名称作为`clientID`、原始secret作为`clientSecret`组成Runner使用的机机凭据文件并通过受保护渠道下发。

#### 4.3.2 公开 Token 校验

Gateway 公开提供 `POST /auth/check`。调用方只携带待校验的 Bearer Token：

```http
POST /auth/check
Authorization: Bearer <token>
```

该接口不要求 mTLS 客户端证书、服务凭据或其他服务身份。Gateway 验证 JWT 签名、issuer、audience、时间字段和 scope 结构；成功返回认证身份、Token scopes 和过期时间，调用方根据自身需求检查 scopes。Token 非法返回 401，携带非空请求正文返回 400，超过按 `{sub}/{clientIP}` 计算的限流返回 429。接口不查询 User、Runner 或 Job，不执行资源授权，也不向 apiserver 转发。

### 4.4 RegistrationAndPasswordLogin

gateway 提供无需 JWT 的用户自助注册入口：

```text
POST /auth/register
```

```json
{
  "username": "alice",
  "password": "user supplied password",
  "displayName": "Alice",
  "email": "alice@example.com"
}
```

注册请求仅接受 `application/json`，拒绝未知字段，且受 `--max-request-body-bytes` 限制。字段规则如下：

- `username` 必填，必须原样满足 DNS1123 label 约束且不超过 63 个字符；gateway 不做 trim、大小写折叠或其他规范化，避免多个输入映射到同一账号。
- `password` 必填，长度为 12 到 128 个字符，按客户端提交的原值处理，不做 trim 或 Unicode 规范化。
- `displayName` 和 `email` 可选；非空 email 必须是合法地址。
- 客户端不能提交 `metadata`、labels、scopes、`enabled` 或其他 User 字段；新注册 User 的 `spec.enabled` 固定为 `true`、`spec.scopes` 固定为 `["ebs:user"]`。

gateway 完成请求格式和基础校验后，调用 apiserver 的原子语义注册接口：

```text
POST /internal/iam/v1/users/register
```

apiserver 是用户名唯一性和全部字段校验的权威方，并通过单个ES文档原子创建User与初始密码哈希。

注册成功返回 201，且不自动签发 JWT：

```json
{"username":"alice"}
```

用户随后通过登录接口获取 token。注册接口的错误语义为：请求或字段非法返回 400，用户名已存在返回 409，超过注册限流返回 429，apiserver IAM 不可用返回 503；响应不得回显密码。注册按客户端 IP 和 `{username}/{clientIP}` 分别使用独立令牌桶，固定限制为每个 IP 每分钟 5 次、每个用户名和 IP 组合每分钟 3 次，超限响应包含 `Retry-After`。多 gateway 实例部署时，生产环境应使用共享限流后端，确保实例间合并计数。

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

认证成功后，gateway 读取 User，确认 `spec.enabled=true` 且 `spec.scopes` 恰好包含一项允许的 User scope，然后按 4.3 节规则签发具有同一单一 scope 的完整 JWT。登录请求不能指定 scope、有效期或其他权限 claim。登录失败统一返回 401，不区分用户不存在、密码错误、角色配置非法或账号锁定。登录请求体和密码不得写入访问日志、审计日志或错误响应。

登录成功返回：

```json
{
  "token": "<jwt>",
  "tokenType": "Bearer",
  "expiresIn": 86400
}
```

响应必须包含 `Cache-Control: no-store`；`expiresIn` 为从签发时间开始计算的秒数。

用户通过 `PUT /auth/users/{name}/password` 修改本人密码：

- 路径 `{name}` 必须与 JWT `sub` 完全一致。
- 请求提交 `currentPassword` 和 `newPassword`；gateway 先调用认证接口验证当前密码，再调用内部密码设置接口。
- gateway 将 `newPassword` 转换为 apiserver 内部接口要求的 `password` 字段并转发，不在本地保存密码。

密码修改成功返回 204，不返回响应体。错误语义如下：

| 场景 | HTTP 状态码 |
|------|-------------|
| 请求体非法、包含未知字段或 `newPassword` 不满足密码规则 | 400 |
| 未携带 token或 token 非法 | 401 |
| `currentPassword` 错误 | 401 |
| 路径 `{name}` 与 JWT `sub` 不一致 | 403 |
| User 不存在或 `spec.enabled=false` | 403 |
| 超过限流 | 429 |
| apiserver IAM 不可用 | 503 |

密码修改不撤销已签发的 JWT，现有 token 继续有效至 `exp`。

### 4.5 UserResolve

`ebs:user`、`ebs:ops` 和 `ebs:admin` token 在完成 4.3 节全部 JWT 校验后执行 UserResolve。`ebs:runner` 和 `ebs:system` 不映射为 User，也不执行 UserResolve。gateway 使用内部请求读取：

```text
GET /apis/iam.ebs/v1/users/{jwt.sub}
```

检查规则：

| 场景 | 结果 |
|------|------|
| User 不存在 | 返回 403 |
| `spec.enabled=false` | 返回 403 |
| User API 不可用且无有效缓存 | 返回 503 |

gateway 必须确认返回对象的 `metadata.name` 与 JWT `sub` 完全一致、`spec.enabled=true`，并且 `spec.scopes` 恰好包含一项且与 JWT 的唯一 scope 完全一致。角色被修改后，携带旧 scope 的 Token 在下一次 UserResolve 时返回 403。gateway 可以按用户名和 scope 缓存上述状态，缓存时间不超过 JWT 剩余有效期。

User API 的内部查询不能使用正在校验的普通用户 token，避免递归鉴权和权限提升。gateway 应使用仅具备 User 读取权限的内部凭据，且不得将该凭据转发给客户端。

### 4.6 RateLimit

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

### 4.7 InjectHeaders

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

### 4.8 ProjectAuthorize

gateway 在转发前进行用户级 Project 鉴权。

Runner 创建自身对象时，gateway 必须解析完整 JSON 对象并执行以下检查：

- JWT `sub`、`runner` claim 和 `metadata.name` 完全一致；请求不能使用 `generateName`。
- 只允许设置 `metadata.name`、`ebs.io/runner-type`、`ebs.io/runner-arch`、名称以 `ebs.io/runner-capability.` 开头的能力 labels，以及 `spec.type`、`spec.arch`、`spec.hostname`。
- type、arch labels 必须分别与 spec 字段一致；`spec.type` 只允许 `ct`、`vm`、`hw`，当前 `spec.arch` 只允许 `aarch64`、`x86_64`。
- `status` 必须为空；不得设置 `resourceVersion`、UID、timestamps、generation、managedFields、annotations、finalizers、ownerReferences、`spec.unschedulable`、`spec.taints` 或其他管理字段。

不满足条件返回 400 或 403且不转发；对象已存在由 apiserver 返回 409。gateway 不把创建转换为更新，Runner 必须 GET 已有对象并按 4.9.1 节发起受限 PUT/PATCH。

以下资源—verb 矩阵是已认证身份对非公开读取和写操作的授权规范；4.1 节公开 `GET/HEAD` 在身份校验通过后优先适用，不受 owner/member 行限制。未列出或标为“禁止”的组合默认拒绝。`get/list/watch` 统称读操作，`create/update/patch/delete` 统称写操作；资源实际不支持的 verb 即使矩阵允许也由 apiserver 拒绝。

| 资源范围 | Owner 用户 | Member 用户 | Ops | Runner | System | Admin |
|----------|------------|-------------|-----|--------|--------|-------|
| Project | `get/list/create/update/patch/delete` | `get/list`，禁止所有写操作 | 禁止 | 禁止 | 全部支持的 verb | 同 System |
| Project 子资源：Snapshot、Build、BuildInfo、RpmRepo | 全部支持的 verb | `get/list/create/update/patch`，禁止 `delete` | 禁止 | 禁止 | 全部支持的 verb | 同 System |
| Project 子资源：BuildResource | 仅自己拥有的 Project 下 `get/list`，禁止全部写操作 | 仅自己作为 member 的 Project 下 `get/list`，禁止全部写操作 | 跨 Project `get/list/create/update/patch/delete` | 禁止 | 全部支持的 verb | 同 System |
| Project 子资源：Job | 全部支持的 verb，watch 仅在 apiserver 支持时允许 | `get/list/create/update/patch`，禁止 `delete`；watch 仅在 apiserver 支持时允许 | 禁止 | 仅已分配 Job 的 `get` 和 `/status` 的 `update/patch` | 全部支持的 verb | 同 System |
| Runner 范围 Job list/watch | 禁止 | 禁止 | 禁止 | 自身路径 `get/list/watch`，由 apiserver按 `status.runner` 强制过滤 | 允许 | 同 System |
| Runner | 禁止 | 禁止 | 仅 `get/list`，禁止 watch、子资源和全部写操作 | 自身 `create/get/update/patch`，其中普通对象和 `/status` 分别受字段白名单约束；禁止 `list/watch/delete` | 全部支持的 verb | 同 System |
| User 与用户密码 | 仅本人修改密码，禁止 User API | 仅本人修改密码，禁止 User API | 仅修改本人密码，禁止 User API | 禁止 | 禁止 | 非 Admin User 支持 `get/list/update/patch/delete`，另可修改本人密码 |
| MachineAccount | 禁止 | 禁止 | 禁止 | 禁止 | 禁止 | 通过专用接口`create`；资源API支持`get/list/delete` |

矩阵中的权限还受 4.9 节完整对象比较和字段约束。User 只能通过 `/auth/register` 创建；Admin 不能读取或操作 `spec.scopes=["ebs:admin"]` 的 User，也不能设置或重置其他用户的密码。Runner 对 Job `/status` 的更新不得改变 `status.runner`。

Runner 范围 Job list/watch 必须由 apiserver根据路径中的 Runner 名称强制过滤 `status.runner`；gateway 校验该路径名称等于 token 的 `runner` claim。客户端传入的 `fieldSelector` 一律拒绝，不能依赖 Runner 客户端自行隐藏对象。单对象 Job `get` 和 `/status` 写入仍必须读取对象并校验 `status.runner` 等于 token 的 `runner` claim，不匹配时返回 403。Job 对象仍不得保存密码、访问令牌、私钥或其他明文敏感信息；执行所需凭据必须通过独立的受控凭据交付机制提供。

如果 gateway 无法确认 Project 归属，应返回 403，不能放行。

公开 `GET/HEAD` 不查询 Project owner/member。BuildResource 不属于公开读取资源；普通用户读取时，gateway 必须先读取路径指定的 Project 并校验 owner/member 关系，不能读取无归属关系 Project 下的 BuildResource。`default` 是没有同名 Project 的系统保留作用域，因此普通用户不能借助 Project owner/member 权限读取其中的全局默认对象。普通用户修改 Project 或其他 Project 子资源时，gateway 先读取 Project 并校验 owner/member 权限；需要区分 owner 与 member 的写操作继续按矩阵限制。

Runner 范围 Job list/watch 只允许 GET，拒绝客户端 `fieldSelector`，仅透传 `resourceVersion`、`timeoutSeconds`、`allowWatchBookmarks` 等受支持参数；过滤条件由 apiserver 从可信路径生成。

### 4.9 AccessLabels

#### 4.9.1 PUT/PATCH 完整对象比较

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
metadata.finalizers
metadata.ownerReferences
```

`metadata.resourceVersion` 只能保持为 `oldObject` 的值。路径中的资源类型、namespace、name 必须分别与候选对象的 `apiVersion/kind`、`metadata.namespace`、`metadata.name` 一致；缺失、冲突或试图跨 Project 移动对象均返回 400。普通对象路径只允许修改 `metadata` 和 `spec` 中角色有权修改的字段，候选对象的 `status` 必须与旧对象完全相同；`/status` 路径只允许修改授权的 `status` 字段，`metadata`（除保持原值的 `resourceVersion` 外）和 `spec` 必须与旧对象完全相同。

字段比较后的角色规则：

| 调用方和路径 | 允许发生变化的字段 | 额外约束 |
|--------------|--------------------|----------|
| Owner 更新 Project | `metadata.labels`、`metadata.annotations`、`spec` | `ebs.io/owner-user` 必须保持不变；member labels 按本节后续规则校验 |
| Member 更新 Project | 无 | 4.8 节禁止 member 修改 Project，请求返回 403 |
| Owner/Member 更新 Project 子资源普通路径 | `metadata.labels`、`metadata.annotations`、`spec` | member 禁止 DELETE，但可按 4.8 节执行 update/patch |
| Owner/Member 更新 Project 子资源 `/status` | `status` | 仅当对应资源暴露 `/status` 且 4.8 节允许该调用方更新时允许 |
| Runner 更新自身普通对象 | `metadata.labels["ebs.io/runner-type"]`、`metadata.labels["ebs.io/runner-arch"]`、`metadata.labels["ebs.io/runner-capability.*"]`、`spec.type`、`spec.arch`、`spec.hostname` | 路径名称必须等于 token 的 `runner` claim；type/arch label 必须与 spec 一致；`spec.unschedulable`、`spec.taints`、其他 labels、annotations、status 和服务端 metadata 保持不变 |
| Runner 更新自身 `/status` | `status.phase`、`status.conditions`、`status.capacity`、`status.allocatable`、`status.addresses`、`status.info`、`status.heartbeat` | 路径名称必须等于 token 的 `runner` claim；`spec` 和全部 metadata 保持不变 |
| Runner 更新已分配 Job `/status` | `status.phase`、`status.stage`、`status.startTime`、`status.endTime`、`status.resultRoot`、`status.message` | 旧对象和候选对象的 `status.runner` 均必须等于 token 的 `runner` claim；Runner 不得修改 `status.runner` 或 `status.restartCount` |
| System 更新业务资源 | 普通路径为 `metadata`、`spec`，`/status` 路径为 `status` | 仍受身份字段、subresource 隔离和 `resourceVersion` 规则约束 |
| Admin 更新非管理员 User | `metadata.labels`、`metadata.annotations`、`spec.enabled`、`spec.scopes`、`spec.displayName`、`spec.email` | 旧对象和候选对象的 `spec.scopes` 均不得为 `["ebs:admin"]`；候选 scopes 仍由 apiserver 校验为单一合法 User scope |

表中的允许字段是上限；状态值、不可变 spec 字段和资源自身校验规则仍由 apiserver 执行。没有列入允许集合的任何差异都返回 403。gateway 应在审计日志中记录请求方法、原始 patch 类型、旧/新 `resourceVersion`、被拒绝的 JSON Pointer 路径和拒绝原因，但不得记录密码、完整对象或完整 patch。

#### 4.9.2 Project access labels

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

### 4.10 ReverseProxy

gateway 使用反向代理将请求转发到 `ebs-apiserver`。

代理要求：

- 除 4.9.1 节规定的 PUT/PATCH 规范化外，保持原始 HTTP 方法。
- 保持查询参数。
- 除 access label 注入和 4.9.1 节生成完整候选对象外，保持请求体。
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
| `POST /auth/register` | 否 | 用户自助注册；创建普通 User 和初始密码，不签发 JWT |
| `POST /auth/login` | 否 | 账号密码登录，成功后签发 JWT |
| `POST /auth/runner-token` | MachineAccount Basic凭据 | 验证机机账号和Runner名称格式，签发短期 `ebs:runner` JWT |
| `POST /auth/check` | Bearer Token | 公开校验 Token 并返回身份与 scopes；无请求正文，无需额外服务身份，不执行资源授权 |
| `PUT /auth/users/{name}/password` | 是 | 用户验证当前密码后修改本人密码 |
| `POST /auth/machineaccounts` | 是 | 原子创建MachineAccount和初始凭据，仅允许 `ebs:admin` |
| `GET/HEAD /apis/ebs/v1/*` | 部分否 | 4.1 节白名单资源允许匿名 get/list 及单对象 `/status` 读取并返回完整对象；Runner、watch 和非白名单资源需要认证 |
| `POST/PUT/PATCH/DELETE /apis/ebs/v1/*` | 是 | `ebs/v1` 写请求按身份和 Project 权限校验 |
| `ANY /apis/iam.ebs/v1/users*` | 是 | Admin 可查询、修改和删除普通 User；POST 返回 405 |
| `ANY /apis/iam.ebs/v1/machineaccounts*` | 是 | MachineAccount查询和删除API；POST/PUT/PATCH返回405，创建使用`POST /auth/machineaccounts` |

不提供 `/api/*` 简化别名。客户端统一使用 Kubernetes-like API 路径，避免出现两套路由标准。

### 5.2 Watch 透传

受信任 system 调用方和 Runner 可能通过 gateway 对支持 watch 的资源建立连接：

```text
GET /apis/ebs/v1/runners/{runner}/jobs?watch=true
GET /apis/ebs/v1/runners?watch=true
```

当前只有 etcd 主存储的 Job、Runner 支持 watch；Project、Snapshot、Build、BuildInfo、RpmRepo 使用 Elasticsearch 主存储，不支持 watch。所有对外 watch 请求必须认证。Runner token通过自身 Runner范围接口 watch Job；system token可通过 gateway watch Runner。普通用户通过 Project范围的 API访问资源，Project范围的 Job watch在权限校验后透传；匿名 Job watch返回401。

每个 Runner token 同时最多建立一个 Runner 范围 Job watch。超过限制返回429并包含 `Retry-After`；连接正常结束、上游失败或客户端断开时必须释放计数。Runner watch建议使用不超过300秒的 `timeoutSeconds` 定期重连，使 token和权限能够重新校验。

gateway 需要支持流式响应：

- 不读取完整 upstream response 后再返回。
- 不为 watch 请求设置过短超时。
- 客户端断开时及时关闭 upstream 请求。
- 审计日志记录连接建立和最终关闭状态。

## 六、配置

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

### 7.2 注册 User

普通用户通过公开接口自助注册：

```bash
curl -X POST http://localhost:8080/auth/register \
  -H "Content-Type: application/json" \
  -d '{
    "username": "alice",
    "password": "example password",
    "displayName": "Alice",
    "email": "alice@example.com"
  }'
```

成功响应为 `201 Created` 和 `{"username":"alice"}`。注册不会自动登录。

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

Runner 使用 MachineAccount 长期凭据换取仅包含 `ebs:runner` 的短期 token，再通过自身范围 Job list-watch发现分配给自己的 Job：

```bash
TOKEN_RESPONSE=$(curl --fail-with-body 'http://localhost:8080/auth/runner-token' \
  --user "runner-site-a:${RUNNER_CLIENT_SECRET}" \
  -H 'Content-Type: application/json' \
  --data '{"runner":"runner-001"}')
RUNNER_TOKEN=$(printf '%s' "${TOKEN_RESPONSE}" | jq -r .accessToken)
```

```bash
curl -N 'http://localhost:8080/apis/ebs/v1/runners/runner-001/jobs?watch=true&allowWatchBookmarks=true&timeoutSeconds=300' \
  -H "Authorization: Bearer ${RUNNER_TOKEN}"
```

## 八、安全边界

| 风险 | 处理 |
|------|------|
| 未认证请求访问业务 API | 仅4.1节白名单资源及其单对象 `/status` 的GET/HEAD进入匿名读取流程；watch、Runner、IAM、写请求和未知资源返回401 |
| 公开对象包含敏感信息 | 匿名接口返回完整对象；公开资源的数据结构和写入校验必须禁止保存密码、Token、私钥等秘密，敏感数据使用非公开资源或独立受控存储 |
| 匿名批量抓取 | 按客户端IP独立低额度限流，限制collection分页大小，禁止无界list和watch |
| 注册接口被滥用或批量占用用户名 | 按客户端 IP 和用户名/IP 组合独立限流；限制请求体和字段；多实例生产部署使用共享计数 |
| User凭据泄漏 | User REST响应不包含credential字段，密码和哈希不写入日志、审计事件或错误响应 |
| 登录接口泄漏密码 | 登录请求体不记录日志，gateway 与 apiserver 之间使用 TLS，错误响应不回显输入 |
| 密码暴力尝试 | gateway 按账号和客户端地址限流，IAM 模块维护失败次数和临时锁定状态 |
| MachineAccount凭据泄漏 | 删除账号并使用新secret创建账号；每个Runner或受控站点使用独立账号和随机secret，secret只从受限文件读取且不写日志 |
| MachineAccount提升权限 | 交换接口只固定签发`ebs:runner`，调用方不能指定scope、TTL或除Runner名称外的claim |
| 已删除或禁用的用户访问 | UserResolve 返回 403，不进入业务鉴权 |
| User API 不可用 | 仅使用仍有效的短期缓存；无有效缓存时返回 503，不默认放行 |
| Admin 越权操作管理员 User | User list 过滤 `spec.scopes=["ebs:admin"]` 对象；单对象读取和写入先校验旧对象；PUT/PATCH 保护管理员角色 |
| 客户端伪造内部身份头 | 转发前删除所有客户端 `X-EBS-*`，只重建 `X-EBS-User` 和 `X-EBS-Scopes` |
| 客户端伪造 Project owner | 普通用户创建 Project 时强制覆盖 `metadata.labels["ebs.io/owner-user"]`；system 创建时校验指定 owner User |
| 客户端越权修改 Project members | 只有 owner user 或 system token 可以修改 member user labels，新增成员必须是已启用 User |
| 用户写入未授权 Project | gateway 查询 Project owner/member labels，不匹配则返回 403；公开 `GET/HEAD` 不受该写权限限制 |
| Runner 范围 Job 过滤被绕过 | gateway绑定路径身份，apiserver按 `status.runner` 强制过滤，并拒绝客户端提供 `fieldSelector` |
| token 泄漏 | 使用短有效期、固定 issuer/audience 和最小 scope；轮换单一密钥会使全部旧 token 失效，`jti` 进入审计记录以支持后续撤销扩展 |
| 公开 Token 校验接口被用于探测或请求风暴 | 不回显 Token 和具体签名失败原因；限制请求体，并按 Token 身份和客户端地址限流 |
| 请求风暴 | 基于调用方限流 |
| 上游 TLS 风险 | 生产环境启用 TLS 校验或配置 CA |
| watch 长连接占用 | 限制单调用方并发 watch 数，保留合理超时 |

## 九、测试设计

测试覆盖：

| 模块 | 场景 |
|------|------|
| Auth | 缺失 token、非法签名、非 HS256、非法 header、issuer/audience 错误、时间 claim 越界、缺失 `jti`、非法 scope 组合以及合法的 user/ops/runner/system/admin token；密钥文件缺失、非法或过短时启动失败 |
| PublicRead | 匿名与已认证调用方对 Project 的 get/list，以及五类 Project 级公开资源的 Project 范围 get/list 和单对象 `/status` GET/HEAD 使用相同的完整对象透传与分页规则；已认证请求仍先校验 Token/User；非法Token不降级匿名；Runner/IAM/write/watch/未知资源及非白名单 `/status` 拒绝；`watch=true`、`watch=1`和重复参数均不能绕过 |
| Registration | User和密码哈希单文档原子创建、注册成功但不签发token、User固定启用、未知/越权字段、用户名和email校验、密码长度、重复用户名、请求体过大、限流、IAM不可用、REST响应不包含credential以及敏感字段不入日志 |
| MachineAccountRegistration | 对象和凭据原子创建、重复名称409、非法名称/secret/TTL、响应不回显secret、通用资源POST返回405、非Admin返回403以及失败不保留可认证账号 |
| PasswordLogin | User 根据唯一的 `spec.scopes` 获得 `ebs:user`、`ebs:ops` 或 `ebs:admin`、请求 scope 不产生提权、非法或多 scope 配置、密码错误、用户不存在、用户禁用、账号锁定、认证接口不可用和登录限流 |
| RunnerTokenExchange | MachineAccount认证成功、不存在/错误secret统一401、非法Runner名称400、固定scope和TTL、独立限流、响应no-store以及凭据不入日志 |
| TokenCheck | 公开访问、合法身份与 scopes 响应、非法 Token 401、非空请求正文 400 和调用方限流 |
| UserResolve | User 不存在、名称与 JWT `sub` 不匹配、禁用、Token scope 与当前唯一 User scope 不一致、缓存命中和 User API 不可用；runner/system token 跳过 User 查询 |
| Header | 删除伪造 `X-EBS-*` 并注入可信身份 |
| ProjectAuthz | 普通用户只能写入自己拥有或作为 member 的 Project；公开读取不按 owner/member 过滤；Runner 可以创建、读取和受限更新自身 Runner，只能 list/watch 自身已分配 Job，并对匹配的单个 Job执行 get和 status写入 |
| Admin | user、runner 和 system 均不能管理 MachineAccount，仅 `ebs:admin` 可以创建、查询和删除对象 |
| AdminUser | Admin 只能 get/list/update/patch/delete 非管理员 User，list 不返回管理员，禁止 create、把用户提升为 `ebs:admin`、操作管理员 User 和重置他人密码 |
| Ops | 只允许 Runner collection list 和单对象 get；拒绝 watch、Runner 子资源、全部写操作和其他资源 |
| ObjectCompare | PUT 缺失/过期 `resourceVersion`；Merge Patch 和 JSON Patch 构造完整候选对象；拒绝不支持的 patch 类型、非法 JSON Pointer、重复 key、超大对象和跨 subresource 修改；检查后并发更新返回 409且不自动重放 |
| AccessLabels | 普通用户创建 Project 时强制写入 owner user label；system 创建时校验 owner User；PUT/PATCH 不能通过 `null`、删除父 map、`move` 或 `copy` 绕过 owner/member user label 保护 |
| RunnerObject | 创建时身份三元组一致、拒绝 status 和非白名单字段；PUT/PATCH 只允许修改自身声明字段，保护 system 管理的 unschedulable、taints、labels 和服务端 metadata；禁止 list/watch/delete 和其他 Runner |
| RunnerStatus | Runner 只能修改自身 Runner status 白名单字段和已分配 Job status 白名单字段，不能修改 Job runner、restartCount、spec 或 metadata |
| RateLimit | 超过令牌桶容量后返回 429 |
| Proxy | `/apis/ebs/v1/*` 原样透传 |
| Watch | system的 Job/Runner watch和 Runner自身范围 Job list-watch能够流式转发；校验强制过滤、fieldSelector注入、BOOKMARK、410重建、timeoutSeconds和每 Runner单连接限制；路径身份不匹配和普通用户全局 watch返回403；ES-only资源不支持 watch |
| Audit | 请求结束后记录 method/path/status/latency/user |
