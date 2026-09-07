# Job Controller 设计

## 一、定位与范围

Job Controller 是 `controller-manager` 中负责 Job 控制面收敛和历史对象回收的控制器。Scheduler 完成 Job 与 Runner 的绑定，Runner 负责实际执行、超时、日志及产物封账；Job Controller 处理已经绑定但失去执行主体的悬挂 Job，并删除超过保留期的终态 Job。

首版职责：

- 监听 Job 和 Runner 变化；
- 识别绑定到已删除或明确 `Offline` Runner 的非终态 Job；
- 经过离线宽限期并再次确认后，将悬挂 Job 标记为 `Failed`；
- 删除 `Completed`、`Failed`、`Aborted` 状态下超过 30 天保留期的历史 Job；
- 保证状态更新幂等，并在并发更新时以 apiserver 中的最新对象为准；
- 暴露必要的结构化日志和指标。

首版不负责：

- 不选择 Runner，不写入或清除 `status.runner`；
- 不创建、删除或重启 Runner 上的执行环境；
- 不重复实现 `spec.timeoutSeconds`。业务执行超时仍由 Runner 负责停止任务、封账日志和产物并写入终态；
- 不自动把失联 Job 改回 `Pending`，也不自动重新调度。控制面无法证明旧 Runner 已停止执行，自动重放可能产生两个并发构建；
- 不维护 Build、BuildInfo 等上层资源状态，这些状态由对应业务 Controller 根据 Job 终态聚合；
- 不级联删除日志、Artifact 或上层业务对象，这些数据由各自组件的保留策略管理；
- 不负责把心跳超时的 Runner 标记为 `Offline`。该能力属于 Runner 健康管理；Job Controller 只消费持久化的 Runner 状态。

## 二、依赖与组件边界

Job Controller 运行在现有 `controller-manager` 框架内，建议实现目录为：

```text
components/controller-manager/pkg/controllers/job/
  controller.go       # 初始化、事件注册和 Sync
  reconciler.go       # 纯业务状态判断
  status.go           # Job status 更新与冲突处理
  gc.go               # 终态 Job 保留期判断与安全删除
  index.go            # runner -> Job Key 反向索引
  metrics.go
```

依赖关系如下：

```text
Job Watch Source ---------> Job Controller queue ------> Job DELETE
                                  |
Runner Watch Source ------> runner -> jobs index
                                  |
                                  v
                         Job status subresource
```

Job 和 Runner 均支持 watch，应使用 `WatchSourceFactory` 创建并共享 Source。事件处理器必须在 `Manager.Run` 之前注册；控制器启动后不得再注册 handler。Manager 完成全部 Source 的首次同步后才启动 worker，因此 `Sync` 可以依赖两个本地缓存已经完成初始 List。

Initializer 保存 Factory 返回的两个 `source.CachedSource`：

```go
type Controller struct {
    jobs    source.CachedSource
    runners source.CachedSource
    clock   clock.Clock
    // queue、反向索引和宽限期状态省略
}
```

`clock.Clock` 使用 `k8s.io/utils/clock`。生产 initializer 注入 `clock.RealClock{}`，单元测试注入 `clocktesting.FakeClock`；业务代码不得直接调用 `time.Now()`、`time.Since()` 或 `time.Until()`。

Job 使用 `{namespace}/{name}` 调用 `jobs.GetByKey`；Runner 是集群级资源，使用 `{runnerName}` 调用 `runners.GetByKey`。`CachedSource` 返回的对象已经是深拷贝，Controller 仍需进行类型断言并在类型不符时返回内部错误。业务代码不得通过具体类型断言取得 informer 或 Indexer。

Manager 保证 worker 在 cache sync 后启动；若 `GetByKey` 仍返回 `source.ErrCacheNotSynced`，按临时错误限速重试，不能退化为直接 API List。写入前的权威 GET 与缓存读取用途不同，仍按第六节保留。

Job Controller 在包内声明最小客户端接口，共享 API Client 通过适配器实现它：

```go
type Client interface {
    GetJob(ctx context.Context, namespace, name string) (*ebsv1.Job, error)
    GetRunner(ctx context.Context, name string) (*ebsv1.Runner, error)
    UpdateJobStatus(ctx context.Context, job *ebsv1.Job) (*ebsv1.Job, error)
    DeleteJob(
        ctx context.Context,
        namespace, name string,
        preconditions client.DeletePreconditions,
    ) error
}
```

`UpdateJobStatus` 必须验证成功响应非 nil、UID 与请求对象一致、resourceVersion 非空；Job Controller 写 Runner 丢失终态时，还必须验证响应的 phase 为 `Failed`、runner 未变化、stage 未变化。任一成功响应异常都包装为 `WriteUnknown`。

