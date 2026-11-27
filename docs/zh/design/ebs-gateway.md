# ebs-gateway 设计

OAuth/OIDC 社区登录和首次注册的 Gateway 后端及前端已实现，社区基本注册和登录已验证，安全及异常场景仍需验收，契约见 [OAuth 登录设计](#oauth-login)。启用 OAuth 后关闭本地密码登录和注册，Runner Token 和授权规则保持不变；首版不提供已有账号关联。

## 一、定位

BuildGraph 的字段及生命周期见 [数据模型](data-models.md#buildgraph构建依赖图)。

集群级 Config 仅 Ops/Admin 可读写，不依赖工程成员关系。详见 [构建配置设计](data-models~config.md#11-config-公共资源与访问权限)。

集群级 Script 使用 `/apis/ebs/v1/scripts`：已认证的 User（含 Ops/Admin）和 Runner 身份可读取列表和单个脚本，Service Token 返回 403，匿名请求及无效 Token 返回 401。读写统一使用已认证限流；仅 Ops/Admin 可创建、PUT/PATCH 修改和 DELETE 脚本。Watch、status 子资源和 Project-scoped 路径均不开放。删除请求若携带 UID/resourceVersion 前置条件，由 apiserver 校验；Gateway 不按脚本名称限制删除，也不校验脚本正文。字段校验及更新冲突交给 apiserver。详见 [Script 设计](data-models~script.md)。

Gateway 不开放 Build 对象及其 `/status` 的 PUT/PATCH，任何身份均不得通过 Gateway 更新 Build；Build Controller 直连 apiserver 固化增量构建的 `spec.packages`。用户创建 Build 和使用已有中止接口的权限保持不变。

Project 分类标签 `ebs.io/project-type` 的创建、修改权限及 PATCH 保护规则见 [标签约定](labels.md#33-工程分类)；分类不改变既有访问权限。

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

`ebs-gateway` 不保存业务资源，User 和 MachineAccount 由 `ebs-apiserver` 的 IAM 模块提供。OAuth 使用加密 HttpOnly Cookie 携带短期登录上下文，不增加 Gateway 到 ES/etcd 的连接；多副本共享密钥环，不依赖实例会话或粘性路由。用途、期限和重放边界见 [OAuth 上下文设计](#1041-cookie-上下文与跨副本)。匿名调用方可以读取公开业务资源的完整对象，但不能 watch、写入或访问 Runner/IAM；认证用户仍按身份获得对应写权限。gateway 通过 User API 获取用户状态，并基于认证身份限制用户写入自己有权限的 Project；通过 MachineAccount 凭据认证签发 Runner 或 Service Token；后者按 4.3.1 的专属规则授权。

统一交换路径 `/auth/machine-token` 已支持 Runner/Service Token；Service Token 按账号名称实时核验身份。
Gateway 注册入口透传 MachineAccount.spec.projects 和 permissions，每次 Service Token 请求按账号当前工程范围和操作权限授权；任一列表为空时无资源操作权限。
下文一般性的公开读取、用户及 Runner 权限不向 service 继承，授权规则见 4.3.1。

## 二、设计目标

| 目标 | 说明 |
|------|------|
| 统一入口 | 用户、外部系统和 runner 统一通过 gateway 访问 `ebs/v1` API |
| 公开读取 | 匿名和已认证调用方均可 get/list Project、Snapshot、Build、BuildInfo、BuildGraph、RpmRepo 和 Job 的完整对象，不能匿名 watch |
| 统一认证 | gateway 提供用户自助注册和登录入口，调用 apiserver IAM 模块管理账号凭据；登录成功后由 gateway 签发 JWT，业务请求进入 apiserver 前由 gateway 完成 token 校验 |
| 用户校验 | 通过 apiserver 的 User API 检查用户是否存在并启用 |
| 机机认证 | 统一验证 MachineAccount 凭据，按账号类型签发短期 Runner/Service Token；Service 按工程范围和显式操作权限授权 |
| 请求转发 | 将合法请求反向代理到 `ebs-apiserver` |
| Watch 透传 | 为已授权的 Project 范围和 Runner 范围 Job 列表透传长连接和流式响应 |
| 审计记录 | 记录请求路径、方法、状态码、耗时和调用方信息 |
| 限流保护 | 对调用方进行基础限流，保护 apiserver |
| 写入隔离 | 基于用户身份和 Project labels 限制写入范围，支持 Project 在用户间共享 |
| 无业务状态 | 不保存业务资源或直接读写其存储；OAuth 短期上下文使用加密 Cookie |

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
- `spec.scopes` 必须恰好包含 `ebs:user`、`ebs:ops` 或 `ebs:admin` 中的一项；密码注册用户名为 `admin` 时由 apiserver 赋予 `["ebs:admin"]`，其他用户名赋予 `["ebs:user"]`。OAuth 注册固定为普通用户，不按用户名提权。
- 管理员标记由受信任内部流程通过 apiserver User API 设置，gateway 不提供对外提升管理员的接口。
- apiserver IAM 模块验证用户密码或 MachineAccount client secret，gateway 签发 JWT；IAM资源保存gateway鉴权需要的主体状态和token TTL。
- 普通用户通过公开注册接口创建自己的 User 和初始密码。
- gateway 每次 UserResolve 都读取 User API，不缓存用户状态。

### 3.2 业务资源归属

| 资源 | 归属规则 |
|------|----------|
| User | 集群级资源，由 apiserver 用户管理插件提供；`metadata.name` 是用户唯一标识 |
| Project | 集群级资源，`metadata.name` 是项目唯一标识，访问权限由 Project labels 表达 |
| Snapshot | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| Build | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| BuildInfo | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| BuildGraph | Project 级依赖图，公开只读；由 BuildInfo Controller 通过内部 API 创建和更新 |
| RpmRepo | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| Job | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| Runner | 集群级资源，不归属于 Project |

Project 级资源通过 Project API 创建：

```text
/apis/ebs/v1/projects/{project}/snapshots
/apis/ebs/v1/projects/{project}/builds
/apis/ebs/v1/projects/{project}/buildinfos
/apis/ebs/v1/projects/{project}/buildgraphs
/apis/ebs/v1/projects/{project}/rpmrepos
/apis/ebs/v1/projects/{project}/jobs
```

`{project}` 会由 apiserver 映射到对象的 `metadata.namespace`。gateway 不给 Snapshot、Build、BuildInfo、BuildGraph、RpmRepo、Job 注入用户字段，这些对象通过 Project 命名空间继承 Project 访问权限。

Project 创建和更新时，gateway 使用 labels 表达 Project 的 owner user 和 member users：

标签名称、值域和写入权限的集中定义见 [EulerMaker 标签约定](./labels.md)。

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

普通用户、Ops 和 Admin 创建 Project 时，gateway 统一写入或覆盖 `ebs.io/owner-user=<jwt.sub>`，不采用客户端指定的 owner，也不额外查询 owner User。

Gateway 不开放 Project 删除路由，工程删除由内部运维通过 apiserver 执行。用户身份仅 Project owner 可更新已有 Project；service 可按 4.3.1 更新授权范围内的已有工程主资源；更新时 `ebs.io/owner-user` 必须保持不变。成员用户 label 用于表达共享权限，owner 可增删；新增的 member user 必须对应已存在且启用、scope 为 `ebs:user` 或 `ebs:ops` 的 User。

`ebs.io/build-purpose` 的创建需要 Ops/Admin，更新需要同时具备 owner 和 Ops/Admin 身份。普通 owner 只能保留原值；替换整个 labels 或通过 PATCH 删除同样受校验。Admin owner 仅获准修改该标签，其他工程更新权限不扩大；非 owner 的 Ops/Admin 仍被拒绝。用途固化及调度约定见 [标签约定 3.4](labels.md#34-门禁构建用途)。

匿名调用方可以读取第 4.1 节定义的公开资源，认证用户通过 Project API 访问 Project 级资源。Runner 使用独立的 runner token。普通用户 token 只有在对应 User 存在且已启用时才有效；gateway 直接使用 JWT `sub` 作为用户身份，并限制用户只能写入自己拥有或被授权为 member 的 Project 和 Project 子资源。

## 四、请求处理流程

业务请求的处理链：

```text
Request
  -> Audit
  -> Recovery
  -> PrepareQuery（Watch 校验及普通列表分页规范化）
  -> Authenticate（按路由认证模式解析当前身份）
  -> LimitIdentity（身份/IP 或匿名 IP）
  -> Authorize（路由身份准入、Service 工程/操作权限、对象归属）
  -> 请求准备与字段权限校验
  -> ReverseProxy
  -> Response
```

全局 `PrepareQuery` 在认证前检查 Watch 支持；允许的 Watch 保留原查询参数，后续仍须认证和授权。非 Watch 请求仅对显式配置 `ListLimit` 的 GET 列表规范化分页。当前适用于 Project、Build、Job、OldJob、Snapshot、BuildInfo、BuildGraph、RpmRepo 列表；未配置的 Runner、IAM、Config、Script 等列表保持原行为。缺失、非法、重复或超出上限的 `limit` 统一设置为路由上限，合法值及其他查询参数保留。

### 4.1 公开读取

Gateway 在要求 Bearer Token 前先识别匿名公开读取。只有不携带 Authorization header 的 GET 请求可以直接进入该分支；携带 Token 时必须先完成 Token 和 User 状态校验，非法、过期、已禁用或身份不一致时返回 401/403，不能降级为匿名访问。service 不进入公开读取兜底，其请求遵循 4.3.1 的专属授权规则。其他适用身份校验通过后，公开读取使用与匿名调用方相同的路由、完整对象、分页和响应头规则，不执行 Project owner/member 读取过滤。Snapshot、BuildInfo、BuildGraph、RpmRepo 的集合、对象及对象 `/status` 仅注册 GET。

对匿名和已认证调用方开放以下读取路径：

| 资源 | 对外公开 API |
|------|--------------|
| Project | `/apis/ebs/v1/projects`、`/projects/{name}`；状态通过普通 GET 的 `includeFields=status` 获取 |
| Snapshot | `/apis/ebs/v1/projects/{project}/snapshots[/{name}]`、`/projects/{project}/snapshots/{name}/status` |
| Build | `/apis/ebs/v1/projects/{project}/builds[/{name}]`、`/projects/{project}/builds/{name}/status` |
| BuildInfo | `/apis/ebs/v1/projects/{project}/buildinfos[/{name}]`、`/projects/{project}/buildinfos/{name}/status` |
| BuildGraph | `/apis/ebs/v1/projects/{project}/buildgraphs[/{name}]`、`/projects/{project}/buildgraphs/{name}/status` |
| RpmRepo | `/apis/ebs/v1/projects/{project}/rpmrepos[/{name}]`、`/projects/{project}/rpmrepos/{name}/status` |
| Job | `/apis/ebs/v1/projects/{project}/jobs[/{name}]`、`/projects/{project}/jobs/{name}/status` |
| 历史 Job | `/apis/ebs/v1/projects/{project}/oldjobs[/{name}]`，仅 GET，无 `/status` |

`GET /apis/ebs/v1/build-targets` 仅供已认证用户读取可选的发行版及其架构，与 `service-endpoints` 通过全局请求认证和限流。限流未触发时，非 Watch 请求携带无效 Token 或未登录时返回 401；Watch 请求由全局 `PrepareQuery` 在认证前拒绝并返回 403，全局准入规则拒绝 Runner、Service 身份。Gateway 读取内置构建配置，仅返回按名称排序的 `items[].distro` 和 `items[].arches`，不返回镜像地址、Config 元数据或原文。配置不可用时返回 503。Config 不应存放凭据。

Runner 通过专用服务地址投影接口读取 Artifact Manager 和 Git Server 的 Runner 地址，不改变上述 Config 读取权限。接口、授权和响应契约见 4.11 节。

匿名请求不开放 Runner、Runner 子资源、User、MachineAccount、非白名单资源的 `/status` 或其他未列入白名单的新资源。公开对象的 `/status` 仅允许单对象读取并返回完整对象；collection 不存在 `/status`。写操作必须通过认证，因此匿名调用方不能借公开 `/status` 修改状态。查询参数中出现非 `false` 的 `watch` 值即按 Watch 请求处理；不支持 Watch 的已匹配路由直接返回 403，支持 Watch 的列表路由仍要求认证，匿名请求返回 401。`watch=1`、空值或重复参数不能绕过检查。Project、Snapshot、Build、BuildInfo、BuildGraph 和 RpmRepo 本身不支持 watch，也不能由 Gateway 模拟轮询。

公开读取直接透传 apiserver 返回的完整单对象或 List，不解码、删除或重写对象字段。公开范围内的 `metadata`、`spec` 和 `status` 均可被匿名或已认证调用方读取，包括对象后续新增的字段。因此 Project、Snapshot、Build、BuildInfo、BuildGraph、RpmRepo 和 Job 的 API 数据结构不得保存密码、Token、私钥或其他不应公开的信息；需要保密的数据必须存放在非公开资源或独立的受控存储中。

Gateway 管理 API 不注册 HEAD。代理不区分公开或已认证响应，不使用公开响应标记或响应头白名单；保留上游的 `Retry-After`、`Warning`、`Audit-ID` 和压缩相关响应头，逐跳头由标准反向代理处理。请求侧继续移除调用方凭据和不可信代理头，不向上游注入最终用户身份。

匿名请求与已认证请求共用限流器和额度配置，按 `anonymous/{clientIP}` 隔离计数；collection 必须设置服务端允许的 `limit` 上限，禁止匿名调用方请求无界列表。Gateway 不注入最终用户身份，使用自身内部身份读取 apiserver 并原样转发对象响应。审计日志使用固定身份 `anonymous`，记录资源、verb、客户端地址、响应数量和 requestID。

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
| `token_type` | `user`、`runner` 或 `service`；用户权限另由 scope 表示 |
| `jti` | JWT 唯一标识，不记录完整 token |
| `user_agent` | 客户端 User-Agent |

### 4.3 Auth

客户端通过 Bearer Token 访问 gateway：

```text
Authorization: Bearer <token>
```

Gateway 的用户认证按部署配置使用密码或 OAuth；OAuth 流程见第十章。MachineAccount 换取 Runner/Service Token 不受此切换影响。密码登录时，Gateway 将账号密码提交给 apiserver IAM 模块验证，认证成功后再读取 User，确认用户存在且 `spec.enabled=true`。Runner 换取 token 时，Gateway 将 MachineAccount client ID 和 client secret 提交给 apiserver IAM 模块验证，并独立校验请求中的 Runner 名称。以上检查通过后，由 gateway 使用同一 HMAC 密钥签发对应的短期 JWT。Apiserver IAM 模块只负责凭据验证，不签发、解析或刷新 JWT。后续业务请求由 gateway 验证 JWT，并根据 token 类型决定是否执行 UserResolve。

普通用户 JWT claims：

```json
{
  "sub": "alice",
  "type": "user",
  "scopes": ["ebs:user"],
  "iss": "ebs-gateway",
  "aud": "ebs-api",
  "iat": 1790000000,
  "nbf": 1790000000,
  "exp": 1790003600,
  "jti": "01JEXAMPLETOKENID"
}
```

Runner JWT 使用 `type=runner` 标识身份类别，`sub` 保存 Runner 对象名称：

```json
{
  "sub": "runner-001",
  "type": "runner",
  "scopes": [],
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
| `sub` | 稳定调用方标识；普通用户与 User `metadata.name` 一致，Runner 与 Runner 对象名称一致，Service 与 MachineAccount 名称一致 |
| `type` | 主体类别：`user`、`runner` 或 `service` |
| `scopes` | 用户权限；`type=user` 恰好一个，其他类型为空数组 |
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

Gateway 必须验证 `sub`、`type`、`scopes`、`iss`、`aud`、`iat`、`nbf`、`exp` 和 `jti`。`sub` 必须是非空字符串，`type` 必须是合法主体类别，`scopes` 必须是数组；`type=user` 时恰好包含一个合法用户权限，`type=runner` 或 `type=service` 时为空。`iss` 和 `aud` 必须分别精确等于固定值 `ebs-gateway` 和 `ebs-api`，不接受多 audience 数组。普通用户 token 有效期在代码中固定为 24 小时，允许的时钟偏差固定为 30 秒，接受的 token 最大有效期固定为 24 小时；`iat` 和 `nbf` 均不得晚于“当前时间 + 时钟偏差”，`exp` 必须晚于“当前时间 - 时钟偏差”、`iat` 和 `nbf`，且 `exp-iat` 不得超过最大有效期。缺失必需 claim、claim 类型错误或校验失败均返回 401。

固定 scope 及合法组合：

| Token 类型 | `type` | 合法 scopes | 签发方式 | 是否 UserResolve |
|------------|--------|-------------|----------|------------------|
| 普通用户 | `user` | 仅 `ebs:user` | 由密码或 OAuth 登录签发 | 是 |
| 运维用户 | `user` | 仅 `ebs:ops` | `spec.scopes=["ebs:ops"]` 的 User 通过密码或 OAuth 登录签发 | 是 |
| Runner | `runner` | `[]` | 只由 `POST /auth/machine-token` 使用 MachineAccount 凭据签发 | 否 |
| Service | `service` | `[]` | `POST /auth/machine-token` 使用 service 账号凭据签发 | 否，实时核验 MachineAccount |
| 管理员 | `user` | 仅 `ebs:admin` | `spec.scopes=["ebs:admin"]` 的 User 通过密码或 OAuth 登录签发 | 是 |

JWT 在线签发包括用户密码/OAuth 认证和机器账号认证：`POST /auth/login` 根据 User 唯一的 `spec.scopes` 项签发 `type=user` 且仅包含 `ebs:user`、`ebs:ops` 或 `ebs:admin` 之一的 token；`POST /auth/machine-token` 根据 MachineAccount 类型签发 runner 或 service Token，scopes 均为空。调用方不能指定 type、scope、`sub` 或有效期；Runner Token 的 `sub` 从 runner 账号请求中的合法 Runner 名称生成。用户权限不可重复、未知或组合，主体类型与 scopes 不匹配时返回 401。所有签发路径使用同一 HMAC 密钥和固定 issuer `ebs-gateway`，生成不可预测且不重复的 `jti`，并遵守固定的 24 小时验证上限；user、ops 和 admin token 固定为 24 小时，机器账号 token 由 MachineAccount 的 `tokenTTLSeconds` 决定且最长 24 小时。

此次 Token 格式变更不兼容缺少 `type` 或仍使用旧机器权限 scope 的 Token；部署后用户需重新登录，Runner 需重新换取 Token。

普通用户 JWT 不携带额外的资源归属信息，`sub` 就是唯一用户身份；Runner 使用独立的 `type=runner`。

认证后的 `Principal` 统一以 `Subject` 保存 JWT `sub`，不另存 Runner 名称。`Type=runner` 时，
Runner 名称匹配、Job 归属校验和 Watch 并发限制均使用 `Subject`。

认证失败返回：

| 场景 | HTTP 状态码 |
|------|-------------|
| 未携带 token | 401 |
| header、签名或算法不合法 | 401 |
| issuer、audience 或时间 claims 不合法 | 401 |
| scope 未知、冲突或组合不合法 | 401 |
| `sub` 缺失、为空或类型错误 | 401 |

<a id="machine-token"></a>

#### 4.3.1 MachineAccount Token 交换与固定接口授权

统一机器账号交换路径及 Runner 调用已调整为 `/auth/machine-token`，OpenAPI 同步更新，不保留旧路径别名。
两类账号均可换取对应 Token，service 使用空对象请求；资源操作按账号当前工程范围及 `projects:update`、`builds:create`、`builds:abort` 权限授权。

Runner 使用 HTTP Basic 提交 MachineAccount 名称和 client secret，并在 JSON 请求体中声明 Runner 名称：

```text
POST /auth/machine-token
Authorization: Basic base64(<client-id>:<client-secret>)
Content-Type: application/json
```

```json
{"runner":"runner-001"}
```

Gateway 限制请求体大小并拒绝未知字段，调用 `/internal/iam/v1/machineaccounts/authenticate`，
在正文中传递 `name=client-id` 和 `clientSecret` 验证凭据。Token 类型只能来自已认证账号的 `spec.type`：

| 账号类型 | 请求正文 | Token 内容 |
|----------|----------|------------|
| `runner` | `{"runner":"runner-001"}` | `type=runner`，`sub` 为请求中的 Runner 名称 |
| `service` | `{}` | `type=service`，`sub` 为 MachineAccount 名称 |

两类 Token 的 `scopes=[]`。runner 名称必须原样满足 DNS1123 label；service 请求禁止携带 runner 字段。
请求不能指定身份类型、主体、有效期或授权列表；认证响应类型缺失或未知时返回 503。
Service Token 不映射到同名 User；Runner Token 只允许操作自身 Runner 和分配给自己的 Job。
Runner 的 MachineAccount 名称只作为审计字段 actor；Service Token 的 actor 与 sub 均为机器账号名称。

成功响应为：

```json
{
  "accessToken": "<jwt>",
  "tokenType": "Bearer",
  "expiresIn": 3600
}
```

响应必须包含 `Cache-Control: no-store`。账号不存在、凭据错误和账号锁定统一返回 401，不能泄漏账号状态；请求或 Runner 名称非法返回 400；超过独立的 client ID/IP 令牌桶限制返回 429 并包含 `Retry-After`；IAM 不可用返回 503。client secret、Basic header、完整 JWT 和凭据验证细节不得进入访问日志或审计日志。每次成功和失败交换都记录 client ID、目标 Runner、结果、客户端地址；成功记录签发 token 的 `jti`。

两类客户端均不使用 refresh token，到期前使用 MachineAccount 凭据重新交换。
删除账号阻止新 Token 签发；既有 Runner Token 最长 24 小时自然失效。
Service Token 每次请求实时核验账号，删除后的请求不能继续通过认证；已通过核验的在途请求不作回滚保证。

管理员通过 `POST /auth/machineaccounts` 原子创建 MachineAccount 和初始凭据：

```json
{
  "name": "runner-site-a",
  "clientSecret": "base64url-encoded-random-secret",
  "type": "runner",
  "tokenTTLSeconds": 3600
}
```

`name` 必填，必须原样满足 DNS1123 label 约束且不超过 63 个字符。`clientSecret` 必须是无填充的 Base64URL 字符串，解码后至少包含 32 字节，编码后长度不得超过 256 字节。`tokenTTLSeconds` 可选，默认为 3600，取值范围为 300 至 86400。Gateway 按原值校验和转发 `name` 与 `clientSecret`，不执行 trim、大小写转换或其他规范化。注册同时接受 `projects`、`permissions`，仅 service 允许非空授权列表。Gateway 校验请求格式，apiserver 在生成凭据及持久化前完整校验授权内容；未知字段或任一字段非法时返回 400。

Gateway要求 admin 权限，校验请求后调用`POST /internal/iam/v1/machineaccounts/register`。成功返回201和`{"name":"runner-site-a"}`，不回显secret；名称已存在返回409，非法请求返回400。管理端将名称作为`clientID`、原始secret作为`clientSecret`组成Runner使用的机机凭据文件并通过受保护渠道下发。

##### Service Token 的工程范围与操作权限

MachineAccount.spec 增加 `projects` 和 `permissions`，注册和持久化约束见 [MachineAccount 类型](ebs-apiserver.md#machineaccount-type)。`projects` 为精确工程名称列表，不支持通配符；权限适用于列表中的每个工程。任一列表缺省或为空时无资源操作权限，但仍可换取 Token 和调用 `/auth/check`。不增加独立权限对象，不根据客户端传入的权限、JWT scopes 或同名 User 推导授权。

| 权限 | 方法 | 路径 | 能力 |
|------|------|------|------|
| `projects:update` | PUT / PATCH | `/apis/ebs/v1/projects/{project}` | 更新已有工程主资源，不限制为 packageRepos |
| `builds:create` | POST | `/apis/ebs/v1/projects/{project}/builds` | 创建该工程的 Build |
| `builds:abort` | POST | `/apis/ebs/v1/projects/{project}/builds/{name}/abort` | 中止该工程的指定 Build |

三个权限相互独立：创建 Build 不自动获得中止权限，中止权限不要求具有创建权限。未配置构建类型、发布或优先级专属限制，`builds:create` 可以提交 apiserver 允许的构建输入，仍受现有默认值和业务校验约束。`builds:abort` 可作用于授权工程中的任意 Build，不要求由该账号创建；需要更小权限范围时使用独立工程和账号。

Service 不映射到同名 User，不要求成为工程 owner/member，也不继承用户、Runner 或匿名读取权限。Project 更新沿用完整对象更新及 Merge Patch / JSON Patch 处理；owner、UID、resourceVersion 等受保护字段仍遵守现有校验，service 不能修改门禁用途标签。成员标签变更仍核验对应用户，apiserver 继续执行对象完整校验。Build 创建在授权后原样代理，由 apiserver 校验路径与 namespace 一致性、构建输入，处理默认值和并发构建限制。Gateway 不预读或重写 Build 创建正文，保留限流和上游错误透传。

Build 中止复用既有 `/abort` 路由及请求/响应契约，不通过主资源或 `/status` PUT/PATCH 中止，不直接调用 Job `/abort`。Gateway 在转发前执行 service 授权；apiserver 继续处理对象不存在、并发和终态等业务情况。接口返回仅表示服务端已处理中止请求，不代表所有派生 Job 已停止执行或完成清理。

请求处理顺序：

1. 校验 JWT；在公开读取分支前读取一次 `GET /apis/iam.ebs/v1/machineaccounts/{sub}`，确认名称一致且 `type=service`，不比较账号 UID，不缓存。
2. 校验返回授权结构，并将本次读取的 `projects`、`permissions` 放入服务端请求上下文，授权阶段复用该结果，不重复 GET。缺省列表按空列表处理；错误类型、非法名称或未知权限视为 IAM 响应异常，不部分放行。
3. 从已解析路由确定目标工程：Project 对象路由取对象名称，Build 路由取 Project 路径参数；不能从请求正文或查询参数扩大范围。
4. 同时检查工程命中及路由对应权限，再执行现有候选对象/字段检查或 `/abort` 转发；身份、权限和业务校验全部通过后才调用上游写入。

账号不存在或名称/类型不匹配返回 401，上游不可用或响应异常返回 503，工程未授权、缺少操作权限或操作未开放返回 403（未注册的路径或方法走未匹配路由，认证通过后返回 404）。Token 只携带身份，不固化授权列表；每次请求按账号当前配置执行，已通过核验的在途请求不作撤回保证。公开业务资源读取继续主动不带 Authorization；带 Service Token 的资源读取请求（包括 Script）返回拒绝，不降级为匿名。

除上述三项操作和 `/auth/check` 外，资源操作默认拒绝，包括 Project 创建/删除、Build 删除/普通更新、所有 `/status` 写入，以及 Runner、Job、Config、Script 和 IAM 管理。授予 `projects:update` 不会获得 MachineAccount 或其他资源更新能力。

授权随 Admin 调用 `POST /auth/machineaccounts` 创建账号写入；前端创建和编辑 service 账号时支持配置工程名称及三个操作权限。Admin 可通过 `PUT/PATCH /apis/iam.ebs/v1/machineaccounts/{name}` 修改 runner 和 service 账号的 `spec.tokenTTLSeconds`（300～86400 秒），service 账号还允许修改 `spec.projects`、`spec.permissions`，不能改变账号类型、元数据或凭据。普通用户、Ops、Runner Token 和 Service Token 均不具备该更新能力。Gateway 保留原始方法、正文、Content-Type 和查询参数直接转发，不预读账号、不将 PATCH 转为 PUT。Apiserver 校验不可变字段、有效期和授权内容，并处理版本冲突；上游错误原样返回，不自动重试覆盖。PUT/merge patch 应携带当前 UID/resourceVersion，JSON Patch 使用 test 操作保护对象身份和版本。有效期变更仅影响之后签发的 Token，已签发 Token 的到期时间不变。授权变更对后续请求生效，既有 Token 不需重签，已受理操作不撤销。按现有名称身份契约，同名账号重建后未过期旧 Token 可以继续使用，并按新账号授权判断。更换 secret 不撤销已签发 Token；若必须隔离旧 Token，使用新的账号名称。上线顺序为 apiserver mapping/字段支持、准备显式授权账号、Gateway 启用范围校验；缺少授权的旧账号不再全工程放行。

审计记录账号名称、目标工程、路由要求的权限、授权允许/拒绝结果、最终 HTTP 状态、请求路径、方法及请求 ID，不记录凭据、完整 JWT 或请求正文；授权通过不代表后续业务更新成功。测试覆盖三个权限的单独及组合授权、跨工程拒绝、空授权、普通字段保护、中止不越权到 Job 或 `/status`、匿名 GET 与携带 service 的 GET 区别，以及账号删除/同名重建、IAM 异常和多副本读取同一授权配置。

#### 4.3.2 公开 Token 校验

Gateway 公开提供 `POST /auth/check`。调用方只携带待校验的 Bearer Token：

```http
POST /auth/check
Authorization: Bearer <token>
```

该接口不要求 mTLS 客户端证书、服务凭据或其他服务身份。Gateway 验证 JWT 签名、issuer、audience、时间字段和 scope 结构；成功返回认证身份、Token scopes 和过期时间，调用方根据自身需求检查主体类型及权限。Token 非法返回 401，携带非空请求正文返回 400，触发 `{type}/{sub}/{clientIP}` 限流时返回 429。接口只执行认证、身份准入和限流，不执行资源授权；有效的 User、Runner 和 Service 身份均可调用，Service 无需配置工程或操作权限。

- User：查询当前 IAM User，确认账号名称匹配、已启用且角色与 Token 一致。账号不存在、已禁用或角色不匹配时返回 403。
- Runner：不查询 User、Runner 或 Job。
- Service：按上述规则实时核验 MachineAccount；账号不存在、名称或类型不匹配时返回 401。

User 或 MachineAccount 查询不可用时返回 503。

响应中的 `identity.type` 为 user、runner 或 service；service 的名称为 MachineAccount 名称。
用户权限由 `identity.scopes` 表示；Runner 和 Service 的 scopes 为空数组，不在响应中返回服务授权列表或凭据。

### 4.4 RegistrationAndPasswordLogin

本节本地密码登录和注册仅在 OAuth 未启用时可用；启用 OAuth 后，`POST /auth/login` 和 `POST /auth/register` 直接返回 403，不调用 IAM。OAuth 提供方故障不会自动恢复本地入口。

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
- 客户端不能提交 `metadata`、labels、scopes、`enabled` 或其他 User 字段；新注册 User 的 `spec.enabled` 固定为 `true`，`spec.scopes` 由 apiserver 按用户名赋值：`admin` 为 `["ebs:admin"]`，其他用户名为 `["ebs:user"]`。部署时应在开放匿名注册入口前由运维人员注册 `admin`，避免该名称被他人抢注。

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
POST /internal/iam/v1/users/authenticate
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

`type=user` 的 token 在完成 4.3 节全部 JWT 校验后执行 UserResolve。`type=runner`、`type=service` 不映射为 User，也不执行 UserResolve；service 按 4.3.1 实时核验 MachineAccount。gateway 使用内部请求读取：

```text
GET /apis/iam.ebs/v1/users/{jwt.sub}
```

检查规则：

| 场景 | 结果 |
|------|------|
| User 不存在 | 返回 403 |
| `spec.enabled=false` | 返回 403 |
| User API 不可用 | 返回 503 |

gateway 必须确认返回对象的 `metadata.name` 与 JWT `sub` 完全一致、`spec.enabled=true`，并且 `spec.scopes` 恰好包含一项且与 JWT 的唯一 scope 完全一致。角色被修改后，携带旧 scope 的 Token 在下一次 UserResolve 时返回 403。

User API 的内部查询不能使用正在校验的普通用户 token，避免递归鉴权和权限提升。gateway 应使用仅具备 User 读取权限的内部凭据，且不得将该凭据转发给客户端。

### 4.6 RateLimit

通用限流在全局请求链的认证之后执行，不在路由或业务 Guard 中重复扣减：

- `LimitIdentity` 位于认证之后，匿名和已认证请求统一复用 `limits` 及相同的额度配置。已认证请求使用 `{type}/{sub}/{clientIP}`，没有认证身份的请求使用 `anonymous/{clientIP}`，不同 key 独立计数。此层覆盖公开资源、OpenAPI、providers、登录及注册入口；已认证用户读取公开资源也使用身份 key。认证失败不进入此层，也不消耗通用限流额度。
- `GET /healthz` 使用独立链路，不进入认证或限流；限流器不包含健康检查特判。其他方法、相似路径仍走业务链。业务链中不支持的 Watch 在限流前返回 403。允许的 Watch 仅在建连时扣减请求额度，事件流不逐条扣减，Runner 并发 Watch 限制独立保留。

配置：

| 配置 | 默认值 | 说明 |
|------|--------|------|
| `--rate-limit-per-sec` | `100` | 每个身份/IP 或匿名 IP key 每秒补充令牌数 |
| `--rate-limit-burst` | `200` | 每个身份/IP 或匿名 IP key 的桶容量 |

超过限流返回：

```text
HTTP 429 Too Many Requests
Retry-After: 1
```

通用令牌桶按实例、按限流器最多保留 4096 个键；容量用尽时拒绝新键，已有键继续按原额度计算，不驱逐活跃记录。
请求触发的过期扫描最多每分钟执行一次，清理空闲至少 10 分钟且额度已完全恢复的记录，避免清理导致额度提前重置。
限流按 Gateway 副本独立计算，不提供集群全局配额。当前不信任转发头，客户端 IP 取自直接连接；位于 Nginx、NAT 或共享代理后的匿名请求及认证专用 IP 限制可能共用额度，不能直接信任外部传入的 `X-Forwarded-For`。

登录、注册、机器换 Token、OAuth 的专用限制继续由各 Handler 执行，不因全局限流而删除。本地密码登录同时限制每个 IP 每分钟 20 次（桶容量 20）及每个用户名/IP 组合每分钟 5 次（桶容量 5），专用 IP 限流在正文解析和 IAM 查询前执行。专用限制保留各自的 `Retry-After`；通用额度通过不代表专用额度通过。

### 4.7 上游请求头

用户和 Runner 权限由 Gateway 在转发前校验；apiserver 后续仅通过 mTLS 识别组件身份，不信任请求头中的身份信息。转发时删除外部 `Authorization`、`Proxy-Authorization` 和代理来源头，避免把客户端凭据传给 apiserver。

Runner 范围的 Job list-watch 请求还需校验 token 中的 Runner 名与路径名称一致，随后由 apiserver 固定添加 `status.runner` 过滤条件；不依赖身份头绑定。

### 4.8 ProjectAuthorize

gateway 在转发前进行用户级 Project 鉴权。

Runner 创建接口仅允许有效的 Runner Token 自行注册，普通用户、Ops/Admin 和 Service Token 均不得创建 Runner。全局身份准入依据路由规则限制身份，路由使用 `prepareRunnerCreate → upstream.Forward`；正文准备阶段必须解析完整 JSON 对象并执行以下检查：

- JWT `sub` 和 `metadata.name` 完全一致；请求不能使用 `generateName`。
- 只允许设置 `metadata.name`、`ebs.io/runner-type`、`ebs.io/runner-arch`、名称以 `ebs.io/runner-capability.` 开头的能力 labels，以及 `spec.instanceID`、`spec.type`、`spec.arch`、初始 `spec.taints`。污点仅在创建时允许声明，已有对象的污点由 Ops/Admin 管理，Runner 更新仍须保留原值。
- `status` 必须为空；不得设置 `resourceVersion`、UID、timestamps、generation、managedFields、annotations、finalizers、ownerReferences、`spec.unschedulable` 或其他管理字段。

`spec.instanceID` 必填和 UUID v4 格式、type 取值、`arch` 必填以及 type/arch labels 与 spec 一致性均由 apiserver 统一校验。
污点 key/value 格式、effect 枚举和 key/effect 组合唯一性也由 apiserver 在创建和更新时统一校验；Gateway 仅保留调用方身份和字段权限限制。
apiserver 不限制 `arch` 的枚举值。对象已存在由 apiserver 返回 409。gateway 不把创建转换为更新，Runner 必须 GET 已有对象并比较 `instanceID`，相同才按 4.9.1 节发起受限 PUT/PATCH，不同则停止注册。

以下资源—verb 矩阵适用于 user 和 runner；service 仅按 4.3.1 的专属授权规则处理。4.1 节公开 `GET` 在身份校验通过后优先适用，不受 owner/member 行限制。未列出或标为“禁止”的组合默认拒绝。`get/list/watch` 统称读操作，`create/update/patch/delete` 统称写操作；资源实际不支持的 verb 即使矩阵允许也由 apiserver 拒绝。

| 资源范围 | Owner 用户 | Member 用户 | Ops | Runner | Admin |
|----------|------------|-------------|-----|--------|-------|
| Project | `get/list/create/update/patch`，用途标签另需 Ops/Admin；禁止删除 | `get/list`，禁止所有写操作 | 按 owner/member 关系同普通用户；owner 可管理用途标签 | 禁止 | `get/list/create`；仅 owner 可 PUT/PATCH 用途标签，禁止其他更新及删除 |
| Project 子资源：Snapshot、BuildInfo、BuildGraph、RpmRepo | 仅 GET | 仅 GET | 仅 GET | 禁止 | 仅 GET |
| Project 子资源：历史 Job（oldjobs） | 仅 GET | 仅 GET | 仅 GET | 禁止 | 仅 GET |
| Project 子资源：Build | 可读取、创建、删除和中止；禁止 PUT/PATCH | 可读取、创建和中止；禁止删除、PUT/PATCH | 按 owner/member 关系同普通用户 | 禁止 | 可读取；仅 owner/member 可创建和中止，仅 owner 可删除；禁止 PUT/PATCH |
| 集群级 Config | 禁止 | 同 Owner | `get/list/create/update/patch/delete`，内置对象禁止删除 | 禁止 | 同 Ops |
| Project 子资源：Job（不含 `/abort`） | 主资源 `get/list`，禁止创建、删除和 `/status` 写入 | 同 owner | 同 owner | 仅已分配 Job 的 `get` 和 `/status` 的 `patch` | 同 owner |
| Job `/abort` | 本工程 POST | 本工程 POST | 仅 owner/member 工程 POST | 禁止 | 仅 owner/member 工程 POST |
| Runner 范围 Job list/watch | 禁止 | 禁止 | 禁止 | 自身路径 `get/list/watch`，由 apiserver 按 `status.runner` 强制过滤 | 禁止 |
| Runner | 禁止 | 禁止 | `get/list/update/patch/delete`，禁止创建，包括 `/status` 读写；禁止 Watch | 自身 `create/get/update/patch`，普通对象受字段白名单约束，`/status` 由 apiserver 校验；禁止 `list/watch/delete` | 同 Ops |
| User 与用户密码 | 仅本人修改密码，禁止 User API | 仅本人修改密码，禁止 User API | 仅修改本人密码，禁止 User API | 禁止 | 所有 User 支持 `get/list`；仅非 Admin User 支持 `update/patch/delete`，另可修改本人密码 |
| MachineAccount | 禁止 | 禁止 | 禁止 | 禁止 | 通过专用接口 `create`；资源 API 支持 `get/list/delete`，两类账号允许更新 Token 有效期，service 账号还允许更新工程范围和权限 |

Ops token 仍只携带 `ebs:ops`，不与 `ebs:user` 组合；Gateway 在授权时将 Ops 视为具备普通用户的 Project owner/member 能力，并额外授予集群级 Config 写权限和 Runner 管理能力。Ops 不因此获得其他用户工程的 Project/Build/Job 写权限，也不获得其他 Admin 权限或 Runner 身份。

Gateway 对所有身份拒绝删除内置 `Config/build-target`、`Config/build-resource`、`Config/services`；其他 Config 按上表授权。

Build DELETE 仅允许所属 Project 的 owner 调用，按 Token subject 与 Project 的 `ebs.io/owner-user` label 比较，Ops/Admin 同样按 owner 关系判断，并转发删除前置条件。存在 `ebs.io/build-cleanup` finalizer 时，成功响应仅表示删除已受理，客户端可通过 GET 的 deletionTimestamp/finalizers 观察进度，404 表示 Build 已不存在；不把受理描述为清理完成。Gateway 不自行枚举删除派生资源，不开放 Build PUT/PATCH 或 OldJob DELETE；内部 Build Controller 的清理协议见 [Build 删除设计](controller-manager~build-controller.md#77-build-删除与派生-api-资源清理)。目标清理范围包含 Job/OldJob 的日志、产物及上传元数据，以及本 Build 的过程仓和发布仓；文件清理由各子资源 Controller 的 finalizer 流程完成。

Build 删除的状态资格统一由 [apiserver 删除入口](ebs-apiserver.md#build-删除-finalizer) 校验，Gateway 原样转发 `409 Conflict`；具备删除权限不代表当前状态允许删除。

矩阵中的权限还受 4.9 节完整对象比较和字段约束。User 只能通过 `/auth/register` 创建；Admin 可以读取但不能修改或删除 `spec.scopes=["ebs:admin"]` 的 User，也不能设置或重置其他用户的密码。Runner 对 Job `/status` 的更新不得改变 `status.runner`。

Ops 和 Admin 可通过现有 `PATCH /apis/ebs/v1/runners/{name}/status` 将 Runner 标记为 `Evicted`，无需独立驱逐接口。普通用户不允许该操作（403），匿名请求返回 401。Gateway 原样转发 apiserver 的响应，包括 404 和 resourceVersion 冲突的 409。

先 GET Runner 获取最新 `metadata.resourceVersion`，再发送以下请求；冲突后重新读取对象，不重放旧版本请求：

```bash
curl -X PATCH "${GATEWAY_URL}/apis/ebs/v1/runners/${RUNNER_NAME}/status" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H 'Content-Type: application/merge-patch+json' \
  --data '{"metadata":{"resourceVersion":"<最新版本>"},"status":{"phase":"Evicted"}}'
```

前端对 Ops 和 Admin 在已驱逐 Runner 行显示“取消驱逐”。确认后重新 GET 校验同一 UID，并携带最新 resourceVersion PATCH phase 为 `Offline`，等待新心跳恢复 Online；不修改 spec.unschedulable。若读取时已解除驱逐则无需再次写入，409 不自动重放。

Scheduler 不再向 Evicted Runner 分配新 Job，已有 Job 继续执行。Runner agent 保留驱逐状态；Ops 和 Admin 可通过同一路径显式更新为 `Offline`（等待新心跳恢复在线）或 `Online` 解除驱逐。

Runner 范围 Job list/watch 必须由 apiserver根据路径中的 Runner 名称强制过滤 `status.runner`；gateway 校验该路径名称等于 token 的 `sub`。客户端传入的 `fieldSelector` 一律拒绝，不能依赖 Runner 客户端自行隐藏对象。单对象 Job `get` 和 `/status` 写入仍必须读取对象并校验 `status.runner` 等于 token 的 `sub`，不匹配时返回 403。Job 对象仍不得保存密码、访问令牌、私钥或其他明文敏感信息；执行所需凭据必须通过独立的受控凭据交付机制提供。

Project 归属查询通过 `includeFields=metadata.name,metadata.labels` 只读取授权所需字段，不读取软件包配置。

IAM 认证读取同样使用字段投影：User 查询仅请求 `metadata.name,metadata.uid,spec.enabled,spec.scopes`；机器账号查询仅请求 `metadata.name,spec.type,spec.projects,spec.permissions`。User 删除保护仅查询 `spec.scopes`，无需读取完整用户资料。字段投影缩小响应数据，不改变身份、账号状态及授权校验；需要合并更新正文时仍读取完整对象。

Project 或 Runner Job 查询确认对象不存在时返回 404；连接失败、上游限流或其他非成功响应返回 503；响应解析失败或 Project 身份不匹配返回 502。
只有已确认的 owner/member 或 Runner 归属不匹配才返回 403，查询失败时不放行，也不误报无权限。

普通公开 `GET` 不查询 Project owner/member。Build 创建、中止和工程 Job Watch 对所有用户角色统一校验 Project owner/member，Ops/Admin 不例外；Service Token 的 Build 操作仍按工程范围及操作权限校验。需要区分 owner 与 member 的写操作继续按矩阵限制。

Runner 范围 Job list/watch 仅允许 Runner Token 对自身路径执行 GET，普通用户、Ops/Admin 和 Service Token 均拒绝；拒绝客户端 `fieldSelector`，仅透传 `resourceVersion`、`timeoutSeconds`、`allowWatchBookmarks` 等受支持参数；过滤条件由 apiserver 从可信路径生成。

### 4.8.1 Job 主动中止授权

Gateway 不开放 Job 创建和删除接口；用户通过创建 Build 发起任务，由 BuildInfo Controller 直连 apiserver 创建 Job。清理任务通过删除所属 Build 触发，由 Controller 管理 Job 生命周期。ebsctl 在本地拒绝 Job 创建和删除，不向 Gateway 发送这些请求。

`POST /apis/ebs/v1/projects/{project}/jobs/{name}/abort` 代理到 apiserver 同路径，接口及状态语义见 [Job 主动中止](ebs-apiserver.md#job-主动中止)。不开放全局 Job 列表，不以通用 `/status` 写入实现中止。

| 调用身份 | 权限 |
|---|---|
| 用户身份 | 仅 Project 的 owner 或 member 可中止 |
| 非 owner/member 用户 | 403 |
| Runner token、MachineAccount 凭据、匿名请求 | 不允许；未认证 401，已认证非用户身份 403 |

- 仅注册 `POST /abort`，并对该请求执行本节专用鉴权。
- 读取路径所指的服务端 Project，以可信 owner/member 标签判定；归属查询失败不放行。不得使用请求体、客户端标签或 Job payload 判断归属。
- 用户只能通过 `/abort` 中止 Job；普通用户、ops 和 admin 均不获得 Job `/status` 写权限。Gateway 不注册 Job 创建、删除、本体 PUT/PATCH 和 `/status` PUT 路由。Runner 继续通过 `PATCH /status` 更新已分配任务；内部组件直连 apiserver 的接口不变。
- 转发原始 apiserver 成功对象和 Status 错误，不把中止转成删除，不自动修改请求 UID。审计记录操作者、工程、Job、请求 UID、响应码与最终返回 phase；原因属于不可信文本，结构化记录并限制长度，不记录 token/payload。
- 测试覆盖各用户角色的 owner/member 放行与非成员拒绝、Runner/机器身份、匿名、错误方法、Project 查询失败及 404/409/422 透传。

### 4.8.2 前端 Job 中止入口

- 工程详情增加按工程查询的 Job 列表（分页，支持 Pending/Running 筛选），仅 owner/member 可见中止操作，admin/ops 也遵循此规则；按钮权限仅用于交互，gateway 负责最终授权。
- 运维页面供 admin/ops 按工程只读查询 Job；中止操作通过有 owner/member 权限的工程详情页发起，不新增全局 Job 明细接口。
- 确认弹窗显示工程、Job、当前状态，允许填写中止原因；提交带选中对象 UID，防重复点击。终态不显示中止按钮。
- 200 后以服务端返回 phase 刷新：Aborted 显示“已中止任务；执行端将在收到通知后停止运行”；Succeeded/Failed 显示“任务已结束，未改变原结果”。不得将 HTTP 200 一律显示为中止成功。
- 超时/断线时先 GET 确认，无法确认则提示结果未知并提供刷新；UID 变化不自动中止新对象。403 提示无权限，404 提示已删除，409 刷新对象，422 显示请求校验错误。不自动无限重试。
- 中英文文案、键盘可访问的确认弹窗及权限/并发完成/结果未知场景纳入测试。

### 4.9 AccessLabels

#### 4.9.1 PUT/PATCH 完整对象比较

Project 创建、PUT/PATCH 和 Job 状态 PATCH 分别在路由中显式挂载 `prepareProjectCreate`、`prepareProjectUpdate`、`prepareJobStatusUpdate`，保留身份、旧对象读取和字段校验。Build 创建、中止、删除、Job 中止以及 Runner 删除通过相应授权后直接调用 `upstream.Forward`，不经过通用写请求正文处理；保留原始方法、正文、Content-Type 和查询参数，原样返回上游错误。apiserver 负责 Build 创建和删除的正文限制、对象校验及并发保护，Job 中止独立限制正文为 16 KiB；Build 中止当前不读取正文。Runner 删除的 DeleteOptions、5 MiB 正文上限和状态资格由 apiserver 处理。

Config 请求在通过认证、Ops/Admin 权限、限流和 Watch 检查后直接代理，保留原始方法、正文和 Content-Type；DELETE 还须通过内置配置删除保护。Gateway 不预读 Config、不将 PATCH 转为 PUT，也不对 Config 请求执行正文大小检查；解码、PATCH 应用、元数据及内容校验、版本冲突检查、DeleteOptions 解析和正文大小限制均由 apiserver 负责，响应原样返回。

Script 同样直接代理，由 apiserver 执行正文大小限制、解码、PATCH 应用、版本冲突和资源校验。已认证的 User（含 Ops/Admin）和 Runner 身份可读取列表和单个脚本，Service Token 返回 403；写请求须具备 Ops/Admin 权限。Gateway 统一执行已认证限流并拒绝 Watch，列表查询参数原样透传。

MachineAccount 在通过 Admin 权限校验后直接代理列表、读取、删除及 PUT/PATCH 请求。Gateway 不预读账号，不改写更新正文；解码、PATCH 应用、字段校验和并发保护均由 apiserver 负责，具体约束见 4.3.1。

以下完整对象比较及 Gateway 请求格式、大小限制仅适用于 Project、Runner 普通对象、User 的 PUT/PATCH，以及 Job `/status` PATCH；不适用于上述直通代理请求或 Runner `/status`。gateway 不能只检查请求体中显式出现的字段，必须先构造本次请求将产生的完整候选对象，再对旧对象和候选对象执行授权比较；只有比较通过的候选对象可以写入 apiserver。

处理流程：

1. 根据规范化后的路由确定资源、对象名称和 subresource；路径中不存在对象名称的 collection 路由不接受 `PUT`、`PATCH`。gateway 先执行不依赖对象内容的 scope、verb、Project 和路径权限检查，调用方对该类请求必然无权时直接返回 403，不使用内部凭据探测对象。
2. gateway 使用内部读取凭据从相同资源路径读取当前对象，记为 `oldObject`。上游返回 404 时向客户端返回 404，读取失败时不执行写入。需要依据对象内容授权的 Runner Job 请求在读取后继续核对 `status.runner`。
3. `PUT` 只接受 `application/json`，请求体必须是单个完整 JSON 对象，以请求体作为 `candidateObject`。Runner 请求中的 `metadata.resourceVersion` 原样交给 apiserver 校验，gateway 不自行判断冲突。
4. `PATCH` 只接受 `application/merge-patch+json` 和 `application/json-patch+json`。gateway 将 patch 应用到 `oldObject` 的规范 JSON 表示，得到 `candidateObject`。JSON 语法错误、patch 操作失败或结果不是对象时返回 400。
5. PATCH 基于 `oldObject` 生成候选对象，未修改的 `metadata.resourceVersion` 自然保留。Project、User 和 Job 状态更新不得改变该字段；Runner 普通对象更新保留请求中的版本值，由 apiserver 执行乐观并发校验。
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

除 Runner 外，`metadata.resourceVersion` 只能保持为 `oldObject` 的值；Runner 的版本值由 gateway 原样转发并由 apiserver 校验。路径中的 namespace、name 必须与候选对象的 `metadata.namespace`、`metadata.name` 一致；Runner 的 `apiVersion`、`kind`、对象结构和字段类型由 apiserver 严格解码，gateway 只核对其身份名称。缺失、冲突或试图跨 Project 移动对象均返回 400。普通对象路径只允许修改 `metadata` 和 `spec` 中角色有权修改的字段；Gateway 不比较候选对象与旧对象的 `status`，允许省略该字段。对于具有 `status` 的资源，apiserver 在普通 PUT/PATCH 时始终保留旧状态，即使请求携带不同的 `status` 也不生效。Job `/status` 路径只允许修改授权的 `status` 字段，`metadata` 和 `spec` 必须与旧对象完全相同。Runner `/status` 不执行 Gateway 的候选对象比较，直接由 apiserver 的状态更新策略保护 spec 和 metadata。

字段比较后的角色规则：

| 调用方和路径 | 允许发生变化的字段 | 额外约束 |
|--------------|--------------------|----------|
| Owner 更新 Project | `metadata.labels`、`metadata.annotations`、`spec` | `ebs.io/owner-user` 必须保持不变；member labels 按本节后续规则校验 |
| Member 更新 Project | 无 | 4.8 节禁止 member 修改 Project，请求返回 403 |
| Runner 更新自身普通对象 | `metadata.labels["ebs.io/runner-type"]`、`metadata.labels["ebs.io/runner-arch"]`、`metadata.labels["ebs.io/runner-capability.*"]`、`spec.type`、`spec.arch` | 路径名称必须等于 token 的 `sub`；`spec.unschedulable`、`spec.taints`、其他 labels、annotations、status 和服务端 metadata 保持不变 |
| Runner 更新自身 `/status` | Gateway 原样转发 PUT/PATCH，由 apiserver 校验 Runner 状态 | 路径名称必须等于 token 的 `sub`；apiserver 保留旧 spec 和受保护 metadata，保留请求的 resourceVersion 用于版本冲突校验 |
| Runner 更新已分配 Job `/status` | `status.phase`、`status.stage`、`status.result`、`status.startTime`、`status.endTime`、`status.message` | 旧对象和候选对象的 `status.runner` 均必须等于 token 的 `sub`；Runner 不得修改 `status.runner`；脚本结果及 missingDeps 的使用范围由 apiserver 校验 |
| Admin 更新非管理员 User | `metadata.labels`、`metadata.annotations`、`spec.enabled`、`spec.scopes`、`spec.displayName`、`spec.email` | 旧对象和候选对象的 `spec.scopes` 均不得为 `["ebs:admin"]`；候选 scopes 仍由 apiserver 校验为单一合法 User scope |

表中的允许字段是上限；状态值、不可变 spec 字段和资源自身校验规则仍由 apiserver 执行。Runner 的 `instanceID` 不可变、type/arch labels 一致性和 `resourceVersion` 冲突也只由 apiserver 判定，gateway 不重复实现。没有列入允许集合的任何差异都返回 403。gateway 应在审计日志中记录请求方法、原始 patch 类型、旧/新 `resourceVersion`、被拒绝的 JSON Pointer 路径和拒绝原因，但不得记录密码、完整对象或完整 patch。

#### 4.9.2 Project access labels

普通用户、Ops 和 Admin 创建 Project 时，gateway 对 `POST` JSON 请求注入或覆盖：

```text
metadata.labels["ebs.io/owner-user"] = jwt.sub
```

gateway 对 Project 的 `PUT`、`PATCH` 请求需要保护以下 labels：

```text
metadata.labels["ebs.io/owner-user"]
metadata.labels["ebs.io/member-user.<username>"]
```

访问规则：

- 普通 member user 不能修改 Project access labels。
- owner user 可以增删 `ebs.io/member-user.<username>` labels；新增成员前必须确认对应 User 存在且已启用。
- owner user 不能把 `ebs.io/owner-user` 改成其他用户。
- Admin 可以创建 Project，但不能通过 Gateway 修改已有 Project 的 owner/member labels。

Project access labels 的保护逻辑只应用于 Project 对象。Snapshot、Build、BuildInfo、BuildGraph、RpmRepo、Job 通过所属 Project 继承权限。

### 4.10 ReverseProxy

gateway 使用反向代理将请求转发到 `ebs-apiserver`。

代理要求：

- 除 4.9.1 节规定的 PUT/PATCH 规范化外，保持原始 HTTP 方法。
- 保持查询参数。
- 除 access label 注入和 4.9.1 节生成完整候选对象外，保持请求体。
- 支持 watch 长连接，不缓冲完整响应。
- Watch 按路由允许列表放行，仅支持 Project 范围 Job 列表及具名 Runner 的 Job 列表；其他路由由 Gateway 拒绝，不模拟轮询。
- 透传 apiserver 返回的状态码和响应体。
- 使用 `--apiserver-addr` 设置上游地址。

### 4.11 Runner 服务地址投影

固定路径为 `GET /apis/ebs/v1/service-endpoints`。仅 `type=runner` 的有效 Token 可调用，所有 Runner 获取同一份地址列表，无需在路径或参数中指定 Runner 名。Gateway 每次读取 `Config/services`，返回 `artifactManagers` 和 `gitServer` 两个同级字段，仅包含 Runner 内外网地址；接口不接收 Job 或 Project 参数，也不查询 Job、Project 或选择上传实例。Runner 无需获得 原始 Config 的直接读取权限。

Watch 请求由全局 `PrepareQuery` 在认证前返回 403；Runner 请求通过认证和限流后，携带其他查询参数（包括 `watch=false`）时，Handler 返回 400。

响应示例：

```json
{
  "artifactManagers": {
    "default": {"runnerInternalURL": "http://127.0.0.1:8081", "runnerExternalURL": ""},
    "art-01": {"runnerInternalURL": "http://artifact-manager.art-01:8081", "runnerExternalURL": "https://artifacts.example.com"}
  },
  "gitServer": {"runnerInternalURL": "http://git-server:8080", "runnerExternalURL": "https://git.example.com"}
}
```

Gateway 仅投影 `runnerInternalURL` 和 `runnerExternalURL`，不返回任何服务的 `internalURL`、原始 `spec.content` 或其他 Config 字段；响应设置 `Cache-Control: no-store`。Config 的完整内容校验由 apiserver 在创建、更新时完成；Gateway 不重复校验实例名和 URL，仅处理读取、响应契约与反序列化错误，这些错误返回服务不可用，不使用旧缓存地址。地址不是凭据，但此接口仍须校验 Runner 身份。上传实例的选择不属于此接口；原 Config 仅运维可读写，不得通过普通 Config GET 向 Runner 放行。Runner 启动时读取此接口，之后约每 600 秒刷新地址表，使用 Job annotation 选择 Artifact Manager 实例；Artifact Manager 按 Runner 网络模式选址；源码在 internal 模式使用 `gitServer.runnerInternalURL`，external 模式直接使用 Job payload 的 `origin_url`，不依赖 Git Server 外网地址。

## 五、路由设计

`internal/auth` 独立负责密码注册、登录、机器凭据换 Token、Token 检查、密码修改和机器账号注册，持有对应的认证专用限流器；`internal/oauth` 负责第三方登录。启动处分别创建两类 Handler，并传入 Gateway Router。`internal/gateway` 的业务 Handler 不再持有认证接口的 IAM 客户端、Token 签发器和专用限流器；Bearer 认证仍由 `internal/middleware` 负责。密码登录开关由启动参数中 OAuth 配置文件路径是否为空决定，不依赖 OAuth Handler。

Gateway 的业务请求链为“路径路由 → 审计 → Recovery → Watch 路由检查 → 请求认证 → 身份/匿名限流 → Authorize（身份准入、Service 授权、声明的对象归属）→ 资源专用校验 → 最终 Handler”。路由显式匹配对外资源、对象和受支持子资源的完整路径，并绑定该资源的授权策略；`Authenticate` 统一完成 Token 校验和当前账号身份解析，路由及业务 Guard 不重复认证或通用限流。Handler 执行具体认证/IAM 操作、公开读取、Job 中止或反向代理。业务链保留审计在外、Recovery 在内的顺序，后续处理 panic 被恢复后，审计仍记录最终响应状态。未注册的资源与子资源不得透传 apiserver；增加 apiserver 资源时，必须同步登记 Gateway 对外路由和授权策略。

健康检查在业务 `router.Use` 之前直接注册 Handler，返回固定文本，不挂载 `gin.Recovery()`，不产生审计日志，不进入 Watch 检查、认证或限流。Gin 在注册路由时确定中间件链，因此无需在认证白名单和限流器中另行豁免 `/healthz`。

`Authenticate` 按 HTTP 方法和 Gin `c.FullPath()` 精确分类，未列出的路由默认必须认证，不按 URL 前缀放行：

非 Watch 请求中，`PrepareQuery` 仅对显式配置 `ListLimit` 的 GET 列表处理分页。当前 Project、Build、Job、OldJob、Snapshot、BuildInfo、BuildGraph、RpmRepo 列表上限为 100；缺失、非法、重复或超出上限的 `limit` 设置为上限，合法值及其他查询参数保留。Runner、IAM、Config、Script 列表及对象读取不受该分页规则影响。Watch 保留原始查询参数，不套用普通列表上限。

身份区分匿名、普通用户、Ops、Admin、Runner、Service；User 角色只在身份类型为 User 时生效。`Authenticate`、`Authorize` 和 `PrepareQuery` 共用由路由注册生成的 `RoutePolicies`，不各自维护路径分支。构造时复制规则，后续修改源 map 不影响已有 Router；健康检查在业务链外注册，不加入规则表。

| 认证模式 | 路由范围 | 行为 |
|----------|----------|------|
| 无需 Bearer 认证 | `GET /openapi.json`、`GET /auth/providers`、`GET /auth/oauth/:provider/login`；`POST /auth/register`、`POST /auth/login`、`POST /auth/machine-token`、`POST /auth/oauth/:provider/callback`、`POST /auth/oauth/register` | 不解析 Bearer Token；登录、注册、机器换 Token 和 OAuth Handler 仍校验自身凭据或流程上下文 |
| 可选认证 | Project 的 GET 集合和对象；Project 范围 Build、Job、Snapshot、BuildInfo、BuildGraph、RpmRepo 的 GET 集合、对象和 `/status`；OldJob 的 GET 集合和对象 | 无 Authorization 时匿名；携带 Token 必须验证，失败不得降级匿名；允许的 Watch 请求升级为必须认证 |
| 必须认证 | 其余路由，包括 Config、Script、Runner、IAM、配置投影、业务写操作、`/auth/check` 和密码修改 | 校验 Token 并解析身份后才进入限流和授权 |

未匹配路由也执行必须认证规则：未触发限流时，匿名返回 401，认证成功后由 `NoRoute` 返回 404。已注册路由没有配置准入规则，或调用方身份不在允许集合时，`Authorize` 返回 403，Service 拒绝记录审计结果；不能因 GET 或管理员身份而绕过规则。无需 Bearer 认证的接口按匿名身份准入，不受过期 Bearer Token 阻断，机器换 Token 的 Basic 凭据仍由 Handler 处理。

全局 `Authorize` 将调用方、Project、Runner、路径及 Watch 信息交给 `policy.Authorizer.Authorize`，只负责 HTTP 错误响应、Service 审计和请求内旧对象的传递。Policy 不依赖 Gin：先校验允许身份；Service 使用认证阶段读取的最新工程范围和 `ServicePermission`，不重复查询账号；其他身份按 `ObjectAccess` 或 `WatchAccess` 检查对象归属。用户工程归属检查不赋予 Ops/Admin 跨工程特权。

路由在 `internal/gateway/router.go` 按认证、OAuth、透传及其他接口分组注册。每条定义直接声明 `Authentication`、`Authorization policy.Rule`、查询选项、额外中间件及 Handler；注册时组装 `RouteAccess` 和 `RoutePolicy`，不维护第二份路径规则表。显式 `AuthenticationNone` 且授权规则完全为空时自动补齐匿名准入；其他路由必须声明允许身份。注册时拒绝重复路由、空 Handler、空中间件及规则冲突，并检查对象归属所需路径参数；允许 Service 的业务路由必须声明有效的 `ServicePermission` 和工程路径参数。

完成全局检查后，透传路由统一使用 `upstream.Forward`，仅在需要处理正文或附加约束时挂载以下中间件：

| 请求 | 路由内处理 | 与全局授权、apiserver 的边界 |
|------|------------|-----------------------------|
| Project 创建 | `prepareProjectCreate` | 注入 owner，校验创建字段 |
| Project PUT/PATCH | `prepareProjectUpdate` | 用户授权通过 `ProjectOwnerUpdate` 读取完整对象，更新校验复用；Service 更新在授权后读取对象 |
| Runner 创建 | `prepareRunnerCreate` | 仅 Runner 自行注册，检查身份与注册字段 |
| Runner 普通 PUT/PATCH | `prepareRunnerUpdate` | 路径归属由 `RunnerSelf` 校验，正文仍需字段权限检查 |
| Runner 任务列表及 Watch | `prepareRunnerJobs` | 路径归属由全局授权检查；此处校验查询参数并持有 Watch 名额至转发结束 |
| Job `/status` PATCH | `prepareJobStatusUpdate` | `AssignedJob` 已读取完整 Job 并确认分配关系，此处复用对象、合并 PATCH、校验字段后以 PUT 转发 |
| User PUT/PATCH | `prepareUserUpdate` | 仅 Admin；读取旧对象并校验，禁止修改管理员或将普通用户提升为管理员 |
| User DELETE | `protectAdminUser` | 仅投影读取 `spec.scopes`，拒绝删除管理员；User GET/list 直接转发且允许读取管理员 |
| Config DELETE | `rejectBuiltInConfigDeletion` | Ops/Admin 准入后拒绝删除内置配置，其他校验由 apiserver 处理 |

Build 创建、中止、删除、Job 中止、Runner 删除，以及 Config、Script、MachineAccount 的普通读写，在全局授权及适用的上述检查后直接代理，不预读或重写正文。Runner `/status` PUT/PATCH 同样直接代理，由 apiserver 保留旧 spec 和受保护 metadata、校验状态及版本冲突。完整对象更新、正文限制和错误返回边界见 4.9.1。

普通公开 GET 不读取 Project 成员关系，直接代理完整响应；工程 Job Watch 必须为 owner/member。具名 Job GET 对匿名和用户直接转发，Runner 通过 `AssignedJob` 仅读取名称、命名空间及 `status.runner` 校验分配关系，再代理原始 GET；保留调用方字段投影和其他查询参数。Project、OldJob 不开放 `/status`，其他资源的实际开放路径以 5.1 为准。

`/auth/check` 在全局业务鉴权前注册，共享审计、恢复、查询检查、认证及限流，再执行 `AdmitIdentity(AllowAuthenticated)`；Service 无需业务 permission 即可检查 Token，但不能因此获得业务资源访问权限。配置投影 `build-targets` 仅允许 User，`service-endpoints` 仅允许 Runner，均由专用 Handler 返回而非直接代理。

`iamclient` 读取用户时直接返回公共 `iamv1.User`，使用 4.8 节所述字段投影。调用方将缺失或为 false 的 `spec.enabled` 视为未启用。OAuth 认证和注册统一返回 `OAuthResult`，保留 `RegistrationRequired` 区分首次注册；机器认证返回仅含名称、类型和令牌有效期的 `MachineAuthResult`。OAuth Handler 直接使用该客户端，测试通过 HTTP transport 验证 IAM 请求契约。

### 5.1 路由与权限速览

Service Token 的资源权限仅按 4.3.1，不继承下表中的公开读取或用户权限。

下表中的“读取”指 GET（支持时包含 list）；`[/{name}]` 表示可选的对象路径，`/status` 仅适用于具名对象。写入仍受第 4.8、4.9 节的对象与字段规则约束。`{资源}` 只代表表中列出的资源，不是通配路由。

| 路由 | 读取权限 | 写入或专用操作权限 |
|------|----------|--------------------|
| `/healthz` | 匿名健康检查 | 无 |
| `/openapi.json` | 匿名及所有身份可 GET 同一份静态接口文档，见 5.3 | 无 |
| `/auth/register`、`/auth/login` | 无 | 匿名注册普通用户、密码登录 |
| `/auth/machine-token`、`/auth/check` | 无 | 前者凭 MachineAccount Basic 凭据换取 Runner/Service Token；后者凭 Bearer Token 校验身份 |
| `/auth/users/{name}/password`、`/auth/machineaccounts` | 无 | 登录用户仅修改本人密码；仅 Admin 可创建 MachineAccount |
| `/apis/ebs/v1/projects[/{name}]` | 匿名及已认证用户可读取 | 登录用户可创建；仅 owner 用户可修改，Service 按工程范围及 `projects:update` 权限更新；不开放 Project 删除和 `/status`；member 不可写 Project |
| `/apis/ebs/v1/projects/{project}/{资源}[/{name}][/status]`；资源为 `snapshots`、`buildinfos`、`buildgraphs`、`rpmrepos` | 匿名及已认证用户仅可 GET | 无；只注册 GET |
| `/apis/ebs/v1/projects/{project}/builds[/{name}][/status]`、`/apis/ebs/v1/projects/{project}/builds/{name}/abort` | 匿名及已认证用户可读取；watch 不开放 | 按上表权限创建或中止；仅 Project owner 可删除；Gateway 不允许任何身份 PUT/PATCH Build 及其 `/status` |
| `/apis/ebs/v1/projects/{project}/jobs[/{name}][/status]` | 匿名及已认证用户可读取；Runner 仅可读取已分配的具名 Job；watch 需认证并授权 | 不开放 Job 创建、删除和本体更新；Runner 仅可 PATCH 已分配 Job 的 `/status` |
| `/apis/ebs/v1/projects/{project}/jobs/{name}/abort` | 无 | 仅本 Project 的 owner/member 用户可 POST |
| `/apis/ebs/v1/projects/{project}/oldjobs[/{name}]` | 匿名及已认证用户仅可 GET；Runner 不开放 | 无；只注册 GET |
| `/apis/ebs/v1/runners[/{name}][/status]` | Ops/Admin 可读；Runner 仅可读自身普通对象 | 仅 Runner Token 可创建自身对象；Ops/Admin 可管理已有 Runner，但不得创建；Runner 可受限修改自身对象和 `/status`，不得删除 |
| `/apis/ebs/v1/runners/{name}/jobs` | 仅 Runner Token 可读取或 Watch 自身任务；Ops/Admin 不允许 | 无 |
| `/apis/ebs/v1/build-targets` | 已认证用户可读取发行版及架构列表；机器身份不可读 | 无 |
| `/apis/ebs/v1/configs[/{name}]` | 仅 Ops/Admin 可读 | 仅 Ops/Admin 可写；内置 `build-target`、`build-resource`、`services` 不可删除 |
| `/apis/ebs/v1/service-endpoints` | 仅 Runner Token 可读取各实例的 Runner 地址投影 | 无 |
| `/apis/ebs/v1/scripts[/{name}]` | 已认证 User（含 Ops/Admin）和 Runner 可读取、列表；Service 禁止 | 仅 Ops/Admin 可创建、修改、删除 |
| `/apis/iam.ebs/v1/users[/{name}]`、`/machineaccounts[/{name}]` | 仅 Admin；User 包含管理员对象 | 仅 Admin 可修改/删除非管理员 User、删除 MachineAccount、更新机器账号 Token 有效期及 service 账号的工程范围和权限；创建分别使用 `/auth/register`、`/auth/machineaccounts` |

未列出的资源、子资源和 `/api/*` 别名不对外开放；匿名不能 watch 或写入。Project 范围资源的原生全局路径也不开放。完整的角色差异、Runner 字段限制和 Job 中止特例以第 4.8 节为准。

历史 Job 的 [oldjobs 路由](ebs-apiserver.md#历史-job-归档ebs-oldjobs) 加入公开读取白名单，沿用普通 Job 读取的 Token 校验、限流、分页及字段选择透传规则。不注册 `/status`、Watch 或写方法；全局 oldjobs 和 Job `/archive` 仅用于内部访问，不经过 Gateway 对外开放。既有 jobs 路由不自动回退到归档数据。

### 5.2 Watch 透传

Runner 和已授权用户可能通过 gateway 对支持 watch 的资源建立连接：

```text
GET /apis/ebs/v1/runners/{runner}/jobs?watch=true
GET /apis/ebs/v1/projects/{project}/jobs?watch=true
```

全局 `PrepareQuery` 按 HTTP 方法和 `c.FullPath()` 精确匹配上述两类 GET 路由模板，不按原始 URL 前缀放行。非 Watch 请求仅对配置 `ListLimit` 的 GET 列表规范化分页；其他已匹配路由的 Watch 请求在认证、限流前返回 403，无论是否携带有效 Token。未匹配路由交给既有 `NoRoute`，匿名返回 401、认证成功后返回 404。允许列表只确认接口支持 Watch，不授予访问权限：两类列表仍执行认证、资源授权、查询参数和适用的并发检查，匿名请求返回 401。Runner token 通过自身 Runner 范围接口 Watch Job；用户通过工程 Job 列表建立 Watch 时必须为该工程的 owner/member，Ops/Admin 不享有跨工程访问特权。

每个 Gateway 实例内，同一 Runner 同时最多建立一个 Runner 范围 Job watch。超过限制返回429并包含 `Retry-After`；连接正常结束、上游失败或客户端断开时必须释放计数。Runner watch建议使用不超过300秒的 `timeoutSeconds` 定期重连，使 token和权限能够重新校验。

gateway 需要支持流式响应：

- 不读取完整 upstream response 后再返回。
- 上游 Transport 设置 30 秒响应头超时，从请求正文发送完成后开始计时；包括 Watch 在内的透传请求若迟迟未收到响应头，返回 502，响应仅包含 `upstream unavailable`，不暴露连接细节。
- 响应头超时不限制后续正文读取；已经建立的 Watch 流不设总时长限制，保持流式转发，并随客户端取消而关闭上游请求。
- Gateway 内部 `Do` 查询和操作另有 30 秒总超时，包含响应正文读取，并遵守调用方更早的取消或截止时间；该总超时不施加到反向代理。
- 客户端断开时及时关闭 upstream 请求。
- 审计日志记录连接建立和最终关闭状态。

### 5.3 对外 OpenAPI 文档

Gateway 提供对外 API 的 OpenAPI 3.0 JSON 文档，前端展示方式见 [前端设计](frontend.md#510-api-文档)。本节定义文档发布契约，不改变现有业务接口的权限或开放范围。首版只展示文档，不提供在线执行。

#### 文档范围与来源

- 以 Gateway 实际开放的路径、HTTP 方法及授权策略为准，覆盖认证、IAM、EBS 资源、受支持的子资源和 Runner 服务地址投影；按业务域使用 tags 分组。Gin 的 `Any` 注册不表示所有方法都对外可用，生成及核对时须结合 Handler 和权限策略。
- 复用 `api/ebs/v1`、`api/iam/v1` 公共资源的 Schema 生成机制；由 Gateway 补充路径、操作、请求/响应、权限说明和示例。接口使用经过 Gateway 转换的响应时，定义其实际响应 Schema，不能直接套用内部类型。
- 不直接透传 apiserver 的完整 `/openapi/v3` 文档：排除 `/internal/*`、Project 子资源的内部全局列表以及 Gateway 未开放的方法。Artifact Manager 的上传、日志、仓库内容接口不属于 Gateway 路由，首版不纳入本文档。
- 每个操作具有唯一且稳定的 `operationId`，明确路径参数、适用的分页/过滤/字段选择参数、请求体、响应类型及错误码。`watch`、`includeFields`、`excludeFields` 等参数仅出现在实际支持的操作中，流式响应须说明格式。
- 请求与响应分别标明字段的必填、只读及更新约束；记录 UID/resourceVersion 前置条件、冲突和写入结果未知时的注意事项。错误响应按接口区分 Gateway JSON 错误与 apiserver Kubernetes Status，不统一伪装为同一种格式。

#### 身份与权限表达

OpenAPI 的 `securitySchemes` 定义 HTTP Bearer JWT 和 MachineAccount Basic 认证。各操作单独声明认证要求：匿名接口使用空 security，允许匿名或 JWT 的操作列出两种选择，机器账号 token 换取接口使用 Basic。交换正文分别描述 runner 与 service 的输入；注册请求包含 projects/permissions，Project PUT/PATCH、Build POST、Build POST /abort 分别标注所需权限与工程范围，不将其表达为 OAuth scope。Bearer 认证不代表任意主体都可调用。

Bearer scheme 只表达令牌格式，不能表达 owner/member、Ops/Admin 或 Runner 任务归属。各操作的 description 应明确允许的身份与对象关系，沿用 5.1 和第四章的规则。文档向所有查看者展示同一接口集合，接口可见不代表调用获授权；不读取当前用户或 Project 数据来动态裁剪文档。

#### 生成与发布

1. 在 Gateway 模块提供离线生成入口，将公共类型 Schema 与 Gateway 操作定义组装为单份 `openapi.json`。`$ref` 仅引用文档内的 `components`，`servers` 使用同源相对根地址 `/`，不包含 apiserver 地址、部署实例地址或真实凭据。
2. 生成产物随代码提交，并由 `internal/openapi` 包使用 `go:embed` 嵌入 Gateway 二进制。构建和运行不依赖已启动的 apiserver；不在浏览器请求文档时读取业务对象或调用上游生成 Schema。
3. 显式注册 `GET /openapi.json`，返回 `200`、`Content-Type: application/json`。这是独立的静态文档路由，经过全局审计与 panic 恢复，不执行用户解析或业务资源授权；请求不需要携带 Token，也不向上游转发。其他方法不开放。
4. 响应采用 `Cache-Control: no-cache`，允许客户端保存但每次使用前重新验证，避免升级后长期展示旧接口。首版只提供 JSON，不单独维护另一份 Swagger YAML 或 Gateway 内置 Swagger UI。
5. 前端 Nginx 和 Vite 对该精确路径代理到 Gateway，`/api-docs` 仍作为 SPA 页面。文档路径不得被 history fallback 返回为 HTML；代理要求见前端设计 7.1。

生成命令（仓库根目录执行）：

```bash
# 公共 API 类型变化时，先更新公共资源 Schema。
bash backend/ebs-apiserver/hacks/update-openapi.sh
bash backend/ebs-gateway/hacks/update-openapi.sh

# 检查生成产物是否与当前输入一致，不写文件。
bash backend/ebs-gateway/hacks/update-openapi.sh --check
```

生成入口在 `backend/ebs-gateway/hacks/openapi/`，借用 apiserver 模块的生成类型定义，仅在生成时依赖该模块；Gateway 运行时不依赖 apiserver 的 Schema 服务。新增路由或变更方法、权限、专用响应时，同步更新操作定义、生成产物和路由契约测试。

#### 一致性验收

- 校验 OpenAPI 版本、Schema 引用、operationId 唯一性、参数与响应定义；离线重新生成应无未提交差异。
- 对照实际路由、方法限制和授权测试验证操作集合，覆盖只读资源、Build PUT/PATCH 限制、支持的 Watch 路径以及专用认证接口；新增接口时同步更新操作定义。
- 验证匿名获取、JSON 响应和无上游依赖；检查文档不包含内部路由、真实凭据或集群内部地址。
- UI 的只读限制不是服务端安全控制；现有接口仍须独立通过权限测试。在线调试、自动带入会话 Token 和写操作确认另行设计，不属于首版。

## 六、配置

启动配置：

| 参数 | 默认值 | 必填 | 说明 |
|------|--------|------|------|
| `--port` | `8080` | 否 | gateway 监听端口 |
| `--apiserver-addr` | `https://ebs-apiserver:8443` | 是 | 上游 apiserver 地址 |
| `--jwt-secret-file` | 空 | 是 | 保存单个 base64 HMAC 密钥的文件路径，解码后至少 32 字节 |
| `--oauth-config-file` | 空 | 否 | OAuth 配置文件；指定后关闭本地密码登录和注册，详见 10.8 |
| `--max-request-body-bytes` | `1048576` | 否 | 登录和代理写请求的最大请求体，以及 PATCH 后候选对象的最大字节数 |
| `--insecure-skip-verify` | `false` | 否 | 是否跳过 apiserver TLS 校验，仅测试环境使用 |
| `--apiserver-ca` | 空 | 否 | apiserver CA 文件路径 |
| `--apiserver-client-cert` / `--apiserver-client-key` | 空 | 否 | apiserver 客户端证书与私钥，启用内部证书认证时成对提供 |
| `--rate-limit-per-sec` | `100` | 否 | 身份/IP 与匿名 IP key 的每秒令牌数，必须是有限正数 |
| `--rate-limit-burst` | `200` | 否 | 身份/IP 与匿名 IP key 的桶容量 |
| `--v` | `0` | 否 | klog verbosity |
| `--vmodule` | 空 | 否 | 按 Go 文件名模式配置 klog verbosity |
| `--logtostderr` | `true` | 否 | klog 日志输出到 stderr |

启动时逐项校验参数，错误包含参数名和具体约束；数值错误附上当前值，密钥内容不进入错误信息。日志统一接入 klog 标准参数，启动、正常退出及请求审计使用 `InfoS`，进程失败使用 `ErrorS`，正常及错误退出前均执行 `Flush`。verbosity 控制显式 `V(n)` 日志，不是最低严重级别过滤，默认保留请求审计，不记录 Token、请求正文或密钥。Gin 固定使用 release 模式。

成功绑定监听端口后才记录启动成功。收到 SIGINT/SIGTERM 后停止接收新连接，最多等待 15 秒让在途请求排空，等待结束前主流程不退出；超时则强制关闭连接并返回错误。Watch 等持续请求也受此退出期限约束。退出前释放信号订阅并关闭上游空闲连接。

配置 `--apiserver-client-cert` 和 `--apiserver-client-key` 后，Gateway 使用独立的 mTLS 客户端证书访问 apiserver；启用 `--internal-client-ca-file` 的 apiserver 根据证书 URI SAN 识别 Gateway，不依赖转发的用户身份头。IAM 的 OAuth 分支仅允许 Gateway 身份调用，其余接口尚未实施完整的按组件授权规则；部署时仍须限制 apiserver 的网络访问。

密钥文件由 Secret 以只读方式挂载，文件权限应限制为 `0600`，内容为单个 base64 字符串，例如：

```text
MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=
```

当前版本不支持新旧密钥并行验证。轮换密钥需要更新 Secret 并重启所有 gateway 实例，所有旧 token 随即失效，用户必须重新登录。多实例滚动更新期间，不同密钥的实例不能互相验证对方签发的 token，因此应在维护窗口内同步重启、临时缩容为单实例，或明确接受轮换窗口内的 401。密钥文件、解码后的密钥以及完整 token 不得写入日志；配置缺失时不得像旧系统一样动态生成随机密钥。

## 七、调用示例

### 7.1 健康检查

仅 `GET /healthz` 直接由 Handler 返回 `200` 和 `text/plain` 正文 `ok\n`，不经过中间件，不检查 Token 或查询参数；例如携带无效 Token 或 `watch=true` 仍返回普通健康响应，不建立 Watch。`POST /healthz`、`/healthz/` 等未注册方法和路径走业务链的认证、限流及 NoRoute 处理，不作为探针入口。

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
      "defaultRef": "refs/heads/master",
      "distro": "openEuler-22.03-LTS",
      "buildArches": ["aarch64"]
    }
  }'
```

### 7.5 Runner Watch Job

Runner 使用 MachineAccount 长期凭据换取 `type=runner`、`scopes=[]` 的短期 token，再通过自身范围 Job list-watch 发现分配给自己的 Job：

```bash
TOKEN_RESPONSE=$(curl --fail-with-body 'http://localhost:8080/auth/machine-token' \
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
| 未认证请求访问业务 API | 仅4.1节白名单资源及其单对象 `/status` 的已注册读取方法进入匿名读取流程；不支持 Watch 的已匹配路由返回403，支持 Watch 的列表、Runner、IAM、写请求和未知资源要求认证，匿名返回401 |
| 公开对象包含敏感信息 | 匿名接口返回完整对象；公开资源的数据结构和写入校验必须禁止保存密码、Token、私钥等秘密，敏感数据使用非公开资源或独立受控存储 |
| 匿名批量抓取 | 按 anonymous/IP key 独立计数限流，限制collection分页大小，禁止无界list和watch |
| 注册接口被滥用或批量占用用户名 | 按客户端 IP 和用户名/IP 组合独立限流；限制请求体和字段；多实例生产部署使用共享计数 |
| User凭据泄漏 | User REST响应不包含credential字段，密码和哈希不写入日志、审计事件或错误响应 |
| 登录接口泄漏密码 | 登录请求体不记录日志，gateway 与 apiserver 之间使用 TLS，错误响应不回显输入 |
| 密码暴力尝试 | gateway 按账号和客户端地址限流，IAM 模块维护失败次数和临时锁定状态 |
| MachineAccount凭据泄漏 | 删除账号并使用新secret创建账号；每个Runner或受控站点使用独立账号和随机secret，secret只从受限文件读取且不写日志 |
| MachineAccount提升权限 | 按已认证账号类型签发 Runner/Service Token；调用方不能在换 Token 时声明权限、主体或 TTL，service 逐请求核验账号、工程范围和显式操作权限 |
| 已删除或禁用的用户访问 | UserResolve 返回 403，不进入业务鉴权 |
| User API 不可用 | 返回 503，不默认放行 |
| Admin 越权修改/删除管理员 User | User GET/list 允许读取管理员；DELETE 先校验旧对象，PUT/PATCH 校验旧对象及候选对象，保护管理员角色 |
| 客户端伪造 Project owner | 普通用户、Ops 和 Admin 创建 Project 时统一将 `metadata.labels["ebs.io/owner-user"]` 设置为 JWT `sub` |
| 客户端越权修改 Project members | 仅 owner user 可以修改 member user labels，新增成员必须是已启用 User |
| 用户写入未授权 Project | gateway 查询 Project owner/member labels，不匹配则返回 403；公开 `GET` 不受该写权限限制 |
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
| Auth | 缺失 token、非法签名、非 HS256、非法 header、issuer/audience 错误、时间 claim 越界、缺失 `jti`、非法 scope 组合以及合法的 user/ops/runner/service/admin token；密钥文件缺失、非法或过短时启动失败 |
| PublicRead | 匿名与已认证调用方对 Project 的 get/list，以及 Project 级公开资源的 Project 范围 get/list 和单对象 `/status` 使用相同的完整对象透传与分页规则；Snapshot、BuildInfo、BuildGraph、RpmRepo 仅 GET；已认证请求仍先校验 Token/User；非法Token不降级匿名；Runner 对常规公开资源、IAM/write/watch/未知资源及非白名单 `/status` 拒绝；Config 不在公开资源列表内；`watch=true`、`watch=1`和重复参数均不能绕过 |
| ConfigRead | 非运维身份无权读取 Config；Ops/Admin 可具名读取及 list；公开构建目标投影不包含镜像或配置原文 |
| Registration | User和密码哈希单文档原子创建、注册成功但不签发token、User固定启用、未知/越权字段、用户名和email校验、密码长度、重复用户名、请求体过大、限流、IAM不可用、REST响应不包含credential以及敏感字段不入日志 |
| MachineAccountRegistration | 对象和凭据原子创建、重复名称409、名称/secret/TTL校验、runner/service 类型校验、未知注册字段拒绝、响应不回显secret、非Admin拒绝；工程名/三个权限枚举校验、重复条目拒绝、runner非空授权拒绝及service空授权 |
| PasswordLogin | User 根据唯一的 `spec.scopes` 获得 `ebs:user`、`ebs:ops` 或 `ebs:admin`、请求 scope 不产生提权、非法或多 scope 配置、密码错误、用户不存在、用户禁用、账号锁定、认证接口不可用和登录限流 |
| MachineTokenExchange | 同一路径签发 Runner/Service Token、类型来自 IAM、service 禁止 runner 字段、主体使用账号名称、请求越权字段拒绝、错误凭据401、协议异常503、TTL与限流、no-store及脱敏 |
| TokenCheck | 合法身份与 scopes 响应、service 实时账号核验、非法 Token 401、IAM 故障503、非空正文400及限流 |
| UserResolve | User 不存在、名称与 JWT `sub` 不匹配、禁用、Token scope 与当前唯一 User scope 不一致、逐请求查询和 User API 不可用；runner token 跳过 User 查询 |
| Header | 不向上游转发外部 Bearer Token，也不注入最终用户身份 |
| ProjectAuthz | 普通用户和 Ops 按 owner/member 关系操作 Project 与子资源；公开读取不按 owner/member 过滤；Runner 可以创建、读取和受限更新自身 Runner，只能 list/watch 自身已分配 Job，并对匹配的单个 Job执行 get和 status写入 |
| ServiceAuthz | 工程范围与 projects:update/builds:create/builds:abort 独立授权、空授权及跨工程拒绝、匿名读不继承、owner及元数据保护、门禁标签保护、JSON Patch版本冲突、Build中止复用原协议、账号删除及同名重建后按当前授权处理、IAM异常拒绝、不查询同名User |
| Admin | 普通 user、ops、runner 和 service 均不能管理 MachineAccount；Gateway 覆盖 Admin 准入、原始方法/正文/Content-Type/查询参数透传、无预读和上游错误透传；apiserver 覆盖有效期范围、字段保护、版本冲突及凭据不变 |
| AdminUser | Admin 可 get/list 所有 User，仅能 update/patch/delete 非管理员 User；禁止 create、把用户提升为 `ebs:admin`、修改/删除管理员 User 和重置他人密码 |
| Ops | 按 owner/member 关系执行普通用户的 Project 与子资源操作；额外允许集群级 Config 写操作及 Runner 管理（不含创建和 Runner 范围 Job list/watch，包含普通对象更新、`/status`，不开放 Runner 对象 Watch） |
| ObjectCompare | Merge Patch 和 JSON Patch 构造完整候选对象；拒绝不支持的 patch 类型、非法 JSON Pointer、重复 key、超大对象和跨 subresource 修改；Runner 的 `resourceVersion` 由 apiserver 校验，冲突返回 409且 gateway 不自动重放 |
| AccessLabels | 普通用户、Ops 和 Admin 创建 Project 时统一写入当前 Token 的 owner user label；PUT/PATCH 不能通过 `null`、删除父 map、`move` 或 `copy` 绕过 owner/member user label 保护 |
| RunnerObject | 仅 Runner Token 可创建；普通用户、Ops/Admin、Service 拒绝且不得转发；创建时身份三元组一致、拒绝 status 和非白名单字段；PUT/PATCH 只允许修改自身声明字段，保护由运维管理的 unschedulable、taints、labels 和服务端 metadata；禁止 list/watch/delete 和其他 Runner |
| RunnerStatus | Runner 仅可更新自身 Runner `/status`，Gateway 原样转发 PUT/PATCH，apiserver 保留 spec 和受保护 metadata；已分配 Job 的 status 仍按 Gateway 白名单校验，不能修改 Job runner、spec 或 metadata |
| RateLimit | 认证失败不扣减通用额度、认证后身份与匿名额度隔离、公开读取不重复扣减、429 和 Retry-After，以及登录/OAuth/Watch 专用限制 |
| RouteIdentityAdmission | 全部已注册业务路由有规则、方法与模板精确匹配、未配置默认拒绝、角色与机器身份隔离、Service 拒绝审计，以及准入后仍执行对象归属和字段权限检查 |
| MiddlewareChains | 健康检查独立链路、不消耗额度或产生审计、不访问 IAM；非探针路径仍受保护；业务 panic 恢复后仍记录一次含最终状态的审计 |
| Proxy | `/apis/ebs/v1/*` 原样透传 |
| Watch | 已授权用户的 Project 范围 Job Watch 和 Runner Token 自身的 Runner 范围 Job Watch 能够流式转发；校验强制过滤、fieldSelector 注入、BOOKMARK、410 重建、timeoutSeconds 和每 Runner 单连接限制；Runner 列表及其他非允许列表路由的 Watch 均拒绝 |
| Audit | 请求结束后记录 method/path/status/latency/user |

<a id="oauth-login"></a>

## 十、补充：OAuth/OIDC 社区登录

### 10.1 范围

支持单个认证源的 Authorization Code + PKCE S256 登录，以及首次登录补全注册。现有 JWT、密码登录和业务授权规则沿用本文前述章节；IAM 身份一致性以 [apiserver OAuth 契约](ebs-apiserver.md#oauth-iam-扩展)为准。

不提供已有账号关联、自动合并、外部角色同步、解绑、统一退出、第三方 Token 长期托管或 CLI Device Flow。社区基本注册和登录已验证，安全及异常场景按 10.9 验收。

### 10.2 组件职责与流程

```text
前端 → Gateway login：开始登录
Gateway → 浏览器：设置加密登录 Cookie，302 跳转授权页
第三方 → 前端 /auth/callback：浏览器携带 code/state 返回
前端 → Gateway callback：同源 POST code/state，自动携带 Cookie
Gateway → 第三方：核验上下文后交换授权码、验证身份
Gateway → apiserver IAM：users/authenticate（method=oauth）
  已关联：核验当前 User，直接返回 JWT
  未关联：设置加密注册 Cookie，返回 registrationRequired
前端 → /auth/register：用户补全本地用户名等资料
前端 → Gateway register：提交资料，自动携带注册 Cookie
Gateway → apiserver IAM：users/register，再 authenticate 并核验 User
Gateway → 前端：直接返回 JWT
```

第三方登记的 redirectURI 是前端页面，不是 Gateway POST callback API。授权码交换和身份认证仍在 Gateway 执行，浏览器自报的 issuer/subject 不作为身份来源。

### 10.3 对外 API

下列接口已在 Gateway 注册，不继承业务代理的匿名读取或 Runner 权限规则。

| 方法与路径 | 输入与行为 |
| --- | --- |
| `GET /auth/providers` | 返回本地认证可用性、provider ID 和 loginURL，不暴露凭据 |
| `GET /auth/oauth/{provider}/login` | 可选站内 returnTo，默认 /；设置登录 Cookie，302 跳转 |
| `POST /auth/oauth/{provider}/callback` | JSON code/state 或 error/state，校验登录 Cookie、Origin，返回 JWT 或待注册结果 |
| `POST /auth/oauth/register` | JSON registrationID、action、username，以及 create 时可选 displayName/email；确认账号后直接返回 JWT |

providers 响应：

```json
{
  "passwordLoginEnabled": false,
  "registrationEnabled": false,
  "providers": [
    {"id": "community", "loginURL": "/auth/oauth/community/login"}
  ]
}
```

OAuth 关闭时 providers 数组为空，OAuth 路由返回 404，本地密码登录和注册可用。启用 OAuth 后，本地 `/auth/login` 和 `/auth/register` 均返回 403；providers 的两个可用性字段统一为 false，只作为响应信息，不提供独立配置。首次 OAuth 登录未关联账号时统一进入补全注册流程。

#### 10.3.1 请求和响应细则

- callback 要求唯一且非空的 state，以及 code 或 error 二选一；前端保留并提交提供方返回的 iss（若有），Gateway 按提供方契约精确校验，不能用它选择请求端点。拒绝重复参数、重复或未知 JSON 字段及类型不符；不接受客户端指定 redirectURI、身份或 Token。
- 未关联账号返回 `{"registrationRequired":true,"registrationID":"<随机ID>","expiresAt":"<RFC3339>","profile":{"username":"","displayName":"","email":""}}` 并设置注册 Cookie，不返回 JWT。前端将 registrationID、expiresAt 和 profile 保存至 sessionStorage，用于进入注册页及刷新恢复；registrationID 仅用于匹配 Cookie，不是独立身份凭据。
- register 的 action 必须显式为 create 或 confirm。create 是用户主动提交资料；confirm 只认证查询，不调用 IAM register，也不接受 displayName/email。未知结果处理见 10.6.3。
- JSON 请求要求 application/json，正文上限 16 KiB。错误格式为 `{"code":"<stable-code>","error":"<safe-message>"}`，失败不携带 token 或 registrationRequired。
- 通用错误：400 InvalidRequest、401 OAuthAuthenticationFailed、403 BrowserBindingInvalid、404 ProviderUnavailable、409 OAuthConfigChanged、410 ContextExpired、429 RateLimited、503 OAuthUnavailable。取消授权校验上下文后返回 400 OAuthCancelled，不回显第三方原始错误。IAM 错误见 10.6.2。

#### 10.3.2 Token 交付

callback 和 register 的登录成功响应相同：

```json
{
  "token": "<EulerMaker JWT>",
  "tokenType": "Bearer",
  "expiresIn": 86400,
  "returnTo": "/operations"
}
```

签发前通过 IAM authenticate 取得 username/UID，再读取相同 UID 的 User，核验 enabled 和唯一合法 scope。register 已返回 UID 时须与认证结果一致；不通过公开 User API 读取私有 externalIdentity。

returnTo 只来自加密上下文。前端成功后才写 sessionStorage，沿用 auth/check、用户资料及路由恢复；失败不覆盖原登录 Token。JWT 只出现在同源 POST 成功 JSON，不进 URL、不缓存供重取。响应丢失不回滚已创建 User，恢复规则见 10.6.3。

认证响应统一 Cache-Control: no-store；前端回调页使用 Referrer-Policy: no-referrer，不加载第三方脚本。code/state 只在第三方回调 URL 中短暂出现，读取到内存后立即 replaceState 清除，不写持久存储或错误监控。

### 10.4 身份验证与安全边界

#### 10.4.1 Cookie 上下文与跨副本

每轮 login 独立生成至少 32 字节安全随机数据的 state、PKCE verifier 和 OIDC nonce。state 格式为 `<providerID>.<random>`，provider ID 仅帮助前端选择固定 callback 路由；可信配置由 Gateway 决定。登录 Cookie 保存 state 摘要、provider、configFingerprint、verifier、nonce 摘要、returnTo、issuedAt、expiresAt。

两种 Cookie 为 `__Host-ebs-oauth-login` 和 `__Host-ebs-oauth-register`，均设置 Secure、HttpOnly、SameSite=Lax、Path=/，不设置 Domain；用途分别为 login、register，不能互换或用于业务鉴权。

首版每个浏览器仅保留一轮流程，新 login 覆盖登录 Cookie 并清除注册 Cookie。多标签并行登录可能使旧流程失效，不承诺并行隔离。两种 Cookie 均使用登录开始后 10 分钟的固定 expiresAt，注册不重新起算，回调、填表、查询及重试不续期。

callback、register 为同源 JSON POST，必须携带精确匹配 publicBaseURL origin 的 Origin；前端显式使用 credentials: same-origin，不沿用普通 API 的 credentials: omit。不开放跨域 Cookie API。缺少 Cookie、解密失败、purpose/provider/configFingerprint/期限不符或 state/registrationID 不符时拒绝，不调用上游；state/registrationID 摘要采用恒定时间比较。

通过绑定校验后的 callback 无论成功失败都在响应中清除登录 Cookie；账号注册或登录成功时清除注册 Cookie。未经绑定校验的请求不清理其他流程。删除 Cookie 仅是浏览器清理，不等于撤销已复制的凭据或阻止在途请求。

短期上下文仅保存在加密 Cookie。使用 AES-256-GCM，封装 keyID、随机 nonce 和密文；每次加密生成新 nonce，AAD 绑定协议版本、Cookie 名称、purpose 和 publicBaseURL。密钥与 JWT 签名密钥、client secret 分离；副本共享密钥环及 provider 配置，不依赖实例会话 Map、粘性会话或共享存储。

每个完整 Set-Cookie 字段最多 3800 字节，实际序列化、加密及 Base64 后检查；优先省略可选 profile，不截断 issuer/subject。必要字段仍超限时返回 400 OAuthContextTooLarge，不静默降级或增加存储。IAM 最大身份字段长度不代表所有组合都能放入 Cookie，提供方联调须验证真实身份长度；代理须容纳两份 Cookie 和其他既有请求头。

多副本同步 UTC 时钟，每个入口、耗时调用结束及签发 JWT 前检查 expiresAt。浏览器未删除 Cookie 也不能过期使用，不需要服务端记录清理任务。

#### 10.4.2 授权码与 OIDC

授权请求固定 response_type=code、PKCE S256、精确登记的 redirect_uri 和最小 scopes；OIDC 至少包含 openid。首版只支持顶层 GET 导航的 query 回调，不接受 form_post。Gateway 核对 Cookie/state 后，使用原 verifier 及配置的客户端认证方式交换 code，不自动重试兑换。

OIDC 验证签名、算法白名单、精确 issuer、audience、必要的 azp、期限及 nonce；60 秒时钟容差仅适用于身份 Token，不延长 Cookie。JWKS 来自可信配置或验证后的 discovery，未知 kid 刷新须合并和限频，不按 Token 自带 URL 访问网络；UserInfo sub 须与 ID Token 相同。[OIDC Core](https://openid.net/specs/openid-connect-core-1_0.html)

普通 OAuth 首版提供 `userinfo-sub` adapter，通过 Bearer access token 调固定用户接口，要求 JSON 的 sub 是不可变且不复用的用户 ID，可选 name、email 和布尔 email_verified；仅已验证邮箱用于预填。该配置必须由运维核对提供方契约，不表示仅返回 username 的社区服务已兼容。缺少稳定身份、授权码/客户端绑定或 PKCE 时不启用，不回退到用户名/邮箱匹配。授权服务器须拒绝重复兑换授权码，Gateway 不另存跨副本消费记录。OIDC discovery 必须声明支持 S256；固定端点模式的 PKCE 能力须在提供方联调中确认。

交换结果未知、身份校验失败或实例退出时重新登录，不自动重发 callback。第三方 Token 只在请求内使用，不进入 Cookie、日志、IAM 或持久化存储。

回调必须 HTTPS 精确路径。returnTo 只接受已知站内相对业务路由，拒绝 scheme、host、双斜线、反斜线、控制字符及编码绕过；不依据不可信 Host/X-Forwarded-* 拼接地址。上述绑定及校验用于防止回调注入和登录 CSRF。[OAuth 安全最佳实践](https://www.rfc-editor.org/rfc/rfc9700.html)

### 10.5 外部身份与本地账号

身份使用精确的 issuer/subject；OIDC subject 为 sub，普通 OAuth adapter 使用固定认证源命名空间及稳定用户 ID。provider ID 不替代 issuer，不按用户名或邮箱自动匹配已有账号。字段约束见 [apiserver 外部身份契约](ebs-apiserver.md#外部身份校验与身份键)。

User 私有身份存储、跨副本唯一性及占位恢复由 [apiserver IAM](ebs-apiserver.md#oauth-iam-扩展)负责。Gateway 只提交已验证身份，不操作 ES 或释放占位；Cookie 过期、取消或查询未确认均不代表可以撤销已发出的开户请求。

#### 10.5.1 首次登录补全注册

只有 IAM 明确返回 registrationRequired 才创建注册凭据并展示表单。加密注册 Cookie 包含 purpose、provider、configFingerprint、随机 registrationID、已验证 issuer/subject、原 returnTo、固定期限及有限预填资料；不含 code、verifier、第三方 Token 或业务 scopes。

用户填写必填 username，以及可选 displayName/email，遵循 IAM 资料规则。username 默认使用社区返回的
`openeuler_username`，字段缺失或为空时留空，由用户手动填写。该值只用于预填，允许用户修改，
不做大小写转换或截断；不符合本地格式或名称已占用时由用户修正，不能据此关联已有账号。
可用昵称及已验证邮箱预填，编辑后的邮箱只是未验证联系资料，不作为身份。表单不要求密码、不允许选择权限。

提交前不创建账号或占位。Gateway 核验 Cookie、registrationID、Origin 和期限后构造 method=oauth 请求，外部身份只取自 Cookie；IAM 成功后再次认证、读取 User 并签发 JWT。

取消或过期不撤销已发出的开户请求。账号成功但响应丢失时不回滚，下次登录复用账号；重试和后续登录不覆盖用户资料。不构造虚假邮箱，无密码账号不接受空密码登录；新增密码、恢复密码不在首版范围。

#### 10.5.2 权限与管理员边界

User 决定 enabled 和唯一 scope，外部角色不映射管理权限。OAuth 注册即使用户名为 admin 也只创建普通用户，不进入密码注册管理员初始化分支；已有密码管理员不自动关联。

JWT sub 为本地用户名、type=user、scopes 为当前权限，有效期沿用 24 小时。OAuth 不读取或清除密码失败计数/锁定，但账号禁用或权限非法仍拒绝。工程归属及机器认证不变。

### 10.6 IAM 调用与失败恢复

#### 10.6.1 重放及失败边界

本方案不提供全局一次性消费、注册请求独占或单个凭据即时撤销：

- code 单次兑换由授权服务器保证，PKCE 将其绑定到本轮登录；并发 callback 可能均调用 token endpoint，但重复兑换错误不能当成功。
- 注册 Cookie 到期前可以重复使用，清除不撤销复制件。其只能用于注册相关操作，不能访问业务 API，仍须核验 Origin、registrationID 和 IAM 状态。
- 并发 create 可能多次调用 IAM，甚至提出不同用户名，最终由 IAM 唯一性及占位判定；前端禁用按钮或草稿锁定不是一致性保证。
- IAM 已确认相同账号时，重复 confirm 可以再次签发 JWT，不承诺凭据只签发一次；每次重新读取 UID、enabled、scopes。
- 结果未知后的正常前端只读确认，但 Gateway 不保存“是否已发出注册”的历史；草稿丢失时不能恢复该事实，不自动重放开户。

若后续要求跨副本独占、严格一次性注册凭据或即时撤销单次登录，需要共享消费记录，不能仅用加密 Cookie 声称实现。

#### 10.6.2 两个 IAM 内部接口

| 方法与路径 | 用途 |
| --- | --- |
| `POST /internal/iam/v1/users/authenticate` | method=oauth，提交已验证 externalIdentity，只读核验本地账号 |
| `POST /internal/iam/v1/users/register` | method=oauth，提交资料及注册 Cookie 内身份，创建无密码普通 User |

OAuth 分支只接受 mTLS 验证的 Gateway 身份；普通代理不开放内部 IAM，密码入口不透传浏览器 method/externalIdentity。IAM 不接收 Cookie、code、registrationID 或 Gateway 密钥；公开账号状态通过现有 User API 读取。

| IAM 结果 | Gateway 行为 |
| --- | --- |
| authenticated=true、username/UID | 读取同 UID User，核验后允许登录 |
| registrationRequired=true | callback 可建立注册凭据；confirm 只表示未确认，不自动写入 |
| InvalidRequest / UsernameAlreadyExists | 确定输入错误或用户名冲突，可修正，不认领已有账号 |
| RegistrationInProgress | 保留原输入，只读确认或重新登录 |
| IdentityAlreadyLinked | 提示已有账号，重新登录，不转移身份 |
| IAMUnavailable | 受控暂时不可用，不自动重放注册 |
| IdentityStateInvalid | 受控错误并告警，不当成未注册 |
| AuthenticationFailed / AccountUnavailable | 拒绝 JWT，不另建账号替代 |

仅识别 IAM Handler 已知 code/error；传输错误、非 JSON、未知格式或中间件 Status 不解释成未注册或允许改名。已识别业务错误保留契约状态码；不可识别错误为 503 OAuthUnavailable，不暴露内部 URL、证书及 ES 原始信息。

#### 10.6.3 注册结果未知与前端恢复

首次 create 发出前，前端将 registrationID、固定 username、资料及“已提交”标记保存至 sessionStorage，再禁用重复提交；本地标记写入失败时不发请求。不同 registrationID 的草稿不得混用。

create 先完整校验 Cookie 及输入，再调用 IAM register。200/201 后 authenticate，比较 username/UID、读取 User 后直接返回 JWT；确定的 InvalidRequest/UsernameAlreadyExists 可解锁修正，其他未知写入结果保持原输入。

confirm 只调用 authenticate，不调用 register。username 必须等于固定提交值，以认证返回 UID 读取 User 并核验后签发 JWT；返回其他用户名时要求重新登录。registrationRequired 或 RegistrationInProgress 返回 409 RegistrationInProgress，不证明旧写入未在执行，不自动解锁或 create。

每个 confirm 请求最多调用一次 authenticate；前端每 3 秒一次、最多 3 次自动确认，之后手动确认或重新登录，不延长期限。sessionStorage 草稿丢失时重新 OAuth 登录，不猜测旧请求，也不释放未决占位。

注册页面从 sessionStorage 恢复回调信息及本地草稿，不提供服务端表单恢复接口或共享 editable/confirming 状态。前端存储仅用于交互，外部身份和有效期仍由 Gateway 校验加密 Cookie；恶意调用仍可重放 create，账号唯一性依靠 IAM。

注册 JWT 响应丢失时，有有效 Cookie 和匹配的已提交草稿可 confirm，否则重新登录；授权码处理响应丢失则重新登录，不重发 code。恢复不回滚或覆盖已创建 User。

### 10.7 前端交互

- 登录页读取 providers；本地模式显示密码表单，社区模式显示“社区账号登录”并导航到 Gateway login，隐藏本地注册和密码修改入口。查询失败不回退本地认证。
- 回调页按 10.3 处理授权响应：清除 URL 参数，同源 POST 到固定 API，登录成功后验证并保存 JWT。注册信息及草稿按 10.6.3 保存、恢复和确认。
- 成功、取消或过期时清理本地注册上下文；OAuth 失败不覆盖既有登录 Token。OAuth 请求携带 Cookie，业务 API 使用 Bearer Token。
- 已登录用户发起社区登录是账号切换，不是绑定当前账号；退出只清除本地 Token，不保证社区 SSO 退出，第三方退出也不即时撤销本地 JWT。用户禁用沿用 UserResolve。

### 10.8 配置与部署

通过 `--oauth-config-file` 加载配置，认证入口切换规则见 10.3。启动时拒绝未知字段、空文件、缺少必填字段、无效密钥及不支持的 adapter。配置更新通过重启加载。Provider 在登录和回调请求中按需初始化，不运行后台预热或定时重试任务；首次登录可能等待一次 Discovery 请求。初始化失败后，后续请求在 30 秒重试间隔结束后再次尝试；初始化成功后复用端点及验证器，不定时刷新 Discovery。JWKS 仍由 Token 验证链路按需获取和限频刷新。提供方暂不可用不影响业务代理，也不回退密码认证。

以下为配置示例，并非可用社区地址：

```yaml
publicBaseURL: https://ebs.example.com
contextKeys:
  activeKeyID: key-1
  keys:
    key-1: <独立生成的32字节随机密钥的Base64值>
provider:
  id: community
  protocol: oidc
  issuer: https://id.example.com
  clientID: eulermaker
  clientSecret: <第三方应用密钥>
  tokenEndpointAuthMethod: client_secret_basic
  scopes: [openid, profile, email]
  allowedEndpointOrigins: [https://id.example.com]
  signingAlgorithms: [RS256]
```

| 配置 | 约束 |
| --- | --- |
| provider.id | 首版一个 provider，ID 为 DNS1123 label |
| publicBaseURL | HTTPS origin，不带路径/query/fragment，redirectURI 固定为 `${publicBaseURL}/auth/callback` |
| protocol / issuer | 显式 oidc 或 oauth2，不自动降级，issuer 精确匹配 |
| discovery / 固定端点 | OIDC 用 issuer discovery，或成套 authorizationURL/tokenURL/jwksURL 及可选 userInfoURL，不混用 |
| oauth2 adapter | 固定授权/Token/用户端点及 userinfo-sub，不允许任意 JSON 表达式代替身份认证 |
| tokenEndpointAuthMethod | client_secret_basic 或 client_secret_post，与登记一致 |
| provider.clientSecret | 第三方应用密钥，直接保存在 Secret 内的配置正文，不引用额外文件 |
| contextKeys | 配置内嵌的共享 Cookie 密钥环，不复用 JWT 密钥，不需要 ES 凭据 |
| 上下文期限 | 内部常量 10 分钟，从 login 计时，不续期 |

端点为绝对 HTTPS URL，无 userinfo/fragment，origin 位于 allowedEndpointOrigins；后端客户端不跟随重定向。上游调用单次超时 10 秒、响应上限 1 MiB，callback/register 总上限 30 秒且不超过上下文期限。授权码交换和 IAM register 禁用透明自动重试。

returnTo 限制见 10.4.2，非法值返回 400。

contextKeys 包含 activeKeyID 和 keys 映射，每个 key 为 Base64 编码的独立 32 字节密钥，可用 `openssl rand -base64 32` 生成。各副本先加载新旧密钥，再切 activeKeyID，旧 Cookie 全部过期后移除旧 key；每一步更新配置后重启副本加载。未知 keyID/解密失败重新登录。紧急移除密钥会使对应在途流程失效。

完整 OAuth YAML 作为敏感配置保存在 Secret 中，所有副本使用相同配置、Cookie 密钥环及独立的 JWT 密钥。Gateway 只读挂载文件，不访问 Kubernetes API；配置更新后重启加载，解析错误及日志不回显密钥。具体操作见 [Kubernetes OAuth 部署](../../../deploy/kubernetes/README.md#71-启用社区-oauth-登录)。

Gateway 将 publicBaseURL 和完整 provider 配置确定性序列化后计算 SHA-256 指纹，保存在加密 Cookie 的 configFingerprint 中；不包含 Cookie 密钥环，允许独立轮换密钥。各副本使用相同认证配置；配置指纹不匹配返回 OAuthConfigChanged，不保留旧配置池，接受滚动更新期间部分登录失败。指纹不对外返回或记录日志。前端登录按钮统一显示“社区账号登录”，不配置提供方展示名称。

启用前确认管理员权限和恢复预案，不假定旧密码管理员自动关联。部署须保证 HTTPS、精确回调登记及 Gateway→apiserver mTLS，并按 [ES 初始化契约](ebs-apiserver.md#es-mapping-与初始化)准备 IAM Mapping。前端代理区分回调/注册页面和认证 API，禁止缓存认证响应及记录敏感请求数据。

### 10.9 错误、审计与验收

错误响应及恢复行为分别见 10.3、10.6。

有界进程内限流：login 每 IP 每分钟 10 次，callback/register 每 IP 每分钟 30 次，已验证注册凭据每 registrationID 每分钟 10 次；429 返回 Retry-After。仅为每副本限制，外层代理补充异常流量防护。

日志只记录 requestID、provider、结果及必要 username/UID，不记录 code、state 原值、Cookie、verifier、第三方 Token 或 secret。监控请求错误数、提供方/IAM 延迟、未决注册、配置及验签失败，不使用用户或 registrationID 等高基数标签。

测试至少覆盖：

1. OIDC PKCE/nonce、issuer/audience/azp/签名/期限、JWKS 轮换、UserInfo sub；OAuth adapter 需真实测试应用验证。
2. 前端 query 清除后 POST、固定 redirectURI、Origin、Cookie 缺失/篡改/用途混用、state 不符、开放重定向及错误回调。
3. A 副本开始、B 回调、C 注册；密钥轮换、认证配置指纹变化、过期、Cookie 大小边界、多标签覆盖旧流程。
4. 不依赖 Gateway ES 连接或上下文索引；关闭 OAuth 不影响密码登录、业务代理或 Runner。
5. 首次注册提交前不开帐；admin 名称不提权，同名同邮箱不合并，资料不被重试覆盖。
6. 并发/重放可多次调用 IAM，但身份只关联一个账号；签发前核验当前 UID、enabled、scopes。
7. code 未知不重发；注册未知只读 confirm；confirm 查无结果不触发 create，不静默接受其他 username。
8. 账号成功但响应丢失、清 Cookie 响应丢失、进程退出、禁用及同名重建，不接管原身份。
9. 注册页刷新、已提交草稿锁定、草稿缺失、过期及重新登录；不把前端锁定作为安全保证。
10. IAM OAuth Gateway-only 授权、密码入口不透传 method/身份；现有 IAM 并发唯一性及未知写入测试继续有效。
11. Cookie 与 Bearer 请求分离、JWT 仅成功 JSON 交付、代理日志脱敏、OpenAPI 成功分支互斥。
