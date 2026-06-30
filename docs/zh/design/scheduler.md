# Scheduler 设计

## 一、定位

Scheduler 负责监听 `ebs/v1` Job 和 Runner，把 `Pending` 状态的 Job 绑定到合适的 Runner 上执行。

Scheduler 是系统组件，直接访问 `ebs-apiserver` 的全局系统 API：

```text
scheduler -> ebs-apiserver -> etcd / Elasticsearch
```

Scheduler 不直接访问 etcd 或 Elasticsearch，不直接执行 Job，也不负责维护 Runner 本地执行状态。Runner 是否忙、当前可用资源和心跳信息由 runner 进程通过 `Runner.status` 上报；具体 Job 与 Runner 的绑定关系以 Job 自身的 `status.runner` 为准。

## 二、设计目标

首版 Scheduler 目标是实现一个简单、可运行、可扩展的调度闭环：

| 目标 | 说明 |
|------|------|
| watch 驱动 | 监听全局 Job 和 Runner 变化，及时触发调度 |
| Project 无关 | 通过全局 Job API 跨 Project 调度，不逐个 Project 建立 watch |
| 轻量过滤 | 基于 Runner 类型、架构、状态、污点、标签和可调度资源过滤 |
| 简单打分 | 基于空闲优先、可调度资源和标签匹配进行排序 |
| 状态绑定 | 只更新 Job status，写入实际 Runner 名称并将 Job 置为 Running |
| 可扩展 | 后续可逐步引入更多 Filter/Score 插件 |

## 三、API 交互

### 3.1 监听 Job

Scheduler 使用全局 Job API 监听所有 Project 下的 Job：

```text
GET /apis/ebs/v1/jobs?watch=true
```

首版不依赖自定义 field selector。Scheduler 收到 Job 事件后，在本地判断：

```text
job.status.phase == "Pending"
```

只有 Pending Job 进入调度队列。

### 3.2 读取 Runner

Scheduler 使用 Runner API 获取候选执行机：

```text
GET /apis/ebs/v1/runners
GET /apis/ebs/v1/runners?watch=true
```

Runner 是集群级资源，候选信息来自：

- `metadata.name`
- `metadata.labels`
- `spec.type`
- `spec.arch`
- `spec.unschedulable`
- `spec.taints`
- `status.phase`
- `status.allocatable`
- `status.heartbeat`

### 3.3 绑定 Job

Job 是 Project 级资源。Scheduler 从 Job 对象的 `metadata.namespace` 获取所属 Project，然后通过 Project API 更新 Job status：

```text
PUT /apis/ebs/v1/projects/{project}/jobs/{name}/status
```

绑定时更新：

```yaml
status:
  phase: Running
  stage: Pending
  runner: runner-dc-aarch64-01
  startTime: "2026-06-09T10:00:00Z"
```

Scheduler 不更新 Runner status。绑定关系只写入 Job status；如后续需要统计每个 Runner 的运行负载，Scheduler 应基于 watch 到的 Job 按 `status.runner` 聚合。

## 四、输入数据模型

### 4.1 JobSpec

首版调度只使用当前 `JobSpec` 已定义字段：

```go
type JobSpec struct {
    Runner       string               `json:"runner,omitempty"`
    Arch         string               `json:"arch,omitempty"`
    Runtime      int64                `json:"runtime,omitempty"`
    DockerImage  string               `json:"dockerImage,omitempty"`
    RepoUrl      string               `json:"repoUrl,omitempty"`
    Package      string               `json:"package,omitempty"`
    ImageConfig  runtime.RawExtension `json:"imageConfig,omitempty"`
    Env          map[string]string    `json:"env,omitempty"`
    Commands     []string             `json:"commands,omitempty"`
}
```

调度含义：

| 字段 | 调度用途 |
|------|----------|
| `runner` | 期望的执行机规格。首版可按 Runner `spec.type` 或 `metadata.labels` 匹配 |
| `arch` | 必须匹配 Runner `spec.arch` |
| `runtime` | 暂不参与资源计算，可用于后续超时或长任务隔离策略 |
| `dockerImage` | 暂不参与首版调度，可用于后续镜像缓存亲和 |
| `package` | 暂不参与首版调度，可用于后续构建缓存亲和 |

当前数据模型没有 `cpuMinimum`、`memoryMinimum`、`nodeSelector`、`tolerations` 等字段，首版 Scheduler 不依赖这些字段。

### 4.2 RunnerSpec

```go
type RunnerSpec struct {
    Type          string        `json:"type,omitempty"`
    Arch          string        `json:"arch,omitempty"`
    Hostname      string        `json:"hostname,omitempty"`
    Unschedulable bool          `json:"unschedulable,omitempty"`
    Taints        []RunnerTaint `json:"taints,omitempty"`
}
```

调度标签统一使用 `Runner.metadata.labels`，不使用 `spec.labels`。

### 4.3 RunnerStatus

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

首版调度使用：

| 字段 | 用途 |
|------|------|
| `phase` | 过滤 Offline、Booting 等不可执行状态 |
| `allocatable` | 判断是否仍有可调度容量 |
| `heartbeat` | 过滤心跳超时的 Runner |

## 五、调度流程

```text
watch Job
  -> Pending Job 入队
  -> 读取 Runner 快照
  -> Filter
  -> Score
  -> Pick
  -> Bind Job.status
```

### 5.1 入队

以下 Job 可以进入调度队列：

- `status.phase == "Pending"`
- `status.runner` 为空
- 未处于本地 backoff 时间窗口