`DeleteJob` 必须同时接收 UID 和 resourceVersion，不能暴露无条件删除。Get 返回 Kubernetes 标准读取错误；两个写方法返回的错误必须能通过下列方式读取 Outcome，并保留 `apierrors.IsConflict`、`IsNotFound`、`IsUnauthorized`、`IsForbidden` 和 `IsTooManyRequests` 判断能力：

```go
var writeErr *client.WriteError
if errors.As(err, &writeErr) {
    outcome := writeErr.Outcome
}
```

## 三、字段所有权

多个组件会更新 Job status，必须以字段所有权限制写冲突。更新 `/status` 时仍需携带完整最新 status，并只修改本组件拥有的字段。

| 组件 | 拥有的 Job status 字段或迁移 |
|------|-----------------------------|
| Scheduler | 绑定时写 `phase=Running`、`runner=<name>` |
| Runner | 写 `stage`、`startTime`、`endTime`、`resultRoot`、`message`、`restartCount` 以及正常执行产生的终态 |
| Job Controller | 仅在 Runner 丢失时写 `phase=Failed`、`endTime`、`message`，保留当前 `stage` |

Job Controller 不修改 `runner`、`startTime`、`resultRoot` 和 `restartCount`。若数据模型包含 Artifact 摘要字段，也必须原样保留。

终态为：

```text
Completed | Failed | Aborted
```

Job Controller 自身把 `Completed`、`Failed`、`Aborted` 视为终态：观察到这些 phase 后不再更新其 status，只根据 `endTime` 安排历史对象清理。首版不要求 apiserver 强制校验终态不可回退，该约束暂由 Scheduler、Runner 和各 Controller 共同遵守。

## 四、事件与本地索引

### 4.1 队列键

工作队列键统一为：

```text
{namespace}/{name}
```

UID 不编码在字符串键中。`Sync` 每次从缓存取得最新对象，并在写入前通过 GET 再次确认 UID 和 `resourceVersion`，防止同名 Job 删除重建后旧周期更新新对象。

### 4.2 Job 事件

- `Add`：所有 Job 都入队；非终态且 `status.runner` 非空时还要更新反向索引。Running Job 执行 Runner 丢失判定，终态 Job 安排历史清理，其他 Job 会快速结束。
- `Update`：先按旧对象删除索引，再按新对象增加索引。UID、phase、runner、`endTime` 或 Runner 丢失判定相关状态变化时入队；仅有无关字段变化不重复入队。Informer resync 事件仍需入队，作为历史清理计划和离线观察的恢复兜底。
- `Delete`：删除反向索引和本地宽限期记录，不需要写 API。

事件处理器只做类型检查、索引维护和入队，不调用外部 API，不执行状态机。

### 4.3 Runner 事件

控制器维护并发安全的 `runnerName -> set(JobKey)` 反向索引。Runner 的 `phase`、UID 或删除状态发生变化时，把索引中绑定到该 Runner 的 Job 全部入队。

Runner 的普通心跳更新不应导致所有 Job 反复入队。只有 Runner 进入或离开 `Offline`、对象 UID 改变、对象删除时才触发关联 Job。

删除事件必须兼容 informer tombstone。索引集合只保存字符串键，不保存可变 Job 指针。

### 4.4 周期性重同步

Watch resync 作为丢事件后的兜底，但不能替代定时器。处于 Runner 丢失宽限期或历史保留期内的 Job 返回 `ReconcileResult{RequeueAfter: remaining}`，由 BaseController 在相应截止时间重新入队；进程重启后，初始 List 事件会重建两类计划。

## 五、状态判定

### 5.1 可处理 Job

Job 同时满足下列条件才进入 Runner 丢失判定：

```text
status.phase == Running
status.runner != ""
metadata.deletionTimestamp == nil
```

`Pending` Job 由 Scheduler 管理；终态 Job 不处理；正在删除的 Job 不产生新的状态写入。

### 5.2 Runner 可用性

| Runner 情况 | Job Controller 行为 |
|-------------|---------------------|
| 对象存在且 phase 不是 `Offline` | Job 保持不变，清除本地宽限期记录 |
| 对象存在且 phase 为 `Offline` | 进入或继续离线宽限期 |
| Runner 对象不存在 | 进入或继续离线宽限期 |
| Runner 删除后以相同名称重建且 phase 不是 `Offline` | 视为同一个逻辑 Runner 已恢复，Job 保持不变 |

