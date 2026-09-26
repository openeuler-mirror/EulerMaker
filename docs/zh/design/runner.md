# Runner 设计

## 一、定位

Runner 是 EulerMaker 的 Job 执行机，负责注册自身能力、上报心跳、监听已调度给自己的 Job、执行构建任务，并将执行状态回写到 `ebs/v1` API。

Runner 不直接访问 etcd 或 Elasticsearch，也不直接依赖这些组件的内部存储路径。资源对象读写通过统一 API 完成，构建产物和日志正文直接发送给 Artifact Manager：

```text
runner -> ebs-gateway -> ebs-apiserver -> etcd / Elasticsearch
runner -> artifact-manager -> local persistent storage
```

其中：

- `ebs-gateway` 负责认证、鉴权、审计和请求转发。
- `ebs-apiserver` 负责资源语义、校验、默认值、`/status` 子资源、list/watch 和存储访问。
- `artifact-manager` 负责产物上传、实时日志追加、日志封账和 Job 上传清单。
- etcd 和 Elasticsearch 是组合主存储，Runner 不直接访问。

## 二、核心职责

| 职责 | 说明 |
|------|------|
| 注册 Runner | 创建或受限更新与 token 身份一致的集群级 `Runner` 对象，声明安装实例 ID、执行机类型、架构和能力标签 |
| 上报状态 | 定期更新 `Runner.status`，包括 phase、资源容量、可调度资源、地址、系统信息和心跳时间 |
| 监听 Job | 通过自身 Runner 范围的 Job list-watch 获取已分配任务，服务端只返回 `status.runner` 等于自身名称的 Job |
| 执行 Job | 根据 Job spec 准备执行环境、提供 payload 参数、运行任务、收集产物 |
| 上传日志和产物 | 容器运行期间向 Artifact Manager 实时追加日志，执行结束后封账日志、上传产物并完成 Job 上传清单 |
| 回写结果 | 通过 Project API 更新 Job status，推进 Job phase/stage，并记录 Artifact 完成摘要 |

## 三、API 交互

Runner 访问 `ebs-gateway`，API 路径保持 `ebs-apiserver` 的资源路径不变，由 gateway 转发。

### 3.1 获取 Runner token

Runner 不保存长期 Runner JWT，而是使用预置的 MachineAccount client ID 和 client secret 换取短期 token：

```text
POST /auth/runner-token
Authorization: Basic base64(<client-id>:<client-secret>)
Content-Type: application/json

{"runner":"runner-001"}
```

`runner-001` 必须满足 DNS1123 label。Gateway 成功后返回：

```json
{
  "accessToken": "<jwt>",
  "tokenType": "Bearer",
  "expiresIn": 3600
}
```

`accessToken` 是最长 24 小时的 `ebs:runner` JWT，token 的 `sub`、`runner` claim 和本地 Runner 名称完全相同；`tokenType` 固定为 `Bearer`；`expiresIn` 是从响应签发时间开始计算的有效秒数，范围为 300～86400。Runner 必须验证三个字段的类型和值，并使用发起交换请求时记录的本地单调时钟加 `expiresIn` 计算保守到期时间，不依赖解析 JWT claims。响应缺失字段、字段类型错误或值超出范围均视为交换失败。Runner 仅在内存中保存短期 token，不将 client secret、Basic header 或完整 token 写入日志和状态对象。

Runner 在首次注册前获取 token，并在到期前重新交换。建议在剩余有效期小于 10 分钟或总有效期的 20%（取较小值）时刷新，并加入随机抖动避免大量实例同时请求。交换接口返回 401 表示长期凭据无效，400 表示 Runner 名称或请求格式错误，两者均进入低频退避并报告配置错误，不能无限快速重试。429 遵循 `Retry-After`，网络错误和 503 使用带抖动的指数退避。Runner 不使用 refresh token。

心跳、Runner 状态更新、Job list/watch 和 Job 状态更新共享同一个并发安全的 token provider。任一调用方发现 token 缺失、进入刷新窗口或收到 401 时可以触发刷新，但同一时刻最多只能有一个 `/auth/runner-token` 请求；其他调用方等待并复用该次刷新结果，不得各自交换 token。刷新成功后以原子方式替换内存中的 token 和到期时间；刷新失败时保留尚未过期的旧 token，已经过期或因 401 被拒绝的 token 不得继续用于新请求。处理 401 时只在被拒绝的 token 仍是当前 token 时将其失效；如果并发刷新已经替换了 token，则直接使用新 token 重试，不能使新 token 失效或再次发起交换。

普通业务请求收到 gateway 返回的 401 后，必须使当前 token 失效，等待一次强制刷新，并使用新 token 重放原请求一次；重放后仍返回 401 时将错误交给上层，不再循环刷新。Runner 发出的 JSON 请求必须能够从内存中的结构重新编码，不能重用已经消费的请求体。watch 建连返回 401 时执行同样的单次强制刷新，然后重新执行 list/watch；已经建立的 watch 断开后按正常重连流程重新取得当前 token。403 不触发刷新，按权限或身份配置错误处理。

Runner 从 `--machine-credential-file` 指定的 JSON 文件同时读取 client ID 和 client secret：

```json
{
  "clientID": "runner-site-a",
  "clientSecret": "base64url-encoded-random-secret"
}
```

文件只允许上述两个必填字符串字段，拒绝未知字段、空值、重复 JSON key 和文件尾部附加内容。文件权限应限制为 `0600` 或等效的只读 Secret 挂载权限。Runner 启动时读取并校验一次凭据文件，随后仅使用内存中的 client ID 和 client secret，不监视或重新读取文件；凭据文件被替换后必须重启 Runner 才能生效。Runner 不接受通过其他启动参数直接覆盖文件中的 client ID 或 client secret，避免凭据两部分配置不一致。

### 3.2 Runner API

```text
GET    /apis/ebs/v1/runners/{name}
POST   /apis/ebs/v1/runners
PUT    /apis/ebs/v1/runners/{name}
PATCH  /apis/ebs/v1/runners/{name}
PUT    /apis/ebs/v1/runners/{name}/status
```

Runner 是集群级资源，`metadata.name` 在集群内唯一。Runner token 的 `sub`、`runner` claim 以及请求对象或路径中的 Runner 名称必须完全相同；Runner 不能 list/watch Runner 集合或访问其他 Runner，并且禁止对所有资源执行 DELETE。

Runner 采用受限自注册模型。Runner 在首次启动时生成 UUID v4 作为安装实例 ID，以小写规范形式持久化到 `--root-dir/runner-instance-id`；文件必须使用原子创建并限制为 Runner 运行用户读写。进程重启和镜像升级必须复用该值，不能按进程重新生成，也不能在多个 Runner 实例之间复制同一数据目录。`instanceId` 只是实例标识，不是认证凭据，不能替代 MachineAccount token，也不得用于授权判断。

对象不存在时通过 collection `POST` 创建；对象已存在时先 GET 最新对象并比较 `spec.instanceId`：相同才视为同一安装实例并允许受限更新，不同则本地终止注册并报告同名冲突，不能覆盖已有对象。创建冲突和并发更新冲突返回 409，Runner 必须重新读取：若最新对象的 `instanceId` 与本地相同，视为前一次创建已成功或同实例并发注册并继续；不同则终止。gateway 不自动重放写请求。

