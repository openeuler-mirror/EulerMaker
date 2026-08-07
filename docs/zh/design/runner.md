# Runner 设计

## 一、定位

Runner 是 EulerMaker 的 Job 执行机，负责注册自身能力、上报心跳、监听已调度给自己的 Job、执行构建任务，并将执行状态回写到 `ebs/v1` API。

Runner 不直接访问 etcd 或 Elasticsearch，也不直接依赖内部存储路径。所有资源读写都通过统一 API 完成：

```text
runner -> ebs-gateway -> ebs-apiserver -> etcd / Elasticsearch
```

其中：

- `ebs-gateway` 负责认证、鉴权、审计和请求转发。
- `ebs-apiserver` 负责资源语义、校验、默认值、`/status` 子资源、list/watch 和存储访问。
- etcd 和 Elasticsearch 是组合主存储，Runner 不直接访问。

## 二、核心职责

| 职责 | 说明 |
|------|------|
| 注册 Runner | 创建或受限更新与 token 身份一致的集群级 `Runner` 对象，声明执行机类型、架构、主机名和能力标签 |
| 上报状态 | 定期更新 `Runner.status`，包括 phase、资源容量、可调度资源、地址、系统信息和心跳时间 |
| 监听 Job | 通过自身 Runner 范围的 Job list-watch 获取已分配任务，服务端只返回 `status.runner` 等于自身名称的 Job |
| 执行 Job | 根据 Job spec 准备执行环境、提供 payload 参数、运行任务、收集产物 |
| 回写结果 | 通过 Project API 更新 Job status，推进 Job phase/stage/resultRoot/message |

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

MachineAccount 必须已启用，`runner-001` 必须满足 DNS1123 label。Gateway 成功后返回最长 24 小时的 `ebs:runner` JWT，token 的 `sub`、`runner` claim 和本地 Runner 名称完全相同。Runner 仅在内存中保存短期 token，不将 client secret、Basic header 或完整 token 写入日志和状态对象。

Runner 在首次注册前获取 token，并在到期前重新交换。建议在剩余有效期小于 10 分钟或总有效期的 20%（取较小值）时刷新，并加入随机抖动避免大量实例同时请求。业务请求收到 401 时只触发一次立即刷新和重试；交换接口返回 401 表示长期凭据无效，400 表示 Runner 名称或请求格式错误，两者均进入低频退避并报告配置错误，不能无限快速重试。429 遵循 `Retry-After`，网络错误和 503 使用带抖动的指数退避。Runner 不使用 refresh token。

Runner 从 `--machine-credential-file` 指定的 JSON 文件同时读取 client ID 和 client secret：

```json
{
  "clientID": "runner-site-a",
  "clientSecret": "base64url-encoded-random-secret"
}
```

文件只允许上述两个必填字符串字段，拒绝未知字段、空值、重复 JSON key 和文件尾部附加内容。文件权限应限制为 `0600` 或等效的只读 Secret 挂载权限。Runner 不接受通过其他启动参数直接覆盖文件中的 client ID 或 client secret，避免凭据两部分配置不一致。

### 3.2 Runner API

```text
GET    /apis/ebs/v1/runners/{name}
POST   /apis/ebs/v1/runners
PUT    /apis/ebs/v1/runners/{name}
PATCH  /apis/ebs/v1/runners/{name}
PUT    /apis/ebs/v1/runners/{name}/status
```

Runner 是集群级资源，`metadata.name` 在集群内唯一。Runner token 的 `sub`、`runner` claim 以及请求对象或路径中的 Runner 名称必须完全相同；Runner 不能 list/watch Runner 集合、访问其他 Runner 或删除任何 Runner。

Runner 采用受限自注册模型：对象不存在时通过 collection `POST` 创建；对象已存在时先 GET 最新对象，再携带 `metadata.resourceVersion` 通过 PUT/PATCH 更新。创建冲突和并发更新冲突返回 409，Runner 重新读取后按需重试，gateway 不自动重放写请求。

Runner 可以声明和更新的普通对象字段为：

```text
metadata.labels["ebs.io/runner-type"]
metadata.labels["ebs.io/runner-arch"]
metadata.labels["ebs.io/runner-capability.*"]
spec.type
spec.arch
spec.hostname
```