首版明确接受 Runner UID 无法参与绑定判断的限制。`Job.status.runner` 只保存 Runner 名称，Scheduler、Runner 和 Job Controller 都把该名称视为稳定的逻辑 Runner 身份；Job Controller 不记录或比较 Runner UID。Runner 删除后，只要相同名称的 Runner 在宽限期内重新出现且不为 `Offline`，就视为原逻辑 Runner 恢复，即使新对象 UID 已变化。

因此首版无法区分“同一 Runner 重新注册”和“另一执行实例复用了相同名称”，也不能保证后一种情况下旧 Job 不被新实例接管。这与 Kubernetes 普通 Pod 只通过 `spec.nodeName` 绑定 Node 的名称模型一致，是有意接受的一致性边界，而不是 Controller 的临时缓存缺陷。

部署和运维必须保证 Runner 名称具有稳定逻辑含义：旧 Running Job 尚未进入终态时，不得把该名称分配给无权恢复旧任务的另一执行实例。需要替换执行实例且不希望其接管旧 Job 时，应使用新的 Runner 名称。若未来需要实例级强隔离，再为 Job status 增加 `runnerUID`，并由 Scheduler 绑定、Runner 鉴权和 Job Controller 状态判断共同校验；首版不实现该能力。

### 5.3 离线宽限期

默认 `--job-runner-lost-grace-period=5m`。首次观察到 Runner 不存在或 `Offline` 时记录：

```go
type LostRunnerObservation struct {
    JobUID   types.UID
    Runner   string
    FirstSeen time.Time
}
```

记录仅存在于进程内，并由控制器锁保护：

- Job UID 或 runner 变化时丢弃旧记录；
- Runner 恢复可用时立即丢弃；
- 宽限期未到时返回 `ReconcileResult{RequeueAfter: remaining}`；
- 宽限期到期后执行第六节的权威确认。

该宽限期用于吸收 Runner 正常重启和 watch 暂时不一致，不用于判断心跳超时。Runner 健康管理必须先把确认失联的 Runner 持久化为 `Offline`。

Runner 对象不存在时，controller-manager 重启会丢失 `FirstSeen`，宽限期会重新开始一次。这会延迟失败收敛，但不会错误终止仍在运行的 Job，首版接受这一取舍。若将来要求跨重启保留精确截止时间，应增加 Job Condition，而不是使用进程内时间猜测。

### 5.4 时间来源

Job Controller 的所有主动时间判断都来自构造时注入的 `clock.Clock`：

- `LostRunnerObservation.FirstSeen` 使用 `clock.Now()`；
- Runner 丢失后写入的 `status.endTime` 使用同一次调谐取得的 `now`；
- 宽限期截止时间、历史清理截止时间及 `RequeueAfter` 都使用该 Clock 计算；
- 已有 Job 的 `status.endTime` 是 apiserver 中的业务数据，Controller 只读取，不用本地时间覆盖。

每次 `Sync` 开始时只调用一次 `clock.Now()` 并保存为 `now`，本周期的所有比较和新时间戳复用该值。写入 API 所消耗的时间不重新计入本周期；下一次调谐会取得新的 `now`。写入 `metav1.Time` 时使用 `metav1.NewTime(now.UTC())`，持久化时间统一为 UTC。

截止时间计算使用绝对时间：

```text
lostDeadline = firstSeen + gracePeriod
deleteAfter  = endTime + historyRetention
remaining    = deadline - now
```

`remaining <= 0` 表示已经到期，不调用 `AddAfter`。系统时间向前跳可能提前触发，向后跳可能延迟触发；首版采用墙上时间并接受该行为，因为 GC 要与持久化的 `endTime` 比较。测试必须通过 FakeClock 推进时间，不使用真实 sleep。

## 六、Reconcile 流程

### 6.1 Runner 丢失收敛

单个队列键的 `Sync(ctx, key)` 按以下顺序执行：

1. 从 Job cache 读取对象。不存在时清理索引和宽限期记录并成功返回。
2. 深拷贝对象，检查是否为第五节定义的可处理 Job；否则清理宽限期记录并成功返回。
3. 从 Runner cache 读取 `job.status.runner`。
4. Runner 可用时清理宽限期记录并成功返回。
5. Runner 不存在或为 `Offline` 时建立或读取相同 Job UID、Runner 的宽限期记录。
6. 宽限期未到时返回 `ReconcileResult{RequeueAfter: remaining}, nil`；定时重入不是错误，不增加 rate-limit 次数。
7. 宽限期到期后，通过 apiserver GET 读取最新 Job，不能直接以 cache 对象写 status。
8. 再次检查 name、namespace、UID、deletionTimestamp、phase 和 runner。对象已经终态、删除重建、不再 Running 或 runner 变化时停止处理。
9. 再通过最新 Runner cache 检查一次；若 Runner 已恢复则停止处理。缓存仍显示 Runner 不存在或为 `Offline` 时，必须调用 `GetRunner` 从 apiserver 读取权威对象，不能仅凭缓存把 Job 置为 `Failed`：
   - 返回同名 Runner 且 phase 不是 `Offline`：视为已经恢复，清除宽限期记录并成功结束；
   - 返回同名 Runner 且 phase 为 `Offline`：确认 Runner 不可用，继续状态更新；
   - 返回 404：确认 Runner 不存在，继续状态更新；
   - 返回 401/403：返回 PermanentError，记录鉴权失败；
   - 429、5xx、超时或其他无法取得权威结果的错误：返回临时错误，由 BaseController 限速重试，不更新 Job。
