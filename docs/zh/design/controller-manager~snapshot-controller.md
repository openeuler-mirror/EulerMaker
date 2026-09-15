# Snapshot Controller 设计

## 一、定位与范围

Snapshot Controller 是 `controller-manager` 中负责将 `Snapshot` 资源沿 `Pending → Processing → Active` 单向推进的控制器，完成 commit 解析。它是 build 流程的前置环节，与 build_controller、build_info_controller 协同工作。

首版职责：

- 识别 `status.phase ∈ {Pending, Processing}` 的 Snapshot 资源；
- 调用 git-server API 解析 `PackageRepo` 的 commit；
- 推进 Snapshot 状态：`Pending → Processing → Active`；
- 将包级解析失败写入对应的 `status.packageRepoStatuses[package].error`，整体级异常写入 `status.conditions`；
- 保证状态更新幂等，并在并发更新时以 apiserver 中的最新对象为准；
- 暴露必要的结构化日志和指标。

首版不负责：

- 不创建或删除 Snapshot 资源（由 build_controller 负责）；
- 不修改 `Snapshot.spec.defaultRef` 或 `Snapshot.spec.packageRepos`（从 Project 继承）；
- 不维护 Build、BuildInfo、RpmRepo 等上层资源状态；
- 不承担 `PackageRepo.ref` 的权威准入校验，该校验由 apiserver 负责；git-server 客户端仍在安全边界执行防御性复核，Snapshot Controller 只消费分类后的错误；
- 不实现 leader election（首版单副本部署）。

## 二、依赖与组件边界

Snapshot Controller 运行在现有 `controller-manager` 框架内，建议实现目录为：

```text
components/controller-manager/pkg/controllers/snapshot/
  controller.go       # 初始化、事件注册和 Sync
  reconcile.go        # reconcile 入口、父 Build 读取和目标范围判定
  resolve.go          # 两阶段并发解析（固定 worker pool + budgetCtx）
  conditions.go       # condition upsert/清理/消毒
  update.go           # advancePhase + updateSnapshot
  metrics.go
```

依赖关系如下：

```text
Snapshot PollingSource ---> Snapshot Controller queue ---> Snapshot GET/PUT
                                       |
Build --------------------------------> 构建类型与目标包范围判定
                                       |
git-server HTTP API -------> Commit 解析
```

**字段所有权**：

Build Controller 创建 single Snapshot 时，只复制 Build.spec.packages 指定且在 Project 中命中的仓库；其他类型复制全量仓库。Snapshot Controller 保留现有目标集合筛选与缺失目标处理，不回查 Project 补入仓库，也不要求 single Snapshot 包含工程全量 packageRepos。
- `Snapshot.spec.defaultRef` / `Snapshot.spec.packageRepos`：由 build_controller 创建 Snapshot 时从同一次 Project GET 复制并固化；Snapshot Controller 优先使用包 ref，整体为空时回退到 Snapshot.spec.defaultRef，不查询 Project、不回写 spec。部分填写的 ref 不触发回退，由 apiserver 拒绝；两者均为空时，当轮记录包级 ValidationFailed 并跳过。下文 ref 均指回退后的有效引用，包括并发结果冲突比较
- `Snapshot.status.packageRepoStatuses`：由 snapshot_controller **写入**（每个包的 commit 解析结果或错误）
- `Snapshot.status.phase`：由 snapshot_controller **写入**
- `Snapshot.status.conditions`：由 snapshot_controller **写入**

Project apiserver 在创建和更新 Project 时保证 `spec.packageRepos[].name` 非空；build_controller 只从该字段固化 Snapshot 输入。因此，Snapshot Controller 将 PackageRepo 名称非空视为输入不变量，不重复处理空名称分支。

Snapshot 不支持 watch，应使用 `PollingSourceFactory` 创建 Source。事件处理器必须在 `Manager.Run` 之前注册；控制器启动后不得再注册 handler。Manager 完成全部 Source 的首次同步后才启动 worker。PollingSource 仅负责通过 List 差异发现事件并将键加入队列，不向 Snapshot Controller 提供对象缓存读取接口；`Sync` 必须通过 API GET 获取最新 Snapshot，不能把事件携带的对象或 PollingSource 内部快照作为调和依据。

Initializer 保存 Factory 返回的 `source.Source` 和共享客户端：

```go
type Controller struct {
    snapshots source.Source
    client    Client
    gitClient GitServerClient
    clock     clock.Clock
    // queue、跨轮失败计数 map 省略
}
```

`clock.Clock` 使用 `k8s.io/utils/clock`。生产 initializer 注入 `clock.RealClock{}`，单元测试注入 `clocktesting.FakeClock`；业务代码不得直接调用 `time.Now()`、`time.Since()` 或 `time.Until()`。

Snapshot Controller 在包内声明最小客户端接口，共享 API Client 通过适配器实现它：

```go
type Client interface {
    GetSnapshot(ctx context.Context, namespace, name string) (*ebsv1.Snapshot, error)
    GetBuild(ctx context.Context, namespace, name string) (*ebsv1.Build, error)
    UpdateSnapshotStatus(ctx context.Context, snapshot *ebsv1.Snapshot) (*ebsv1.Snapshot, error)
}

type SyncCheckResult struct {
    Synced   bool
    CloneURL string
}

// GitServerClient 是 Snapshot Controller 唯一依赖的 git-server 客户端接口。
// HTTP 请求/响应结构和缓存均封装在实现内部，不暴露给 Controller。
type GitServerClient interface {
    // PublishSyncTask 调用 POST /api/v1/repo/sync，注册仓库同步任务
    PublishSyncTask(ctx context.Context, originURL string) error

    // CheckSynced 调用 POST /api/v1/repo/status，并在客户端内部判断
    // sync_time != nil && sync_time >= baseline。
    // 仓库尚无本地副本、尚未首次同步或同步时间早于 baseline 时返回
    // SyncCheckResult{Synced:false}, nil。
    // 确认同步完成时同时返回 git-server 响应中的 CloneURL。
    CheckSynced(ctx context.Context, originURL string, baseline time.Time) (SyncCheckResult, error)

    // ResolveCommit 通过 POST /command 从已经就绪的本地镜像解析 Branch/Tag。
    // 成功时只返回经过校验的完整 40 字符小写 commit SHA。
    // ref.Type 只能是 GitRefBranch 或 GitRefTag；GitRefCommit 由 Controller 直接采用，
    // 不得调用该方法。
    ResolveCommit(ctx context.Context, originURL string, ref ebsv1.GitRef) (string, error)
}
```

`GitServerClient` 的失败必须返回可由 `errors.As` 识别的分类错误，Controller 不解析错误文本或底层 HTTP 状态：

```go
type GitServerErrorKind string

const (
    GitServerErrorValidation GitServerErrorKind = "Validation"
    GitServerErrorNotFound   GitServerErrorKind = "NotFound"
    GitServerErrorTemporary  GitServerErrorKind = "Temporary"
    GitServerErrorPermanent  GitServerErrorKind = "Permanent"
)

type GitServerError struct {
    Operation string
    Kind      GitServerErrorKind
    Err       error
}
```