`spec.instanceId` 创建后不可修改或清空。若本地 ID 文件丢失，Runner 必须停止并要求恢复原文件或由管理员删除旧 Runner 后重新注册，不能自动接管。

Runner 创建时可以声明 `spec.instanceId`。创建后可以更新的普通对象字段为：

```text
metadata.labels["ebs.io/runner-type"]
metadata.labels["ebs.io/runner-arch"]
metadata.labels["ebs.io/runner-capability.*"]
spec.type
spec.arch
```

`spec.instanceId` 必须是规范的小写 UUID，创建时必填，创建后不可修改或清空。`ebs.io/runner-type`、`ebs.io/runner-arch` 必须分别与 `spec.type`、`spec.arch` 一致。Runner 创建对象时 `status` 必须为空，且不能提供 annotations、finalizers、ownerReferences 或其他服务端 metadata。`spec.unschedulable`、`spec.taints`、`ebs.io/zone`、信任级别和安全域等调度管理字段由 system 调用方维护；Runner 更新完整对象时必须保留这些已有字段，不能通过 PUT 字段缺失、Merge Patch 的 `null` 或 JSON Patch 删除父级字段绕过保护。

### 3.3 Job API

Runner 使用自身范围的 Job API执行 list-watch：

```text
GET /apis/ebs/v1/runners/{runner}/jobs
GET /apis/ebs/v1/runners/{runner}/jobs?watch=true
```

`{runner}` 必须与 Runner token 的 `sub` 和 `runner` claim 完全一致。该接口由 apiserver强制按 `status.runner={runner}` 过滤；客户端不能提供或覆盖 `fieldSelector`。list返回当前已分配 Job 和 `metadata.resourceVersion`，watch从该版本继续。

Job 是 Project 级资源。Runner 从 list/watch 对象的 `metadata.namespace` 获取所属 Project，并通过 Project API回写 Job状态：

```text
GET /apis/ebs/v1/projects/{project}/jobs/{name}
PUT /apis/ebs/v1/projects/{project}/jobs/{name}/status
```

`{project}` 来自 Job 对象的 `metadata.namespace`。Job spec 中不重复保存 `projectName`。

Runner 必须从 Job 的 `metadata.uid` 获取不可变的 `jobUID`。缺少 `metadata.namespace`、`metadata.name` 或 `metadata.uid` 的 Job 不能执行，也不能使用 Job 名称推导 UID。Job 被删除后以相同名称重建时，新 UID 对应独立的日志流、Artifact 和上传清单。

CT Job 的 `spec.scriptRefs` 非空时，Runner 使用自身 token 经 Gateway 按名称读取每个全局 Script：

```text
GET /apis/ebs/v1/scripts/{name}
```

Runner 不列举或修改 Script。数组为空时不访问 Script API，沿用镜像入口；数组第一项是主脚本，其余脚本供主脚本调用。具体拉取、缓存与容器入口规则见 8.1。

### 3.4 Artifact Manager API

Runner 使用 `--artifact-manager` 配置的独立地址直接访问 Artifact Manager，正文不经过 Gateway：

```text
POST /artifacts/v1/projects/{project}/jobs/{job}/logs/chunks
GET  /artifacts/v1/projects/{project}/jobs/{job}/logs/status
POST /artifacts/v1/projects/{project}/jobs/{job}/logs/complete
POST /artifacts/v1/projects/{project}/jobs/{job}/artifacts
POST /artifacts/v1/projects/{project}/jobs/{job}/manifest/complete
```

这些写入和状态查询请求复用 3.1 节的并发安全 token provider，将当前 `ebs:runner` Token 作为 Bearer Token 直接交给 Artifact Manager。Artifact Manager 再通过 Gateway 校验 Token；Runner 不额外申请 Artifact Token。Artifact Manager 返回 401 时执行与普通业务请求相同的一次强制刷新和单次重放，但请求体必须可重建：日志 chunk 保留在本地待确认缓冲中，普通 Artifact 保留本地文件，JSON 请求从结构重新编码。403 不刷新 Token。

Runner 为 Gateway 和 Artifact Manager 分别配置 TLS。`--gateway-ca` 只用于 Gateway，`--artifact-manager-ca` 只用于 Artifact Manager；测试环境可以分别显式跳过校验，不能因为其中一个地址使用 HTTP 或跳过 TLS 而放宽另一个客户端。

## 四、Runner 资源模型

Runner 使用 `ebs/v1` 数据模型：

```go
type Runner struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   RunnerSpec   `json:"spec,omitempty"`
    Status RunnerStatus `json:"status,omitempty"`
}
```

### 4.1 RunnerSpec

```go
type RunnerSpec struct {
    InstanceID   string        `json:"instanceId,omitempty"`
    Type          string        `json:"type,omitempty"`
    Arch          string        `json:"arch,omitempty"`
    Unschedulable bool          `json:"unschedulable,omitempty"`
    Taints        []RunnerTaint `json:"taints,omitempty"`
}
```

| 字段 | 说明 |
|------|------|
| `instanceId` | Runner 安装实例持久化 UUID，用于识别同名对象是否属于同一安装实例 |
| `type` | 执行机类型：`ct` / `vm` / `hw` |
| `arch` | CPU 架构标识，必填但不限制枚举值 |
| `unschedulable` | 是否禁止调度新 Job |
| `taints` | 反亲和污点 |

`instanceId`、`type` 和 `arch` 由 Runner 自身声明；`instanceId` 创建后不可变，`unschedulable` 和 `taints` 虽然位于 RunnerSpec，但由 system 调用方管理，Runner 自注册和更新时不得修改。

调度标签统一写入 `metadata.labels`，不在 `spec` 中重复定义。例如：

系统保留的 Runner 标签及其写入权限见 [EulerMaker 标签约定](./labels.md)。

```yaml
apiVersion: ebs/v1
kind: Runner
metadata:
  name: runner-ct-aarch64-01
  labels:
    ebs.io/runner-type: ct
    ebs.io/runner-arch: aarch64
    ebs.io/zone: local
spec:
  instanceId: 5d65d05e-37b6-4e7b-bfcb-264930f4436b
  type: ct
  arch: aarch64
```

### 4.2 RunnerStatus

```go
type RunnerStatus struct {
    Phase       RunnerPhase        `json:"phase,omitempty"`
    Conditions  []metav1.Condition `json:"conditions,omitempty"`
    Capacity    map[string]string  `json:"capacity,omitempty"`
    Allocatable map[string]string  `json:"allocatable,omitempty"`
    Addresses   []RunnerAddress    `json:"addresses,omitempty"`
    Info        RunnerInfo         `json:"info,omitempty"`
    Heartbeat   metav1.Time        `json:"heartbeat,omitempty"`
}
```

| 字段 | 说明 |
|------|------|
| `phase` | 公共 `ebs/v1` API 定义的 `RunnerPhase`，稳定取值为 `Online` / `Offline` / `Evicted` |
| `conditions` | 详细状态条件 |
| `capacity` | Runner 上报的总资源容量，当前包含 `cpu`、`memory`、`ephemeral-storage` |
| `allocatable` | Runner 上报的可调度资源容量，当前 `cpu`、`memory` 与 `capacity` 一致，`ephemeral-storage` 为 runner 工作目录所在文件系统的可用空间 |
| `addresses` | 当前 runner agent 上报 Hostname，并在发现非 loopback 地址时上报 InternalIP |
| `info` | OS、内核、架构、agent 版本；当前 runner agent 暂不主动填充运行时版本 |
| `heartbeat` | 最后一次成功心跳时间 |

