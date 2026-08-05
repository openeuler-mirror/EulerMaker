# ebs-gateway 设计

## 一、定位

`ebs-gateway` 是 EulerMaker 对外请求入口，位于客户端和 `ebs-apiserver` 之间，负责令牌认证、用户状态检查、租户鉴权、审计、限流和请求转发。

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

`ebs-gateway` 保持无状态，User 由 `ebs-apiserver` 的用户管理插件提供。gateway 通过 User API 获取用户状态和租户归属，并基于认证身份限制用户只能访问自己有权限的 Project。

## 二、设计目标

| 目标 | 说明 |
|------|------|
| 统一入口 | 用户、外部系统和 runner 统一通过 gateway 访问 `ebs/v1` API |
| 统一认证 | 使用 apiserver IAM 模块验证账号密码并签发 JWT，在业务请求进入 apiserver 前完成 token 校验 |
| 用户校验 | 通过 apiserver 的 User API 检查用户状态，并从 User label 解析租户归属 |
| 请求转发 | 将合法请求反向代理到 `ebs-apiserver` |
| Watch 透传 | 为 apiserver 支持 watch 的 Job、Runner 透传长连接和流式响应 |
| 审计记录 | 记录请求路径、方法、状态码、耗时和调用方信息 |
| 限流保护 | 对调用方进行基础限流，保护 apiserver |
| 租户隔离 | 每个用户只属于一个租户；基于 User 和 Project labels 限制访问范围，支持 Project 在租户间共享 |
| 无业务状态 | 不保存资源对象，不直接访问主存储 |

## 三、资源归属边界

### 3.1 用户与租户模型

用户以集群级 `User` 对象表示。每个用户只属于一个租户，租户归属记录在 `ebs.io/tenant` label 中。

User 由 `ebs-apiserver` 的用户管理插件提供，建议使用独立 API group，避免与构建资源耦合：

```text
/apis/iam.ebs/v1/users
/apis/iam.ebs/v1/users/{name}
```

`ebs-apiserver` 将 `ebs.io/tenant`、`ebs.io/owner-tenant` 和 `ebs.io/member-tenant.*` 视为普通 labels，只负责存储、查询和 label selector，不解释租户语义，也不执行租户权限校验。租户 label 的格式校验、注入、修改保护和访问控制全部由 gateway 完成。

最小 User 模型：

```yaml
apiVersion: iam.ebs/v1
kind: User
metadata:
  name: alice
  labels:
    ebs.io/tenant: tenant-a
spec:
  enabled: true
  displayName: Alice
  email: alice@example.com
```

约束如下：

- `metadata.name` 是稳定用户标识，与用户 JWT 的 `sub` 一致。
- `metadata.labels["ebs.io/tenant"]` 是用户唯一所属租户。
- `spec.enabled` 控制用户是否可以访问业务 API。
- apiserver IAM 模块验证密码，gateway 签发 JWT；User API 保存 gateway 鉴权需要的用户状态和租户归属。
- 同时包含 `ebs:system` 和 `ebs:user-admin` 的管理调用方负责 User 的创建、删除、启停和租户 label 变更。
- gateway 可以短期缓存 User 查询结果，但 User API 是用户状态和租户归属的权威来源。

### 3.2 业务资源归属

| 资源 | 归属规则 |
|------|----------|
| User | 集群级资源，由 apiserver 用户管理插件提供；租户归属记录在 `ebs.io/tenant` label |
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

`{project}` 会由 apiserver 映射到对象的 `metadata.namespace`。gateway 不给 Snapshot、Build、BuildInfo、RpmRepo、Job 注入租户字段，这些对象通过 Project 命名空间继承 Project 访问权限。

Project 创建和更新时，gateway 使用 labels 表达 Project 的 owner tenant 和 member tenants：

```yaml
metadata:
  labels:
    ebs.io/owner-tenant: tenant-a
    ebs.io/member-tenant.tenant-b: "true"
    ebs.io/member-tenant.tenant-c: "true"
```

label 语义：

| Label | 说明 |
|------|------|
| `ebs.io/owner-tenant` | Project 所有者租户，单值 |
| `ebs.io/member-tenant.<tenant>` | 允许操作该 Project 的成员租户，值固定为 `"true"` |