各方法的 error 契约如下：

| error | 含义与 Controller 行为 |
|-------|-------------------------|
| `nil` | 调用成功；`CheckSynced` 的正常未就绪也返回 `Synced=false, nil` |
| `GitServerError{Kind: Validation}` | Controller 提供的 URL 或 ref 不符合约束，确定性包级 `ValidationFailed`，当轮进入 Skipped |
| `GitServerError{Kind: NotFound}` | `ResolveCommit` 确认 Branch/Tag 不存在，确定性包级 `ResolveFailed`，当轮进入 Skipped |
| `GitServerError{Kind: Temporary}` | 408、429、5xx、临时网络错误、异常/不完整响应等；`PublishSyncTask`/`CheckSynced` 映射为 `SyncFailed`，`ResolveCommit` 映射为 `ResolveFailed`，均设置 `retryable=true` 并计入跨轮失败预算 |
| `GitServerError{Kind: Permanent}` | 401、403 或无法通过重试修复的协议错误；Controller 返回 `PermanentError`，不得把基础设施配置错误固化成包级 Skipped |
| `context.Canceled` | 原样返回；Manager context 取消时停止本周期，不生成业务错误 |
| `context.DeadlineExceeded` | 若由 `budgetCtx` 或单请求超时触发，归入 `SyncTimeout` 或对应的临时解析失败；Manager context 已取消时仍原样结束 |

客户端内部固定重试耗尽后只返回一次最终分类错误，并保留 `errors.Is(err, context.Canceled)`、`errors.Is(err, context.DeadlineExceeded)`，以及通过 `var gitErr *GitServerError; errors.As(err, &gitErr)` 读取分类的能力。Controller 对未知且无法分类的 error 按 `Temporary` 处理并记录 `unexpected-git-server-error` 指标，避免误把暂时故障永久固化到 Snapshot。

HTTP 及协议结果必须在客户端内按以下规则归一化：400/422 且明确表示输入 URL/ref 非法时为 `Validation`；`CheckSynced` 的仓库 404 为正常未就绪；`ResolveCommit` 明确确认 ref 不存在时为 `NotFound`；401/403、接口不存在导致的 404/405 和其他不可恢复协议错误为 `Permanent`；408、429、5xx、网络错误及无法解析或字段不完整的成功响应为 `Temporary`。不得将服务端异常响应归为输入 `Validation`，避免把基础设施故障永久写入包状态。

接口边界约定：

- Snapshot Controller 只调用上述三个方法，不直接构造 `/status`、`/command` 请求，不解析 `RepositoryResponse` 或 `CommandResponse`。
- `PublishSyncTask` 只负责幂等发布同步任务，不等待同步完成。
- `CheckSynced` 返回 `SyncCheckResult{Synced:false}, nil` 是正常的“尚未就绪”结果，不计为一次 git-server 请求失败；HTTP 404、响应无 `sync_time` 或 `sync_time < baseline` 均归一化为该结果，此时 `CloneURL` 必须为空。
- `CheckSynced` 仅在 `sync_time >= baseline` 且响应包含非空 `clone_url` 时返回 `SyncCheckResult{Synced:true, CloneURL:...}, nil`；同步时间达标但缺少 `clone_url` 属于 git-server 响应异常，返回临时错误。
- `ResolveCommit` 在内部将 Branch 转为 `refs/heads/<value>`，将 Tag 转为 `refs/tags/<value>^{commit}`，调用只读 `/command` 并校验输出。Controller 不自行拼接命令数组或完整 ref。
- 相同仓库已经排队或正在执行同步时，git-server 合并重复发布；Controller 不依赖某次请求的操作 ID。
- `PackageRepo.url`、`ref.value` 和组装后的 Git ref 均是不可信输入，客户端必须执行防御性校验；违反约束返回 `GitServerError{Kind: Validation}`。该复核不替代 apiserver 的权威准入校验。
- 三个方法都必须遵守传入 context；context 取消或超时后及时返回。
- L1 TTL 缓存属于客户端实现。同步状态缓存以规范化仓库 URL 为 key，同时缓存原始 `sync_time` 和 `clone_url`，每次调用仍使用本次 baseline 比较；不得仅缓存某次 `CheckSynced` 的布尔结果。`ResolveCommit` 不跨调用缓存 Branch/Tag 的解析结果，避免仓库完成新一轮同步后仍返回旧分支或标签结果。
- 客户端实现可以提供 `PublishSyncTask`、`GetSyncStatus`、`ExecuteCommand` 等底层私有方法，但这些方法不属于 Controller 接口。

`UpdateSnapshotStatus` 必须验证成功响应非 nil、UID 与请求对象一致、resourceVersion 非空。任一成功响应异常都包装为 `WriteUnknown`。

`advancePhase` 接收 API GET 得到的 Snapshot，构造 `Pending → Processing` 写入，并返回可供后续处理的服务端 Snapshot：

```go
func (c *Controller) advancePhase(
    ctx context.Context,
    snapshot *ebsv1.Snapshot,
) (*ebsv1.Snapshot, error)
```

- `/status` 返回完整成功响应时，`advancePhase` 校验响应后返回该响应对象；后续父 Build 读取、包解析、合并和最终状态写入都以该对象及其新 `resourceVersion` 为基础。
- `/status` 返回 Conflict 时，`advancePhase` 返回可被 `apierrors.IsConflict` 识别的错误；调用方立即结束本周期并返回 `ReconcileResult{Requeue: true}`，不得在当前周期继续解析，也不得在方法内部 GET、合并或重放。
- 写入结果为 `WriteUnknown` 时，按 7.4.4 GET 确认。若写入意图已实现，返回确认 GET 得到的最新 Snapshot；若未实现，结束本周期并重新入队。不得继续使用调用 `advancePhase` 前的对象。
- Snapshot 已是 `Processing` 时不调用 `advancePhase`，直接使用本周期 API GET 得到的对象。

`UpdateSnapshotStatus` 的写入失败必须能通过下列方式读取 Outcome，并保留 `apierrors.IsConflict`、`IsNotFound`、`IsUnauthorized`、`IsForbidden` 和 `IsTooManyRequests` 判断能力。`GetSnapshot`、`GetBuild` 不返回 `WriteError`，但必须保留对应的 `apierrors.Is*` 判断能力；git-server 方法遵循前述 `GitServerError` 契约：

```go
var writeErr *apiserver.WriteError
if errors.As(err, &writeErr) {
    outcome := writeErr.Outcome
}
```

## 三、字段所有权

多个组件会更新 Snapshot，必须以字段所有权限制写冲突。

| 组件 | 拥有的 Snapshot 字段 |
|------|---------------------|
| build_controller | 创建 Snapshot，写 `metadata`、`spec.packageRepos`（从 Project 继承） |
| snapshot_controller | 写 `status.packageRepoStatuses`、`status.phase`、`status.conditions` |
| 其他组件 | 不写 Snapshot |

**禁止修改的字段**：
- `metadata.name`、`metadata.namespace`（创建后不可变）
- `metadata.labels`、`metadata.annotations`（由创建方管理）
- `spec.packageRepos`（由 build_controller 创建 Snapshot 时固化，snapshot_controller **只读不修改**）

