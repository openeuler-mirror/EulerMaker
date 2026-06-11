# ebs-gateway 设计

## 一、定位

`ebs-gateway` 是 EulerMaker 对外请求入口，位于客户端和 `ebs-apiserver` 之间，负责认证、租户鉴权、审计、限流和请求转发。

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

`ebs-gateway` 不承载业务状态，不直接访问 etcd 或 Elasticsearch，不直接修改 Snapshot、Build、Job、Runner 的业务语义。首版多租户以 Project 为权限根，gateway 负责基于认证身份限制用户只能访问自己有权限的 Project。

## 二、设计目标

| 目标 | 说明 |
|------|------|
| 统一入口 | 用户、外部系统和 runner 统一通过 gateway 访问 `ebs/v1` API |
| 认证前置 | 在请求进入 apiserver 前完成 token 校验 |
| 请求转发 | 将合法请求反向代理到 `ebs-apiserver` |
| Watch 透传 | 支持 list/watch 的长连接和流式响应 |
| 审计记录 | 记录请求路径、方法、状态码、耗时和调用方信息 |
| 限流保护 | 对调用方进行基础限流，保护 apiserver |
| 租户隔离 | 基于 JWT tenant claim 限制 Project 访问范围，支持一个 Project 被多个租户操作 |
| 无业务状态 | 不保存资源对象，不直接访问主存储 |

## 三、资源归属边界

首版引入多租户，但不引入 `Tenant`、`Role`、`EBSUser` API 对象。租户身份来自 JWT claim，访问权限以 Project 为根。

| 资源 | 归属规则 |
|------|----------|
| Project | 集群级资源，`metadata.name` 是项目唯一标识，访问权限由 Project labels 表达 |
| Snapshot | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| Build | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| Job | Project 级资源，归属来自 `metadata.namespace`，访问权限继承自 Project |
| Runner | 集群级资源，不归属于 Project |

Project 级资源通过 Project API 创建：

```text
/apis/ebs/v1/projects/{project}/snapshots
/apis/ebs/v1/projects/{project}/builds
/apis/ebs/v1/projects/{project}/jobs
```

`{project}` 会由 apiserver 映射到对象的 `metadata.namespace`。gateway 不给 Snapshot、Build、Job 注入租户字段，这些对象通过 Project 命名空间继承 Project 访问权限。

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

`<tenant>` 必须满足 Kubernetes label key name 片段约束，建议使用小写字母、数字和 `-`，例如 `tenant-a`。如果外部租户 ID 包含点号、空格或大写字母，需要在签发 JWT 前归一化为稳定的内部 tenant id。

创建 Project 时，gateway 必须写入或覆盖 `ebs.io/owner-tenant=<jwt.tenant>`。客户端传入的 owner tenant 不可信，必须由 gateway 覆盖。

更新 Project 时，gateway 必须保护 `ebs.io/owner-tenant` 不被普通用户伪造或篡改。成员租户 label 可以作为首版共享权限表达，但谁有权增删 member tenant 需要由 gateway 鉴权控制，默认只有 owner tenant 和 system token 可以修改。

系统组件可以使用 system token 访问全局 API；普通用户 token 只能访问自己拥有或被授权为 member 的 Project 和 Project 子资源。

## 四、请求处理流程

业务请求的处理链：

```text
Request
  -> Audit
  -> Auth
  -> RateLimit
  -> InjectHeaders
  -> TenantAuthorize
  -> ProjectAccessLabels
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

首版认证目标是确认请求来自可信调用方。可以先使用 HMAC JWT，后续再扩展 OIDC/OAuth。

推荐首版 JWT claims：

```json
{
  "sub": "alice",
  "tenant": "tenant-a",
  "scopes": ["ebs:user"],
  "exp": 1790000000
}
```

| Claim | 说明 |
|------|------|
| `sub` | 调用方标识 |
| `tenant` | 调用方所属租户 |
| `scopes` | 调用方权限范围，普通用户为 `ebs:user`，系统组件为 `ebs:system` |
| `exp` | 过期时间 |

认证失败返回：

| 场景 | HTTP 状态码 |
|------|-------------|
| 未携带 token | 401 |
| token 签名错误 | 401 |
| token 已过期 | 401 |

### 4.3 RateLimit

首版可以按租户、调用方和客户端地址限流：

```text
{tenant}/{sub}/{clientIP}
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