10. 基于最新 Job 深拷贝，保留不拥有的 status 字段，仅修改：

```yaml
status:
  phase: Failed
  stage: <保持当前执行阶段>
  endTime: <本次 Sync 开始时由 clock.Now() 取得的 UTC 时间>
  message: "runner <name> is unavailable after 5m grace period"
```

11. 使用带 `resourceVersion` 的 `/status` 更新。
12. 成功后清理宽限期记录，并记录结构化日志和指标；Conflict 立即重新入队读取最新对象，其他临时错误交给框架 rate-limited 重试。

`message` 是面向用户的稳定摘要，不拼接底层 HTTP 错误，也不覆盖已经终态 Job 的 Runner 诊断信息。日志中可以记录更详细原因。

### 6.2 历史 Job 清理

历史清理是同一个 Job Controller 的第二条调和分支，共用 Job Watch Source、队列、worker 和单键串行保证，不注册单独的顶层 Controller。Job 进入终态后，`Sync` 按以下流程处理：

1. 仅接受 `Completed`、`Failed`、`Aborted`；其他 phase 不参与 GC。
2. `metadata.deletionTimestamp` 非空时不重复删除。
3. 必须存在非零 `status.endTime`。`endTime` 缺失的终态 Job 记录告警和指标，但不得使用 `creationTimestamp` 或 `resourceVersion` 时间代替，以免误删异常对象。
4. 计算固定时长截止时间：

```text
deleteAfter = status.endTime + jobHistoryRetention
```

默认 `jobHistoryRetention=720h`，即 30×24 小时，不表达自然月。

5. 截止时间未到时，返回 `ReconcileResult{RequeueAfter: deleteAfter-now}, nil`，由 BaseController 安排重新检查，不增加 rate-limit 次数。
6. 截止时间到达后，GET apiserver 中的最新 Job，重新校验 namespace、name、UID、phase、`endTime`、deletionTimestamp 和截止时间。
7. 使用下列删除前置条件调用 Job DELETE：

```yaml
preconditions:
  uid: <latest-job-uid>
  resourceVersion: <latest-job-resource-version>
```

8. 删除成功或返回 404 时结束。UID/resourceVersion 前置条件冲突时重新入队，不放宽条件重试。
9. 删除请求超时或结果未知时先 GET 相同名称：对象不存在则视为删除成功；对象仍存在则基于其最新 UID 和状态重新调和，不盲目重复旧删除请求。

GC 只删除 Job API 对象。Artifact Manager 中的日志、产物和清单可能具有不同的合规及存储保留要求，Job Controller 不调用 Artifact Manager，也不把 Job 删除成功解释为 Artifact 可以同步删除。

终态 Job 的 `endTime` 由产生终态的 Runner 或 Job Controller 写入。首版不依赖 apiserver 保证终态或 `endTime` 不可修改；相关字段发生变化时，Job Update 事件会重新入队并按最新值重建或取消清理计划。真正删除前必须 GET 最新对象并重新校验全部 GC 条件，同时使用最新 UID/resourceVersion 作为删除前置条件，因此字段变化与删除并发时不会基于旧版本误删对象。

`--job-history-gc-enabled=false` 时，终态分支直接结束且不设置延迟条目；Runner 丢失处理不受影响。

## 七、并发与一致性

### 7.1 单键串行与共享状态

BaseController 保证同一队列键在一个时刻只由一个 worker 调和；不同 Job 可以并发。反向索引和宽限期 map 仍可能被 Source handler 与 worker 并发访问，必须由各自的 `RWMutex` 保护，锁内不得执行 API 请求或队列阻塞操作。

### 7.2 乐观并发

Job、Runner 和上层 Controller 都可能更新对象。Job Controller 必须：