**终态定义**：
```text
Active
```

Snapshot Controller 自身把 `Active` 视为终态：观察到该 phase 后不再更新其 status，HandlerFuncs 不放行，不再入队。

## 四、事件与本地索引

### 4.1 队列键

工作队列键统一为：
```text
{namespace}/{name}
```

UID 不编码在字符串键中。`Sync` 通过 API Get 获取最新对象，并在写入前再次确认 UID 和 `resourceVersion`，防止同名 Snapshot 删除重建后旧周期更新新对象。

### 4.2 Snapshot 事件（PollingSource）

- `Add`：`phase ∈ {Pending, Processing}` 时入队；`Active` Snapshot 不入队。
- `Update`：不比较新旧 `resourceVersion`；只要新对象的 `phase ∈ {Pending, Processing}` 就入队，新对象已经进入 `Active` 时不入队。PollingSource 每轮扫描都会产生 Update，同一 resourceVersion 的 Update 也必须入队，作为丢事件、慢速重试和外部依赖恢复后的 resync 兜底；工作队列负责合并同一 key。
- `Delete`：无需处理（Snapshot Controller 不删除 Snapshot）。

事件处理器只做类型检查和入队，不调用外部 API，不执行状态机。

**查询优化**：ebs-apiserver 已支持 Snapshot 的 `status.phase` fieldSelector（支持 `=`、`==`、`!=` 操作符），推荐使用 `status.phase!=Active` 在服务端过滤，减少查询数据量：

```go
// 推荐：在 Initializer 中使用 PollingSourceFactory 创建带 fieldSelector 的 PollingSource
snapshots, err := init.Dependencies.PollingFactory.ForResource(
    snapshotsGVR,
    30*time.Second,
    metav1.ListOptions{FieldSelector: "status.phase!=Active"},
)
if err != nil {
    return nil, false, err
}
```

### 4.3 周期性重同步

PollingSource 默认每 30s 轮询一次，作为丢事件和进程重启后的恢复兜底。延迟重入计划无需持久化；同步等待的判定与重新入队规则统一见 6.2 和第八章。

### 4.4 跨轮失败计数

控制器维护并发安全的跨轮失败计数 map：

```go
type failureTracker struct {
    mu             sync.Mutex
    counts         map[failureKey]int       // failureKey = Snapshot UID + 包名
    uidByObjectKey map[string]types.UID      // objectKey = namespace/name
}
```

- 计数仅存在于进程内，控制器重启清零、重新给予预算；
- Snapshot GET 成功后调用 `Observe(objectKey, uid)`；首次观察记录映射，UID 与已有映射不同时清除旧 UID 的全部计数并替换映射；
- Snapshot GET 404 时调用 `ClearObjectKey(objectKey)`，清除该键记录的 UID、该 UID 的全部计数；
- GET 得到 `Active` Snapshot 时清除该 UID 的全部计数和 objectKey 映射；
- GET 得到已有成功状态或不可重试 error 时，该状态已经由 apiserver 确认，可立即清除对应包的遗留计数；
- 本轮新产生 Resolved、Waiting 或终态 Skipped（包括确定性失败和 `RetryExhausted`）时，将清理动作作为候选变更；只有 status 写入成功或 WriteUnknown 确认写入意图已实现后才提交；如果 status 无变化，则在返回预先计算的 `postWriteResult/postWriteErr` 前直接提交；
- 未达到上限的非确定性 Failed 仅在 status 写入成功或 WriteUnknown 确认后增加计数；达到上限并成功持久化 `RetryExhausted` 后清除该包计数。Conflict、写入失败或确认未实现时丢弃候选增量和清理动作；
- Snapshot 成功推进 Active 后清除该 UID 的全部计数和 objectKey 映射；
- 对本轮非确定性失败计算 `nextCount = currentCount + 1`。当 `nextCount >= failureRetryLimit` 时，本轮直接写入不可重试的 `PackageRepoStatus.error`（code=`RetryExhausted`）并跳过；否则写入可重试 error。默认 `failureRetryLimit=3` 表示第 1、2 次已确认失败仍可重试，第 3 次已确认失败转为 `RetryExhausted`；
- 锁内不得执行 API 请求或队列阻塞操作。

## 五、状态判定

### 5.1 可处理 Snapshot

Snapshot 同时满足下列条件才进入处理流程：

```text
status.phase ∈ {Pending, Processing}
metadata.deletionTimestamp == nil
```

`Active` Snapshot 不处理；正在删除的 Snapshot 不产生新的状态写入。

### 5.2 关键设计决策

| 决策编号 | 决策 | 选择 | 理由 |
|----------|------|------|------|
| DR-1 | `Processing` 状态不回退 | **不回退，保持 Processing，继续填充** | reconcile 取到 phase=Processing（异常重入）时不回退 Pending：回退会丢失"已在填充中"语义，保持 Processing 更符合"进行中"语义，避免抖动 |
| DR-2 | git-server 同步与 commit 获取 | **Branch/Tag 两阶段解析，Commit 校验后直接采用** | 不同 ref 类型采用不同处理路径；客户端协议见第二章，解析状态迁移见 6.2 |
| DR-3 | 本地镜像同步完成判定 | **baseline 时间戳比较** | 不依赖某次同步请求的操作 ID；判定契约见第二章 CheckSynced |
| DR-4 | 失败原因记录位置 | **包级 error 与整体 condition 分离** | 避免将不受控包名编码为 condition type；字段约束见 6.4 |
| DR-5 | 部分失败处理 | **允许部分成功** | 已完成包不重复解析，完成判定见 6.3 |
| DR-6 | 失败分类与重试预算 | **确定性失败跳过，非确定性失败跨轮重试，超限跳过** | 计数边界及提交、清理规则统一见 4.4；错误码见 6.4 |
| DR-7 | Snapshot 命名 | `<build-name>` | 与所属 Build 同名，通过 `metadata.name` 直接定位 Snapshot 与反查父 Build，无需 label 关联；由 build_controller 创建时确定 |
| DR-9 | commit 冲突（同 repo 不同包） | **跳过并记录包级错误，不覆盖已接受结果** | 冲突比较与合并顺序见 6.2.1，错误码见 6.4 |
| DR-10 | git-server 客户端 | **Controller 仅依赖三个业务方法** | HTTP、重试和缓存封装在 pkg/clients/gitserver；接口与缓存约束见第二章 |
| DR-11 | CheckSynced 返回值 | **返回同步判定及 clone URL** | Controller 不解释底层 HTTP；完整返回值与错误契约见第二章 |
| DR-13 | 并发解析 | **固定 worker pool + 总预算** | 限制并发及单轮耗时；派发、取消和合并规则见 6.2.1，配置默认值见 9.1 |
| DR-14 | 单包轮内检查与错误重试上限 | **每轮最多调用 `CheckSynced` 8 次；请求错误最多连续出现 5 次**（检查间隔为 1s/2s/4s/8s，之后按 8s 封顶，可被 ctx 中断） | 8 次调用均为正常未就绪结果时进入 Waiting，不计失败；5xx/网络错误连续达到上限后进入 Failed，计入跨轮失败预算（DR-6） |
| DR-15 | 指定包构建 | **single 解析指定目标；其他类型解析全部仓库** | 目标筛选、缺失包处理及完成判定统一见 6.3 |
| DR-16 | 父 Build 生命周期 | **父 Build 必然存在** | Build 持久化后才创建 Snapshot，且 Snapshot 存续期间不删除父 Build；读取异常处理见 6.3 和 7.4.1 |