### 4.4 InjectHeaders

gateway 在转发前删除客户端伪造的内部身份头，再写入可信身份头。

删除：

```text
X-EBS-Tenant
X-EBS-User
X-EBS-Scopes
```

注入：

```text
X-EBS-Tenant: <jwt.tenant>
X-EBS-User: <jwt.sub>
X-EBS-Scopes: <jwt.scopes>
```

这些 header 只来自 gateway，客户端传入值一律丢弃。

### 4.5 TenantAuthorize

gateway 在转发前进行基础租户鉴权。

普通用户 token：

- 可以创建 Project，Project owner tenant 为 `jwt.tenant`。
- 可以访问 owner tenant 为自己的 Project。
- 可以访问 member tenant 包含自己的 Project。
- 可以访问上述 Project 下的 Snapshot、Build、Job。
- 不能访问全局 Snapshot/Build/Job list/watch API。
- 不能访问 Runner API。

系统组件 token：

- 必须包含 `ebs:system` scope。
- 可以访问全局 list/watch API。
- 可以访问 Runner API。
- 可以跨 Project 访问资源，用于 controller、scheduler 和 runner 协作。

首版 Project 级鉴权规则：

| 请求 | 普通用户 | 系统组件 |
|------|----------|----------|
| `POST /apis/ebs/v1/projects` | 允许，强制写入 `owner-tenant=<jwt.tenant>` | 允许 |
| `GET /apis/ebs/v1/projects` | 只返回自己拥有或被授权的 Project | 允许全量 |
| `GET /apis/ebs/v1/projects/{project}` | 有 owner/member 权限才允许 | 允许 |
| `/apis/ebs/v1/projects/{project}/...` | 有 owner/member 权限才允许 | 允许 |
| `/apis/ebs/v1/jobs?watch=true` | 禁止 | 允许 |
| `/apis/ebs/v1/runners...` | 禁止 | 允许 |

如果 gateway 无法确认 Project 归属，应返回 403，不能放行。

实现建议：

- Project 列表请求：普通用户请求 `GET /apis/ebs/v1/projects` 时，gateway 查询并合并两类 Project：
  - `ebs.io/owner-tenant=<jwt.tenant>`
  - `ebs.io/member-tenant.<jwt.tenant>=true`
- Project 详情请求：gateway 先读取 Project，确认 JWT tenant 是 owner 或 member，再转发原请求。
- Project 子资源请求：gateway 先根据路径中的 `{project}` 读取 Project 并校验 owner/member 权限，再转发原请求。
- Project 写请求：gateway 在转发前注入或保护 Project owner/member labels。
- 系统组件请求：包含 `ebs:system` scope 时不做租户过滤。

由于 Kubernetes label selector 不支持 OR，Project 列表请求需要 gateway 发起两次查询后合并结果，或者全量 list 后在 gateway 内存过滤。首版 Project 数量较少时可以使用内存过滤；后续规模变大再引入权限索引。

### 4.6 ProjectAccessLabels

gateway 对 Project 的 `POST` JSON 请求注入或覆盖：

```text
metadata.labels["ebs.io/owner-tenant"] = jwt.tenant
```

gateway 对 Project 的 `PUT`、`PATCH` 请求需要保护以下 labels：

```text
metadata.labels["ebs.io/owner-tenant"]
metadata.labels["ebs.io/member-tenant.<tenant>"]
```

首版规则：

- 普通 member tenant 不能修改 Project access labels。
- owner tenant 可以增删 `ebs.io/member-tenant.<tenant>` labels。
- owner tenant 不能把 `ebs.io/owner-tenant` 改成其他租户。
- system token 可以修改 owner/member labels。

该逻辑只应用于 Project 对象，不应用于 Snapshot、Build、Job、Runner。

### 4.7 ReverseProxy

gateway 使用反向代理将请求转发到 `ebs-apiserver`。

代理要求：

- 保持原始 HTTP 方法。
- 保持查询参数。
- 保持请求体。
- 支持 watch 长连接，不缓冲完整响应。
- 透传 apiserver 返回的状态码和响应体。
- 设置上游地址为 `EBS_APISERVER`。

## 五、路由设计

### 5.1 对外路由

首版只暴露当前标准 API 路由：