- 写入前 GET 最新 Job；
- 携带最新 `resourceVersion` 更新 `/status`；
- 在副本上只改变自己拥有的三个字段；
- 将 Conflict 视为最新状态优先，重新入队而不是覆盖；
- 404 视为对象已删除并成功结束；
- 401/403 视为本次调谐的永久错误，记录错误日志和鉴权失败指标；同一对象不做限速重试，等待认证配置修复后的 resync、新事件或进程重启；单次鉴权失败不改变 Controller 健康状态；
- 网络超时或结果未知时不得立即构造第二个不同状态请求，应先 GET 确认。若 Job 已是预期 Failed 则视为成功，否则以最新对象重新判断。

### 7.3 与 Runner 的竞态

宽限期到期时 Runner 可能同时完成 Job。resourceVersion 冲突只保证基于同一个旧版本的并发更新至多一个成功：

- Runner 先写终态：Job Controller 冲突后读取终态并停止；
- Job Controller 先写 Failed：遵守本设计的 Runner 在冲突后读取最新 Job，发现终态后停止写入，不得把 Failed 改回 Running 或 Completed；
- Runner 恢复但其状态事件尚未到达本地缓存：宽限期到期后的强制 Runner GET 读取权威状态并阻止错误的 Failed 更新。

首版明确接受强制 Runner GET 与 Job status 更新之间仍存在不可消除的竞态窗口：Controller 可能在 GET 确认 Runner 不存在或为 `Offline` 后、提交 Job status 前遇到 Runner 恢复，并最终成功把 Job 写为 `Failed`。该 GET 只用于排除 Watch 缓存陈旧造成的误判，不构成 Runner 状态与 Job status 更新之间的事务前置条件，也不承诺写入瞬间 Runner 仍不可用。

一旦 Job 成功进入 `Failed`，后续 Runner 恢复不得把它回退为 Running 或 Completed。实现不在 status 更新后再次 GET Runner 并补偿 Job，也不自动重新调度，因为无法证明旧执行是否已经停止。若在 Job status 写入返回 Conflict 后观察到 Runner 已恢复，只按最新 Job 和 Runner 状态重新调谐，不覆盖并发结果。

若未来必须消除此窗口，需要由 apiserver 提供能够原子校验 Runner 身份、phase 或版本的专用 Job 状态迁移接口；仅增加客户端 GET 次数无法形成事务保证。

首版明确不实现 apiserver Job 状态迁移校验，因此无法在服务端阻止错误或旧版本客户端后续把终态改回非终态。Job Controller 的正确性依赖 Scheduler、Runner 和其他 status 写入方遵守字段所有权、Conflict 后重新读取以及终态不回退约定；这是一项已接受的一致性限制，不阻塞 Job Controller 开发。后续若补充服务端状态机，应保持当前合法流程兼容，并把终态不可回退从客户端约定提升为 API 不变量。

### 7.4 完整错误矩阵

Job Controller 不从错误字符串推断结果。所有分支最终只返回第八节定义的队列结果；业务代码不直接操作 workqueue。

#### 7.4.1 缓存读取和 GET

| 调用与结果 | 处理 |
|------------|------|
| Job cache 不存在 | 清理该 key 的索引和宽限期记录，成功结束 |
| Runner cache 不存在 | 仅表示进入或继续宽限期；到期后仍必须执行 Runner GET |
| cache 返回 `ErrCacheNotSynced` | 临时错误，`AddRateLimited` |
| cache 对象类型错误或违反内部不变量 | 内部临时错误并记录指标，`AddRateLimited` |
| Job GET 2xx 且对象、UID、resourceVersion 合法 | 使用返回对象继续调谐 |
| Runner GET 2xx 且对象名称、UID、resourceVersion 合法 | 根据权威 phase 判断恢复或 Offline |
| Job GET 404 | 目标 Job 已不存在，本周期成功结束 |
| Runner GET 404 | 权威确认 Runner 不存在；宽限期到期后允许把 Job 置为 Failed |
| GET 401/403 | PermanentError，记录鉴权失败指标，不改变健康状态 |
| GET 408、429 或 5xx | 临时错误，`AddRateLimited`；读取接口不使用 WriteError 的 RetryAfter 分支 |
| GET 409、410、412、422、其他 4xx 或最终 3xx | PermanentError，记录非预期读取协议错误 |
| GET 网络超时、EOF、连接重置或 DNS/TLS 临时错误 | 读取没有写副作用，作为临时错误 `AddRateLimited` |
| GET 2xx 但响应为空、解码失败、类型错误或缺少 UID/resourceVersion | 内部临时错误，记录 `unexpected-read-response` 并 `AddRateLimited` |
| Manager context 已取消 | 原样返回 context 错误，由 BaseController 停止 worker，不重入队 |

Runner GET 只有 2xx 的非 Offline 对象、2xx 的 Offline 对象和 404 能形成 Runner 可用性的权威结论。其他任何错误都不得触发 Job status 更新。

#### 7.4.2 明确收到写响应