### 5.3 幂等性

| 场景 | 幂等保证 |
|------|----------|
| 重复处理同一 Snapshot | 已有 `commitId` 或不可重试 error 的包不重复解析；带可重试 error 的包继续解析 |
| 部分 repo 解析失败 | 成功包写入 commitId；确定性失败或重试超限写入不可重试 error；未超限失败写入可重试 error，下轮只重试该项 |
| 控制器重启 | 从 `Pending` / `Processing` 状态恢复（HandlerFuncs 放行两者），继续未完成工作；`Processing` 为异常重入，保持不回退（DR-1） |

## 六、Reconcile 流程

### 6.1 状态机

`Snapshot.status.phase` 取值：`Pending` / `Processing` / `Active`。推进路径 `Pending → Processing → Active` 单向，**禁止回退**（`Active → 任何`、`Processing → Pending` 均非法）；`Processing → Processing` 为异常重入，保持继续填充（DR-1）。**不设 `Failed` 终态**——包级失败经 `PackageRepoStatus.error` 表达，整体级失败经 conditions 表达；确定性失败与重试超限的包被跳过后 Snapshot 仍推进 `Active`（DR-6）。

```
build_controller 在 Prepared 阶段创建 Snapshot
                         │
                         ▼
                    [ Pending ]
                         │
                         │ advancePhase：PUT /status
                         ▼
                    [ Processing ]
                         │     ▲
       尚未完成，保持状态 └─────┘
                         │
                         │ 全部目标包 Resolved 或 Skipped
                         │ updateSnapshot：写入
                         │ packageRepoStatuses、conditions 和 phase
                         ▼
                     [ Active ]
```

`Processing` 在存在 Waiting 包或未超限的可重试失败包时保持自循环；`budgetCtx` 耗尽时，正常等待同步的包进入 Waiting，尚未开始或请求未返回的包计入失败预算。`Active` 是终态，HandlerFuncs 不再将其加入队列。

| 状态 | 含义 | 进入条件 | 外出条件 |
|------|------|----------|----------|
| `Pending` | 初始态，repo 输入待处理 | build_controller 创建 Snapshot 时置入 | reconcile 取出后经 advancePhase 单 PUT /status → `Processing` |
| `Processing` | 填充进行中（异常重入保持，不回退） | `Pending → Processing` | 全部目标包 Resolved 或 Skipped（确定性失败/重试超限）→ `Active`；存在 Waiting 或未超限可重试失败时保持（自循环） |
| `Active` | 终态，目标包均已处理 | 单轮 reconcile 汇总全部包成功或跳过 | 无（终态；不再入队） |

### 6.2 单包 commit 解析子状态机（reconcile 内瞬时状态，不持久化）

单轮 reconcile 中，`ref.type=Branch/Tag` 的 PackageRepo 使用以下三阶段解析流程。`ref.type=Commit` 校验 `ref.value` 后直接进入 `Resolved`，不进入该子状态机。瞬时状态仅存在于当轮内存，跨轮不保留——下轮从头部重放。

```
┌────────────┐  PublishSyncTask 成功  ┌─────────┐  CheckSynced 达标     ┌───────────┐  rev-parse 成功  ┌──────────┐
│ PendingSync │ ───────────────────→ │ Syncing │ ───────────────────→ │ Resolving │ ──────────────→ │ Resolved │
└────────────┘                       └─────────┘  (SyncTime!=nil&&>=baseline) └───────────┘                 └──────────┘
      │失败（非确定）                       │未就绪/404：轮内检查 ≤8 次后 Waiting │校验失败 / 分支不存在 / commit 冲突（确定性）
      ▼                                     ▼（异常上限 5 次，DR-14）        ▼
┌──────────────────────────────────┐   ┌───────────────────────────────────────────────────────────────┐
│ Failed（可重试，计跨轮预算）        │   │ Skipped（终态分流，不再重试）                                     │
│ 下轮重试；连续失败 ≥ failureRetryLimit│   │ · 确定性失败当轮进入（ValidationFailed/ResolveFailed/CommitConflict）│
│ → 写包级 error（RetryExhausted）→ Skipped│   │ · 非确定性失败重试超限进入（RetryExhausted）                  │
└──────────────────────────────────┘   └───────────────────────────────────────────────────────────────┘
```

**注意**：git-server 客户端把 status 接口的 404、缺少 `sync_time` 和同步时间早于 baseline 统一转换为 `SyncCheckResult{Synced:false}, nil`。Controller 收到未就绪结果后继续轮内检查；检查次数耗尽时进入 `Waiting`，不得归入 `Failed`。它不读取原始 `RepoStatus`。

| 瞬时状态 | 含义 | 迁移条件 |
|----------|------|----------|
| `PendingSync` | 待发布同步任务 | `PublishSyncTask` 成功 → `Syncing`；失败（非确定）→ `Failed` |
| `Syncing` | git-server 本地镜像同步中 | `CheckSynced` 返回 `Synced=true` 时保存 `CloneURL` 并进入 `Resolving`；返回 `Synced=false` 时继续轮内检查，检查次数耗尽 → `Waiting`；返回 error → 按错误分类处理并进入 `Failed` |
| `Resolving` | 本地镜像 ref 解析中 | `rev-parse` 成功且无冲突 → `Resolved`；校验失败/ref 不存在/commit 冲突（确定性）→ `Skipped`；解析 I/O 失败（非确定）→ `Failed` |
| `Resolved` | commit 已解析 | 组装包含 `commitId` 且 `error=nil` 的只读解析结果，进入主 goroutine 的当轮汇总 |
| `Waiting` | git-server 正常同步中 | 不写包级 error、不增加 failure tracker；本轮结束后按 `syncRequeueDelay` 延迟重新入队 |
| `Failed` | 本轮失败（可重试） | 组装失败结果；由主 goroutine 根据跨轮失败计数决定继续重试或转为 `Skipped` |
| `Skipped` | 已跳过（不再重试） | `packageRepoStatuses[repo.name].error` 已持久化确定性失败或 `RetryExhausted`，且 `retryable=false`；后续轮次直接归入 skipped set |

### 6.2.1 包级并发结果的合并规则

并发阶段采用“worker 只计算、主 goroutine 唯一合并”的模型。通过 `context.WithTimeout` 创建 `budgetCtx`，总预算为 `resolveBudget`，默认值见 9.1：