如果 watch 中断，Scheduler 重新 list 全局 Job，并重新筛选 Pending Job。

### 5.2 Filter

首版过滤规则：

| 规则 | 说明 |
|------|------|
| PhaseFilter | Runner `status.phase` 必须是 `Idle` |
| HeartbeatFilter | Runner 心跳不能超时 |
| UnschedulableFilter | Runner `spec.unschedulable` 不能为 true |
| ArchFilter | Job `spec.arch` 为空或等于 Runner `spec.arch` |
| TypeFilter | Job `spec.runner` 为空，或能匹配 Runner `spec.type` / `metadata.labels` |
| CapacityFilter | 当前模型不设置 Job 数量资源维度；首版按单 Runner 单 Job 处理，由 PhaseFilter 过滤忙碌 Runner |
| TaintFilter | 首版没有 Job tolerations 字段，因此存在 `NoSchedule` taint 的 Runner 默认不可调度 |

`status.phase == "Booting"` 的 Runner 可以被 watch 缓存，但不进入候选集，等 Runner 上报 `Idle` 或 `Running` 后再参与调度。

### 5.3 Score

首版打分保持简单：

| 打分项 | 说明 |
|--------|------|
| 标签匹配 | Job `spec.runner` 能精确匹配 Runner label 时加分 |
| 稳定心跳 | 心跳越新越优先 |

示例权重：

```text
score = labelScore * 70 + heartbeatScore * 30
```

首版不引入复杂插件系统。实现上可以先用固定 Filter/Score 函数，后续再拆成插件。

### 5.4 Pick

选择总分最高的 Runner。若分数相同，按 Runner 名称排序，保证结果稳定。

### 5.5 Bind

绑定时只更新 Job status：

```yaml
status:
  phase: Running
  stage: Pending
  runner: runner-dc-aarch64-01
  startTime: "2026-06-09T10:00:00Z"
```

绑定更新必须处理资源版本冲突：

- 如果 Job 已经不是 Pending，放弃本次绑定。
- 如果 Job 已经有 `status.runner`，放弃本次绑定。
- 如果更新时出现 resourceVersion 冲突，重新读取 Job 后再判断是否需要重试。

## 六、Runner 匹配规则

`Job.spec.runner` 是当前数据模型中表达期望执行机规格的字段。首版可采用保守匹配规则：

1. 如果为空，不限制 Runner 类型。
2. 如果等于 `dc` / `vm` / `hw`，匹配 `Runner.spec.type`。
3. 如果不等于上述类型，则按 Runner label 匹配：

```yaml
metadata:
  labels:
    ebs.io/runner: dc-64g
    ebs.io/runner-type: dc
```

这种规则允许当前字段先承载简单执行机规格，后续再扩展更明确的调度字段。

## 七、调度队列

首版可以使用内存队列：

```text
pendingQueue
backoffQueue
```

队列规则：

- 新增或更新为 Pending 的 Job 进入 `pendingQueue`。
- 调度失败但可能恢复的 Job 进入 `backoffQueue`。
- backoff 到期后重新入队。
- Job 被成功绑定、删除、进入 Completed/Failed/Aborted 后，从队列移除。

失败类型：

| 类型 | 处理 |
|------|------|
| 无 Runner | backoff 后重试 |
| Runner 都不可调度 | backoff 后重试 |
| Job 字段非法 | 记录日志，必要时更新 Job status message |
| API 冲突 | 重新读取 Job 后重试 |

## 八、并发与幂等

首版可以单副本部署，简化锁和 leader election。

如果未来多副本部署，需要引入 leader election 或基于 Job status 的乐观并发控制。无论单副本还是多副本，Bind 必须幂等：

- 只绑定 `phase=Pending` 且 `status.runner` 为空的 Job。
- 绑定成功后，重复处理同一 watch 事件不会再次修改已 Running 的 Job。
- 绑定失败不修改 Runner status。

## 九、故障处理

| 场景 | 处理 |
|------|------|
| Job watch 中断 | 使用 resourceVersion 恢复 watch；失败时重新 list |
| Runner watch 中断 | 重新 list Runner，刷新本地缓存 |
| Runner 心跳超时 | 本地过滤该 Runner；Offline 标记由外部控制器处理 |
| Bind 冲突 | 重新读取 Job，若仍 Pending 则重试 |
| 无候选 Runner | Job 进入 backoff，等待 Runner 或资源状态变化 |
| apiserver 不可达 | 指数退避重连 |

## 十、首版实现结构

建议目录结构：

```text
components/ebs-scheduler/
├── Dockerfile
├── README.md
├── main.py
├── ebs_client.py
├── scheduler.py
├── queue.py
├── filters.py
├── scoring.py
└── types.py
```

职责划分：

| 文件 | 职责 |
|------|------|
| `main.py` | 参数解析、启动 scheduler |
| `ebs_client.py` | 封装 ebs-apiserver API、watch、status update |
| `scheduler.py` | 主循环、watch 处理、调度流程 |
| `queue.py` | pending/backoff 队列 |
| `filters.py` | 首版固定过滤规则 |
| `scoring.py` | 首版固定打分规则 |
| `types.py` | 调度上下文、结果类型 |

## 十一、后续扩展

后续可以在不改变首版主流程的前提下扩展：

- 更明确的 Job 资源请求字段，例如 CPU、内存、磁盘、GPU。
- Job tolerations / nodeSelector。
- 镜像缓存、ccache 缓存亲和。
- VM / HW 类型专用 PreBind 检查。
- 多副本 scheduler leader election。
- 插件化 Filter / Score 框架。