下表同时适用于 `UpdateJobStatus` 和 `DeleteJob`。非 2xx 行对应 `WriteRejected`，其中 404 的业务语义按操作区分：

| HTTP 结果 | status 更新 | Job 删除 |
|-----------|-------------|----------|
| 完整 2xx 且成功响应校验通过 | 清理宽限期记录，记录成功日志和指标，成功结束 | 记录删除日志和指标，成功结束；存在 finalizer 时等待后续资源事件观察最终消失 |
| 2xx 但响应为空、解码失败或成功对象校验失败 | WriteUnknown，进入 7.4.4 | DELETE 的完整空 2xx 响应合法并视为成功；响应截断或无法确认完整响应时为 WriteUnknown |
| 400、405、410、422、其他未单列 4xx | PermanentError，记录请求或 API 契约错误 | PermanentError，记录请求或 API 契约错误 |
| 401、403 | PermanentError，记录鉴权失败 | PermanentError，记录鉴权失败 |
| 404 | Job 已删除，成功结束 | Job 已不存在，成功结束 |
| 408 | 临时错误，`AddRateLimited` | 临时错误，`AddRateLimited` |
| 409、412 | `Requeue=true`，下一周期重新 GET 并重算 | `Requeue=true`，下一周期重新 GET 并重新校验 UID、终态和保留期 |
| 429、503，且 `WriteError.RetryAfter > 0` | 原样返回 WriteError，由 BaseController 执行 `Forget + AddAfter` | 同左 |
| 429、503，但无合法 RetryAfter | 临时错误，`AddRateLimited` | 同左 |
| 其他 5xx | 临时错误，`AddRateLimited` | 同左 |
| 最终 3xx 或其他完整非 2xx 响应 | PermanentError，记录非预期协议错误 | 同左 |

写入 2xx 只有在 typed client 完成第二节定义的响应校验后才算成功。HTTP 1xx、截断响应以及无法确认完整 2xx 的情况不属于本表，必须归入 `WriteUnknown`。

#### 7.4.3 WriteNotSent

| 原因 | 处理 |
|------|------|
| namespace/name/GVR、UID、resourceVersion、对象类型或删除前置条件非法 | PermanentError；这是调用方或适配器编程错误 |
| 序列化或请求构造因对象内容确定性失败 | PermanentError，记录 client contract 指标 |
| transport 能证明请求未发送的临时 DNS、连接建立、TLS 或连接池错误 | 临时错误，`AddRateLimited` |
| 无法证明是否发送 | Client 不得返回 NotSent，必须归为 WriteUnknown |

判断永久或临时时使用可由 `errors.As` 穿透的具体底层错误类型，不匹配已知临时类型的本地构造错误默认视为永久错误。

#### 7.4.4 WriteUnknown 与确认读取

`WriteUnknown` 包括请求可能已发送但响应超时、EOF、连接重置、响应截断、异常 2xx，以及不能证明 NotSent/Rejected 的其他错误。Controller 必须先确认，不能原样重放请求对象。

若 Manager context 已取消，Controller 不在停止阶段启动确认请求，直接返回 context 错误；未确认的写入由进程重启后的初始 List 和正常调谐收敛。Client 仍必须按事实把该次写入分类为 WriteUnknown，不能因为 context 已取消而改报 NotSent。

status 更新结果未知后执行 Job GET：

| 确认结果 | 处理 |
|----------|------|
| 404 | Job 已消失，成功结束 |
| 同一 UID，phase 已为 Failed | 目标状态已经收敛，清理宽限期记录并成功结束 |
| UID 不同 | 原 Job 已消失且同名对象已重建，成功结束，不更新新对象 |
| 同一 UID，但已终态、不再为 Running、runner 已变化或正在删除 | 按最新对象成功结束，不覆盖新状态 |
| 同一 UID，仍满足 Runner 丢失处理条件 | 返回 `Requeue=true`；下一周期重新执行 Job GET、强制 Runner GET，并基于新 resourceVersion 构造新请求 |
| GET 失败 | 严格按 7.4.1 的 GET 矩阵处理 |

删除结果未知后执行 Job GET：

| 确认结果 | 处理 |
|----------|------|
| 404 | 删除已生效，成功结束 |
| UID 不同 | 原对象已经消失，成功结束，绝不删除同名新对象 |
| 同一 UID，但不再是终态、endTime/保留期变化或正在删除 | 按最新对象成功结束，不重放旧删除 |
| 同一 UID，且仍满足历史清理条件 | 返回 `Requeue=true`；下一周期使用最新 UID/resourceVersion 重新校验并构造删除请求 |
| GET 失败 | 严格按 7.4.1 的 GET 矩阵处理 |

