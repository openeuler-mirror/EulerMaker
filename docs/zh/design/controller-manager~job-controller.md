# Job Controller 设计

## 一、定位与范围

Job Controller 是 `controller-manager` 中负责 Job 控制面收敛和历史对象回收的控制器。Scheduler 完成 Job 与 Runner 的绑定，Runner 负责实际执行、超时、日志及产物封账；Job Controller 处理已经绑定但失去执行主体的悬挂 Job，并按策略删除或先归档后删除超过保留期的终态 Job，默认归档后继续删除 OldJob 和数据。

首版职责：

- 监听 Job 和 Runner 变化；
- 识别绑定到已删除或明确 `Offline` Runner 的非终态 Job；
- 经过离线宽限期并再次确认后，将悬挂 Job 标记为 `Failed`；
- 处理 `Succeeded`、`Failed`、`Aborted` 状态下超过 30 天保留期的历史 Job，默认归档后删除，可选仅归档保留 OldJob；
- 保证状态更新幂等，并在并发更新时以 apiserver 中的最新对象为准；
- 暴露必要的结构化日志和指标。

职责边界：

- Job 的 Runner 绑定保持不变；失联 Job 直接收敛为失败，不自动重新调度，避免旧 Runner 尚在执行时产生并发构建。
- Runner 健康状态由 Runner Controller 维护；Build、BuildInfo 状态由对应 Controller 聚合。
- 归档通过 apiserver 完成，不直连 ES；Job/OldJob 的数据清理由本控制器调用 Artifact Manager，并通过 finalizer 保留失败恢复入口。