## 五、生命周期

Runner phase 只表达是否可以参与调度：

```text
Offline <-> Online
```

| Phase | 含义 |
|-------|------|
| `Online` | Runner 已完成本地初始化、心跳正常，可以接收新 Job |
| `Offline` | Runner 主动下线或心跳超时，不应继续接收新 Job |

Runner 的启动、初始化和忙闲状态不写入 `status.phase`。具体执行阶段由 Job 的 `status.phase` 和 `status.stage` 表达，Runner 当前负载由绑定到该 Runner 的 Job 计算。

新建 Runner 由 apiserver 默认置为 `Offline`。Runner agent 必须先完成配置加载、工作目录和执行器初始化，并启动 Job list/watch 所需组件；确认具备接收任务能力后，才通过带有当前心跳的状态写入切换为 `Online`。初始化失败时不得上报 `Online`。

### Runner 驱逐状态

`status.phase=Evicted` 表示暂停向该 Runner 分配新 Job。Scheduler 的 PhaseFilter 仅接受 Online；已绑定 Job 继续执行，不因驱逐自动中止或迁移。Runner Controller 不把 Evicted 改为 Offline。
Runner agent 上报心跳或主动下线前读取最新 Runner，保留 Evicted，并携带该对象的 resourceVersion 写入；发生冲突时不重放旧状态，下一次上报重新读取。解除驱逐需显式更新 phase 为 Online 或 Offline，后者由后续心跳恢复在线。此状态不代替 spec.unschedulable，恢复后仍需通过其他调度过滤。


## 六、心跳与状态上报

Runner 定期通过 `/status` 子资源上报状态，建议默认心跳间隔为 30 秒。心跳内容至少包括：

```yaml
status:
  phase: Online
  capacity:
    cpu: "32"
    memory: 65536Mi
    ephemeral-storage: 500Gi
  allocatable:
    cpu: "32"
    memory: 65536Mi
    ephemeral-storage: 450Gi
  addresses:
    - type: Hostname
      address: build-host-01
    - type: InternalIP
      address: 192.168.1.10
  info:
    os: openEuler
    kernelVersion: 5.10.0
    arch: aarch64
    runtimeVersion: docker://26.1.0
    agentVersion: v0.1.0
  heartbeat: "2026-06-09T10:00:00Z"
```

状态上报原则：

- `capacity` 表示执行机总容量，通常变化较少。
- `allocatable` 表示可调度容量；当前实现不按运行中 Job 扣减 `cpu` 或 `memory`，`ephemeral-storage` 反映工作目录所在文件系统的当前可用空间。
- Runner 的忙闲不由 `status.phase` 表达；具体 Job 与 Runner 的绑定关系以 Job 自身的 `status.runner` 为准。
- `heartbeat` 由 Runner 每次心跳刷新。
- 心跳超时后的 `Offline` 标记可以由 apiserver 外部控制器完成。

## 七、Job 执行流程

Runner 通过自身范围 API list-watch 已绑定到自己的 Job：

```text
list /apis/ebs/v1/runners/{runner}/jobs
  -> record list.metadata.resourceVersion
  -> process existing non-terminal Jobs
watch /apis/ebs/v1/runners/{runner}/jobs?watch=true&resourceVersion={rv}
  -> server guarantees event.object.status.runner == runnerName
  -> event.object.status.phase == Running
  -> execute
```

标准恢复流程如下：

1. Runner 启动或 resourceVersion 失效后先 list 自身已分配 Job，处理其中非终态对象并记录列表版本。
2. 使用该 resourceVersion 建立 watch，设置 `allowWatchBookmarks=true` 和有限的 `timeoutSeconds`，建议 300 秒。
3. `ADDED`、`MODIFIED`、`DELETED` 事件推进本地状态；`BOOKMARK` 只更新 resourceVersion，不触发执行。
4. watch 正常超时或网络中断时，从最后收到的 resourceVersion 重连；服务端返回 410 时重新执行完整 list-watch。
5. 429 按 `Retry-After` 等待，网络错误使用带随机抖动的指数退避；401 按 3.1 节执行一次强制刷新和重试，重试仍失败时按身份配置错误处理；403 直接按权限或身份配置错误处理。

gateway 和 apiserver必须共同校验路径中的 Runner 身份。apiserver负责可信过滤，不能依赖 Runner 客户端收到事件后自行隐藏其他 Runner 的 Job。

典型流程：

```text
1. Runner watch 到绑定给自己的 Job
2. 根据 metadata.namespace 确定所属 Project
3. 更新 Job.status.stage=Running
4. 准备执行环境并开始业务执行；环境准备和业务运行统一属于 `Running` stage
5. 创建日志上传状态，查询 Artifact Manager 的日志状态并确定恢复 sequence
6. 启动容器和实时日志采集；按 Job.spec.timeoutSeconds 限制业务执行，将 Job.spec.payload 作为 YAML 参数提供给任务入口
7. 容器结束后等待日志采集 EOF，并确认全部日志 chunk 已提交
8. 将 Job 保持为 phase=Running 并推进到 stage=PostRun，封账日志；业务执行失败或超时也必须尝试封账已有日志
9. 业务执行成功时收集并上传产物，完成 JobUploadManifest
10. 日志封账以及全部必需产物和清单完成后，更新 Job.status.phase=Succeeded 和 Artifact 摘要
11. 业务执行、必需日志封账、必需产物上传或清单封账失败时更新 phase=Failed，并保留明确的失败原因和已完成 Artifact 摘要
12. 清理执行环境和已确认的本地上传状态；Runner 保持 `Online`
```

Job status 使用当前数据模型：

```go
type JobStatus struct {
    Phase              string      `json:"phase,omitempty"`
    Stage              string      `json:"stage,omitempty"`
    Runner             string      `json:"runner,omitempty"`
    StartTime          metav1.Time `json:"startTime,omitempty"`
    EndTime            metav1.Time `json:"endTime,omitempty"`
    ResultRoot         string      `json:"resultRoot,omitempty"`
    Message            string      `json:"message,omitempty"`
}
```

`ResultRoot` 在本地执行期间可以表示 Runner 的结果目录；最终状态中应写为 `artifact://{jobUID}`，不能向其他组件公开 Runner 容器内的本地路径。Job 通过 `phase/stage` 表达执行进度，产物状态和文件数量由 Artifact Manager 的 Manifest 提供，不在 Job Status 中重复记录。Manifest 摘要只保存在 Artifact Manager 内部，不写入 Job Status。

Scheduler 负责选择 Runner，并更新 `Job.status.runner` 和 `Job.status.phase`。Runner 不主动抢占 Pending Job。

## 八、执行器与容器生命周期

Runner agent 的执行逻辑应按 kubelet 管理 Pod sandbox/container 的思路拆分：agent 负责 API watch、状态推进、超时控制和幂等清理；具体运行时由 executor 承接。单个 Runner 资源通过 `spec.type` 声明自身能力，Job 通过 `spec.runtime` 和 `spec.runtimeSpec` 声明本次任务需要的运行时配置。