`ebs.io/runner-type`、`ebs.io/runner-arch` 必须分别与 `spec.type`、`spec.arch` 一致。Runner 创建对象时 `status` 必须为空，且不能提供 annotations、finalizers、ownerReferences 或其他服务端 metadata。`spec.unschedulable`、`spec.taints`、`ebs.io/zone`、信任级别和安全域等调度管理字段由 system 调用方维护；Runner 更新完整对象时必须保留这些已有字段，不能通过 PUT 字段缺失、Merge Patch 的 `null` 或 JSON Patch 删除父级字段绕过保护。

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
    Type          string        `json:"type,omitempty"`
    Arch          string        `json:"arch,omitempty"`
    Hostname      string        `json:"hostname,omitempty"`
    Unschedulable bool          `json:"unschedulable,omitempty"`
    Taints        []RunnerTaint `json:"taints,omitempty"`
}
```

| 字段 | 说明 |
|------|------|
| `type` | 执行机类型：`dc` / `vm` / `hw` |
| `arch` | CPU 架构：`aarch64` / `x86_64` |
| `hostname` | 执行机宿主机名 |
| `unschedulable` | 是否禁止调度新 Job |
| `taints` | 反亲和污点 |

`type`、`arch` 和 `hostname` 由 Runner 自身声明；`unschedulable` 和 `taints` 虽然位于 RunnerSpec，但由 system 调用方管理，Runner 自注册和更新时不得修改。

调度标签统一写入 `metadata.labels`，不在 `spec` 中重复定义。例如：

```yaml
apiVersion: ebs/v1
kind: Runner
metadata:
  name: runner-dc-aarch64-01
  labels:
    ebs.io/runner-type: dc
    ebs.io/runner-arch: aarch64
    ebs.io/zone: local
spec:
  type: dc
  arch: aarch64
  hostname: build-host-01
