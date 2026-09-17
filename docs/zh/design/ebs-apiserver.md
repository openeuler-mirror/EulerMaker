# ebs-apiserver 实现

## 概述

`ebs-apiserver` 是 EulerMaker 的资源 API 服务，代码位于 `components/ebs-apiserver`。服务基于 `k8s.io/apiserver` 的 `GenericAPIServer`，提供 Kubernetes 风格的 REST API、`/status` 子资源，以及按资源类型选择的 etcd 或 Elasticsearch 持久化能力。

apiserver 可以通过 `--enable-iam` 启用内置 IAM 模块，提供 User、MachineAccount 资源及相关认证接口。

资源存储分配如下：

- Job、Runner 使用 etcd。Runner 表示执行节点；两类资源需要可靠的 resourceVersion 和 list/watch，用于调度、心跳和执行状态协作。
- Project、Snapshot、Build、BuildInfo、RpmRepo 使用 Elasticsearch，提供分页、selector 和搜索能力，不支持 watch。
- User、MachineAccount 使用 Elasticsearch和专用IAM Storage，公开对象与认证字段保存在同一文档中，不支持watch。

## 架构

```
client / ebs-gateway
        │
        ▼
components/ebs-apiserver
        │
        ├── k8s.io/apiserver GenericAPIServer
        │   ├── REST storage
        │   ├── status subresource
        │   ├── validation/defaulting
        │   └── list/get/create/update/delete
        ├── IAM module（--enable-iam）
        │   ├── User、MachineAccount REST storage
        │   └── User password、machine client secret authenticator
        │
        ├── etcd
        │   └── Job、Runner：对象、resourceVersion 与 watch
        │
        └── Elasticsearch
            └── 其余资源：对象、索引、过滤与搜索
```

## 项目结构

```
components/ebs-apiserver/
├── cmd/server/main.go                 # 进程入口
├── pkg/
│   ├── apis/ebs/
│   │   ├── register.go                # API group 注册
│   │   ├── v1/
│   │   │   ├── types.go               # 资源类型
│   │   │   ├── register.go            # 版本资源注册
│   │   │   ├── defaults.go            # 默认值
│   │   │   └── zz_generated.deepcopy.go
│   │   └── validation/validation.go   # admission 校验
│   ├── iam/                           # 可选 IAM 模块
│   │   ├── api/                       # iam.ebs/v1 User、MachineAccount 类型与注册
│   │   ├── registry/                  # User、MachineAccount REST storage
│   │   ├── credential/                # Argon2id 哈希与凭据验证
│   │   └── install.go                 # IAM 路由安装
│   ├── registry/
│   │   ├── scoped_store.go            # 命名空间作用域 store 包装
│   │   └── ebs/
│   │       ├── */storage.go            # 各资源 REST storage
│   │       └── scopedresource/         # Project 子资源通用 storage
│   ├── server/
│   │   ├── project_alias.go           # Project API 路由适配
│   │   └── server.go                  # apiserver 配置与资源安装
│   └── storage/
│       ├── es/                        # Elasticsearch client
│       └── esstore/                   # Elasticsearch REST storage
├── Dockerfile                         # openEuler 镜像构建
├── hack/                              # 代码生成脚本
├── go.mod
└── go.sum
```

## API 版本与资源

API group 定义为：

```text
Group: ebs
Version: v1
apiVersion: ebs/v1
```

已安装到 apiserver 的资源如下：