| `spec.type` | 执行方式 | 说明 |
|-------------|----------|------|
| `ct` | 容器环境 | 常规包构建 |
| `vm` | 虚拟机环境 | 需要更强隔离的构建 |
| `hw` | 物理机环境 | 需要裸机能力的任务 |

`Job.spec.runtime` 默认值为 `ct`。Runner 应先判断自身 `spec.type` 是否能承接该 runtime，再把 `runtimeSpec` 交给对应 executor 解释。公共控制逻辑和执行器逻辑建议分离：

```text
runner agent
  ├── client: gateway API 访问、watch、status 更新
  ├── heartbeat: Runner.status 上报
  ├── job worker: Job 生命周期推进
  └── runtime manager
      ├── ct executor: 容器生命周期
      ├── vm executor: 虚拟机生命周期
      └── hw executor: 物理机/宿主机执行生命周期
```

如果一个进程需要管理多类执行能力，应注册多个 Runner 对象，或明确拆分为多个 runner 实例，避免单个 `Runner.spec.type` 同时表达多种能力。

### 8.1 CT 容器运行时

`ct` executor 负责启动实际业务容器，而不是在 runner agent 进程内直接执行构建命令。runner agent 容器自身只是控制面进程，业务容器应作为独立容器创建、启动、等待、停止和清理。

推荐的容器生命周期：

```text
1. 解析 Job.spec.runtimeSpec，得到镜像、网络、权限、工作目录、挂载等容器配置
2. 为 Job 创建本地 workDir 和 resultDir
3. 将 Job.spec.payload 写入 workDir/payload.yaml，作为任务执行所需的 YAML 参数文件
4. `scriptRefs` 非空时按顺序解析全部脚本，写入 workDir/scripts/{name}；全部成功后才继续
5. 拉取或确认业务镜像可用
6. 创建容器，挂载 workDir、resultDir，并写入 Job / Project / Runner 标识 label
7. 启动容器：非空数组以第一项作为入口；空数组沿用镜像入口。业务入口读取 /workspace/payload.yaml 并执行任务
8. 流式采集或落盘容器日志
9. 等待容器退出，按退出码决定 Job 成功或失败
10. 超时时先 stop，超过 grace period 后 kill
11. 收集 resultDir，清理容器和临时目录
```

`scriptRefs` 中的名称不得重复，Runner 也要拒绝不安全的文件名。每个引用包含创建 Job 时观察到的 name、UID、resourceVersion；进程内缓存仅在名称及 UID、resourceVersion 均匹配时复用，否则 GET 当前 Script。响应必须是同名、无 namespace、具备 UID 和 resourceVersion 的有效 Script，正文需满足 UTF-8、无 NUL 和绝对路径 shebang 校验。引用是观测信息，不锁定内容：响应的 UID 或 resourceVersion 与 Job 不同不构成错误，缓存以响应的实际元数据保存；Runner 重启后缓存失效。

脚本通过临时文件写入并原子替换为可执行的 `0555` 文件，在容器中位于 `/workspace/scripts/{name}`。非空数组时覆盖镜像 ENTRYPOINT 为 `/workspace/scripts/{scriptRefs[0].name}`，不传镜像默认 CMD 参数；此时 `runtimeSpec.command/args` 不能同时指定。Runner 拒绝遮蔽 `/workspace` 或脚本目录的额外挂载。空数组时不覆盖镜像入口，仍按 `runtimeSpec.command/args` 的原有方式执行。拉取和退避计入 Job 既有执行期限；取消时停止拉取，不启动容器。

`runtimeSpec` 对 `ct` runtime 可采用以下结构，字段由 ct executor 解释：

```yaml
runtime: ct
runtimeSpec:
  image: openeuler:22.03
  imagePullPolicy: IfNotPresent
  privileged: false
  networkMode: bridge
  workingDir: /workspace
  env:
    BUILD_ENV: production
  mounts:
    - name: work
      mountPath: /workspace
    - name: results
      mountPath: /results
```

首版可固定内置挂载：

| 宿主机目录 | 容器目录 | 说明 |
|------------|----------|------|
| `${rootDir}/work/{project}/{jobUID}` | `/workspace` | payload YAML、`scripts/{name}` 和执行工作目录 |
| `${rootDir}/results/{project}/{jobUID}` | `/results` | 构建产物暂存目录；最终结果通过 Artifact Manager 定位 |

容器 label 建议至少包含：

| Label | 值 |
|-------|----|
| `ebs.io/project` | Job `metadata.namespace` |
| `ebs.io/job` | Job `metadata.name` |
| `ebs.io/runner` | Runner `metadata.name` |

### 8.2 状态与幂等

runner agent 应把容器生命周期映射到 Job status，而不是在 `Runner.status` 中维护运行中 Job 列表：

| 容器阶段 | Job status 建议 |
|----------|-----------------|
| 容器创建前 | `phase=Running, stage=Running` |
| 容器运行中 | 保持 `phase=Running, stage=Running` |
| 容器退出、日志采集到 EOF | `phase=Running, stage=PostRun` |
| 日志、必需产物和清单封账完成 | `phase=Succeeded, stage=PostRun` |
| 容器退出码非 0 | 先尝试封账已有日志，再置 `phase=Failed` 并保留最后到达的 stage；已进入后处理时为 `PostRun` |
| 执行超时 | 终止容器并尝试封账已有日志，再置 `phase=Failed`，stage 保留为 `Running` 或 `PostRun` |

`PostRun` 是业务执行结束后的真实阶段，包括排空实时日志、日志封账、产物上传和 JobUploadManifest 封账。进入 `PostRun` 时 Job 仍为 `phase=Running`；所有必需后处理完成后才能推进最终 phase。

容器清理应按 Job identity 幂等执行：runner 重启后可以根据容器 label 找回未完成容器，决定继续等待、终止或标记失败；重复 stop/remove 不应导致 Job 状态回退。

### 8.3 实时日志上传

首版每个 Job 只上传 `stream=combined`，按照 Runner 从容器运行时读取到的顺序合并 stdout 和 stderr。日志上传器位于 executor 和 Artifact Manager 客户端之间，不由容器运行时直接访问网络：

```text
container runtime logs
        |
        v
durable local spool -> chunk assembler -> one in-flight request -> artifact-manager
        |
        +-> final size / SHA-256
```

#### 8.3.1 本地状态

Runner 必须先把日志写入 `${rootDir}/logs/{project}/{jobUID}/combined.log`，再从该文件生成上传 chunk。该文件既是网络故障时的有界溢写区，也是 Runner 重启后的恢复来源。不得只把未确认日志保存在进程内存中。

每次确定一个 chunk 边界时，Runner 还必须向 `${rootDir}/logs/{project}/{jobUID}/chunks.jsonl` 追加一条完整 JSON Lines 记录并同步落盘：

```go
type LocalLogChunk struct {
    Sequence    int64  `json:"sequence"`
    StartOffset int64  `json:"startOffset"`
    Size        int64  `json:"size"`
    SHA256      string `json:"sha256"`
}
```