```

### 4.2 RunnerStatus

```go
type RunnerStatus struct {
    Phase       string             `json:"phase,omitempty"`
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
| `phase` | `Registering` / `Booting` / `Running` / `Idle` / `Offline` |
| `conditions` | 详细状态条件 |
| `capacity` | Runner 上报的总资源容量，当前包含 `cpu`、`memory`、`ephemeral-storage` |
| `allocatable` | Runner 上报的可调度资源容量，当前 `cpu`、`memory` 与 `capacity` 一致，`ephemeral-storage` 为 runner 工作目录所在文件系统的可用空间 |
| `addresses` | 当前 runner agent 上报 Hostname，并在发现非 loopback 地址时上报 InternalIP |
| `info` | OS、内核、架构、agent 版本；当前 runner agent 暂不主动填充运行时版本 |
| `heartbeat` | 最后一次成功心跳时间 |

## 五、生命周期

Runner phase 使用数据模型中定义的状态：

```text
Registering -> Booting -> Idle -> Running -> Idle
                                  |
                                  v
                               Offline
```

| Phase | 含义 |
|-------|------|
| `Registering` | Runner 已启动，正在创建或更新 Runner 对象 |
| `Booting` | Runner 对象已就绪，正在初始化执行环境和 watch 循环 |
| `Idle` | Runner 可调度，当前无运行中 Job |
| `Running` | Runner 正在执行一个或多个 Job |
| `Offline` | Runner 主动下线或心跳超时，不应继续接收新 Job |

`Executing` 不作为 `Runner.status.phase`。如果实现中需要更细的执行阶段，应放在 Runner 进程内部状态或 Job 的 `status.stage` 中。

## 六、心跳与状态上报

Runner 定期通过 `/status` 子资源上报状态，建议默认心跳间隔为 30 秒。心跳内容至少包括：

```yaml
status:
  phase: Idle
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
- Runner 是否忙由 `status.phase` 表达；具体 Job 与 Runner 的绑定关系以 Job 自身的 `status.runner` 为准。
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
5. 429 按 `Retry-After` 等待，网络错误使用带随机抖动的指数退避；401/403 视为 token 或身份配置问题，不能无限快速重试。

gateway 和 apiserver必须共同校验路径中的 Runner 身份。apiserver负责可信过滤，不能依赖 Runner 客户端收到事件后自行隐藏其他 Runner 的 Job。

典型流程：

```text
1. Runner watch 到绑定给自己的 Job
2. 根据 metadata.namespace 确定所属 Project
3. 更新 Runner.status.phase=Running
4. 更新 Job.status.stage=Running
5. 准备执行环境
6. 按 Job.spec.timeoutSeconds 限制执行时间，将 Job.spec.payload 作为 YAML 参数提供给任务执行入口
7. 收集产物，得到 resultRoot
8. 成功时更新 Job.status.phase=Completed、stage=PostRun、resultRoot
9. 失败时更新 Job.status.phase=Failed、message
10. 清理环境，更新 Runner.status 为 Idle 或继续 Running
```

Job status 使用当前数据模型：

```go
type JobStatus struct {
    Phase      string      `json:"phase,omitempty"`
    Stage      string      `json:"stage,omitempty"`
    Runner     string      `json:"runner,omitempty"`
    StartTime  metav1.Time `json:"startTime,omitempty"`
    EndTime    metav1.Time `json:"endTime,omitempty"`
    ResultRoot string      `json:"resultRoot,omitempty"`
    Message    string      `json:"message,omitempty"`
}
```

Scheduler 负责选择 Runner，并更新 `Job.status.runner` 和 `Job.status.phase`。Runner 不主动抢占 Pending Job。

## 八、执行器与容器生命周期

Runner agent 的执行逻辑应按 kubelet 管理 Pod sandbox/container 的思路拆分：agent 负责 API watch、状态推进、超时控制和幂等清理；具体运行时由 executor 承接。单个 Runner 资源通过 `spec.type` 声明自身能力，Job 通过 `spec.runtime` 和 `spec.runtimeSpec` 声明本次任务需要的运行时配置。

| `spec.type` | 执行方式 | 说明 |
|-------------|----------|------|
| `dc` | Docker / 容器环境 | 常规包构建 |
| `vm` | 虚拟机环境 | 需要更强隔离的构建 |
| `hw` | 物理机环境 | 需要裸机能力的任务 |

`Job.spec.runtime` 默认值为 `dc`。Runner 应先判断自身 `spec.type` 是否能承接该 runtime，再把 `runtimeSpec` 交给对应 executor 解释。公共控制逻辑和执行器逻辑建议分离：

```text
runner agent
  ├── client: gateway API 访问、watch、status 更新
  ├── heartbeat: Runner.status 上报
  ├── job worker: Job 生命周期推进
  └── runtime manager
      ├── dc executor: Docker 容器生命周期
      ├── vm executor: 虚拟机生命周期
      └── hw executor: 物理机/宿主机执行生命周期
```

如果一个进程需要管理多类执行能力，应注册多个 Runner 对象，或明确拆分为多个 runner 实例，避免单个 `Runner.spec.type` 同时表达多种能力。

### 8.1 DC 容器运行时

`dc` executor 负责启动实际业务容器，而不是在 runner agent 进程内直接执行构建命令。runner agent 容器自身只是控制面进程，业务容器应作为独立容器创建、启动、等待、停止和清理。

推荐的容器生命周期：

```text
1. 解析 Job.spec.runtimeSpec，得到镜像、网络、权限、工作目录、挂载等容器配置
2. 为 Job 创建本地 workDir 和 resultDir
3. 将 Job.spec.payload 写入 workDir/payload.yaml，作为任务执行所需的 YAML 参数文件
4. 拉取或确认业务镜像可用
5. 创建容器，挂载 workDir、resultDir，并写入 Job / Project / Runner 标识 label
6. 启动容器，由业务入口读取 /workspace/payload.yaml 并执行任务
7. 流式采集或落盘容器日志
8. 等待容器退出，按退出码决定 Job 成功或失败
9. 超时时先 stop，超过 grace period 后 kill
10. 收集 resultDir，清理容器和临时目录
```

`runtimeSpec` 对 `dc` runtime 可采用以下结构，字段由 dc executor 解释：

```yaml
runtime: dc
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
| `${rootDir}/work/{project}/{job}` | `/workspace` | payload YAML 参数文件和执行工作目录 |
| `${rootDir}/results/{project}/{job}` | `/results` | 构建产物目录，对应 `Job.status.resultRoot` |

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
| 容器退出码为 0，产物收集完成 | `phase=Completed, stage=PostRun, resultRoot=...` |
| 容器退出码非 0 | `phase=Failed, stage=Failed, message=...` |
| 执行超时 | `phase=Failed` 或后续扩展为 `Aborted`，`message` 记录 timeout |

`PostRun` 表示业务执行已经结束并完成结果收集后的最终阶段；当前不表示一个独立异步执行阶段。如果后续需要上传产物、清理缓存等耗时后处理，可以把 `PostRun` 扩展为真实阶段，并在完成后再推进最终 phase。

容器清理应按 Job identity 幂等执行：runner 重启后可以根据容器 label 找回未完成容器，决定继续等待、终止或标记失败；重复 stop/remove 不应导致 Job 状态回退。

## 九、调度协作

Scheduler 使用全局 Job API 和 Runner API：

```text
scheduler -> watch /apis/ebs/v1/jobs
scheduler -> list/watch /apis/ebs/v1/runners
scheduler -> 过滤 Pending Job
scheduler -> 选择可用 Runner
scheduler -> update Job.status.runner / phase
```

调度过滤建议基于：

- `Runner.status.phase`：只选择 `Idle` 或仍有可调度容量的 `Running` Runner。
- `Runner.spec.unschedulable`：为 true 时不调度新 Job。
- `Runner.spec.taints`：过滤不能容忍污点的 Job。
- `Runner.metadata.labels`：匹配类型、架构、机房、能力标签。
- `Runner.status.allocatable`：判断资源是否足够。
- `Job.spec.nodeSelector`、`Job.spec.resources.requests`、`Job.spec.tolerations`：匹配调度约束、资源请求和架构，架构通过 `ebs.io/runner-arch` 标签选择。

Runner 只执行已绑定给自己的 Job，不负责调度决策。

## 十、故障处理

| 场景 | 处理方式 |
|------|----------|
| gateway 不可达 | watch 和心跳失败后指数退避重连 |
| watch 中断 | 从最后收到的 resourceVersion 恢复；410 时重新 list自身已分配 Job后建立 watch，429 遵循 `Retry-After` |
| 心跳超时 | 控制器将 Runner 标记为 `Offline`，scheduler 不再选择该 Runner |
| Runner 重启 | 重新注册 Runner，恢复心跳，根据现有 Job 状态决定是否清理或继续 |
| Job 执行失败 | 更新 `Job.status.phase=Failed` 和 `message`，并清理本地环境 |
| Job 超时 | 终止执行进程，更新 Job 为 Failed 或 Aborted |
| 状态更新冲突 | 使用 apiserver 返回的 resourceVersion 重新读取并重试 |

状态更新应保持幂等：重复上报同一阶段、重复清理、重复标记失败不应破坏对象状态。

## 十一、部署配置

Runner 作为独立组件容器化部署，至少需要以下配置：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--gateway` | `https://ebs-gateway:8443` | gateway 地址 |
| `--machine-credential-file` | 无 | 包含 MachineAccount client ID 和 client secret 的 JSON 文件路径，必填 |
| `--name` | hostname | Runner 资源名称 |
| `--type` | `dc` | Runner 类型：`dc` / `vm` / `hw` |
| `--root-dir` | `/var/lib/ebs-runner` | Runner 工作目录和结果目录的根路径 |
| `--heartbeat-interval` | `30s` | 心跳上报周期 |
| `--gateway-ca` | 无 | gateway 服务端证书的 CA 文件 |
| `--insecure-skip-verify` | `false` | 跳过 gateway TLS 校验，仅用于测试环境 |

Runner 启动时根据运行环境获取架构：`GOARCH=amd64` 映射为 `x86_64`，`GOARCH=arm64` 映射为 `aarch64`。其他架构不受支持，Runner拒绝启动。检测结果写入`Runner.spec.arch`和`ebs.io/runner-arch` label，不提供启动参数覆盖。

示例：

```yaml
services:
  ebs-runner-dc-1:
    image: ebs-runner:latest
    command:
      - --gateway=https://ebs-gateway:8443
      - --machine-credential-file=/run/secrets/runner-machine-credential
      - --name=runner-dc-aarch64-01
      - --type=dc
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - runner-cache:/var/lib/ebs-runner
    secrets:
      - runner-machine-credential

secrets:
  runner-machine-credential:
    file: ./runner-machine-credential.json
```

## 十二、安全边界

- Runner 认证统一经过 `ebs-gateway`。
- 长期 MachineAccount 凭据用于换取最长 24 小时的 Runner token；每个 Runner 或受控站点应使用独立账号以便独立审计和吊销，不应在镜像中内置全局共享 secret。
- Runner token 权限限制为创建自身 Runner、读取自身 Runner、受限更新自身普通对象字段、更新自身 Runner status、list/watch 自身已分配 Job，以及读取和更新已绑定 Job status。
- Runner 身份必须满足 JWT `sub`、`runner` claim、Runner `metadata.name` 和请求路径名称完全一致；不允许 list/watch Runner 集合或访问其他 Runner。
- Runner 创建和普通对象更新只能修改 3.2 节白名单字段；`unschedulable`、taints、zone、信任级别、安全域及服务端 metadata 由 system 维护。
- Runner 禁止对所有资源执行 DELETE。
- `/runners/{runner}/jobs` 的过滤由 apiserver依据可信身份强制执行，客户端 `fieldSelector` 不可信。
- Runner 不应拥有直接访问 etcd、Elasticsearch 的权限。
- DC 类型 Runner 如需挂载 Docker socket，应将运行环境视为高权限执行环境，并通过隔离网络、只读挂载、临时工作目录清理等方式降低风险。