Job/OldJob finalizer 与归档责任移交统一见 [apiserver 删除与归档协议](ebs-apiserver.md#job-删除与归档-finalizer)。停止上传及输入使用者排空由上层业务保证，本控制器不增加 Runner 停止确认协议。

## 二、依赖与组件边界

Job Controller 运行在现有 `controller-manager` 框架内，建议实现目录为：

```text
backend/controller-manager/pkg/controllers/job/
  controller.go       # 初始化、事件注册和入队
  reconciler.go       # Runner 丢失处理、安全删除及写结果确认
  history.go          # 启动及每日分页清理循环
  lifecycle.go        # 删除对象轮询、归档移交和 OldJob 数据清理
  client.go           # 类型化 API 适配
  index.go            # runner -> Job Key 反向索引
  metrics.go
```

依赖关系如下：

```text
Job Watch Source ---------> Job Controller queue
                                  |
Runner Watch Source ------> runner -> jobs index
                                  |
                                  v
                         Job status subresource

Daily history scan -------> paginated Job List -------> start deletion or archive
Lifecycle polling --------> scoped work queue --------> archive / data cleanup / finalizer completion
```

Job 和 Runner 均支持 watch，应使用 `WatchSourceFactory` 创建并共享 Source。事件处理器必须在 `Manager.Run` 之前注册；控制器启动后不得再注册 handler。Manager 完成全部 Source 的首次同步后才启动 worker，因此 `Sync` 可以依赖两个本地缓存已经完成初始 List。

实时 Job 的 List/Watch 携带 `status.phase!=Succeeded,status.phase!=Failed,status.phase!=Aborted`，仅缓存非终态对象。历史到期筛选使用每日分页扫描；删除与已登记归档使用 6.4 的独立生命周期轮询，只入队候选身份，不把完整历史对象加入 informer 缓存。

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

Job Controller 在包内声明类型化客户端，由适配器组合公共 API Client 和 Artifact Manager 客户端。接口按下列职责提供方法，不要求公共 Client 引入 Job 专用业务类型：

- 分页查询到期候选及删除中的 Job/OldJob；List 使用字段投影。
- GET Job、OldJob、Runner；更新 Job status；携带 UID/resourceVersion 发起终态 Job 或 OldJob DELETE。
- 调用 Job archive 子资源，按持久化意图恢复归档；GET archive 查询迁移身份和阶段；通过 finalizers 子资源完成生命周期更新。
- `DeleteJobData`：按 Job 固化的实例 annotation 选择服务，只用于 cleanup 分支，不用于 archive 分支。

类型化 Client 统一校验 GET 响应类型、路径身份及 UID/resourceVersion；Controller 不重复校验这些结构字段，只判断候选 UID、当前状态和业务前置条件。`UpdateJobStatus` 必须验证成功响应非 nil、UID 与请求对象一致、resourceVersion 非空；Job Controller 写 Runner 丢失终态时，还必须验证响应的 phase 为 `Failed`、runner 未变化、stage 未变化。任一成功响应异常都包装为 `WriteUnknown`。

`ArchiveJob` 使用 UID/resourceVersion 前置条件，不发送 Job 正文；归档成功与未知结果确认按 6.3。源 Job 和 OldJob 的生命周期 metadata、resourceVersion 不相等，不能继续使用整对象相等判断。确认需要完整实时 GET，不使用字段投影；归档身份与冻结业务快照以 apiserver 的持久化迁移记录为准。

`DeleteJob` 必须同时接收 UID 和 resourceVersion，不能暴露无条件删除。Get 返回 Kubernetes 标准读取错误；apiserver 写方法返回的错误必须能通过下列方式读取 Outcome，并保留 `apierrors.IsConflict`、`IsNotFound`、`IsUnauthorized`、`IsForbidden` 和 `IsTooManyRequests` 判断能力。`DeleteJobData` 沿用 Artifact Manager 13.1 的 HTTP 成功与错误契约，不把其错误伪装为 apiserver 的 WriteError：

```go
var writeErr *apiserver.WriteError
if errors.As(err, &writeErr) {
    outcome := writeErr.Outcome
}
```

## 三、字段所有权

多个组件会更新 Job status，必须以字段所有权限制写冲突。更新 `/status` 时仍需携带完整最新 status，并只修改本组件拥有的字段。

| 组件 | 拥有的 Job status 字段或迁移 |
|------|-----------------------------|
| Scheduler | 绑定时写 `phase=Running`、`runner=<name>` |
| Runner | 写 `stage`、`startTime`、`endTime`、`message` 以及正常执行产生的终态 |
| Job Controller | 仅在 Runner 丢失时写 `phase=Failed`、`endTime`、`message`，保留当前 `stage` |

Job Controller 不修改 runner 和 startTime。生命周期流程只移除 Job 的 archive 或 OldJob 的 cleanup finalizer，不借此修改 spec/status 或其它 finalizer。上层调用方先中止非终态 Job，确认终态后再删除；Job Controller 不调用 abort。Job 创建时即带 archive finalizer。

终态为：

```text
Succeeded | Failed | Aborted
```

Job Controller 把 `Succeeded`、`Failed`、`Aborted` 视为终态，不再更新其 status；历史对象按 6.2 的每日扫描规则清理。

## 四、事件与本地索引

### 4.1 队列键

Runner 丢失处理使用工作队列键：

```text
{namespace}/{name}
```

UID 不编码在字符串键中。`Sync` 每次从缓存取得最新对象，并在写入前通过 GET 再次确认 UID 和 `resourceVersion`，防止同名 Job 删除重建后旧周期更新新对象。

### 4.2 Job 事件

- `Add`：非终态 Job 立即入队，`status.runner` 非空时还要更新反向索引。
- `Update`：先按旧对象删除索引，再按新对象增加索引。UID、phase、runner、`endTime` 或删除状态变化时入队；仅有无关字段变化不重复入队。Informer resync 重新入队非终态 Job。
- `Delete`：删除反向索引和本地宽限期记录，不需要写 API。Job 进入终态后离开 selector 也产生删除事件，不代表 API 对象已删除。

事件处理器只做类型检查、索引维护和入队，不调用外部 API，不执行状态机。

### 4.3 Runner 事件

控制器维护并发安全的 `runnerName -> set(JobKey)` 反向索引。Runner 的 `phase`、UID 或删除状态发生变化时，把索引中绑定到该 Runner 的 Job 全部入队。

Runner 的普通心跳更新不应导致所有 Job 反复入队。只有 Runner 进入或离开 `Offline`、对象 UID 改变、对象删除时才触发关联 Job。

删除事件必须兼容 informer tombstone。索引集合只保存字符串键，不保存可变 Job 指针。

### 4.4 周期性重同步

Watch resync 作为丢事件后的兜底，但不能替代 Runner 丢失宽限期的 `RequeueAfter`。历史清理由独立扫描循环驱动，不为每个终态 Job 保存延迟条目；重启时重新扫描。

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
| 对象存在且 phase 为 `Online` | Job 保持不变，清除本地宽限期记录 |
| 对象存在且 phase 为 `Offline` | 进入或继续离线宽限期 |
| Runner 对象不存在 | 进入或继续离线宽限期 |
| Runner 删除后以相同名称重建且 phase 为 `Online` | 视为同一个逻辑 Runner 已恢复，Job 保持不变 |

首版明确接受 Runner UID 无法参与绑定判断的限制。`Job.status.runner` 只保存 Runner 名称，Scheduler、Runner 和 Job Controller 都把该名称视为稳定的逻辑 Runner 身份；Job Controller 不记录或比较 Runner UID。Runner 删除后，只要相同名称的 Runner 在宽限期内重新出现且为 `Online`，就视为原逻辑 Runner 恢复，即使新对象 UID 已变化。

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
   - 返回同名 Runner 且 phase 为 `Online`：视为已经恢复，清除宽限期记录并成功结束；
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

历史到期筛选使用独立串行循环：启动时扫描一次，每轮结束后等待 24 小时；不使用非终态 Watch 缓存。依次分页 List Succeeded、Failed、Aborted，每页 500 个，仅取 metadata/status.phase/status.endTime，完整透传 continue，逐页释放。List 失败结束本轮，单个候选失败不阻塞其它候选。

1. 只处理非零 endTime 且已达到 `endTime + historyRetention` 的终态 Job；默认 720h。缺少 endTime 记录异常，不以创建时间代替。
2. 已删除中的对象交给生命周期调谐，不重复发起新意图。所有 Job 默认带 archive finalizer，不能用它是否存在判断已开始归档。
3. GET 最新对象，重新核对 namespace/name/UID、终态、endTime、删除状态和截止时间。
4. delete 模式携带最新 UID/resourceVersion 发起 Job DELETE，登记 action=delete；后续先归档，再由 OldJob 清理数据。扫描循环不直接清理文件。
5. archive 模式调用 archive 子资源登记 action=retain，按 6.3 完成源 Job 迁移；保留 OldJob 和制品，不调用普通 Job DELETE 进行迁移收尾。
6. 明确 404 结束旧候选；其它错误记录后继续下一候选。Unknown 保留日志和指标，本轮不额外 GET 确认、不重放旧请求。已登记意图由生命周期轮询读取最新状态恢复；未登记成功的候选等待下次每日扫描，重新 GET 核验后再构造请求。历史扫描仅返回 error，不生成队列重试结果。

`--job-history-gc-enabled=false` 只停止每日到期筛选，不停止 Runner 丢失处理、已受理删除或归档。修改 history-action 只影响新候选，不改变已登记 action；后续显式 DELETE 可以按 apiserver 契约将 retain 升级为 delete。扫描使用 Manager context，退出时等待本地循环结束，不把取消请求等同于服务端未执行。

### 6.3 归档调谐与恢复

archive finalizer 是源 Job 在删除前必须完成迁移的保护，不是归档已经开始或允许清理数据的标记。归档 action、冻结快照和跨实例写屏障统一见 [apiserver](ebs-apiserver.md#历史-job-归档ebs-oldjobs)。

1. 仅处理已删除中且仍带 archive finalizer 的源 Job；自身 finalizer 已移除时结束，不重复归档收尾。GET 最新对象及迁移记录，确认 UID/action；记录缺失视为契约异常，不从 deletionTimestamp 推导 retain/delete。
2. 核验源 Job 为终态且 endTime 非零，再请求归档。非终态删除由 apiserver 拒绝，上层负责先调用 abort 并确认终态；若发现异常的非终态删除对象，记录错误并保留对象，不自动中止。停止写入由上层业务保证。删除所需归档不被父 Build 删除或缺失阻断。
3. 调用 archive 子资源冻结或恢复同一快照；确认 ES 持久化及 create-only/删除凭据保护。源 Job 仍受 archive finalizer 保护，此分支永不调用 DeleteJobData。
4. 读取最新 action：retain 保留 OldJob；delete 对同 UID OldJob 发起条件 DELETE，并确认其删除标记。若 OldJob 已删除中不重复请求；读取失败或身份不符保留源对象。
5. 以最新源版本移除 archive finalizer，保留其它条目；源对象已经删除中，不再发送普通 Job DELETE。若并发 DELETE 将 retain 升级为 delete，旧版本收尾必须冲突并重新处理，不能漏掉 OldJob 删除。
6. 等待源 Job 真正消失。OldJob 删除标记已持久化时，由 6.4 继续处理；源消失后重启也不丢失清理入口。

| 操作结果 | 本轮处理 |
| --- | --- |
| 完整 200，归档响应通过 Client 校验 | 立即重新入队，读取持久化迁移状态，按最新 action 保留或标记删除 OldJob，再推进源 finalizer 收尾 |
| 源 Job 404 | 结束对源对象的修改；归档完成需由持久化迁移结果确认，不能仅凭 404 推断 |
| Conflict | 放弃旧版本，重新读取并恢复同一意图 |
| WriteRejected 的权限、参数或契约错误 | 告警并保留 archive finalizer，后续轮询允许修复后继续 |
| 临时错误、WriteNotSent 的临时错误 | 退避重试，不降级为直接删除 |
| WriteUnknown 或异常成功响应 | 下轮读取原对象及持久化迁移阶段，必要时恢复相同快照；未确认前不移除 archive finalizer |

归档安全性由服务端冻结快照、create-only 写入及删除凭据保证，不通过客户端超时或内存队列判断在途请求已经结束。重启后继续原意图，不根据新的 GC 策略替换冻结快照。

父 Build 删除中或缺失时拒绝新 retain 意图，但允许 delete 意图所需的归档及已有迁移恢复；停止写入和输入使用者排空由上层业务保证。

### 6.4 OldJob 数据清理

本节与 6.3 共用独立生命周期队列，键为 `{resource}/{project}/{name}`，resource 仅为 jobs/oldjobs；同名 Job 和 OldJob 不合并成一个队列项。原 Runner 丢失队列仍使用 `{project}/{name}`。

启动时及每轮轮询完成后 10 秒扫描一次，周期为内部常量。分别分页查询 deletionTimestamp 非空的 Job/OldJob，仅取 metadata，每页 500 个；两种迁移入口都设置源删除标记，不需要按 archive finalizer 或终态 phase 扫描全部历史对象。不常驻完整历史对象，不注册 OldJob Watch；查询失败下次从首页重试，不影响已入队对象。

调谐按以下顺序执行：

1. GET 当前资源，核对 UID；不存在或 UID 变化结束旧周期。删除中的 Job 只进入 6.3，不执行文件清理。
2. 仅 OldJob 的 deletionTimestamp 非空且带 cleanup finalizer 时执行本流程。没有自身 finalizer 时不接管其它清理；Job 带 cleanup 或 OldJob 带 archive 均属于契约异常，保留并告警。
3. 上层业务须保证已停止上传/签名、排空物化等输入使用者。Job API 终态（尤其 Aborted、RunnerLost）本身不证明容器和在途上传已停止；本轮不提供独立停止确认字段，不以删除文件强制中止业务。
4. 通过 GET archive 确认同 UID 的迁移阶段为 Transferred，即归档已确认且源 Job 不存在。迟到归档写入由服务端删除凭据阻断。缺失记录、读取失败或未完成迁移都保留对象，不能只凭源 Job GET 404 开始清理。
5. 从对象固化的 `ebs.io/artifacts` 和 Config/services 解析实例，调用 [Artifact Manager 13.1](artifact-manager.md#131-job-数据同步删除)。只接受完整 204 作为完成；Unknown 可重复该幂等 DELETE，HTTP 404 不是数据不存在的成功回执。
6. 清理成功后以本轮已读取的 UID/resourceVersion 调用 finalizers，服务端重新核验删除状态与迁移完成。Conflict 或 Unknown 下轮重新 GET 并判定，允许重复幂等清理；不得仅替换版本重放旧对象。
7. 保留其它 finalizer、业务状态和实例归属。不删除过程仓、发布仓及 current；数据已清理但 API 更新失败时允许短暂出现 API 仍在而文件不存在，下轮重复清理即可。

归档成功或 OldJob 删除意图确认成功后返回 `Requeue=true`，立即重新读取最新状态推进；等待源消失等外部条件、自身 finalizer 已移除或清理完成时返回空结果，由周期扫描发现后续变化，不统一设置 10 秒延迟重排。临时故障仍使用队列退避及合法 Retry-After；永久权限/契约错误记录告警并保留 finalizer，周期轮询使修复后仍可继续。一个 Job 失败不阻塞其它 Job。日志分别记录意图已受理、数据清理成功和 API 对象真正删除，不能将 DELETE 的 2xx 等同于最终清理完成。

停止写入仍是上层业务前提；Artifact Manager 的幂等删除不提供上传写入隔离。ES 删除凭据只保护归档元数据，不阻止 Runner 重新上传文件。

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
- Job Controller 先写 Failed：遵守本设计的 Runner 在冲突后读取最新 Job，发现终态后停止写入，不得把 Failed 改回 Running 或 Succeeded；
- Runner 恢复但其状态事件尚未到达本地缓存：宽限期到期后的强制 Runner GET 读取权威状态并阻止错误的 Failed 更新。

首版明确接受强制 Runner GET 与 Job status 更新之间仍存在不可消除的竞态窗口：Controller 可能在 GET 确认 Runner 不存在或为 `Offline` 后、提交 Job status 前遇到 Runner 恢复，并最终成功把 Job 写为 `Failed`。该 GET 只用于排除 Watch 缓存陈旧造成的误判，不构成 Runner 状态与 Job status 更新之间的事务前置条件，也不承诺写入瞬间 Runner 仍不可用。

一旦 Job 成功进入 `Failed`，后续 Runner 恢复不得把它回退为 Running 或 Succeeded。实现不在 status 更新后再次 GET Runner 并补偿 Job，也不自动重新调度，因为无法证明旧执行是否已经停止。若在 Job status 写入返回 Conflict 后观察到 Runner 已恢复，只按最新 Job 和 Runner 状态重新调谐，不覆盖并发结果。

若未来必须消除此窗口，需要由 apiserver 提供能够原子校验 Runner 身份、phase 或版本的专用 Job 状态迁移接口；仅增加客户端 GET 次数无法形成事务保证。

apiserver 的终态 status 保护见 [Job 主动中止](ebs-apiserver.md#job-主动中止)。Job Controller 在 RunnerLost 写 Failed 前及 Conflict/Unknown 确认时重新核验 Job；发现终态后不覆盖原原因和 endTime。删除流程不执行中止操作，终态前置要求见 6.3。

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

下表仅描述实时 Job status 更新；非 2xx 行对应 `WriteRejected`。历史扫描统一按 6.2 返回错误并继续下一候选，已登记删除/归档按 6.3、6.4 恢复。

| HTTP 结果 | status 更新 |
|-----------|-------------|
| 完整 2xx 且成功响应校验通过 | 清理宽限期记录，记录成功日志和指标，成功结束 |
| 2xx 但响应为空、解码失败或成功对象校验失败 | WriteUnknown，进入 7.4.4 |
| 400、405、410、422、其他未单列 4xx | PermanentError，记录请求或 API 契约错误 |
| 401、403 | PermanentError，记录鉴权失败 |
| 404 | Job 已删除，成功结束 |
| 408 | 临时错误，`AddRateLimited` |
| 409、412 | `Requeue=true`，下一周期重新 GET 并重算 |
| 429、503，且 `WriteError.RetryAfter > 0` | 原样返回 WriteError，由 BaseController 执行 `Forget + AddAfter` |
| 429、503，但无合法 RetryAfter | 临时错误，`AddRateLimited` |
| 其他 5xx | 临时错误，`AddRateLimited` |
| 最终 3xx 或其他完整非 2xx 响应 | PermanentError，记录非预期协议错误 |

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

历史删除的 Unknown 由 6.2 的扫描与生命周期轮询恢复，不复用 status 的即时确认流程。

确认 GET 自身超时或失败时只返回该读取错误，保留原始 WriteUnknown 作为日志上下文，不得因为无法确认而推断写入失败。

#### 7.4.5 Client 契约违例

apiserver 写方法应返回可由 `errors.As` 取得的 `*apiserver.WriteError`。普通 error 保守地视为结果未知：status 更新进入 7.4.4 的确认流程，历史扫描记录并交给后续扫描恢复，生命周期下轮重读原对象与迁移状态。写方法返回 `(nil, nil)` 或异常成功对象时，由 typed client 包装成 WriteUnknown，Controller 不再重复响应校验。

## 八、错误与重试

下表仅描述实时 Job 调谐；历史扫描的失败和重试行为统一见 6.2，不进入 BaseController 队列。

| 结果 | 队列行为 |
|------|----------|
| 对象不存在、无需处理、已经终态且 GC 关闭 | `ReconcileResult{}, nil`，BaseController Forget |
| Runner 丢失宽限期未到 | `ReconcileResult{RequeueAfter: remaining}, nil` |
| status 更新成功或 GET 确认已达到预期 | `ReconcileResult{}, nil` |
| resourceVersion Conflict | `ReconcileResult{Requeue: true}, nil`，清除旧退避后立即重读 |
| 429/503 且 WriteError 带合法 RetryAfter | 零值 Result 和原始错误，由 BaseController Forget 后 AddAfter |
| 读取或写入临时错误 | 零值 Result 和临时错误，由 BaseController AddRateLimited |
| WriteUnknown | 先按 7.4.4 确认；确认后成功结束、立即重入或按确认 GET 错误处理 |
| 永久 HTTP、输入或客户端错误 | 零值 Result 和 PermanentError，记录分类指标，不改变 Controller 健康状态 |
| 无法解析对象或违反内部不变量 | 零值 Result 和错误，记录并限速重试；持续失败必须可观测 |

超过 `--controller-max-retries` 后仍不能静默丢弃。BaseController 将 key 转入统一的慢速指数退避并持续重新入队；周期性 resync 或 Watch 事件可以提前唤醒该 key，但不替代慢速重入，也不在调谐成功前清除其失败历史。

客户端地址、TLS 和认证等静态配置必须在 initializer 阶段完成校验；配置非法时 Controller Manager 启动失败，不得延迟到单个 Job 的 Reconcile 中处理，也不通过 HealthChecker 表达。

## 九、配置与权限

新增配置：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--job-runner-lost-grace-period` | `5m` | Runner 已删除或明确 Offline 后，Job 失败收敛前的宽限期 |
| `--job-history-gc-enabled` | `true` | 是否清理超过保留期的终态 Job |
| `--job-history-retention` | `720h` | 从终态 Job 的 `status.endTime` 起计算的历史保留时长，必须大于 0 |
| `--job-history-action` | `delete` | 两者都先归档并移除源 Job；delete 继续删除 OldJob 和数据，archive 保留；其他值启动时报错 |

Go 配置增加 `JobControllerOptions.HistoryAction` 并传入 Job Controller `Config.HistoryAction`；GC 关闭时 action 不执行，但参数仍校验合法。保留期控制在 etcd 中的停留时长，不是 ES 归档的 TTL。

Job Controller 所需最小权限：

```text
jobs:        get, list, watch, delete
jobs/status: get, update
jobs/archive: get, create  # 迁移确认和意图恢复
oldjobs:     get, list, delete # 归档确认及删除意图接力
jobs/finalizers: update
oldjobs/finalizers: update
builds:      get           # 归档前检查父 Build 删除标记
runners:     get, list, watch
```

Controller 不创建 Job、不更新 Runner；归档写入通过专用子资源完成。Job DELETE 仅用于登记删除意图，源迁移收尾使用 finalizers 子资源；OldJob DELETE 用于移交 delete 意图，数据清理仅在 OldJob 分支执行。

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
job_controller_history_delete_requests_total
job_controller_history_missing_end_time_total
job_controller_history_delete_unknown_total
job_controller_history_archive_confirmed_total
job_controller_history_archive_conflict_total
job_controller_history_archive_unknown_total
job_controller_history_archive_error_total
```

首版不创建 Kubernetes Event 或 EBS 业务事件对象，也不依赖 `EventRecorder`。以下行为只通过稳定的日志 reason 和对应指标表达：

| 行为 | 日志 reason |
|------|-------------|
| 首次进入 Runner 丢失宽限期 | `RunnerUnavailable` |
| Runner 在宽限期内恢复 | `RunnerRecovered` |
| 宽限期到期并把 Job 收敛为 Failed | `RunnerLost` |
| 删除过期历史 Job | `HistoryJobDeleted` |
| 已确认归档，可继续条件删除 | `HistoryJobArchived` |
| 归档失败、冲突或未知结果未确认 | `HistoryJobArchiveDeferred` |

归档日志包含 action、源 UID/resourceVersion、HTTP 状态或 WriteError.Outcome，不打印完整 Job。`HistoryJobArchived` 只表示本轮已确认归档，不代表新增文档；删除成功仍单独记录。

相同 reason 的日志必须限速或聚合，避免 Runner 离线时产生日志风暴。Event 作为后续独立能力设计；未来增加时需要同时定义 Recorder 接口、事件存储、RBAC、聚合策略和写入失败语义，不能改变当前调谐结果。

## 十一、测试要求

### 11.1 单元测试

- Runner 丢失分支跳过 Pending、终态、未绑定和删除中的 Job；生命周期分支仍处理终态删除和已登记归档；
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
- status 更新的 HTTP 错误按 7.4.2 分流；历史删除失败仅返回错误，继续其它候选，不产生无效队列结果；
- WriteNotSent 的永久本地错误、可证明未发送的临时错误及错误分类违例分别进入 7.4.3 的分支；
- status 更新成功响应字段异常时按结果未知处理；
- status WriteUnknown 确认得到 404、预期 Failed、不同 UID、不可处理最新状态和仍可处理状态时分别正确收敛；
- 非终态、终态但 `endTime` 为空、尚未到期以及已经到期的 GC 判断正确；
- GC 使用 UID 和 resourceVersion 删除前置条件；
- delete WriteUnknown 本轮不额外 GET 或重放请求；已登记意图可被生命周期轮询发现，未登记候选下次扫描重新核验；
- Client 校验 GET 与写入响应，异常成功写响应分类为 WriteUnknown；Controller 保留业务 UID 对比与状态判断；
- Manager context 取消后不启动 Unknown 确认请求，重启后的初始 List 可以继续收敛；
- 关闭 GC 后不启动历史扫描；启动扫描、24 小时间隔、分页续读、失败后继续扫描和取消退出均可测试；
- delete/archive 两种模式均调用归档；分别删除/保留 OldJob，任何未确认写入均保留源 Job；
- 归档成功但源删除失败后可重入；生命周期版本变化不替换冻结快照，并发扫描和重启均保持条件删除；
- Job 默认 archive、OldJob 默认 cleanup，不存在类型切换；两种策略遇到持续 ES 故障均保留源对象，不阻塞其它候选；
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
- Watch 仅缓存非终态 Job；历史到期筛选每天扫描，删除/已登记归档由独立轮询发现，只缓存候选键；
- 超过 30 天的终态 Job 被删除，未到期或缺少 `endTime` 的对象被保留；
- Job 同名删除重建以及删除前发生状态变化时，删除前置条件可以防止误删；
- 历史删除结果未知时由后续轮询/扫描重读恢复，不在当前扫描追加确认或重复删除；
- controller-manager 重启后能够从初始 List 重建索引并重新开始安全宽限期；
- 认证失败不产生紧密重试，也不由单个对象错误改变 Controller 健康状态；临时网络错误能够恢复。

### 11.3 删除与归档协议验收

| 场景 | 验收要求 | 验证层级 |
| --- | --- | --- |
| 两类对象默认值及意图登记 | Job archive、OldJob cleanup；默认条目不触发迁移；意图与源删除标记原子落库 | apiserver 事务集成测试 |
| DELETE 先到、retain 先到、源收尾并发 | delete 不降级；retain 可升级；源分支从不调用 Artifact Manager | 并发测试 |
| 普通 Job 与 OldJob 删除 | Job 必须先归档；仅 OldJob 清理数据；普通 DELETE 不更改业务状态 | Controller 单元测试 |
| 非终态 Job、ES 不可用 | apiserver 拒绝非终态 DELETE；上层中止并确认终态后再删除；ES 不可用时保留源，恢复后继续 | 集成测试 |
| AM 204、503、404、响应丢失 | 204 推进；其它结果保留恢复入口；重试同一 Job 作用域 | HTTP 故障注入 |
| Job 已终态但仍有上传/输入使用者 | 上层必须先停止并排空，再允许删除；本轮没有 Runner 停止确认协议 | 跨组件集成测试 |
| archive ES 成功、源 finalizer 收尾失败 | 重入恢复同一快照；源仍在时不清理产物和日志 | etcd/ES/AM 集成测试 |
| OldJob 提前删除、源有其它 finalizer | OldJob 等待源消失；源归档不等待 OldJob 清理，不形成死锁 | Controller 集成测试 |
| 父 Build 删除中或缺失 | 阻止新 retain，允许删除必需的归档及已有意图恢复 | apiserver 集成测试 |
| 同名源与归档对象 | 队列键区分资源；UID/实例不匹配时保留并告警 | 单元测试 |
| 生命周期推进与等待 | 已确认推进立即重入；等待源消失及自身 finalizer 完成时不主动延迟重排；失败仍按错误分类处理 | 单元测试 |
| 终态删除与 retain 迁移 | 两者均有删除标记；生命周期轮询可发现；普通 Job 的默认 finalizer 不造成入队；GC 关闭仍推进 | 轮询集成测试 |
| OldJob 删除标记确认后、源消失后立即重启 | 从 OldJob 恢复清理；retain 则继续保留，不靠内存判断 | 进程重启测试 |
| 冻结意图、ES 写入、数据清理、finalizer 更新各阶段重启 | 恢复原意图，通过 UID 和摘要确认冻结快照；持久化凭据阻断迟到归档；成功清理可重复 | 进程重启故障注入 |
| 多 apiserver 归档请求延迟落库 | 旧执行者被 fencing 阻断；OldJob 清理后没有迟到重建 | 实际服务故障注入 |
| 数据文件与仓库硬链接 | 仅删 Job 目录；过程仓和发布仓仍可下载，repodata 不变 | AM 联动测试 |

单元测试已覆盖状态机和失败恢复；实际多进程、etcd/ES 故障注入及停止上传的业务保证仍需部署环境验收。