索引从 sequence 0 连续，offset 也必须连续。正文和索引采用与服务端相同的提交原则：先同步正文，再追加并同步完整索引行，最后更新 checkpoint。启动时截断不完整索引尾行；正文长于最后一条索引结束位置的部分是尚未组块的已生产日志，可以继续聚合；正文短于索引边界表示本地损坏，不能继续上传。

同目录原子保存 `upload.json`，至少包含：

```go
type LogUploadCheckpoint struct {
    SchemaVersion   int       `json:"schemaVersion"`
    Project         string    `json:"project"`
    JobName         string    `json:"jobName"`
    JobUID          string    `json:"jobUID"`
    Stream          string    `json:"stream"`
    NextSequence    int64     `json:"nextSequence"`
    ConfirmedOffset int64     `json:"confirmedOffset"`
    ProducedBytes   int64     `json:"producedBytes"`
    State           string    `json:"state"` // Open, Draining, Completed, Failed
    UpdatedAt       time.Time `json:"updatedAt"`
}
```

`NextSequence` 是下一个待发送 sequence，`ConfirmedOffset` 是服务端已确认的连续字节边界，`ProducedBytes` 是本地日志文件已完成写入的字节数。checkpoint 使用临时文件、文件同步和原子 rename 更新；正文先落盘，再更新 `ProducedBytes`。只有服务端确认 chunk 后才能同时推进 `NextSequence` 和 `ConfirmedOffset`。最终日志的完整 SHA-256 可以在采集时增量计算，但重启后必须能够从本地正文重新计算，不能把不可恢复的哈希内存状态作为唯一来源。

#### 8.3.2 聚合和发送

- 解压后的目标 chunk 大小默认为 256 KiB；未达到大小时最多等待 500 ms，日志 EOF 时立即刷新剩余字节。空 chunk 不发送。
- 一个日志流同一时间最多存在一个未确认请求。请求失败或结果未知时，使用相同 sequence、相同字节和相同 SHA-256 重试。
- `X-Content-SHA256` 对原始日志字节计算；启用 gzip 时只改变传输编码，不改变 sequence、size、摘要或本地 checkpoint。
- 上传器使用有界内存队列；未确认正文始终可从本地 spool 重新读取。单 Job 的 spool 总大小达到 `--log-spool-limit` 时暂停读取容器日志，让容器运行时或其日志文件承担背压，不能静默丢弃、跳过或伪造日志。服务端确认不会立即释放本地正文，因为封账和崩溃恢复仍需要完整文件；只有 8.3.5 节的终态清理才能回收。
- 采集 goroutine、chunk 上传 goroutine和容器等待 goroutine由同一 Job context 管理。业务超时取消容器执行，但日志读取使用独立的排空期限；必须先等待运行时日志 EOF，再关闭 chunk 输入，不能在 `Wait` 返回时立即取消日志读取。

追加请求严格使用 Artifact Manager 10.2.1 节定义的 header。200 响应只有在 `acceptedSequence` 等于请求 sequence、`nextSequence` 等于请求 sequence+1 且 `committedBytes` 等于本 chunk 结束 offset 时才算确认；响应字段不一致按结果未知处理并查询状态。

#### 8.3.3 启动与断线恢复

开始或恢复 Job 日志前，Runner 调用 `/logs/status?jobUID={uid}&stream=combined`：

1. 服务端不存在日志流时，以 `nextSequence=0`、`committedBytes=0` 开始。
2. 服务端 `state=Open` 时，以服务端值为事实来源；校验本地 spool 至少包含 `committedBytes`，将 checkpoint 回退或前进到该边界，并从对应 sequence 继续。
3. 服务端 `state=Completed` 时，不再追加；记录其 `artifactID`。如果本地仍认为容器正在运行，这是不可继续写入的状态冲突，停止该 Job 并报告错误。
4. 服务端 `state=Failed/Expired` 时停止上传，Job 的 Artifact 状态标记为 Failed。

本地文件短于服务端 `committedBytes`、无法从本地索引确定 `nextSequence` 对应 offset，或未确认区间已经被删除时，日志不可恢复。Runner 必须保留已有数据、停止封账并将 Job 标记为失败，不能从新的 sequence 继续制造一份不连续日志。

本地 `chunks.jsonl` 必须保留至日志封账完成。Runner 使用服务端 `nextSequence` 查找相同 sequence 的本地记录，并验证此前所有本地记录的结束 offset 恰好等于服务端 `committedBytes`；不匹配时不能通过猜测固定 chunk 大小恢复，因为按时间刷新的 chunk 长度并不固定。

#### 8.3.4 错误处理

| 响应或错误 | Runner 行为 |
|------------|-------------|
| 网络中断、超时、503 | 保留当前 chunk，指数退避后以相同 sequence 和正文重试；结果未知时先查 status |
| 401 | 强制刷新一次 Token，以相同 chunk 重放一次；再次 401 进入低频配置错误重试 |
| 403 | 不刷新 Token，标记身份/权限配置错误 |
| 429 | 保留当前 chunk，遵循 `Retry-After` 并增加抖动 |
| 409 `SequenceGap` | 读取响应 `details.nextSequence`，再查询 status；按服务端连续边界恢复 |
| 409 `SequenceConflict` | 查询 status 和本地 chunk 索引；摘要无法证明一致时将日志标记为不可恢复 |
| 409 `LogAlreadyFinalized` | 查询 status；Completed 视为可能的重复完成，其他状态按冲突失败 |
| 409 `JobIdentityConflict` | 不重试，标记本地 Job 身份错误 |
| 413/422 `LogChunkMismatch` | 校验本地 chunk 大小和摘要；实现错误或正文改变时停止上传，不能跳过该 sequence |
| 507 | 保留本地日志并低频重试；达到本地 spool 上限后暂停日志读取 |

#### 8.3.5 排空和封账

容器无论成功、失败、超时或被取消，只要已经产生日志，Runner 都应在有限的 `--log-drain-timeout` 内执行：等待日志 EOF、刷新最后一个非空 chunk、确认所有 sequence、重新计算本地完整正文的 size 和 SHA-256，然后调用 `/logs/complete`。空日志使用 `lastSequence=-1`、`size=0` 和 SHA-256 空输入。

完成请求使用稳定的 `Idempotency-Key={jobUID}-log-complete`。网络错误或结果未知时先查询 status；已 Completed 且返回的最终 size、SHA-256 与本地一致时视为成功。重复完成必须得到同一个 Artifact。封账摘要不匹配时保留 spool 和 checkpoint 供诊断，不重新从 sequence 0 上传，也不删除服务端活动日志。

首版所有 `ct` Job 都把封账后的 `logs/container.log` 作为 JobUploadManifest 的必需文件。业务执行失败时，日志封账成功不会把 Job 改为 Succeeded；Runner 保留业务失败原因，并提交只包含日志的清单。业务执行成功但日志无法封账时，Job 必须 Failed，不能先发布 Succeeded 再后台补日志。

### 8.4 首版普通产物策略

首版使用以下固定策略，不由构建镜像、文件内容或扩展名之外的隐式规则改变：