集群级 `BuildConf` 使用 Elasticsearch 存储，在 Ready 前幂等初始化 `default` 对象，并在创建 Build 前校验目标映射。接口、初始化和错误规则见 [BuildConf 设计](build-configuration.md#2-buildconf构建环境)。

| 资源 | 主存储 | Project API | 全局 API | Watch | 子资源 |
|------|--------|-------------|----------|-------|--------|
| Project | Elasticsearch | `/apis/ebs/v1/projects` | - | 否 | `/status` |
| Snapshot | Elasticsearch | `/apis/ebs/v1/projects/{project}/snapshots` | `/apis/ebs/v1/snapshots` | 否 | `/status` |
| Build | Elasticsearch | `/apis/ebs/v1/projects/{project}/builds` | `/apis/ebs/v1/builds` | 否 | `/status`, `/abort` |
| BuildInfo | Elasticsearch | `/apis/ebs/v1/projects/{project}/buildinfos` | `/apis/ebs/v1/buildinfos` | 否 | `/status` |
| RpmRepo | Elasticsearch | `/apis/ebs/v1/projects/{project}/rpmrepos` | `/apis/ebs/v1/rpmrepos` | 否 | `/status` |
| Job | etcd | `/apis/ebs/v1/projects/{project}/jobs` | `/apis/ebs/v1/jobs` | 是 | `/status` |
| Runner | etcd | - | `/apis/ebs/v1/runners` | 是 | `/status` |

其中 `Snapshot`、`Build`、`BuildInfo`、`RpmRepo`、`Job` 是 Project 下的子资源，路径中的 `{project}` 是项目归属来源。Job 的全局 API 用于调度器跨 Project list/watch；其他资源的全局 API 用于跨 Project list 和查询。`Project` 和 `Runner` 为集群级资源。

apiserver还为 Runner提供服务端过滤的 Job list-watch：

```text
GET /apis/ebs/v1/runners/{runner}/jobs
GET /apis/ebs/v1/runners/{runner}/jobs?watch=true
```

该路由在服务端固定添加 `status.runner={runner}` 过滤条件，不接受客户端提供的 `fieldSelector`。它支持 `resourceVersion`、`timeoutSeconds` 和 `allowWatchBookmarks`，list响应提供后续 watch使用的 resourceVersion。请求来自 Runner token时，路径名称必须由受信任 gateway身份头绑定到对应 Runner，过滤不能由客户端自行完成。

Project API 内部会重写为 scoped storage 请求，因此 Project 名需要满足 DNS1123 label 约束，只能使用小写字母、数字和 `-`，不能包含 `.`。`default` 保留给全局默认资源，apiserver 拒绝创建同名 Project。页面展示名称使用 `Project.spec.displayName`。

### IAM API

启用 IAM 模块后注册集群级 User 和 MachineAccount 资源：

```text
GET    /apis/iam.ebs/v1/users
POST   /apis/iam.ebs/v1/users
GET    /apis/iam.ebs/v1/users/{name}
PUT    /apis/iam.ebs/v1/users/{name}
PATCH  /apis/iam.ebs/v1/users/{name}
DELETE /apis/iam.ebs/v1/users/{name}

GET    /apis/iam.ebs/v1/machineaccounts
GET    /apis/iam.ebs/v1/machineaccounts/{name}
DELETE /apis/iam.ebs/v1/machineaccounts/{name}
```

User 对象保存账号资料和状态：

```yaml
apiVersion: iam.ebs/v1
kind: User
metadata:
  name: alice
spec:
  enabled: true
  scopes:
    - ebs:ops
  displayName: Alice
  email: alice@example.com
```

MachineAccount 保存 Runner token 有效期配置：

```yaml
apiVersion: iam.ebs/v1
kind: MachineAccount
metadata:
  name: runner-site-a
spec:
  tokenTTLSeconds: 3600
```

`metadata.name` 是 client ID，满足 DNS1123 label 且全局唯一。`tokenTTLSeconds` 在创建时设置，范围为 300～86400 秒，省略时默认为 3600 秒；客户端不能在交换请求中指定或扩大有效期。

IAM 模块还注册仅供 gateway 使用的内部接口：

```text
POST /internal/iam/v1/users/register
PUT  /internal/iam/v1/users/{name}/password
POST /internal/iam/v1/authenticate
POST /internal/iam/v1/machineaccounts/register
POST /internal/iam/v1/machineaccounts/{name}/authenticate
```

注册请求和成功响应：

```json
{
  "username": "alice",
  "password": "user supplied password",
  "displayName": "Alice",
  "email": "alice@example.com"
}
```

```json
{"username":"alice"}
```

注册接口以 201 返回成功结果，并提供“创建普通 User 和初始凭据”的单一原子语义。`username` 必须满足 DNS1123 label 约束且不超过 63 个字符，密码长度为 12 到 128 个字符，非空 email 必须合法；新 User 的 `spec.enabled` 固定为 `true`、`spec.scopes` 固定为 `["ebs:user"]`，请求不能设置 metadata、labels、scopes、enabled 或其他权限相关字段。用户名已存在返回 409，非法请求返回 400。

apiserver 将 User 对象和 Argon2id 密码哈希通过一次 create-only 请求写入同一 ES 文档。创建成功后账号即可认证；名称已存在返回 409，不需要注册中状态或补偿流程。

MachineAccount 和初始 client secret 通过单一内部接口创建。请求为：

```json
{
  "name": "runner-site-a",
  "clientSecret": "base64url-encoded-random-secret",
  "tokenTTLSeconds": 3600
}
```

接口以201返回`{"name":"runner-site-a"}`，不回显secret。`name`满足DNS1123 label；client secret由管理端使用密码学安全随机源生成，采用无`:`的base64url编码，原始熵不少于32字节且编码长度不超过256字符；`tokenTTLSeconds`范围为300～86400，省略时默认为3600。apiserver将MachineAccount对象和Argon2id凭据哈希通过一次create-only请求写入同一ES文档；名称已存在返回409，非法请求返回400。

机机认证请求为：

```json
{"clientSecret":"base64url-encoded-random-secret"}
```

认证成功返回账号当前的权威授权信息：

```json
{
  "authenticated": true,
  "name": "runner-site-a",
  "tokenTTLSeconds": 3600
}
```

认证接口不签发 JWT。对象不存在、credential 不存在、secret 错误或账号锁定均返回相同的 401 响应；同一账号连续失败 5 次后锁定 15 分钟，成功后清零失败次数。Gateway 使用认证成功响应中的 `tokenTTLSeconds` 决定有效期，Runner 名称由交换请求提供并由 Gateway 独立校验。

设置密码请求：

```json
{"password":"user supplied password"}
```

认证请求和成功响应：

```json
{"username":"alice","password":"user supplied password"}
```

```json
{"authenticated":true,"username":"alice"}
```

内部接口不加入 API discovery，只接受 URI SAN 被识别为 `ebs-gateway` 的 mTLS 客户端身份。请求体、密码、client secret及其哈希不得写入日志、审计事件或错误响应。

用户密码和 MachineAccount client secret 均使用 Argon2id 自描述哈希保存，随机 salt、算法版本和参数编码在哈希字符串中。固定参数为 `memory=19456 KiB`、`iterations=2`、`parallelism=1`。密码长度为 12 到 128 个字符；两类凭据分别维护失败次数，同一主体连续失败 5 次后锁定 15 分钟，成功认证后清零失败次数。认证失败返回统一结果，不区分主体不存在、凭据错误、未设置凭据或账号被锁定。

## 存储设计

### 存储路由

REST storage 按资源静态路由：

| Storage | 资源 | 能力 |
|---------|------|------|
| generic etcd store | Job、Runner | CRUD、List、Watch、原生 resourceVersion |
| ESStore | Project、Snapshot、Build、BuildInfo、RpmRepo | CRUD、List、分页、label selector、有限的 field selector、搜索 |
| User store | User | 单文档CRUD、List、密码设置和认证 |
| MachineAccount store | MachineAccount | 单文档内部原子创建、Get、List、Delete和凭据认证 |

Job、Runner 的 `/status` 子资源必须使用对应的 etcd store；ES-only 资源的 `/status` 和 Build 的 `/abort` 必须使用对应的 ESStore。子资源不能回退到另一种存储。

Job etcd store的 selection predicate至少暴露 `metadata.name`、`metadata.namespace`、`status.runner` 和 `status.phase`。Runner范围 Job路由使用该 predicate对 list和 watch执行相同过滤；Job从其他 Runner转入当前 Runner时产生 ADDED，从当前 Runner转出时产生 DELETED，仍匹配时的变化产生 MODIFIED。过滤在 apiserver/watch cache中完成，不能把全局事件流转发给 gateway或 Runner后再过滤。

ESStore 不实现 `rest.Watcher`，API discovery 不为 ES-only 资源声明 `watch` verb。对这些资源请求 `watch=true` 应返回不支持该操作的错误，而不是轮询 ES 模拟 watch。

### etcd 存储

apiserver 使用 `k8s.io/apiserver/pkg/registry/generic/registry.Store` 将资源对象写入 etcd，默认前缀为：

```text
/registry/ebs
```

只有 Job 和 Runner 写入 etcd：

```text
/registry/ebs/jobs/{project}/{name}
/registry/ebs/runners/{name}
```

Job 按 `{project}/{name}` 存在全局资源前缀下。全局 list/watch 监听 `/registry/ebs/jobs`，Project API 的 list/watch 监听 `/registry/ebs/jobs/{project}`。Runner 是集群级资源，list/watch 监听 `/registry/ebs/runners`。

etcd store 继续使用 Kubernetes 原生 label selector、field selector、resourceVersion、冲突检测和 watch 语义。

### Elasticsearch 存储

ESStore 直接实现 GenericAPIServer 所需的 REST storage 接口：

- `Get/Create/Update/Delete` 直接读写 Elasticsearch。
- `List` 将 `ListOptions` 转换为 ES 查询，并还原为对应的 Kubernetes List 对象。
- 根据资源策略实现 `/status` 和 `/abort`，继续保证普通更新保留旧 `status`、`/status` 更新保留旧 `spec`。
- 不实现 Watch。

Project scoped 对象写入 ES 时使用 `{project}/{name}` 作为文档 ID，HTTP 请求中会对 `/` 做 URL escape。

ES-only 资源使用以下索引：

```text
ebs-projects
ebs-snapshots
ebs-builds
ebs-buildinfos
ebs-rpmrepos
ebs-users
ebs-machineaccounts
```

`ebs-users` 使用User名称作为文档ID，每个文档同时保存公开对象和内部credential字段：

```json
{
  "apiVersion": "iam.ebs/v1",
  "kind": "User",
  "metadata": {
    "name": "alice"
  },
  "spec": {
    "enabled": true,
    "scopes": ["ebs:ops"],
    "displayName": "Alice",
    "email": "alice@example.com"
  },
  "credential": {
    "passwordHash": "$argon2id$...",
    "passwordUpdatedAt": "2026-08-08T00:00:00Z",
    "failedAttempts": 0,
    "lockedUntil": null
  }
}
```

User的REST Storage是专用实现。注册接口写入包含credential的完整文档；GET/List只返回公开对象；PUT/PATCH保留已有credential；密码设置和认证接口读取或更新credential字段；DELETE删除整个文档。认证失败计数、锁定时间和密码修改使用ES的`seq_no`、`primary_term`执行乐观并发更新。

`ebs-machineaccounts` 使用MachineAccount名称作为文档ID，每个文档同时保存公开对象和内部credential字段：

```json
{
  "apiVersion": "iam.ebs/v1",
  "kind": "MachineAccount",
  "metadata": {
    "name": "runner-site-a"
  },
  "spec": {
    "tokenTTLSeconds": 3600
  },
  "credential": {
    "secretHash": "$argon2id$...",
    "secretCreatedAt": "2026-08-08T00:00:00Z",
    "failedAttempts": 0,
    "lockedUntil": null
  }
}
```

MachineAccount的REST Storage是专用实现。内部创建接口写入完整文档，认证接口读取credential字段；GET/List只构造并返回`apiVersion`、`kind`、`metadata`和`spec`，不得序列化credential。认证失败计数和锁定时间使用ES的`seq_no`、`primary_term`执行乐观并发更新。DELETE删除单个文档，删除成功后账号不能继续认证。

apiserver 启动时检查并创建全部 ES-only 资源索引。Build 和其他业务资源使用基础 mapping，User 和 MachineAccount 使用独立的 IAM mapping。IAM mapping 单独定义不建立索引的 `credential` 字段，避免认证数据字段出现在业务资源 mapping 中。
物理索引使用 `-v1` 版本后缀，例如 `ebs-builds-v1`，并在创建时绑定无版本的稳定 alias `ebs-builds`。alias 设置 `is_write_index: true`，所有读写和 PIT 操作只使用 alias。生产环境不依赖动态 mapping 或首次写入自动建索引。

#### ES 文档与 mapping

ES 文档在 `data` 中保存完整 API 对象，并对需要过滤的字段建立显式 mapping：

```json
{
  "apiVersion": "ebs/v1",
  "kind": "Build",
  "documentID": "openeuler-22-03-lts/123e4567-e89b-42d3-a456-426614174000",
  "metadata": {
    "name": "123e4567-e89b-42d3-a456-426614174000",
    "namespace": "openeuler-22-03-lts",
    "labels": [
      {"key": "arch", "value": "x86_64"}
    ],
    "creationTimestamp": "2026-01-01T00:00:00Z"
  },
  "data": {
    "apiVersion": "ebs/v1",
    "kind": "Build",
    "metadata": {"...": "完整对象元数据"},
    "spec": {
      "buildTarget": {
        "os": "openEuler-22.03-LTS",
        "arch": "x86_64"
      }
    },
    "status": {
      "phase": "Processing",
      "stage": "build"
    }
  }
}
```

mapping 约束：

- `documentID`、`metadata.name`、`metadata.namespace`、`kind`、`apiVersion` 使用 `keyword`。
- `metadata.creationTimestamp` 使用 `date`。
- `metadata.labels` 使用包含 `key/value` 两个 `keyword` 字段的 `nested` 数组。这样既避免 label key 动态展开导致 mapping 膨胀，也能正确处理包含 `.`、`/` 的 Kubernetes label key。
- 所有 ES 文档的 `data` 使用 `object` 且 `dynamic: false`：完整对象保留在 `_source` 中。通用 mapping 将 `data.status.phase`、`data.status.stage` 建立为 `keyword`；RpmRepo 使用独立 mapping，仅将 `data.status.repository.phase`、`data.status.release.phase` 建立为 `keyword`。Build 的构建目标暂不建立字段 mapping，调用方通过 Build label 表达并过滤 OS、架构。
- 需要查询的业务字段必须显式定义 mapping，禁止将整个 `spec/status` 动态索引。

Build 查询字段直接来自待持久化的完整 API 对象，不生成额外的查询投影。Create、Update、Patch、`/status` 和 `/abort` 更新 `data` 后，对应的索引字段随同一次 ES 写入更新。`status.stage` 为空时不写 `data.status.stage`。

#### Label 和 field selector

EulerMaker 系统保留标签及其适用对象见 [EulerMaker 标签约定](./labels.md)。

ESStore 从 `internalversion.ListOptions` 读取已经解析的 selector，并转换为 ES bool query。基础 label selector 支持：

| Selector | ES 查询 |
|----------|---------|
| `key=value`、`key==value` | 同一 nested 元素内匹配 `key` 和 `value` |
| `key!=value` | 排除同一 nested 元素内的 `key/value` 匹配，并遵循 Kubernetes 对缺失 label 的语义 |
| `key in (a,b)` | 同一 nested 元素内匹配 `key` 和 `terms(value)` |
| `key notin (a,b)` | 排除对应 nested 匹配，并遵循 Kubernetes 对缺失 label 的语义 |
| `key` | nested 查询匹配 `key` |
| `!key` | `must_not` nested 查询匹配 `key` |

Project、Snapshot、Build、BuildInfo 和 BuildResource 使用通用状态字段；RpmRepo 的过程仓与正式发布各自使用独立状态字段：

| API 字段 | ES 字段 | 操作符 |
|----------|---------|--------|
| `status.phase` | `data.status.phase` | `=`、`==`、`!=` |
| `status.repository.phase`（仅 RpmRepo） | `data.status.repository.phase` | `=`、`==`、`!=` |
| `status.release.phase`（仅 RpmRepo） | `data.status.release.phase` | `=`、`==`、`!=` |
| `status.stage` | `data.status.stage` | `=`、`==`、`!=` |

IAM 资源不支持 `status.phase` 和 `status.stage`。多个 requirement 以及 label selector、Project 路径隐含的 namespace 条件均按 AND 组合；客户端提供与路径不同的 namespace 时返回空列表，不能查询到其他 Project。无法识别或不支持的字段、操作符和语法必须返回 `BadRequest`，不能静默忽略。

`status.stage` 具有特殊的存在性语义：只有字段存在且非空的对象才参与 stage requirement 匹配。`status.stage=publish` 只匹配明确处于 publish 阶段的对象；`status.stage!=publish` 也只匹配 stage 存在、非空且不等于 publish 的对象。缺少 stage、值为 `null` 或空字符串的对象对两种查询都不匹配。ES 查询必须为两种操作符都附加 `exists(data.status.stage)`；不等值查询不能只生成 `must_not term`。

#### 分页、版本与一致性

- `ListOptions.limit` 映射为 ES `size`。
- `continue` token 封装排序字段和 `search_after`，禁止使用深分页 `from + size`。
- 所有 ES-backed 资源默认按 `metadata.creationTimestamp desc` 排序，缺少创建时间的历史对象排在最后；时间相同时按 `documentID desc` 稳定决胜。
- continue token 保存创建时间和 documentID 两个 `search_after` 值；排序策略版本必须进入 token fingerprint，分页 token 还必须带格式版本并进行完整性校验。
- List 返回值设置 `metadata.continue`；可可靠取得时设置 `remainingItemCount`。
- Create/Update 使用 `refresh=wait_for`，保证写请求成功后紧随其后的 List/Search 能看到结果。
- ES-only 对象的 `metadata.resourceVersion` 由 ES `_seq_no` 和 `_primary_term` 编码生成。
- Update、Patch、Delete 使用 `if_seq_no` 和 `if_primary_term` 做乐观并发控制，版本不匹配返回 `409 Conflict`。
- `resourceVersion` 只用于单对象并发控制，不承诺 etcd watch revision 语义；ES-only 资源不接受基于 resourceVersion 的 watch。
- ES 批量查询应使用 Point in Time 与 `search_after` 保持同一分页过程的一致视图；PIT 标识封装在 continue token 中并设置有限有效期。

#### Mapping 升级

apiserver 只负责在 alias 不存在时初始化 `v1` 物理索引，不自动迁移已有索引。兼容性新增字段可以更新当前物理索引的 mapping；不兼容变更应创建下一版本物理索引，将数据 reindex 后，通过单次 `_aliases` 请求移除旧索引并绑定新索引。切换时必须将新索引设置为 `is_write_index: true`，旧索引保留至确认无需回滚后再删除。

## 默认值与校验

Project 创建和普通更新时将缺失的 `project.ebs.io/type` 补为 `personal`，显式值仅允许 `community` / `personal`；状态更新保留原 labels。存量缺失标签的读取及筛选语义见 [标签约定](labels.md#33-工程分类)。

各资源 storage strategy 负责在创建和更新时保护 `spec/status` 边界：

- 普通资源更新会保留旧 `status`。
- `/status` 更新会保留旧 `spec`。
- `Project` 创建默认 `status.phase = Active`。
- `Snapshot` 创建默认 `status.phase = Pending`。
- `Build` 创建默认 `status.phase = Pending`。
- `BuildInfo` 创建默认 `status.phase = Pending`。
- `RpmRepo` 创建默认 `status.repository.phase = Pending`，`status.release` 在产生正式发布意图前保持为空。
- `Job` 创建默认 `status.phase = Pending`。
- `Runner` 创建默认 `status.phase = Offline`。Runner agent 完成本地初始化并具备接收任务能力后，通过首次状态上报将其更新为 `Online`。
- 新建 Runner 的 `spec.instanceId` 必须是规范小写 UUID v4，创建后不可修改或清空。同名 POST 继续使用标准 create-only 语义并返回 409，apiserver 不把创建转换为更新。
- `User.spec.enabled` 默认为 `true`，`User.spec.scopes` 默认为 `["ebs:user"]`，并按字典序规范化。

默认值还包括：

- `Project.spec.displayName` 默认为创建请求中的 Project 名称；`spec.defaultRef` 是 GitRef 对象，仅支持 Branch/Tag，整体为空时默认 `{type: Branch, value: master}`。Project 和 Snapshot 均允许仓库 ref 整体为空，创建/普通更新不补齐；显式 ref 必须完整且合法。解析时优先使用包 ref，整体为空则回退到 Snapshot.spec.defaultRef，不回查 Project。旧字符串形式不再接受。
- `Build.spec.buildType` 默认为 `full`。
- `Job.spec.runtime` 默认为 `ct`，`spec.timeoutSeconds` 默认为 `10800`。
- `Runner.spec.type` 默认为 `ct`。

当前校验逻辑位于 `pkg/apis/ebs/validation/validation.go`，主要包括：

- API 请求使用严格解码：`apiVersion`、`kind`、对象结构或字段类型不合法时直接拒绝，未知字段不会被静默丢弃。

- Project 名称必须满足 DNS1123 label、不能是系统保留名称 `default`，并至少包含一个带 `os`、`arch` 的构建目标。
- Build 创建时根据默认化后的 spec 补齐缺失的 `ebs.io/target-os`、`ebs.io/target-arch`、`ebs.io/build-type` 标签；显式提供但与 spec 不一致的标签返回 `422 Invalid`。Build 创建后整个 `spec` 不可修改；普通 Update 只能修改允许的 metadata，`/status` 更新保留原对象 metadata，不能修改这些标签。
- Snapshot 的 `status.packageRepoStatuses` 由 Snapshot Controller 根据 `spec.packageRepos` 写入：`cloneUrl` 记录 git-server 确认同步后返回的只读地址；包解析成功时记录 commitId，失败时记录包级 error。原始仓库地址始终从 `spec.packageRepos[].url` 读取，不在 status 中重复保存。创建请求不能直接设置该字段。Snapshot conditions 只记录整体级异常，不使用包名作为 condition type。
- Build 必须包含 `buildType`、`packages`，以及带 `os`、`arch` 的 `buildTarget`。创建和普通更新时，`metadata.name` 必须为标准小写、带连字符的 UUID（`8-4-4-4-12`，不限定 v4）；缺失或格式非法返回 `422 Invalid`，错误字段为 `metadata.name`。UUID 格式校验不替代调用方对名称不复用的保证。
- 创建 `buildType=single` 或 `specified` 的 Build 时（含 dry-run），额外读取一次所属 Project，校验 `spec.packages` 中每个包名均存在于 `Project.spec.packageRepos[].name`；允许多个包及重复包名。不存在的包返回 `422 Invalid`，错误字段定位到 `spec.packages[i]`；Project 不存在或读取失败时原样返回对应 API 错误，不创建 Build。full/incremental 不执行该检查，普通更新和 `/status` 更新也不重新校验包存在性；创建后 Project 变化仍需由控制器处理。
- 创建 `full`、`incremental` 或 `specified` Build 时，按 Project + OS + Arch 执行下节的 ES 目标占用协议；省略 buildType 按 full 处理。single 不参与占用。该协议替代当前单实例创建锁，最新一条非 single Build 查询仅作为历史数据门禁，不作为跨实例互斥依据。
- Runner 的 `instanceId` 创建时必须是规范小写 UUID v4，创建后不可变；类型必须为 `ct`、`vm` 或 `hw`，`arch` 必填，type/arch labels 必须分别与 spec 字段一致。etcd generic store 负责校验 `resourceVersion` 并返回更新冲突。
- User 名称必须满足 DNS1123 label；`spec.email` 必须是合法邮箱格式。`spec.scopes` 只允许且必须恰好包含 `ebs:user`、`ebs:ops` 或 `ebs:admin` 中的一项，不得组合或重复；单独的 `ebs:ops` 即表示运维人员。User 不能持有 `ebs:runner` 或 `ebs:system`。User 的 `metadata.name` 是全局唯一的稳定用户标识，与用户 JWT 的 `sub` 一致。User labels 是普通扩展元数据，不参与身份和资源权限判定。
- MachineAccount 名称必须满足 DNS1123 label；`tokenTTLSeconds` 只能为 300～86400。

从旧模型升级时必须在启用新 Gateway 前迁移已有 User：`spec.admin=true` 转为 `spec.scopes=["ebs:admin"]`，其他 User 转为 `spec.scopes=["ebs:user"]`，迁移完成后删除旧 `spec.admin` 字段。存量对象读取不依赖创建默认值，不能把数据迁移交给 storage strategy 隐式完成。

## 非 single Build 的 ES 目标占用

apiserver 使用 ES 内部目标占用文档实现跨实例互斥，不依赖进程内锁或 etcd。Build Controller 只写 Build 状态，不读写占用索引。占用编排和恢复位于 `pkg/registry/ebs/build/claims_*.go`，底层复用 `pkg/storage/es` 的 create-only、实时 GET、CAS 与 PIT 扫描能力。

### 范围与存储

- 目标键为 Project 名、`spec.buildTarget.os`、`spec.buildTarget.arch` 三元组；不同目标可并行，同一目标最多有一个通过本协议创建的非终态 full/incremental/specified Build。single 创建和完成均不影响占用。
- 内部 alias 为 `ebs-build-target-claims`，初始物理索引为 `ebs-build-target-claims-v1`，只允许一个写索引，不暴露为公共 EBS 资源。所有实例必须访问同一索引及一致的 routing；禁止在在线创建期间无协调地切换占用索引，否则同 ID 可能在两个物理索引同时存在。
- 文档 ID 为三元组 UTF-8 JSON 数组紧凑编码后的 SHA-256 小写十六进制串；不使用有歧义的字符串拼接。读取后核对原始三元组，不一致时拒绝并告警。
- Mapping 使用 `dynamic: strict`；身份与 state 为 keyword，时间为 date。不保存 Project 凭据或完整 Build spec。

内部记录字段：

| 字段 | 含义 |
| --- | --- |
| `project` / `os` / `arch` | 目标原始三元组 |
| `claimID` | 本次抢占的随机 UUID，用于区分同一目标的不同占用 |
| `buildName` | UUID 形式且不复用的 Build 名称，作为占用归属标识 |
| `state` | `Reserved`、`Creating`、`Bound`、`Releasing` |
| `createdAt` / `updatedAt` | UTC 观测时间，只用于诊断与扫描，不能作为抢占期限 |
| `releaseReason` | Releasing 时记录 `CreateNotSent`、`CreateRejected`、`Terminal` 或 `Deleted` |

不设 TTL，不因心跳、请求超时或占用年龄直接释放。占用文档更新和删除全部携带当前 `_seq_no`、`_primary_term`；每次先核对 claimID 与 Build 身份。版本冲突后重新 GET，不把旧决定套到新版本上。

### 创建协议

1. 执行认证授权、默认值、字段和 Project 包名校验及 admission；确定 Build name。相同 Build 名称已存在时维持标准 `409 AlreadyExists`，不得当作本次请求成功或转换为更新。后续持久化不得重新生成 UID 或改变目标。
2. 按确定 ID 使用 `_create` 抢占 Reserved 文档。存在其他占用时返回 `409 Conflict`，附带占用 Build 名称；读取失败返回临时错误，禁止绕过。
3. 抢占成功后执行历史门禁：按 Project + OS + Arch 查询创建时间降序最新一条非 single Build（`ebs.io/build-type!=single`，`limit=1`，不按 phase 过滤）。不存在或已为 Success/Failed/Aborted/Skipped 时继续；否则拒绝创建并释放本次 Reserved 占用。空/未知 phase 视为非终态，查询失败也不创建。仅查最新一条，不承诺排除更早的历史非终态对象。
4. CAS 将 Reserved 改为 Creating。只有成功完成这次状态迁移的请求能够发送一次 Build create-only 写入；其他实例和恢复任务不得替它重新发送创建。ES 客户端也不得自动重试这笔写请求。
5. Build 创建确认成功后，CAS 将占用改为 Bound，再返回正常创建结果。若记录 Bound 失败但 Build 已确认成功，仍返回已创建 Build，记录错误并交由恢复任务补齐；Creating 仍阻塞其他创建。

Reserved 与 Creating 是写入分界：请求尚在 Reserved 时不可能发送 Build 创建。恢复任务可以 CAS 将 Reserved 改为 Releasing 后删除；原请求随后 CAS Creating 必然失败，必须结束且不发送 Build 写入。扫描提前取消一个仍存活的 Reserved 请求属于可重试竞争，不影响互斥。

dry-run 只检查现有占用和历史门禁，不创建占用、不写 Build，也不预留后续执行资格；返回成功不保证真实提交仍可通过。由无 dry-run 的真实 POST 执行原子抢占。

### 写入结果与返回

| 操作与结果 | 处理 |
| --- | --- |
| 占用 `_create` 结果未知 | 按固定 ID 实时 GET；claimID 一致才继续，其他 claimID 返回 Conflict。不存在或读取失败时返回临时错误，不创建 Build；可能迟到的 Reserved 记录由扫描安全清理 |
| Reserved → Creating CAS 结果未知 | 本请求不得发送 Build 创建，即使 GET 看到 Creating 也不再接续发送；保留占用等待人工确认，避免与恢复流程形成第二个发送者 |
| Build 写入确定未发送 | CAS 到 Releasing/CreateNotSent 后删除；保留原始请求错误 |
| Build 写入明确被拒绝且未持久化 | CAS 到 Releasing/CreateRejected 后删除；保留原始请求错误。仅客户端能证明未提交时使用此分类，ES 超时/5xx 不默认视为未写入 |
| Build 写入结果未知 | 实时 GET 固定 Build ID；同名称、同 Project、同目标则确认持久化并补 Bound；不存在或读取失败则保持 Creating、返回临时错误，不重放 POST、不释放 |
| Bound CAS 或占用删除结果未知 | GET 占用确认；已达目标或删除后不存在则完成，属于其他 claimID 时结束旧清理，仍为自己的记录则按最新版本继续对应恢复流程 |

进程可能在标记 Creating 后、真正发送前崩溃，这与“请求已发送但尚未可见”无法通过一次 GET 区分。因此 Creating + Build NotFound 不自动解除：持续确认并告警。人工释放前必须隔离原发送实例并排除 ES 仍在处理的迟到创建；仅关闭进程或等待固定时间不构成该证明。无法证明时保持阻塞。此方案优先保证互斥，不承诺所有崩溃窗口自动恢复可用。

### 终态与删除释放

占用释放由 apiserver 完成，覆盖 `/status`、`/abort`、DELETE 与 DeleteCollection 的逐项删除路径：

- Build 成功持久化终态后，实时 GET 核实同名称、同 Project、同目标且 phase 为 Success/Failed/Aborted/Skipped，将自己的占用 CAS 为 Releasing/Terminal 后删除。若占用还在 Creating 但同名称、同 Project、同目标 Build 已存在，可先确认 Bound。不得在 Build 写入成功前释放。
- Build DELETE 先完成权限、UID/resourceVersion 前置条件和删除校验。实际删除成功后，使用被删对象的名称、Project 和目标核实占用归属，CAS 为 Releasing/Deleted 后删除。仅设置 deletionTimestamp、finalizer 尚未完成时不得释放。
- 对 Bound 记录实时 GET Build 返回 NotFound，也可 CAS 到 Releasing/Deleted 后删除：Bound 已证明该次唯一创建写入曾完成，且协议禁止重发该创建。Creating + NotFound 不适用此规则。名称、Project 或目标不匹配属于身份异常，保留占用并告警，不更新或删除替代对象。
- Build 状态/删除已成功，但释放失败时，不把成功的业务操作改报失败；输出结构化错误，由扫描补偿。占用漏释放只会暂时阻止新构建，不会放行两个构建。
- Releasing 记录由任意实例按版本条件删除；旧清理遇到新 claimID 时立即停止。

为使终态释放安全，必须同步增加 Build 终态不可回退校验：旧 phase 已终态时，`/status` 只允许保留原 phase，不得改回非终态或另一终态；可继续完善其他允许的 status 字段。仅有 resourceVersion 校验不足以防止读取最新对象后的主动回退。Build 名称为 UUID 形式且不复用是本协议前提，删除后不得重建同名 Build。调用方保证名称不复用，apiserver 不保存历史名称使用记录，也不校验已删除对象的历史名称。


此互斥限定 Build 对象生命周期：Aborted 或删除不代表对应 Job/Runner 已停止执行；停止旧任务、避免旧任务继续发布由各业务控制器负责，不由占用释放协议保证。

### 多实例恢复与上线

- apiserver 启动 ES 索引就绪后运行恢复扫描，默认每 30s 一轮、分页 100 条，使用服务生命周期 context 和现有 ES 请求超时；停止时取消扫描，不派生脱离生命周期的后台任务。
- 各实例均可扫描，无需 leader。搜索结果只作为候选，处理前按 ID 实时 GET 取得最新状态与版本；CAS 冲突结束本条，下一轮重试。搜索 refresh 延迟只延后清理，不用于授权创建。
- Reserved：CAS 到 Releasing/CreateNotSent 并删除。Creating：GET 同名称、同 Project、同目标 Build，存在则补 Bound 后继续判断，NotFound 则保留并告警。Bound：非终态保留、终态释放、NotFound 按删除规则释放。Releasing：条件删除。任意读取错误不释放；每条失败不阻断本页其他记录。
- 新请求看到占用统一返回 Conflict，不在请求内无限等待恢复。日志包含目标、claimID、Build name、state、操作和结果，避免记录 spec；指标记录抢占冲突、恢复错误、Creating 未确认数量与持续时间，目标和 Build 名称不作为指标 label。
- 上线需暂停创建，停用所有旧版实例，按目标检查并处理已有非终态 Build，为需继续运行的非 single 构建建立 Bound 占用，然后启用新版本。历史门禁本身不能处理多个存量非终态对象；禁止新旧协议混跑，也禁止绕过 apiserver 直接写 Build。
- 备份恢复必须同时考虑 Build 和占用；恢复后在重新开放创建前执行一致性检查，不能只清空占用索引来解除阻塞。

### 实现边界与验证

- `pkg/storage/es` 提供内部索引 mapping、alias 校验及 create-only、实时 GET、CAS 更新/删除、分页扫描；`claims_store.go` 封装占用记录与原始 ES 版本，与公共 EBS 资源隔离。
- Build 创建编排统一封装在 build storage，拆开本地校验与最终持久化，避免校验/admission 被重复执行；本地目标锁可移除，或仅作为减少竞争的优化，不参与正确性证明。
- 终态与删除释放使用持久化成功后的回调，不能复用“删除前必须成功”的清理 hook；扫描器由 server 生命周期管理。Build Controller 不新增 ES 权限或释放调用。
- 测试至少覆盖：两个 apiserver 同目标同时创建仅一方成功、不同目标并行、single 绕过、默认 full、dry-run 无写入、历史门禁拒绝、各步骤崩溃与 Unknown、迟到写入不误释放、Reserved 清理与 Creating CAS 竞争、终态不可回退、删除前置条件失败不释放、释放 CAS 防误删、多实例重复扫描和重启恢复。

## 启动参数

入口为：

```text
components/ebs-apiserver/cmd/server/main.go
```

关键默认参数：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--etcd-servers` | `http://etcd:2379` | etcd 地址 |
| `--secure-port` | `8443` | HTTPS 监听端口 |
| `--es-servers` | `http://elasticsearch:9200` | Elasticsearch 地址 |
| `--enable-iam` | `false` | 启用内置 User、MachineAccount API和凭据认证模块 |

IAM 模块只增加 `--enable-iam` 这一项启动配置。User、MachineAccount索引名称、Argon2id参数、失败计数及锁定策略使用模块固定值；Elasticsearch连接复用`--es-servers`。

示例：

```bash
cd components/ebs-apiserver
go run ./cmd/server \
  --etcd-servers=http://localhost:2379 \
  --es-servers=http://localhost:9200 \
  --enable-iam \
  --secure-port=8443
```

## 镜像构建

组件使用顶层 `Dockerfile` 构建镜像，它基于 openEuler 构建并运行：

```bash
cd components/ebs-apiserver
docker build -t ebs-apiserver:latest .
```

## API 示例

创建 Project：

```bash
curl -k -X POST https://localhost:8443/apis/ebs/v1/projects \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "ebs/v1",
    "kind": "Project",
    "metadata": {"name": "openeuler-22-03-lts"},
    "spec": {
      "displayName": "openEuler 22.03 LTS",
      "description": "openEuler 22.03 LTS",
      "defaultRef": { "type": "Branch", "value": "master" },
      "buildPayload": "debug_package: false",
      "buildTargets": [{
        "os": "openEuler-22.03-LTS",
        "arch": "aarch64",
        "buildFlag": true,
        "publishFlag": true
      }],
      "packageRepos": [{
        "name": "gcc",
        "url": "https://example.com/src-openeuler/gcc.git",
        "ref": {"type": "Branch", "value": "master"}
      }],
      "bootstrapRepo": [{
        "name": "base",
        "repo": "https://repo.example.com/openEuler/24.03-LTS/OS/aarch64/"
      }]
    }
  }'
```

创建 Snapshot：

```bash
curl -k -X POST https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/snapshots \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "ebs/v1",
    "kind": "Snapshot",
    "metadata": {"name": "snapshot-001"},
    "spec": {
      "defaultRef": {"type": "Branch", "value": "master"},
      "packageRepos": [{
        "name": "gcc",
        "url": "https://example.com/src-openeuler/gcc.git",
        "ref": {"type": "Branch", "value": "master"}
      }]
    }
  }'
```

创建 Build：

```bash
curl -k -X POST https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/builds \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "ebs/v1",
    "kind": "Build",
    "metadata": {"name": "123e4567-e89b-42d3-a456-426614174000"},
    "spec": {
      "buildType": "full",
      "packages": ["gcc"],
      "buildTarget": {
        "os": "openEuler-22.03-LTS",
        "arch": "aarch64",
        "buildFlag": true,
        "publishFlag": true
      }
    }
  }'
```

查询 Project 下的 BuildInfo 和 RpmRepo：

```bash
curl -k 'https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/buildinfos'
curl -k 'https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/rpmrepos'
```

更新 Job 状态：

`PUT` 更新需要携带当前对象的 `metadata.resourceVersion`。只更新 `status` 时，建议使用 merge patch：

```bash
curl -k -X PATCH https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/jobs/job-001/status \
  -H "Content-Type: application/merge-patch+json" \
  -d '{
    "status": {
      "phase": "Running",
      "runner": "runner-001"
    }
  }'
```

如果使用 `PUT`，需要先查询对象并把返回的 `metadata.resourceVersion` 填入请求体：

```bash
curl -k -X PUT https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/jobs/job-001/status \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "ebs/v1",
    "kind": "Job",
    "metadata": {
      "name": "job-001",
      "resourceVersion": "<resourceVersion from GET>"
    },
    "status": {
      "phase": "Running",
      "runner": "runner-001"
    }
  }'
```

Watch Job：

```bash
curl -k -N 'https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/jobs?watch=true'
```

Watch 全局 Job：

```bash
curl -k -N 'https://localhost:8443/apis/ebs/v1/jobs?watch=true'
```

Watch Runner：

```bash
curl -k -N 'https://localhost:8443/apis/ebs/v1/runners?watch=true'
```

按 label 查询 ES-only 资源：

```bash
curl -k --get \
  --data-urlencode 'labelSelector=arch=x86_64,channel in (stable,testing)' \
  'https://localhost:8443/apis/ebs/v1/builds'
```

分页查询 ES-only 资源：

```bash
curl -k --get \
  --data-urlencode 'limit=100' \
  'https://localhost:8443/apis/ebs/v1/builds'

# 使用上一页响应 metadata.continue 的原值请求下一页
curl -k --get \
  --data-urlencode 'limit=100' \
  --data-urlencode 'continue=<metadata.continue>' \
  'https://localhost:8443/apis/ebs/v1/builds'
```

按构建目标 label、phase 和 stage 查询 Build：

```bash
curl -k --get \
  --data-urlencode 'labelSelector=ebs.io/target-os=openEuler-22.03-LTS,ebs.io/target-arch=x86_64' \
  --data-urlencode 'fieldSelector=status.phase=Processing,status.stage=build' \
  --data-urlencode 'limit=100' \
  'https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/builds'
```

所有 ES-backed 资源默认按创建时间倒序，因此通过 `ebs.io/target-os`、`ebs.io/target-arch` 完整指定构建目标后配合 `limit=1` 可以取得该 target 最新创建的 Build。Project status 不缓存最新 Build 或其状态，调用方应使用该查询读取最新 Build，并以返回对象的 `status` 为准。未完整限定 target 时，`limit=1` 只表示整个过滤结果中的最新一条，不表示每个 target 各返回一条。

## 待完善项

存储实现需要完成：

- 新增 ESStore，并实现 CRUD、List、selector、分页和乐观并发控制。
- 将 Project、Snapshot、Build、BuildInfo、RpmRepo 及其子资源切换为 ESStore。
- Job、Runner 使用 generic etcd store。
- 更新 API discovery，确保只有 Job、Runner 暴露 watch verb。
- OpenAPI schema 当前是空对象占位，需要生成真实 schema。
- 外部业务请求的认证与 Project 用户鉴权由 gateway 执行；IAM 内部接口校验 gateway 的内部身份，apiserver 仅部署在受信任网络中。