- 进入并发阶段前，主 goroutine 对目标 `PackageRepo` 做一次预检查并为每项分配其在 `Snapshot.spec.packageRepos` 中的稳定索引。同一名称重复时，所有同名项均不启动解析，并在 `packageRepoStatuses[repo.name]` 写入一条不可重试的 `ValidationFailed` error 表示名称冲突；控制器不得任意选择其中一项。
- 主 goroutine 创建容量等于待解析项数量的 `jobs` 和 `results` 通道，将通过预检查且尚未完成的任务按稳定索引写入 `jobs` 后关闭该通道。worker 数为 `min(resolveWorkers, len(jobs))`；没有任务时不启动 worker。
- 每个 worker 是一个固定 goroutine，循环从 `jobs` 领取任务。领取前以及领取后、发起外部请求前都检查 `budgetCtx.Err()`；context 已取消时立即退出，不再领取或处理后续任务。禁止为单个 PackageRepo 再创建子 goroutine。
- 每个 worker 只读取不可变的 `PackageRepo`、baseline 和 reconcile 上下文，调用 git-server 后返回一条不可变的 `packageResolveResult`。worker 不得修改 Snapshot、`status.packageRepoStatuses`、conditions、failure tracker、指标计数器或工作队列。
- 结果至少携带稳定索引、包名、解析状态（Resolved/Waiting/Failed/Skipped）、成功时的 `PackageRepoStatus`，以及失败时的分类、reason 和已消毒 message。`results` 容量等于待解析项数量，worker 发送结果不得因主 goroutine 尚未读取而阻塞。
- 已经启动的 git-server 请求必须遵守 `budgetCtx` 并及时退出。一个专用收尾 goroutine 只负责 `WaitGroup.Wait()` 后关闭 `results`；主 goroutine range 读取结果直至通道关闭。所有 worker 退出前 reconcile 不得返回，不允许遗留 goroutine 在本周期结束后继续执行。
- worker 从 `CheckSynced` 取得 `Synced=false, err=nil` 后必须记住该任务已经进入正常等待；若 budgetCtx 在下一次检查前取消，worker 返回 Waiting 结果。主 goroutine 对 budgetCtx 取消后仍未被领取或未返回结果的其余任务，根据稳定索引与已收到结果的差集生成 `SyncTimeout` Failed。每个目标项必须恰好进入一次最终汇总。Manager context 取消仍直接结束，不生成业务结果。
- 主 goroutine 收齐结果后，按稳定索引升序合并，而不是按 goroutine 完成顺序合并。因此相同输入在不同调度时序下必须生成相同的 `packageRepoStatuses`、conditions 和日志结果。
- 合并以 API GET 得到对象的 DeepCopy 为基础。原始仓库地址始终从不可变的 `Snapshot.spec.packageRepos` 读取，不在 status 中重复保存。已有成功结果或不可重试 error 保持不变；Resolved 结果写入 `commitId` 并清空旧 error；Branch/Tag 在 `CheckSynced` 成功后同时写入其返回的 `cloneUrl`，Commit 类型的 `cloneUrl` 为空；Waiting 结果不创建 `packageRepoStatuses` 条目，并清除该包已有的可重试 error；Skipped 结果写入不可重试 error；Failed 结果写入可重试 error，若此前已经取得 clone URL 则保留该值，达到跨轮预算时改写为不可重试的 `RetryExhausted`。任何 worker 结果都不能覆盖其他组件拥有的字段。
- 同一规范化 URL 和 ref 已有持久化 commitId 时，以该值为基准；本轮解析结果不同则按 `CommitConflict` 跳过，不覆盖已有结果。同一规范化 URL 和 ref 在本轮产生多个结果且没有已有基准时，按稳定索引处理：第一个已接受的 commit 作为本轮基准；后续结果相同则正常接受，不同则按 `CommitConflict` 跳过，禁止后完成的 worker 覆盖先接受的结果。
- 主 goroutine 按 4.4 计算 failure tracker 的候选增量及清理项，用于生成目标 status；计数的提交和丢弃同样遵循 4.4。
- 汇总完成后最多执行一次本轮最终 `/status` 写入；并发阶段不得执行状态写入。若在写入前发现 reconcile context 已取消，则丢弃尚未持久化的合并结果，由后续 List 重新触发。

### 6.3 Sync 流程

单个队列键的 `Sync(ctx, key)` 按以下顺序执行：

1. 通过 API Get 读取最新 Snapshot，404 时静默返回。检查是否为可处理 Snapshot；否则成功返回。
2. 仅当 `phase=Pending` 时，调用 `advancePhase` 以单次 PUT `/status` 推进 `Pending→Processing`（先持久化再进填充，避免内存状态与存储状态不一致；`Processing→Processing` 异常重入跳过本步）。
   - 成功时用 `advancePhase` 返回的服务端 Snapshot 替换当前工作对象，后续处理只能使用其 UID、status 和新 `resourceVersion`。
   - 写入错误按 7.4 处理；成功或确认成功后继续，其他结果结束本周期，不启动包级任务。
3. 通过 `client.GetBuild(namespace, snapshot.name)` 读取同名父 Build，用于确定构建类型和目标包范围。
   - 父 Build 按 DR-16 必然存在；404、5xx、网络错误等读取失败均返回临时错误并限速重试，本周期不得继续解析。
   - 不根据父 Build phase 中止 Snapshot 解析；Build 已进入终态也不改变已经固化的 Snapshot 输入。
   - 父 Build 查询不使用包级 `failureRetryLimit`，不允许失败超限后 fail-open。
4. 获取 PackageRepo 列表：从 `Snapshot.spec.packageRepos` 。
   - `Snapshot.spec.packageRepos` 为空 → 记 type=`InvalidPackageRepos`、reason=`NoPackageRepos` 的 condition，无包可解析 → 直接推进 Active。
5. 确定目标包集合：
   - 仅当 `Build.spec.buildType == "single"` 时，以 `Build.spec.packages` 中去重后的名称作为目标集合，并与 `Snapshot.spec.packageRepos[].name` 求交集。已找到目标进入待解析集合；未找到目标进入 missing set，在完成判定中按逻辑 Skipped 处理，但不得为其创建 `packageRepoStatuses` 条目。
   - missing set 非空时写 type=`TargetPackagesNotFound`、reason=`TargetPackagesNotFound` 的整体 condition，message 使用排序后的缺失包名生成稳定且经过消毒的摘要；missing set 为空时删除已有的同 type condition。
   - `single` 的非目标 PackageRepo 不解析，也不参与完成判定。其他所有非空 buildType 均以全部 PackageRepo 为目标集合、忽略 `Build.spec.packages`，missing set 为空。
   - 若 `single` 的全部目标都缺失，则不启动 worker；写入 condition 后直接推进 Active。
6. 按 6.2 的子状态机解析尚未完成的目标包；已有成功结果或不可重试 error 的包不再解析。并发派发、预算取消和结果合并严格采用 6.2.1 的规则。
7. 汇总结果并应用 4.4 的跨轮失败计数，同时计算最终写入确认后的 `postWriteResult/postWriteErr`。两者必须满足 BaseController 契约：有 error 时 Result 必须为零值。
   - 全部已找到目标包 Resolved 或 Skipped，且所有缺失目标已经按 missing set 记为逻辑 Skipped → 推进 Active。包级错误保留在对应 PackageRepoStatus 中，缺失目标仅保留整体 condition，二者共同作为 Active 快照的质量记录。不得因为部分目标缺失而提前跳过仍需解析的已找到目标。
   - 尚有 Waiting 或可重试 Failed 时保持 Processing；按第八章计算返回值（Waiting 优先）。全部目标完成时返回零值 Result 和 nil。