- 只有容器退出码为 0 时才扫描和上传普通产物。业务失败、超时或取消时只封账日志，不发布 `${rootDir}/results/{project}/{jobUID}` 中的部分文件。
- 扫描根目录固定为 `${rootDir}/results/{project}/{jobUID}`。递归遍历其中的目录并上传全部普通文件，包括点文件；目录本身不生成 Artifact。
- `relativePath` 是文件相对扫描根目录的清理后 slash 路径。Runner 不增加、删除或重命名一级目录，也不根据扩展名自动移动文件；构建脚本需要自行把 RPM 写入希望发布的目录，例如 `/results/packages/`。
- 所有普通文件使用 `category=artifact`、`required=true`。`fileName` 使用相对路径的最后一个路径段。
- `.rpm` 文件使用 `contentType=application/x-rpm`；其他普通文件统一使用 `application/octet-stream`。首版不嗅探文件正文，也不依赖宿主机 MIME 数据库。
- 遇到符号链接、socket、device、FIFO、无法读取的文件、非法 UTF-8 路径、路径规范化失败或路径逃逸时，整个普通产物阶段失败；不得静默忽略后继续封账清单。
- 单文件上限默认 25 GiB，单 Job 普通产物总大小上限默认 100 GiB，文件数量上限默认 10000，并发上传数默认 4；任一限制必须不高于 Artifact Manager 对应部署限制，超过限制时在上传任何新文件前终止扫描并将 Job 标记为 Artifact 失败。
- 扫描结果必须先完整排序并校验，再开始上传。排序键是规范化后的 `relativePath`，保证重试、回执和 Manifest 顺序确定。
- 封账日志始终以 `relativePath=logs/container.log`、`category=log`、`required=true` 加入该 Job 的唯一 Manifest。普通产物不得使用 `logs/container.log`，发生路径冲突时 Job 失败。
- 构建成功但结果目录没有普通文件时，仍提交只包含日志的 Manifest。
- Manifest 完成请求结果未知时，Runner 查询相同 project 和 jobUID 的唯一清单；服务端已返回相同 Completed 清单时继续写回 Job，否则原样重试完成请求。Manifest 完成接口直接以 Job UID 保证幂等，不使用独立幂等键。
- 任何必需普通产物上传或 Manifest 封账最终失败都会令 Job `phase=Failed`，并在 `message` 中记录原因；已经成功上传的 Artifact 和本地回执保留用于幂等恢复，不提交缺少文件的降级清单。

### 8.5 上传回执与本地文件清理

Runner 不需要在 Artifact Manager 已可靠接管普通产物正文后继续保存本地副本。每个文件必须使用以下顺序处理：

1. 流式计算本地文件大小和 SHA-256，构造稳定的 `Idempotency-Key` 并上传。
2. 只在收到 200/201 且响应 Artifact 为 `state=Completed`、project、jobName、jobUID、relativePath、size 和 SHA-256 均与请求一致时，认为正文已被接管。
3. 将完整 Artifact 响应作为上传回执原子写入 `${rootDir}/uploads/{project}/{jobUID}/artifacts/{relativePath}.json`；实际文件名使用安全编码或路径摘要，不能直接信任 relativePath 拼接。
4. JobUploadManifest 从持久化回执生成。Manifest 完成且最终 Job Status 成功写回后即视为上传成功，原子写入 `notBefore=now` 的成功清理标记并立即执行清理；清理失败时保留标记供后台重试，不设置成功保留期。
5. 后台清理器只处理具有有效清理标记且当前时间不早于 `notBefore` 的 Job，并在重新确认目录仍属于同一 project/jobUID 后删除本地内容。

普通产物的幂等键固定为 `{jobUID}-artifact-{sha256(normalizedRelativePath)}`；同一路径重试必须复用该键，路径或元数据变化属于不同请求并应作为冲突处理。回执至少保存 Artifact Manager 返回的 Artifact ID、归属字段、relativePath、size、SHA-256、CompletedAt 和所用幂等键，保证重启后能验证并重建 Manifest 条目。

上传返回网络错误、超时、非 2xx、响应字段不匹配或结果未知时，在重试和状态确认期间不得删除本地文件。Runner 使用相同幂等键重试；如果重试返回原 Completed Artifact，则按上述顺序持久化回执。重试最终失败后，必须先将 Job 成功写为 `phase=Failed` 并记录失败原因，再写入失败清理标记；失败现场从该状态写回时间起保留 `--artifact-failed-retention`，默认 24 小时。最终状态写回失败或结果仍可能恢复时不得启动保留期。到期删除意味着放弃本地重试能力，服务端可能已经接管但响应未知的 Artifact 不由 Runner 猜测或删除。清理本地文件失败不改变 Job 或服务端 Artifact 状态，记录告警并由后台清理器重试。

该顺序允许在任意点崩溃后恢复：

| 本地现场 | 恢复动作 |
|----------|----------|
| 正文存在、无回执 | 使用稳定幂等键重新上传或确认结果 |
| 正文存在、Completed 回执存在 | 校验回执与本地文件元数据，不重复上传；Manifest 和最终状态成功后立即清理 |
| 正文缺失、Completed 回执存在 | 使用回执继续构造 Manifest |
| 正文和回执都缺失、Manifest 未完成 | 标记本地结果不可恢复，Job 不能进入 Succeeded |
| Manifest 已完成但本地回执残留 | 对照 Manifest 和最终 Job Status；成功终态立即清理，失败终态恢复失败清理标记 |

实时日志的 `combined.log`、`chunks.jsonl` 和 `upload.json` 同时承担追加恢复和最终摘要校验，不能在单个 chunk 确认后删除。日志完成接口返回匹配的 Completed Artifact 后先持久化日志完成回执；随后等待 Manifest Completed 和最终 Job Status 成功写回，成功后与普通产物一起立即清理。日志封账或上传最终失败时使用失败保留期，不在错误路径立即删除。

`${rootDir}/work/{project}/{jobUID}` 中的 payload 和临时执行文件在容器退出且不再需要恢复执行后清理，不受 Artifact 保留期影响。上传成功后立即统一删除 `${rootDir}/results/{project}/{jobUID}`、`${rootDir}/logs/{project}/{jobUID}` 和 `${rootDir}/uploads/{project}/{jobUID}`。其他终态默认使用 `--artifact-failed-retention=24h`，到期后删除上述目录及失败清理标记；不得在保留期内按单文件提前删除。所有 Job 本地目录都使用 UID 而不是可复用的 Job 名。最终清理必须限定在当前 Job 的规范化目录内，禁止跟随符号链接或跨越 `rootDir`。

### 8.6 Job 主动中止