| 路由 | 鉴权 | 说明 |
|------|------|------|
| `GET /healthz` | 否 | 健康检查 |
| `ANY /apis/ebs/v1/*` | 是 | `ebs/v1` API 代理，需要租户鉴权 |

不提供 `/api/*` 简化别名。客户端统一使用 Kubernetes-like API 路径，避免出现两套路由标准。

### 5.2 API 透传示例

| 客户端请求 | 转发到 apiserver |
|------------|------------------|
| `GET /apis/ebs/v1/projects` | `GET /apis/ebs/v1/projects` |
| `POST /apis/ebs/v1/projects` | `POST /apis/ebs/v1/projects` |
| `GET /apis/ebs/v1/projects/{project}/jobs` | `GET /apis/ebs/v1/projects/{project}/jobs` |
| `PUT /apis/ebs/v1/projects/{project}/jobs/{name}/status` | `PUT /apis/ebs/v1/projects/{project}/jobs/{name}/status` |
| `GET /apis/ebs/v1/jobs?watch=true` | `GET /apis/ebs/v1/jobs?watch=true` |
| `PUT /apis/ebs/v1/runners/{name}/status` | `PUT /apis/ebs/v1/runners/{name}/status` |

### 5.3 Watch 透传

系统组件和 runner 可能通过 gateway 建立 watch：

```text
GET /apis/ebs/v1/jobs?watch=true
GET /apis/ebs/v1/runners?watch=true
```

普通用户首版不允许访问全局 watch。用户侧如需实时状态，应通过 Project API 查询或后续提供 Project 范围的 watch 能力。

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

### 7.2 签发测试 token

```bash
python3 - <<'PY'
import jwt
import time

token = jwt.encode(
    {
        "sub": "alice",
        "tenant": "tenant-a",
        "scopes": ["ebs:user"],
        "exp": int(time.time()) + 3600,
    },
    "dev-secret",
    algorithm="HS256",
)
print(token)
PY
```

### 7.3 创建 Project

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

### 7.4 系统组件 Watch Job

全局 Job watch 只允许系统组件 token 使用。测试 token 需要包含 `ebs:system` scope：

```bash
python3 - <<'PY'
import jwt
import time

token = jwt.encode(
    {
        "sub": "scheduler",
        "tenant": "system",
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
| 客户端伪造内部身份头 | 转发前删除并重建 `X-EBS-Tenant`、`X-EBS-User`、`X-EBS-Scopes` |
| 客户端伪造 Project owner | Project 创建请求强制覆盖 `metadata.labels["ebs.io/owner-tenant"]` |
| 客户端越权修改 Project members | 只有 owner tenant 或 system token 可以修改 member tenant labels |
| 用户访问未授权 Project | gateway 查询 Project owner/member labels，不匹配则返回 403 |
| 用户访问全局 list/watch | 普通用户禁止，系统组件 token 才允许 |
| token 泄漏 | 依赖 token 过期时间和密钥轮换 |
| 请求风暴 | 基于调用方限流 |
| 上游 TLS 风险 | 生产环境启用 TLS 校验或配置 CA |
| watch 长连接占用 | 限制单调用方并发 watch 数，保留合理超时 |

## 九、测试设计

首版测试建议覆盖：

| 模块 | 场景 |
|------|------|
| Auth | 缺失 token、非法 token、过期 token、合法 token |
| Header | 删除伪造 `X-EBS-*` 并注入可信身份 |
| TenantAuthz | 普通用户只能访问自身租户 Project，系统 token 可访问全局 API |
| ProjectAccessLabels | Project 写请求强制写入 owner tenant label，并保护 member tenant labels |
| RateLimit | 超过令牌桶容量后返回 429 |
| Proxy | `/apis/ebs/v1/*` 原样透传 |
| Watch | system token 的 `watch=true` 请求能够流式转发，普通用户全局 watch 返回 403 |
| Audit | 请求结束后记录 method/path/status/latency/user |

## 十、后续扩展

后续可以在不改变首版代理边界的前提下扩展：

- OIDC/OAuth 认证器。
- 更完整的 Project 级授权策略和权限索引。
- 用户、角色、权限 API 对象。
- 租户配额和租户级限流。
- 更细粒度的 API 限流策略。
- TLS 双向认证。