`<tenant>` 必须满足 Kubernetes label key name 片段约束，建议使用小写字母、数字和 `-`，例如 `tenant-a`。外部租户 ID 由用户管理端归一化为稳定的内部 tenant id，再写入 User label。

创建 Project 时，gateway 必须写入或覆盖 `ebs.io/owner-tenant=<resolvedTenant>`。`resolvedTenant` 来自当前 User 的 `ebs.io/tenant` label，客户端传入的 owner tenant 不可信。

更新 Project 时，gateway 必须保护 `ebs.io/owner-tenant` 不被普通用户伪造或篡改。成员租户 label 用于表达共享权限，只有 owner tenant 和 system token 可以增删。

系统组件使用 system token 访问全局 API。普通用户 token 只有在对应 User 存在且已启用时才有效；gateway 从 User label 解析租户，并限制用户只能访问该租户拥有或被授权为 member 的 Project 和 Project 子资源。

## 四、请求处理流程

业务请求的处理链：

```text
Request
  -> Audit
  -> Auth
  -> UserResolve
  -> RateLimit
  -> InjectHeaders
  -> TenantAuthorize
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
| `tenant` | 认证后的租户标识 |
| `user` | 认证后的调用方标识 |
| `user_agent` | 客户端 User-Agent |

### 4.2 Auth

客户端通过 Bearer Token 访问 gateway：

```text
Authorization: Bearer <token>
```

gateway 提供账号密码登录入口。密码由 apiserver IAM 模块验证，验证成功后 gateway 使用 HMAC 密钥签发 JWT；后续业务请求由 gateway 校验 JWT。

JWT claims：

```json
{
  "sub": "alice",
  "scopes": ["ebs:user"],
  "exp": 1790000000
}
```

| Claim | 说明 |
|------|------|
| `sub` | 调用方标识 |
| `scopes` | 固定权限范围：普通用户为 `ebs:user`，业务系统组件为 `ebs:system`，用户管理员为 `ebs:user-admin` |
| `exp` | 过期时间 |

普通用户的 JWT 不携带租户信息，租户归属以 User 的 `ebs.io/tenant` label 为准。系统组件使用 `ebs:system` scope，不要求存在对应 User 对象。`ebs:user-admin` 是独立的固定管理 scope，不隐含在 `ebs:system` 中。

认证失败返回：

| 场景 | HTTP 状态码 |
|------|-------------|
| 未携带 token | 401 |
| token 签名错误 | 401 |
| token 已过期 | 401 |

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

认证成功后，gateway 读取 User，确认 `spec.enabled=true`，然后签发包含 `sub`、`scopes` 和 `exp` 的 JWT。登录失败统一返回 401，不区分用户不存在、密码错误或账号锁定。登录请求体和密码不得写入访问日志、审计日志或错误响应。

密码设置和修改通过 `PUT /auth/users/{name}/password` 完成：

- `ebs:user-admin` 为用户设置初始密码或重置密码时提交 `newPassword`。
- 用户修改自己的密码时提交 `currentPassword` 和 `newPassword`；gateway 先调用认证接口验证当前密码，再调用内部密码设置接口。
- gateway 将 `newPassword` 转换为 apiserver 内部接口要求的 `password` 字段并转发，不在本地保存密码。

### 4.4 UserResolve

普通用户 token 通过签名和有效期校验后，gateway 使用受信任的内部凭据读取：

```text
GET /apis/iam.ebs/v1/users/{jwt.sub}
```

检查规则：

| 场景 | 结果 |
|------|------|
| User 不存在 | 返回 403 |
| `spec.enabled=false` | 返回 403 |
| `metadata.labels["ebs.io/tenant"]` 缺失或非法 | 返回 403 |
| User API 不可用且无有效缓存 | 返回 503 |

gateway 将解析出的租户记为 `resolvedTenant`，并可以按用户名缓存 `enabled` 和 `resolvedTenant`。缓存时间不超过 JWT 剩余有效期，JWT 和缓存使用较短有效期，使用户禁用能够及时生效。

User API 的内部查询不能使用正在校验的普通用户 token，避免递归鉴权和权限提升。gateway 应使用仅具备 User 读取权限的内部凭据，且不得将该凭据转发给客户端。

### 4.5 RateLimit

gateway 按租户、调用方和客户端地址限流：

```text
{resolvedTenant}/{sub}/{clientIP}
```

如果请求未通过认证，不进入业务反向代理。

建议配置：

| 配置 | 默认值 | 说明 |
|------|--------|------|
| `RATE_LIMIT_PER_SEC` | `100` | 每秒补充令牌数 |
| `RATE_LIMIT_BURST` | `200` | 突发桶容量 |

超过限流返回：

```text
HTTP 429 Too Many Requests
```

### 4.6 InjectHeaders

gateway 在转发前删除客户端伪造的内部身份头，再写入可信身份头。

删除：

```text
X-EBS-Tenant
X-EBS-User
X-EBS-Scopes
```

注入：

```text
X-EBS-Tenant: <resolvedTenant>
X-EBS-User: <jwt.sub>
X-EBS-Scopes: <jwt.scopes>
```

这些 header 只来自 gateway，客户端传入值一律丢弃。系统组件请求的 `X-EBS-Tenant` 为空，权限由 `X-EBS-Scopes` 中的 `ebs:system` 表达。

### 4.7 TenantAuthorize

gateway 在转发前进行基础租户鉴权。

普通用户 token：

- 必须对应一个已启用且包含合法 `ebs.io/tenant` label 的 User。
- 可以创建 Project，Project owner tenant 为 `resolvedTenant`。
- 可以访问 owner tenant 为自己的 Project。
- 可以访问 member tenant 包含自己的 Project。
- 可以访问上述 Project 下的 Snapshot、Build、BuildInfo、RpmRepo、Job。
- 不能访问 Snapshot、Build、BuildInfo、RpmRepo、Job 的全局 API。
- 不能访问 Runner API。

系统组件 token：

- 必须包含 `ebs:system` scope。
- 可以访问各资源的全局 API；watch 仅适用于 Job、Runner。
- 可以访问 Runner API。
- 可以跨 Project 访问资源，用于 controller、scheduler 和 runner 协作。

Project 级鉴权规则：

| 请求 | 普通用户 | 系统组件 |
|------|----------|----------|
| `POST /apis/ebs/v1/projects` | 允许，强制写入 `owner-tenant=<resolvedTenant>` | 允许 |
| `GET /apis/ebs/v1/projects` | 只返回自己拥有或被授权的 Project | 允许全量 |
| `GET /apis/ebs/v1/projects/{project}` | 有 owner/member 权限才允许 | 允许 |
| `/apis/ebs/v1/projects/{project}/...` | 有 owner/member 权限才允许 | 允许 |
| `/apis/ebs/v1/{snapshots,builds,buildinfos,rpmrepos}` | 禁止 | 允许跨 Project list/查询，不支持 watch |
| `/apis/ebs/v1/jobs`（含 `watch=true`） | 禁止 | 允许跨 Project list/watch |
| `/apis/ebs/v1/runners...` | 禁止 | 允许 |

User API 鉴权规则：

| 请求 | 普通用户 | 系统组件 |
|------|----------|----------|
| `/apis/iam.ebs/v1/users...` | 禁止 | 只有额外包含 `ebs:user-admin` scope 才允许管理 |

User 管理使用独立的 `ebs:user-admin` scope。该 scope 由部署管理员控制的受信任签发方授予。

如果 gateway 无法确认 Project 归属，应返回 403，不能放行。

实现建议：

- Project 列表请求：普通用户请求 `GET /apis/ebs/v1/projects` 时，gateway 查询并合并两类 Project：
  - `ebs.io/owner-tenant=<resolvedTenant>`
  - `ebs.io/member-tenant.<resolvedTenant>=true`
- Project 详情请求：gateway 先读取 Project，确认 `resolvedTenant` 是 owner 或 member，再转发原请求。
- Project 子资源请求：gateway 先根据路径中的 `{project}` 读取 Project 并校验 owner/member 权限，再转发原请求。
- Project 写请求：gateway 在转发前注入或保护 Project owner/member labels。
- 系统组件请求：包含 `ebs:system` scope 时不做租户过滤。

由于 Kubernetes label selector 不支持 OR，Project 列表请求由 gateway 发起两次查询并合并结果：一次查询 owner tenant，一次查询 member tenant。

### 4.8 AccessLabels

gateway 对 User 的 `POST`、`PUT`、`PATCH` 请求保护：

```text
metadata.labels["ebs.io/tenant"]
```

只有同时包含 `ebs:system` 和 `ebs:user-admin` 的管理请求可以创建或修改 User。gateway 校验每个 User 恰好具有一个合法、非空的 `ebs.io/tenant` label。

gateway 对 Project 的 `POST` JSON 请求注入或覆盖：

```text
metadata.labels["ebs.io/owner-tenant"] = resolvedTenant
```

gateway 对 Project 的 `PUT`、`PATCH` 请求需要保护以下 labels：

```text
metadata.labels["ebs.io/owner-tenant"]
metadata.labels["ebs.io/member-tenant.<tenant>"]
```

访问规则：

- 普通 member tenant 不能修改 Project access labels。
- owner tenant 可以增删 `ebs.io/member-tenant.<tenant>` labels。
- owner tenant 不能把 `ebs.io/owner-tenant` 改成其他租户。
- system token 可以修改 owner/member labels。

Project access labels 的保护逻辑只应用于 Project 对象；User tenant label 使用独立的管理规则。Snapshot、Build、BuildInfo、RpmRepo、Job 通过所属 Project 继承权限。

### 4.9 ReverseProxy

gateway 使用反向代理将请求转发到 `ebs-apiserver`。

代理要求：

- 保持原始 HTTP 方法。
- 保持查询参数。
- 保持请求体。
- 支持 watch 长连接，不缓冲完整响应。
- 不为不支持 watch 的 Project、Snapshot、Build、BuildInfo、RpmRepo 模拟轮询；相关错误由 apiserver 原样返回。
- 透传 apiserver 返回的状态码和响应体。
- 设置上游地址为 `EBS_APISERVER`。

## 五、路由设计

### 5.1 对外路由

gateway 暴露业务 API 和用户管理插件 API：

| 路由 | 鉴权 | 说明 |
|------|------|------|
| `GET /healthz` | 否 | 健康检查 |
| `POST /auth/login` | 否 | 账号密码登录，成功后签发 JWT |
| `PUT /auth/users/{name}/password` | 是 | 设置或修改密码，仅允许本人或 `ebs:user-admin` |
| `ANY /apis/ebs/v1/*` | 是 | `ebs/v1` API 代理，需要租户鉴权 |
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

系统组件和 runner 可能通过 gateway 对支持 watch 的资源建立连接：

```text
GET /apis/ebs/v1/jobs?watch=true
GET /apis/ebs/v1/runners?watch=true
```

当前只有 etcd 主存储的 Job、Runner 支持 watch；Project、Snapshot、Build、BuildInfo、RpmRepo 使用 Elasticsearch 主存储，不支持 watch。普通用户通过 Project 范围的 API 访问资源，Project 范围的 Job watch 在权限校验后透传。

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

| 参数 | 环境变量 | 默认值 | 必填 | 说明 |
|------|----------|--------|------|------|
| `--port` | `PORT` | `8080` | 否 | gateway 监听端口 |
| `--apiserver-addr` | `EBS_APISERVER` | `https://ebs-apiserver:8443` | 是 | 上游 apiserver 地址 |
| `--jwt-secret` | `JWT_SECRET` | 空 | 是 | HMAC JWT 签名密钥 |
| `--user-cache-ttl` | `USER_CACHE_TTL` | `30s` | 否 | User 状态缓存时间，实际缓存不能超过 JWT 剩余有效期 |
| `--insecure-skip-verify` | `INSECURE_SKIP_VERIFY` | `false` | 否 | 是否跳过 apiserver TLS 校验，仅测试环境使用 |
| `--apiserver-ca` | `APISERVER_CA` | 空 | 否 | apiserver CA 文件路径 |
| `--rate-limit-per-sec` | `RATE_LIMIT_PER_SEC` | `100` | 否 | 每秒令牌数 |
| `--rate-limit-burst` | `RATE_LIMIT_BURST` | `200` | 否 | 令牌桶容量 |
| `--log-level` | `LOG_LEVEL` | `info` | 否 | 日志级别 |

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
      "name": "alice",
      "labels": {"ebs.io/tenant": "tenant-a"}
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

登录成功响应包含 JWT。token 中的 `sub` 与 User 的 `metadata.name` 一致；gateway 从 User 的 `ebs.io/tenant` label 获取租户。

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

### 7.5 系统组件 Watch Job

全局 Job watch 只允许系统组件 token 使用。测试 token 需要包含 `ebs:system` scope：

```bash
python3 - <<'PY'
import jwt
import time

token = jwt.encode(
    {
        "sub": "scheduler",
        "scopes": ["ebs:system"],
        "exp": int(time.time()) + 3600,
    },
    "dev-secret",
    algorithm="HS256",
)
print(token)
PY
```

```bash
curl -N http://localhost:8080/apis/ebs/v1/jobs?watch=true \
  -H "Authorization: Bearer ${SYSTEM_TOKEN}"
```

## 八、安全边界

| 风险 | 处理 |
|------|------|
| 未认证请求访问业务 API | 返回 401，不转发到 apiserver |
| 登录接口泄漏密码 | 登录请求体不记录日志，gateway 与 apiserver 之间使用 TLS，错误响应不回显输入 |
| 密码暴力尝试 | gateway 按账号和客户端地址限流，IAM 模块维护失败次数和临时锁定状态 |
| 已删除、禁用或缺少合法租户 label 的用户访问 | UserResolve 返回 403，不进入业务鉴权 |
| User API 不可用 | 仅使用仍有效的短期缓存；无有效缓存时返回 503，不默认放行 |
| 普通用户管理 User | `/apis/iam.ebs/v1/users*` 返回 403 |
| User 内部查询凭据泄漏 | 使用最小只读权限、独立密钥和轮换机制，不写入审计日志或转发请求头 |
| 客户端伪造内部身份头 | 转发前删除并重建 `X-EBS-Tenant`、`X-EBS-User`、`X-EBS-Scopes` |
| 客户端伪造 Project owner | Project 创建请求强制覆盖 `metadata.labels["ebs.io/owner-tenant"]` |
| 客户端越权修改 Project members | 只有 owner tenant 或 system token 可以修改 member tenant labels |
| 用户访问未授权 Project | gateway 查询 Project owner/member labels，不匹配则返回 403 |
| 用户访问全局 API | 普通用户禁止，系统组件 token 才允许；其中只有 Job、Runner 支持 watch |
| 客户端绕过 gateway 直连 apiserver | apiserver 仅暴露在内部网络，并只接受 gateway 和受信任系统组件的内部凭据 |
| token 泄漏 | 依赖 token 过期时间和密钥轮换 |
| 请求风暴 | 基于调用方限流 |
| 上游 TLS 风险 | 生产环境启用 TLS 校验或配置 CA |
| watch 长连接占用 | 限制单调用方并发 watch 数，保留合理超时 |

## 九、测试设计

测试覆盖：

| 模块 | 场景 |
|------|------|
| Auth | 缺失 token、非法 token、过期 token、合法 token |
| PasswordLogin | 登录成功、密码错误、用户不存在、用户禁用、账号锁定、认证接口不可用和登录限流 |
| UserResolve | User 不存在、禁用、租户 label 缺失或非法、缓存命中、User API 不可用以及 system token 跳过 User 查询 |
| Header | 删除伪造 `X-EBS-*` 并注入可信身份 |
| TenantAuthz | 普通用户只能访问自身租户 Project，系统 token 可访问全局 API |
| UserAdmin | 普通用户和普通业务 system token 不能访问 User API，具有 `ebs:user-admin` 的 system token 可以管理 User |
| AccessLabels | 保护 User tenant label；Project 写请求强制写入 owner tenant label，并保护 member tenant labels |
| RateLimit | 超过令牌桶容量后返回 429 |
| Proxy | `/apis/ebs/v1/*` 原样透传 |
| Watch | Job/Runner 的 `watch=true` 请求能够流式转发，普通用户全局 watch 返回 403，ES-only 资源不支持 watch |
| Audit | 请求结束后记录 method/path/status/latency/user |
