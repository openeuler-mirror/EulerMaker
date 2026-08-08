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

Project API 内部会重写为 scoped storage 请求，因此 Project 名需要满足 DNS1123 label 约束，只能使用小写字母、数字和 `-`，不能包含 `.`。页面展示名称使用 `Project.spec.displayName`。

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
  admin: false
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

注册接口以 201 返回成功结果，并提供“创建普通 User 和初始凭据”的单一原子语义。`username` 必须满足 DNS1123 label 约束且不超过 63 个字符，密码长度为 12 到 128 个字符，非空 email 必须合法；新 User 的 `spec.enabled` 固定为 `true`、`spec.admin` 固定为 `false`，请求不能设置 metadata、labels、scope、enabled、admin 或其他权限相关字段。用户名已存在返回 409，非法请求返回 400。

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
    "admin": false,
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

apiserver 启动时检查并创建全部 ES-only 资源索引。生产环境应使用显式 index template/mapping，不依赖动态 mapping 或首次写入自动建索引。

#### ES 文档与 mapping

ES 文档保存完整 API 对象，同时抽取需要过滤和排序的字段：

```json
{
  "apiVersion": "ebs/v1",
  "kind": "Build",
  "documentID": "openeuler-22-03-lts/build-001",
  "metadata": {
    "name": "build-001",
    "namespace": "openeuler-22-03-lts",
    "labels": [
      {"key": "arch", "value": "x86_64"}
    ],
    "creationTimestamp": "2026-01-01T00:00:00Z"
  },
  "data": {
    "...": "完整 API 对象"
  }
}
```

mapping 约束：

- `documentID`、`metadata.name`、`metadata.namespace`、`kind`、`apiVersion` 使用 `keyword`。
- `metadata.creationTimestamp` 使用 `date`。
- `metadata.labels` 使用包含 `key/value` 两个 `keyword` 字段的 `nested` 数组。这样既避免 label key 动态展开导致 mapping 膨胀，也能正确处理包含 `.`、`/` 的 Kubernetes label key。
- `data` 使用 `object` 且 `enabled: false`，只负责保存和还原完整对象。
- 需要查询的业务字段必须显式抽取并定义 mapping，禁止将整个 `spec/status` 动态索引。

#### Label 和 field selector

ESStore 从 `internalversion.ListOptions` 读取已经解析的 selector，并转换为 ES bool query。基础 label selector 支持：

| Selector | ES 查询 |
|----------|---------|
| `key=value`、`key==value` | 同一 nested 元素内匹配 `key` 和 `value` |
| `key!=value` | 排除同一 nested 元素内的 `key/value` 匹配，并遵循 Kubernetes 对缺失 label 的语义 |
| `key in (a,b)` | 同一 nested 元素内匹配 `key` 和 `terms(value)` |
| `key notin (a,b)` | 排除对应 nested 匹配，并遵循 Kubernetes 对缺失 label 的语义 |
| `key` | nested 查询匹配 `key` |
| `!key` | `must_not` nested 查询匹配 `key` |

首期 field selector 只支持：

- `metadata.name`
- `metadata.namespace`

后续业务字段必须先定义稳定的 API 语义和 ES mapping，再加入允许列表。无法识别或不支持的 selector 必须返回 `BadRequest`，不能静默忽略。

#### 分页、版本与一致性

- `ListOptions.limit` 映射为 ES `size`。
- `continue` token 封装排序字段和 `search_after`，禁止使用深分页 `from + size`。
- 默认使用显式的 `documentID` keyword 字段作为稳定的次级排序字段；分页 token 必须带版本并进行完整性校验。
- List 返回值设置 `metadata.continue`；可可靠取得时设置 `remainingItemCount`。
- Create/Update 使用 `refresh=wait_for`，保证写请求成功后紧随其后的 List/Search 能看到结果。
- ES-only 对象的 `metadata.resourceVersion` 由 ES `_seq_no` 和 `_primary_term` 编码生成。
- Update、Patch、Delete 使用 `if_seq_no` 和 `if_primary_term` 做乐观并发控制，版本不匹配返回 `409 Conflict`。
- `resourceVersion` 只用于单对象并发控制，不承诺 etcd watch revision 语义；ES-only 资源不接受基于 resourceVersion 的 watch。
- ES 批量查询应使用 Point in Time 与 `search_after` 保持同一分页过程的一致视图；PIT 标识封装在 continue token 中并设置有限有效期。