确认 GET 自身超时或失败时只返回该读取错误，保留原始 WriteUnknown 作为日志上下文，不得因为无法确认而推断写入失败。

#### 7.4.5 Client 契约违例

写方法返回非 nil error 时必须是可由 `errors.As` 取得的 `*client.WriteError`。若不是，Controller 将其保守地当作 WriteUnknown，记录 `unexpected-write-error`，并进入 7.4.4 的确认读取流程；不能直接重试写操作。写方法返回 `(nil, nil)` 或异常成功对象时，由 typed client 包装成 WriteUnknown。

## 八、错误与重试

| 结果 | 队列行为 |
|------|----------|
| 对象不存在、无需处理、已经终态且 GC 关闭 | `ReconcileResult{}, nil`，BaseController Forget |
| Runner 丢失宽限期未到 | `ReconcileResult{RequeueAfter: remaining}, nil` |
| status 更新成功或 GET 确认已达到预期 | `ReconcileResult{}, nil` |
| 历史保留期未到 | `ReconcileResult{RequeueAfter: remaining}, nil` |
| DELETE 成功、404 或 GET 确认对象已不存在 | `ReconcileResult{}, nil` |
| resourceVersion Conflict | `ReconcileResult{Requeue: true}, nil`，清除旧退避后立即重读 |
| 429/503 且 WriteError 带合法 RetryAfter | 零值 Result 和原始错误，由 BaseController Forget 后 AddAfter |
| 读取或写入临时错误 | 零值 Result 和临时错误，由 BaseController AddRateLimited |
| WriteUnknown | 先按 7.4.4 确认；确认后成功结束、立即重入或按确认 GET 错误处理 |
| 永久 HTTP、输入或客户端错误 | 零值 Result 和 PermanentError，记录分类指标，不改变 Controller 健康状态 |
| 无法解析对象或违反内部不变量 | 零值 Result 和错误，记录并限速重试；持续失败必须可观测 |

超过 `--controller-max-retries` 后仍不能静默丢弃。框架应记录错误日志和 dropped 指标，并在周期性 resync 时允许对象再次进入队列。

客户端地址、TLS 和认证等静态配置必须在 initializer 阶段完成校验；配置非法时 Controller Manager 启动失败，不得延迟到单个 Job 的 Reconcile 中处理，也不通过 HealthChecker 表达。

## 九、配置与权限