中止 API 和终态第一写入获胜规则以 [apiserver Job 主动中止](ebs-apiserver.md#job-主动中止) 为准。Runner 不调用 `/abort`，只消费服务端已经持久化的终态并执行停止/清理。

#### 感知与任务注册

- 继续使用 `/apis/ebs/v1/runners/{runner}/jobs` 的 List/Watch，服务端仅按 `status.runner` 过滤，不能增加只看 Running 的过滤条件。收到 Aborted 时必须处理，不能沿用“非 Running 一律忽略”的逻辑。
- 活动任务按 Job UID 记录 namespace/name、执行取消函数、产物上传取消函数、结束信号及已观察到的终态。注册、记录终态和移除条目由同一互斥锁保护；取消函数幂等，取消和等待退出在锁外执行，不在锁内调用 API 或容器运行时。
- 启动路径先注册可取消的活动条目，再 GET 当前 Job，核验 UID、runner 和 phase；仅同 UID、绑定本 Runner 且 Running 时继续。观察到终态即记录并取消。旧 Running 事件不得覆盖该活动条目已记录的终态或再次启动同一 UID。
- 启动前状态回写也使用 GET 返回的 resourceVersion；失败先确认最新状态，不得在未知结果下直接执行。执行器在创建/启动外部进程前检查 context，启动期间到达的取消同样进入 Stop/Kill 清理。
- GET 与真实进程启动之间不可能形成跨系统事务；接受中止后短暂启动再停止的窗口，不承诺“API 中止成功后绝不执行任何指令”。

#### 停止与状态回写

- Aborted 事件取消执行和普通产物上传（包含 PostRun），CT 执行器依次 Stop、等待既有宽限期、必要时 Kill；shell 执行器需终止整个任务进程组，不能只终止启动 shell 而遗留子进程。
- 运行时停止/清理使用独立、有上限的 context；Stop/Kill/Wait 都必须有退出边界，不能因后台 Wait 永不返回而永久阻塞。停止失败记录结构化告警，并保留可重试的本地任务/容器记录；实际进程未退出前不释放本地执行占用。
- 日志按 8.3 的独立 `--log-drain-timeout` 尽力排空封账；不启动新的普通产物扫描/上传。已经提交的远端上传无法保证撤销，已完成 Manifest 不删除；RpmRepo Controller 按 Job phase 排除 Aborted 输入，不能只凭 Manifest Completed 纳入新批次。对已经冻结/发布的批次不承诺回滚。
- Runner 的 PostRun 与最终状态写入统一携带 resourceVersion；Conflict 或 Unknown 后 GET 确认，发现任一终态即停止全部业务 status 写入，不能将 Aborted 覆盖为 Failed/Succeeded，也不改 endTime/message/产物字段。终态之后的日志清理只更新本地状态或 Artifact Manager，不更新 Job status。
- 超时引发的执行失败仍按原执行失败流程写 Failed；用户中止以服务端 Aborted 为准，取消异常本身不决定 Job 必须为 Failed。最终写入与 `/abort` 并发时接受服务端先持久化的终态。

#### 断线与重启恢复

- Watch 重连先完整 List 并记录 resourceVersion，再 Watch；List 中终态 Job 也用于取消本地活动任务，而不只是筛选可启动任务。完整列表未出现的本地活动项逐一 GET：404 或 UID 不同则停止旧执行，读取失败保留并重试，不把一次 List 缺项当成确定删除。
- Runner 重启先扫描带 Runner、Project、Job name 和 Job UID 标签的受管容器，将这些标签作为持久化执行身份，与服务端核对；不得仅依赖内存执行表。已 Aborted/其它终态或已删除的旧任务停止并清理，不恢复执行或普通产物上传；不能误杀非本 Runner 管理的容器。当前 CT 执行器不重接旧进程：确认移除受管旧容器后才启动 List/Watch，由最新 Running/PostRun 状态决定重新执行或继续上传；清理无法确认时启动失败，不接收新任务。
- Runner 失联期间无法立即感知中止。Scheduler 可能在 Job 逻辑终态后释放账面占用，但 Runner 必须在实际退出前保留本地占用，并继续执行本地容量准入，避免旧进程尚未退出就超额运行新 Job。

#### 测试

覆盖 Pending 未执行即中止、Running/启动中/PostRun 中止、取消早于启动确认、重复事件、旧 Running 事件、Watch 重连、Runner 重启遗留容器、进程组清理、Stop/Kill 超时、日志排空超时、上传已完成，以及中止与最终状态写入并发。

## 九、故障处理

| 场景 | 处理方式 |
|------|----------|
| gateway 不可达或 watch 中断 | watch 按第七章的 list-watch 恢复流程处理；心跳使用带抖动的指数退避重试 |
| artifact-manager 暂时不可达 | 继续写入本地日志 spool，在上限内重试；不得阻塞心跳和 Job watch |
| 心跳超时 | 控制器将 Runner 标记为 `Offline`，scheduler 不再选择该 Runner |
| Runner 重启 | 重新注册 Runner，恢复心跳，根据 Job、容器 label、本地 spool/checkpoint 和服务端日志 status 恢复或明确失败 |
| 同名 Runner 注册 | `instanceId` 相同才允许恢复；不同则终止注册并报告冲突 |
| 本地 instance ID 丢失 | 不接管已有同名 Runner；恢复 ID 文件或由管理员删除旧对象后重新注册 |
| Script 读取网络失败、超时、429 或 5xx | 在 Job 剩余期限内指数退避重试；初始 1 秒、上限 30 秒并加抖动，429 遵循较长的 Retry-After；取消时停止 |
| Script 不存在、无读取权限、响应或内容非法 | 不启动业务容器，按 Job 执行失败流程记录原因；不回退到镜像入口或其他脚本 |
| `scriptRefs` 名称重复或路径不安全 | 拒绝执行；不能以后一项覆盖前一项或写出 Job 工作目录 |
| Job 执行失败 | 先排空并封账已有日志，再更新 `Job.status.phase=Failed` 和 `message` |
| Job 超时 | 终止执行进程并尝试封账已有日志，再更新 Job 为 Failed 或 Aborted |
| 本地日志不可恢复 | 保留诊断文件，Job 写入 Failed 并记录原因，不得进入 Succeeded |
| 日志已封账但 Job 状态更新失败 | 保留日志完成回执和 spool，按 resourceVersion 重新读取并幂等更新 Job，不重复创建日志 Artifact |
| 上传成功但立即清理失败 | 记录告警并异步重试，不改变 Job/Artifact 成功状态 |
| 失败清理标记尚未到期 | 保留 results、日志和上传回执，不提前回收 |
| 清理标记已到期但本地删除失败 | 保留清理标记并异步重试删除，不重复上传或改变 Job/Artifact 状态 |
| 状态更新冲突 | 使用 apiserver 返回的 resourceVersion 重新读取并重试 |

状态更新应保持幂等：重复上报同一阶段、重复清理、重复标记失败不应破坏对象状态。

## 十、部署配置

Runner 作为独立组件容器化部署，至少需要以下配置：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--gateway` | `https://ebs-gateway:8443` | gateway 地址 |
| `--artifact-manager` | `http://artifact-manager:8081` | Artifact Manager 地址，必填 |
| `--machine-credential-file` | 无 | 包含 MachineAccount client ID 和 client secret 的 JSON 文件路径，必填 |
| `--name` | hostname | Runner 资源名称 |
| `--type` | `ct` | Runner 类型：`ct` / `vm` / `hw` |
| `--root-dir` | `/var/lib/ebs-runner` | Runner 工作目录和结果目录的根路径 |
| `--heartbeat-interval` | `30s` | 心跳上报周期 |
| `--gateway-ca` | 无 | gateway 服务端证书的 CA 文件 |
| `--insecure-skip-verify` | `false` | 跳过 gateway TLS 校验，仅用于测试环境 |
| `--artifact-manager-ca` | 无 | Artifact Manager 服务端证书的 CA 文件 |
| `--artifact-manager-insecure-skip-verify` | `false` | 跳过 Artifact Manager TLS 校验，仅用于测试环境 |
| `--log-chunk-size` | `256KiB` | 解压后单个日志 chunk 的目标和最大大小，不得超过服务端限制 |
| `--log-flush-interval` | `500ms` | 未达到目标大小时的最长聚合时间 |
| `--log-spool-limit` | 按部署配置 | 单 Job 完整本地日志 spool 的空间上限，至少应与服务端 `--max-log-size` 协调 |
| `--log-drain-timeout` | `30s` | 容器结束后等待日志 EOF、上传排空和封账的期限 |
| `--log-retry-max-backoff` | `30s` | 实时日志临时错误的最大退避间隔 |
| `--artifact-max-file-size` | `25GiB` | 单个普通产物大小上限，不能超过 Artifact Manager 配置 |
| `--artifact-max-job-size` | `100GiB` | 单 Job 普通产物总大小上限，不能超过 Artifact Manager 配置 |
| `--artifact-max-files` | `10000` | 单 Job 普通产物文件数量上限 |
| `--artifact-upload-concurrency` | `4` | 单 Job 普通产物并发上传数 |
| `--artifact-upload-timeout` | `2h` | 单 Job 普通产物上传、Manifest 查询及封账的总期限，超时后进入失败终态 |
| `--artifact-retry-max-backoff` | `30s` | 普通产物和 Manifest 临时错误的最大退避间隔 |
| `--artifact-failed-retention` | `24h` | 上传失败、超时、结果未知或封账失败并成功写回失败终态后，本地现场的保留时间 |

Runner 启动时根据运行环境获取架构：`GOARCH=amd64` 映射为 `x86_64`，`GOARCH=arm64` 映射为 `aarch64`。其他架构不受支持，Runner拒绝启动。检测结果写入`Runner.spec.arch`和`ebs.io/runner-arch` label，不提供启动参数覆盖。

示例：

```yaml
services:
  ebs-runner-ct-1:
    image: ebs-runner:latest
    command:
      - --gateway=https://ebs-gateway:8443
      - --artifact-manager=http://artifact-manager:8081
      - --machine-credential-file=/run/secrets/runner-machine-credential
      - --name=runner-ct-aarch64-01
      - --type=ct
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - runner-cache:/var/lib/ebs-runner
    secrets:
      - runner-machine-credential

secrets:
  runner-machine-credential:
    file: ./runner-machine-credential.json
```

## 十一、安全边界

- 长期 MachineAccount 凭据用于换取最长 24 小时的 Runner token；每个 Runner 或受控站点应使用独立账号以便独立审计和吊销，不应在镜像中内置全局共享 secret。
- `runner-instance-id` 必须与 `--root-dir` 一起持久化且只允许 Runner 运行用户读写；UUID 可出现在对象和诊断日志中，但它不是秘密或认证因子，任何授权判断仍以 token 为准。
- CT 类型 Runner 如需挂载 socket，应将运行环境视为高权限执行环境，并通过隔离网络、只读挂载、临时工作目录清理等方式降低风险。
- 日志 spool 和上传 checkpoint 可能包含敏感构建输出，只允许 Runner 运行用户访问；不得把日志正文、Bearer Token 或 MachineAccount secret 写入结构化运行日志。
- 普通产物回执不得包含 Token 或文件内容。Manifest 和最终 Job Status 成功后立即删除 results、日志 spool 和上传回执；失败、超时、结果未知或封账失败现场按 24 小时失败保留期保存，不能在错误路径立即删除。
- 生产环境中 Runner 到 Artifact Manager 必须使用 TLS；使用 HTTP 的 Compose 地址只适用于受控测试网络。

## 十二、测试设计

实时日志开发至少覆盖以下测试：

| 模块 | 场景 |
|------|------|
| Runner identity | 首次启动原子生成规范 UUID v4；重启复用；同名同 ID 恢复；同名不同 ID 终止；POST 409 后 GET 并按 ID 分类；更新不能修改或清空 ID；ID 文件丢失时不接管已有对象 |
| Job identity | 从 `metadata.uid` 取得 jobUID；缺少 namespace/name/UID 时拒绝执行；同名不同 UID 使用独立目录和日志流 |
| Script 执行 | 空 `scriptRefs` 使用镜像入口；单脚本和多脚本均写入 `/workspace/scripts/{name}`，第一项作为入口；重复名称、路径逃逸、遮蔽挂载及与 `runtimeSpec.command/args` 冲突时拒绝 |
| Script 缓存与失败 | 同 name/UID/resourceVersion 命中缓存，UID 或 resourceVersion 变化时重新 GET；响应元数据变化、无效正文、404、401/403、429/5xx、超时和取消分别按契约处理，不执行未完整拉取的脚本集合 |
| Chunk | 256 KiB 聚合、500 ms 刷新、EOF 刷新、空日志不发送 chunk、SHA-256 针对原始字节、可选 gzip |
| 顺序与确认 | 单请求在途、200 响应字段校验、相同 sequence/正文重试不重复推进、checkpoint 只在确认后更新 |
| 本地持久化 | 正文先于索引、JSON Lines 残缺尾行恢复、正文未组块尾部继续聚合、正文短于索引时拒绝恢复 |
| 断线恢复 | status 为 Open/Completed/Failed；服务端领先、客户端 checkpoint 落后、结果未知、Runner 进程在各提交点崩溃 |
| 协议错误 | SequenceGap、SequenceConflict、LogAlreadyFinalized、JobIdentityConflict、LogChunkMismatch |
| 认证和退避 | 401 单次刷新重放、403 不刷新、429 Retry-After、503/网络错误指数退避；日志故障不阻塞心跳和 watch |
| 背压 | 慢服务端、内存队列满、本地 spool 达上限时不丢日志；Job 取消后仍能在 drain timeout 内排空 |
| 封账 | 正常日志、空日志、业务失败日志、超时日志、重复完成、完成结果未知、摘要不匹配及相同 Artifact 响应 |
| 普通产物扫描 | 空目录、嵌套目录、点文件、稳定排序、RPM MIME、普通 MIME、路径冲突、非法 UTF-8、不可读文件、符号链接和特殊文件拒绝、文件数及大小限制 |
| 普通产物上传 | multipart 流式上传、响应字段校验、并发上限、稳定幂等键、整文件重试、部分成功后恢复、业务失败时不上传普通产物 |
| Manifest | 日志必需项、只含日志的清单、普通产物全部 required、稳定排序、单次成功封账、完成结果未知查询和内容冲突处理 |
| 本地清理 | 成功后立即删除、立即删除失败重试、失败清理标记和 `notBefore`、Runner 重启恢复、失败保留期内不删除、24 小时到期统一删除、结果未知未终态不计时 |
| Job 状态 | PostRun 期间保持 Running；必需日志/产物完成后才 Succeeded；上传失败时 phase 为 Failed 且 message 记录原因 |
| 并发安全 | Token 刷新、心跳、watch、多个 Job 日志上传并发运行时通过 race detector |

端到端测试应启动 Gateway、Artifact Manager、Runner 和一个持续输出 stdout/stderr 并向 `/results/packages/` 写入文件的测试容器，验证日志在容器运行期间可通过 SSE 读取，容器退出后生成唯一的 `logs/container.log` Artifact 和普通 Artifact，Job 的唯一 Manifest 包含全部必需项，下载正文与本地源文件一致，最终 Job 的 Artifact 状态和数量直接使用 Manifest 完成响应。