## 默认值与校验

各资源 storage strategy 负责在创建和更新时保护 `spec/status` 边界：

- 普通资源更新会保留旧 `status`。
- `/status` 更新会保留旧 `spec`。
- `Project` 创建默认 `status.phase = Active`。
- `Snapshot` 创建默认 `status.phase = Pending`。
- `Build` 创建默认 `status.phase = Pending`。
- `BuildInfo` 创建默认 `status.phase = Pending`。
- `RpmRepo` 创建默认 `status.phase = Pending`。
- `Job` 创建默认 `status.phase = Pending`。
- `Runner` 创建默认 `status.phase = Registering`。
- `User.spec.enabled` 默认为 `true`，`User.spec.admin` 默认为 `false`。

默认值还包括：

- `Project.spec.displayName` 默认为创建请求中的 Project 名称，`spec.specBranch` 默认为 `master`。
- `Build.spec.buildType` 默认为 `full`。
- `Job.spec.runtime` 默认为 `dc`，`spec.timeoutSeconds` 默认为 `10800`。
- `Runner.spec.type` 默认为 `dc`。

当前校验逻辑位于 `pkg/apis/ebs/validation/validation.go`，主要包括：

- Project 名称必须满足 DNS1123 label，并至少包含一个带 `os`、`arch` 的构建目标。
- Snapshot 必须包含 `specCommits`、`buildTargets` 和 `packageRepos`。
- Build 必须包含 `snapshotName`、`buildType`、`packages`，以及带 `os`、`arch` 的 `buildTarget`。
- Runner 类型必须为 `dc`、`vm` 或 `hw`，`type` 和 `arch` 更新时不可变。
- User 名称必须满足 DNS1123 label；`spec.email` 必须是合法邮箱格式。User 的 `metadata.name` 是全局唯一的稳定用户标识，与普通用户 JWT 的 `sub` 一致。User labels 是普通扩展元数据，不参与身份和资源权限判定。
- MachineAccount 名称必须满足 DNS1123 label；`tokenTTLSeconds` 只能为 300～86400。

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
      "specBranch": "master",
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
        "branch": "master"
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
      "specCommits": {
        "gcc": {
          "specUrl": "https://example.com/src-openeuler/gcc.git",
          "commitId": "0123456789abcdef"
        }
      },
      "buildTargets": [{
        "os": "openEuler-22.03-LTS",
        "arch": "aarch64",
        "buildFlag": true
      }],
      "packageRepos": [{
        "name": "gcc",
        "url": "https://example.com/src-openeuler/gcc.git",
        "commitId": "0123456789abcdef"
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
    "metadata": {"name": "build-001"},
    "spec": {
      "snapshotName": "snapshot-001",
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

## 待完善项

存储实现需要完成：

- 新增 ESStore，并实现 CRUD、List、selector、分页和乐观并发控制。
- 将 Project、Snapshot、Build、BuildInfo、RpmRepo 及其子资源切换为 ESStore。
- Job、Runner 使用 generic etcd store。
- 更新 API discovery，确保只有 Job、Runner 暴露 watch verb。
- OpenAPI schema 当前是空对象占位，需要生成真实 schema。
- 外部业务请求的认证与 Project 用户鉴权由 gateway 执行；IAM 内部接口校验 gateway 的内部身份，apiserver 仅部署在受信任网络中。