8. 回写（updateSnapshot，调用 PUT：PUT /status 写 packageRepoStatuses + phase + conditions）。
   - 脏检查：reconcile 入口 DeepCopy 快照，回写前 `reflect.DeepEqual` 对比 status，无变化跳过 PUT。
   - status 无变化时直接提交候选 failureTracker 变更，并返回第 7 步计算的 `postWriteResult/postWriteErr`。
   - 写入结果按 7.4 分类、按第八章决定返回动作。最终写入确认后按 4.4 提交候选计数变更，并返回第 7 步预先计算的结果；不得因写入确认而改变该队列动作。

### 6.4 错误记录

包级错误写入 `status.packageRepoStatuses[repo.name].error`；整体异常使用下表固定的 condition type，不以包名、URL 或错误文本动态生成类型。

| type | reason | 触发时机 |
|------|--------|----------|
| `InvalidPackageRepos` | `NoPackageRepos` | `spec.packageRepos` 为空 |
| `TargetPackagesNotFound` | `TargetPackagesNotFound` | `buildType=single` 的 Build 指定的一个或多个包无法在 `spec.packageRepos` 中找到；缺失目标不创建 `PackageRepoStatus`，仅通过该整体 condition 记录 |

同一 type 同一时刻只保留当前 reason，对应问题消失时删除 condition。包名重复可以通过该名称写入包级 `ValidationFailed` error，不写整体 condition。

| error.code | retryable | 触发时机 |
|------------|-----------|----------|
| `SyncFailed` | `true` | 发布同步任务或查询同步状态发生临时错误，且跨轮预算尚未耗尽 |
| `SyncTimeout` | `true` | git-server 请求超时，或总预算耗尽时任务尚未开始/请求尚未返回，且跨轮预算尚未耗尽；正常的 `Synced=false` 不产生该错误 |
| `ResolveFailed` | `false` | Branch/Tag 指向的 ref 不存在或命令返回确定性失败 |
| `ResolveFailed` | `true` | `ResolveCommit` 遭遇临时网络、服务端或响应错误，且跨轮预算尚未耗尽 |
| `ValidationFailed` | `false` | 不可信输入校验失败、包名重复等确定性输入错误 |
| `CommitConflict` | `false` | 同一 URL 与规范化 ref 得到不同 commitId |
| `RetryExhausted` | `false` | 非确定性失败达到跨轮预算 |

`PackageRepoStatus` 必须满足以下约束：

- 原始仓库地址只保留在 `Snapshot.spec.packageRepos[].url`，`PackageRepoStatus` 不重复保存；规范化 URL 仅用于内部比较和缓存；
- Branch/Tag 在 git-server 确认同步完成后写入其返回的非空 `cloneUrl`；后续解析成功或失败均保留该值；
- Commit 不访问 git-server，`cloneUrl` 保持为空；同步确认前发生的失败也允许 `cloneUrl` 为空；
- 成功：`commitId` 非空且 `error=nil`；
- 失败：`commitId` 为空且 `error!=nil`；
- 尚未处理：map 中不存在该包；
- 禁止同时设置 `commitId` 和 `error`；
- `PackageRepoStatus.error.message` 与 Snapshot condition message 均需去除换行和控制字符，并截断至 200 runes；
- 包解析成功时清空旧 error 并重置跨轮失败计数；可重试 error 在下一轮被新结果替换；不可重试 error 在后续轮次保持不变。

整体 condition 使用 `meta.SetStatusCondition` 写入、`meta.RemoveStatusCondition` 清理。

## 七、并发与一致性

### 7.1 单键串行与共享状态

BaseController 保证同一队列键在一个时刻只由一个 worker 调和；不同 Snapshot 可以并发。跨轮失败计数 map 仍可能被 PollingSource handler 与 worker 并发访问，必须由 `sync.Mutex` 保护，锁内不得执行 API 请求或队列阻塞操作。

### 7.2 乐观并发

Snapshot Controller 写入前通过 API GET 获取最新对象，携带其 resourceVersion 更新 `/status`，不覆盖并发修改。Conflict、删除及鉴权等错误统一见 7.4；结果未知时按 7.4.4 确认原写入意图，不重放旧请求。

### 7.3 与 git-server 的竞态

git-server 为外部服务，可能不可用或响应延迟。Controller 必须：
- git-server 超时归入 failed set，跨轮重试；
- Snapshot 状态收敛不依赖 git-server 返回某次请求的操作 ID；重复同步请求由 git-server 合并，Controller 自身仍须保证状态写入幂等；
- Controller 不直接解释 status HTTP 响应，只使用客户端的 `CheckSynced(originURL, baseline)` 结果；
- `CheckSynced` 的 baseline 判定及 Waiting 迁移遵循 6.2；git-server 的 HTTP、协议与传输错误统一由客户端转换为第二章定义的 `GitServerError`，Controller 只按该分类契约处理。

### 7.4 完整错误矩阵

#### 7.4.1 API GET

| 调用与结果 | 处理 |
|------------|------|
| Snapshot GET 404 | 目标 Snapshot 已不存在，本周期成功结束 |
| Build GET 404 | 违反父 Build 必然存在的不变量，按临时读取错误限速重试 |
| Snapshot GET 2xx 且对象、UID、resourceVersion 合法 | 使用返回对象继续调谐 |
| GET 401/403 | PermanentError，记录鉴权失败指标，不改变健康状态 |
| GET 408、429 或 5xx | 临时错误，`AddRateLimited` |
| GET 网络超时、EOF、连接重置或 DNS/TLS 临时错误 | 临时错误 `AddRateLimited` |
| GET 2xx 但响应为空、解码失败、类型错误或缺少 UID/resourceVersion | 内部临时错误，记录 `unexpected-read-response` 并 `AddRateLimited` |
| Manager context 已取消 | 原样返回 context 错误，由 BaseController 停止 worker，不重入队 |

#### 7.4.2 明确收到写响应

下表适用于 `UpdateSnapshotStatus`。非 2xx 行对应 `WriteRejected`：

| HTTP 结果 | 处理 |
|-----------|------|
| 完整 2xx 且成功响应校验通过 | 本次写入已确认；`advancePhase` 返回服务端对象并继续本轮，最终 `updateSnapshot` 提交候选 failureTracker 变更并返回预先计算的 `postWriteResult/postWriteErr` |
| 2xx 但响应为空、解码失败或成功对象校验失败 | WriteUnknown，进入 7.4.4 |
| 400、405、410、422、其他未单列 4xx | PermanentError，记录请求或 API 契约错误 |
| 401、403 | PermanentError，记录鉴权失败 |
| 404 | Snapshot 已删除，成功结束 |
| 408 | 临时错误，`AddRateLimited` |
| 409、412 | `Requeue=true`，下一周期重新 GET 并重算 |
| 429、503，且 `WriteError.RetryAfter > 0` | 原样返回 WriteError，由 BaseController 执行 `Forget + AddAfter` |
| 429、503，但无合法 RetryAfter | 临时错误，`AddRateLimited` |
| 其他 5xx | 临时错误，`AddRateLimited` |
| 最终 3xx 或其他完整非 2xx 响应 | PermanentError，记录非预期协议错误 |