新增配置：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--job-runner-lost-grace-period` | `5m` | Runner 已删除或明确 Offline 后，Job 失败收敛前的宽限期 |
| `--job-history-gc-enabled` | `true` | 是否清理超过保留期的终态 Job |
| `--job-history-retention` | `720h` | 从终态 Job 的 `status.endTime` 起计算的历史保留时长，必须大于 0 |

Job Controller 所需最小权限：

```text
jobs:        get, list, watch, delete
jobs/status: get, update
runners:     get, list, watch
```

权限严格限制在上述操作：Controller 不能创建 Job、更新 Runner 或 Runner status，也不能写入其他业务资源。Job 删除权限仅用于按第六节规则清理过期终态对象。

## 十、可观测性

日志至少包含 controller、Job key、Job UID、Runner、原 phase、resourceVersion、宽限期截止时间和 reconcile 结果，不记录 payload、凭据或完整 runtimeSpec。

建议指标：

```text
controller_reconcile_total{controller="job",result}
controller_reconcile_duration_seconds{controller="job"}
job_controller_runner_lost_observations
job_controller_runner_lost_failures_total
job_controller_status_update_conflicts_total
job_controller_status_update_unknown_total
job_controller_history_gc_scheduled
job_controller_history_deleted_total
job_controller_history_missing_end_time_total
job_controller_history_delete_unknown_total
```

首版不创建 Kubernetes Event 或 EBS 业务事件对象，也不依赖 `EventRecorder`。以下行为只通过稳定的日志 reason 和对应指标表达：

| 行为 | 日志 reason |
|------|-------------|
| 首次进入 Runner 丢失宽限期 | `RunnerUnavailable` |
| Runner 在宽限期内恢复 | `RunnerRecovered` |
| 宽限期到期并把 Job 收敛为 Failed | `RunnerLost` |
| 删除过期历史 Job | `HistoryJobDeleted` |

相同 reason 的日志必须限速或聚合，避免 Runner 离线时产生日志风暴。Event 作为后续独立能力设计；未来增加时需要同时定义 Recorder 接口、事件存储、RBAC、聚合策略和写入失败语义，不能改变当前调谐结果。

## 十一、测试要求

### 11.1 单元测试

- Pending、终态、未绑定和正在删除的 Job 均不处理；
- Runner 可用时清除旧宽限期；
- Runner Offline 和删除进入宽限期，到期前返回正确的 `RequeueAfter`；
- FakeClock 推进能够确定性触发宽限期和历史 GC，不依赖真实 sleep；
- 一次 Sync 只读取一次 Clock，Runner 丢失状态的 `endTime` 与该次调谐的 `now` 一致并转换为 UTC；
- 宽限期到期后必须执行 Runner GET；权威对象已恢复时清除观察，Offline 或 404 时才允许更新 Job；
- Runner GET 的 401/403、429、5xx 和结果未知不会把 Job 标记为 Failed，并分别进入约定的错误分支；
- 宽限期到期后只修改拥有的 status 字段；
- Job UID、runner 或 phase 变化后旧观察失效；
- Job/Runner 删除 tombstone 能正确清理索引；
- Runner 同名重建且 UID 变化时仍按名称视为恢复，不产生 UID 相关分支；
- Job/Runner GET 的 2xx、404、401/403、408/429、全部 5xx、其他 4xx、网络错误和异常 2xx 分别进入 7.4.1 的分支；
- status 更新和删除的 400/401/403/404/408/409/412/422/429/503、其他 4xx、其他 5xx 和最终 3xx 分别进入 7.4.2 的分支；
- WriteNotSent 的永久本地错误、可证明未发送的临时错误及错误分类违例分别进入 7.4.3 的分支；
- status 更新成功响应字段异常时按结果未知处理；
- status WriteUnknown 确认得到 404、预期 Failed、不同 UID、不可处理最新状态和仍可处理状态时分别正确收敛；
- 非终态、终态但 `endTime` 为空、尚未到期以及已经到期的 GC 判断正确；
- GC 使用 UID 和 resourceVersion 删除前置条件；
- delete WriteUnknown 确认得到 404、不同 UID、不再满足 GC 条件和仍满足 GC 条件时分别正确收敛；
- 写方法返回普通 error、`(nil, nil)` 或异常成功对象时按 Client 契约违例进入确认流程；
- Manager context 取消后不启动 Unknown 确认请求，重启后的初始 List 可以继续收敛；
- 关闭 GC 后不创建终态 Job 的延迟队列条目；
- 多 worker 并发和 `go test -race` 下索引、观察 map 无竞态。

### 11.2 集成测试

- 初始 List 中已有 Running Job 与 Offline Runner 时可以收敛；
- Runner 在宽限期内恢复时 Job 保持 Running；
- Runner 已恢复但 Watch 事件延迟、本地缓存仍为 Offline 时，权威 Runner GET 能阻止 Job 进入 Failed；
- Runner GET 确认 Offline 后、Job status 更新前恢复的竞态被明确接受；Job 可以进入 Failed，且遵守约定的 Runner 不会令终态回退；
- Runner 在宽限期内以相同名称、不同 UID 重建时仍视为恢复；
- Runner 丢失超过宽限期时 Job 进入 Failed，并保留已有结果和计数字段；
- Runner 与 Job watch 重连、410 relist、resync 不会重复覆盖状态；
- 在 Scheduler、Runner 和 Controller 均遵守终态不回退约定时，Runner 完成 Job 与 Controller 标记失败的并发更新最终保持终态；
- 初始 List 中的历史终态 Job 能按剩余保留时间重建清理计划；
- 超过 30 天的终态 Job 被删除，未到期或缺少 `endTime` 的对象被保留；
- Job 同名删除重建以及删除前发生状态变化时，删除前置条件可以防止误删；
- 删除请求结果未知时通过 GET 收敛，不产生紧密重复删除；
- controller-manager 重启后能够从初始 List 重建索引并重新开始安全宽限期；
- 认证失败不产生紧密重试，也不由单个对象错误改变 Controller 健康状态；临时网络错误能够恢复。

## 十二、开发顺序

1. 为 Controller Manager client 增加 Job、Runner 的 GET、Job `/status` 更新和带 UID/resourceVersion 前置条件的 Job DELETE 接口，并明确写入及删除结果未知的错误分类。
2. 实现反向索引、宽限期记录、历史保留期计算和纯状态判定函数。
3. 注册 Job、Runner Source handler，实现 Runner 丢失与历史 GC 两条 Reconcile 分支及队列结果映射。
4. 注册 `job` initializer、命令行参数、删除权限和指标；Job Controller 没有独立关键后台循环，不实现自定义 HealthChecker，使用框架默认 ping checker。
5. 完成 race、单元及集成测试后，再启用默认 `--controllers=*` 中的 Job Controller。

完成上述内容后，Job Controller 可以独立开发；Runner 心跳超时到 `Offline` 的生产者仍需由单独的 Runner 健康控制器或等价机制提供。