#### 7.4.3 WriteNotSent

| 原因 | 处理 |
|------|------|
| namespace/name/GVR、UID、resourceVersion 或对象类型非法 | PermanentError；这是调用方或适配器编程错误 |
| 序列化或请求构造因对象内容确定性失败 | PermanentError，记录 client contract 指标 |
| transport 能证明请求未发送的临时 DNS、连接建立、TLS 或连接池错误 | 临时错误，`AddRateLimited` |
| 无法证明是否发送 | Client 不得返回 NotSent，必须归为 WriteUnknown |

#### 7.4.4 WriteUnknown 与确认读取

`WriteUnknown` 包括请求可能已发送但响应超时、EOF、连接重置、响应截断、异常 2xx，以及不能证明 NotSent/Rejected 的其他错误。Controller 必须先确认，不能原样重放请求对象。

发起状态写入前，Controller 必须保存本次写入意图，包括 Snapshot UID，以及 Snapshot Controller 拥有字段的目标值：`status.phase`、`status.packageRepoStatuses` 和 `status.conditions`。写入意图中的 map 和 slice 必须深拷贝，不能引用后续仍可能被修改的对象。确认比较采用语义比较：`packageRepoStatuses` 的 nil map 与空 map 等价；`conditions` 按 `type` 匹配并比较 `status`、`reason` 和 `message`，不依赖切片顺序，也不比较可能被服务端规范化的 `lastTransitionTime`。

若 Manager context 已取消，Controller 不在停止阶段启动确认请求，直接返回 context 错误；未确认的写入由进程重启后的初始 List 和正常调谐收敛。

status 更新结果未知后执行 Snapshot GET。仅当同一 UID 且 Snapshot Controller 拥有的全部目标字段均已达到本次写入意图时，才确认写入成功；否则不得重放旧请求，应以最新对象重新调和。确认成功只表示本次写入已持久化，不直接决定 reconcile 是否结束：`advancePhase` 返回确认 GET 的对象并继续处理，最终 `updateSnapshot` 则提交候选 failureTracker 变更并恢复写入前计算的 `postWriteResult/postWriteErr`。UID 不同或对象不存在时结束旧周期。

| 确认结果 | 处理 |
|----------|------|
| 404 | Snapshot 已消失，成功结束 |
| 同一 UID，`phase`、`packageRepoStatuses` 和 `conditions` 均与写入意图语义等价 | 本次写入意图已经收敛；按调用点继续：中间 phase 写入继续本轮，最终写入返回预先计算的后续动作 |
| UID 不同 | 原 Snapshot 已消失且同名对象已重建，成功结束，不更新新对象 |
| 同一 UID，但任一 Controller 所有字段未达到写入意图 | 不重放旧请求，返回 `Requeue=true`，按最新对象重新调谐 |
| GET 失败 | 严格按 7.4.1 的 GET 矩阵处理 |

## 八、错误与重试

| 结果 | 队列行为 |
|------|----------|
| 对象不存在、无需处理、已经终态 | `ReconcileResult{}, nil`，BaseController Forget |
| `advancePhase` 成功或 GET 确认已达到预期 | 使用服务端返回或确认 GET 得到的 Snapshot 继续本轮，不在此处返回 |
| 最终 `updateSnapshot` 成功或 GET 确认已达到预期 | 提交候选 failureTracker 变更，返回汇总阶段预先计算的 `postWriteResult/postWriteErr` |
| 存在 Waiting 包（包括同时存在可重试失败包） | `ReconcileResult{RequeueAfter: syncRequeueDelay}, nil` |
| 仅存在未超限可重试失败包 | 零值 Result 和临时错误，由 BaseController `AddRateLimited` |
| resourceVersion Conflict | `ReconcileResult{Requeue: true}, nil`，清除旧退避后立即重读 |
| 429/503 且 WriteError 带合法 RetryAfter | 零值 Result 和原始错误，由 BaseController Forget 后 AddAfter |
| 读取或写入临时错误 | 零值 Result 和临时错误，由 BaseController AddRateLimited |
| WriteUnknown | 先按 7.4.4 确认；确认成功后按写入调用点继续或返回预先计算的后续动作，确认未实现则立即重入，确认 GET 失败按读取错误处理 |
| 永久 HTTP、输入或客户端错误 | 零值 Result 和 PermanentError，记录分类指标，不改变 Controller 健康状态 |
| 无法解析对象或违反内部不变量 | 零值 Result 和错误，记录并限速重试；持续失败必须可观测 |

超过 `--controller-max-retries` 后仍不能静默丢弃。框架应记录错误日志和 dropped 指标，并在周期性 resync 时允许对象再次进入队列。

客户端地址、TLS 和认证等静态配置必须在 initializer 阶段完成校验；配置非法时 Controller Manager 启动失败，不得延迟到单个 Snapshot 的 Reconcile 中处理，也不通过 HealthChecker 表达。

## 九、配置与权限

### 9.1 配置项

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--workers` | 2 | worker goroutine 数 |
| `--poll-period` | 30s | PollingSource list 周期 |
| `--snapshot-resolve-workers` | 8 | 单 Snapshot 内包解析并发数 |
| `--snapshot-resolve-budget` | 120s | 单轮解析总预算（budgetCtx） |
| `--snapshot-sync-requeue-delay` | 30s | `CheckSynced` 返回 `Synced=false, err=nil` 的轮内检查耗尽后再次检查的固定延迟 |
| `--snapshot-failure-retry-limit` | 3 | 非确定性失败跨轮重试预算，必须大于 0；`nextCount >= limit` 时写入不可重试的 `RetryExhausted` error 并跳过 |
| `--git-server-addr` | `http://localhost:8080` | git-server 地址 |
| `--git-server-timeout` | 30s | 单次请求超时 |
| `--git-server-retry` | 3 | 客户端固定尝试次数 |
| `--git-server-cache-ttl` | 30s | git-server 同步状态的 L1 缓存 TTL |

### 9.2 最小权限

Snapshot Controller 所需最小权限：

```text
snapshots:        get, list, update
snapshots/status: update
builds:           get
```

权限严格限制在上述操作：Controller 不能创建或删除 Snapshot。

## 十、可观测性

### 10.1 日志字段

日志至少包含：
- `controller`: "snapshot"
- `key`: `{namespace}/{name}`
- `snapshot_uid`: Snapshot UID
- `phase`: 原 phase 和新 phase
- `resourceVersion`: 写入前的 resourceVersion
- `result`: reconcile 结果
- `retries`: 重试次数
- `duration`: reconcile 耗时
- `error`: 错误信息（如有）
- `package_name`: 包名称（解析相关日志）
- `git_server_method`: git-server API 方法

不记录 payload、凭据或完整 packageRepos 列表。

### 10.2 指标

沿用公共 metrics 包的无标签 Counter，以固定指标名区分结果，避免包名或 URL 产生高基数序列。框架提供通用 reconcile 指标，Snapshot 与 git-server 客户端补充：

```text
snapshot_controller_phase_transitions_total
snapshot_controller_conditions_total
snapshot_controller_conflict_requeues_total
snapshot_controller_unknown_writes_total
snapshot_controller_resolve_batches_total
snapshot_controller_resolve_duration_nanoseconds_total
snapshot_controller_resolved_total
snapshot_controller_waiting_total
snapshot_controller_failed_total
snapshot_controller_skipped_total
snapshot_controller_retry_exhausted_total
snapshot_controller_unexpected_git_server_errors_total
snapshot_controller_auth_failures_total
git_server_client_sync_requests_total
git_server_client_status_requests_total
git_server_client_resolve_requests_total
git_server_client_request_failures_total
git_server_client_cache_hits_total
```

Resolved、Waiting、Failed 统计本轮解析结果；阶段、condition、Skipped 和 RetryExhausted 仅在写入成功或 Unknown 确认后统计变更。耗时累计值除以批次数得到平均解析耗时。客户端请求数按业务方法调用统计（包含缓存命中），失败数在内部重试耗尽后统计，不按 HTTP 尝试次数重复累计。状态变更日志仅记录已确认且与原状态不同的结果，同一批包按名称排序输出；重复 resync 不重复输出相同包结果。

### 10.3 日志 reason

| 行为 | 日志 reason |
|------|-------------|
| 推进 Pending → Processing | `PhaseAdvanced` |
| 推进 Processing → Active | `PhaseCompleted` |
| 包解析成功 | `PackageResolved` |
| 包解析失败（确定性） | `PackageSkipped` |
| 包解析失败（非确定性） | `PackageFailed` |
| 重试超限 | `RetryExhausted` |
| git-server 不可达 | `GitServerUnavailable` |
| commit 冲突 | `CommitConflict` |
| 无 packageRepos | `NoPackageRepos` |

相同 reason 的日志必须限速或聚合，避免 git-server 不可用时产生日志风暴。

## 十一、测试要求

### 11.1 单元测试

- Active Snapshot 不处理；
- Pending Snapshot 推进到 Processing；
- Processing Snapshot 继续填充（异常重入）；
- 已被删除的 Snapshot 静默返回；
- PackageRepo 已有 commitId 或不可重试 error 时不重复解析，已有可重试 error 时继续解析；
- `ref.type=Commit` 时跳过 git-server 解析并直接采用 `ref.value`；
- 包级确定性失败写入不可重试 error 并跳过；无 packageRepos 写整体 condition；
- `buildType=single` 部分目标缺失时，缺失目标写整体 condition 且不创建包级状态，已找到目标仍完成解析后才推进 Active；全部目标缺失时不启动 worker 并直接推进 Active；其他 buildType 即使填写 `spec.packages` 也解析全部仓库；
- 非确定性失败（5xx/网络/超时）跨轮重试，超限后跳过；
- `CheckSynced` 返回 `Synced=false, err=nil`，轮内检查耗尽后返回 `RequeueAfter=syncRequeueDelay`，不写包级 error、不增加失败预算；
- 全部目标包成功或跳过后推进 Active；
- WriteError 三分类处理（WriteRejected/WriteUnknown/WriteNotSent）；
- `advancePhase` 正常 2xx 或 WriteUnknown 确认成功后继续本轮解析，不提前返回；
- 最终 `updateSnapshot` 正常 2xx 或 WriteUnknown 确认成功后，均提交候选 failureTracker 变更并恢复相同的 `postWriteResult/postWriteErr`；
- PermanentError 清除退避；
- Conflict 重新入队（`Requeue=true`）；
- 404 静默返回；
- FakeClock 推进时间；
- `go test -race` 无竞态；
- 固定 worker 数不超过 `min(resolveWorkers, 待解析包数)`，且不会为每个包创建 goroutine；
- budgetCtx 取消后 worker 停止领取任务并全部退出；已经取得 `Synced=false, err=nil` 的任务返回 Waiting，其余未完成任务各生成一次 `SyncTimeout`；
- Branch/Tag 成功同步后记录 git-server CloneURL，Commit 的 CloneURL 为空；原始仓库地址仅从 `spec.packageRepos` 读取；
- 跨轮失败计数 map 并发安全；
- condition 消毒（去除换行、截断至 200 runes）；
- Snapshot 整体 condition type 只产生 `InvalidPackageRepos` 或 `TargetPackagesNotFound`，包名不会进入 condition type；
- rev-parse 输出为空、格式非法、分支或标签不存在的确定性失败处理；
- L1 缓存命中减少 git-server 请求；
- budgetCtx 超时时，正常等待同步的任务进入 Waiting，尚未开始或请求尚未返回的任务进入 `SyncTimeout` Failed；
- failureTracker 在 UID 变化、Snapshot 404/Active、包成功、Waiting、确定性失败及重试超限场景下按 4.4 的规则清理，且写入冲突不会错误提交计数变更；

### 11.2 集成测试

- 初始 List 中已有 Pending Snapshot 时可以收敛；
- 初始 List 中已有 Processing Snapshot 时继续填充；
- git-server 不可达时跨轮重试；
- git-server 恢复后成功解析；
- 部分包成功部分失败时正确推进；
- Build GET 404 时不继续解析，并按临时错误重试；
- controller-manager 重启后从 Pending/Processing 恢复；
- PollingSource 轮询周期正确；
- L1 缓存命中减少 git-server 请求；
- 同名 Snapshot 删除重建时不误更新新对象；
- 认证失败不产生紧密重试，也不由单个对象错误改变 Controller 健康状态；
- 临时网络错误能够恢复；

## 十二、开发顺序

1. **前置准备**：
   - 确认 `SnapshotStatus.Conditions` 字段已扩展（已完成）
   - 执行 `api/hacks/update-codegen.sh` 生成 DeepCopy 方法

2. **git-server 客户端**（`pkg/clients/gitserver/`）：
   - `validate.go`：不可信输入白名单校验
   - `client.go`：实现 Controller 接口 `PublishSyncTask`、`CheckSynced`、`ResolveCommit`；内部封装 GetSyncStatus、ExecuteCommand（重试 3 次，30s 超时）
   - `revparse.go`：rev-parse 输出解析（提取 40 字符十六进制 SHA）
   - `cache.go`：L1 TTL 进程内缓存包装

3. **Snapshot Controller**（`pkg/controllers/snapshot/`）：
   - `conditions.go`：condition upsert/清理/消毒
   - `update.go`：advancePhase + updateSnapshot
   - `resolve.go`：两阶段并发解析（固定 worker pool + jobs/results 通道 + budgetCtx）
   - `reconcile.go`：reconcile 入口、父 Build 读取、目标范围判定和错误返回约定
   - `controller.go`：Initializer、事件注册、Client 接口定义

4. **集成与配置**：
   - 注册 `snapshot` initializer、命令行参数和指标
   - Snapshot Controller 没有独立关键后台循环，不实现自定义 HealthChecker，使用框架默认 ping checker

5. **测试**：
   - 单元测试（race、边界情况）
   - 集成测试（git-server 交互）
   - 完成后启用默认 `--controllers=*` 中的 Snapshot Controller
