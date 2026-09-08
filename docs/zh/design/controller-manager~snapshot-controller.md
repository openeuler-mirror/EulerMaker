# Snapshot Controller 设计

## 一、定位与范围

Snapshot Controller 是 `controller-manager` 中负责将 `Snapshot` 资源沿 `Pending → Processing → Active` 单向推进的控制器，完成 commit 解析。它是 build 流程的前置环节，与 build_controller、build_info_controller 协同工作。

首版职责：

- 识别 `status.phase ∈ {Pending, Processing}` 的 Snapshot 资源；
- 调用 git-server API 解析 `PackageRepo` 的 commit；
- 推进 Snapshot 状态：`Pending → Processing → Active`；
- 记录解析失败原因到 `status.conditions`；
- 保证状态更新幂等，并在并发更新时以 apiserver 中的最新对象为准；
- 暴露必要的结构化日志和指标。

首版不负责：

- 不创建或删除 Snapshot 资源（由 build_controller 负责）；
- 不修改 `Snapshot.spec.packageRepos`（从 Project 继承）；
- 不负责 git-server 的部署、运维和镜像清理策略；
- 不处理 Project 删除的级联清理；
- 不维护 Build、BuildInfo、RpmRepo 等上层资源状态；
- 不负责 `PackageRepo.commitId` 的用户显式指定验证（DR-8）；
- 不实现 leader election（首版单副本部署）。

## 二、依赖与组件边界

Snapshot Controller 运行在现有 `controller-manager` 框架内，建议实现目录为：

```text
components/controller-manager/pkg/controllers/snapshot/
  controller.go       # 初始化、事件注册和 Sync
  reconcile.go        # reconcile 入口、parentAbortGuard
  resolve.go          # 两阶段并发解析（goroutine 池 + 信号量 + budgetCtx）
  conditions.go       # condition upsert/清理/消毒
  update.go           # advancePhase + updateSnapshot
  metrics.go
```

依赖关系如下：

```text
Snapshot PollingSource ---> Snapshot Controller queue ---> Snapshot GET/PUT
                                       |
Project ------------------------------> packageRepos 来源（继承到 Snapshot.spec.packageRepos）
                                       |
Build --------------------------------> parentAbortGuard / 单包判定
                                       |
git-server HTTP API -------> Commit 解析
```

**字段所有权**：
- `Snapshot.spec.packageRepos`：从 `Project.spec.packageRepos` 继承
- `Snapshot.status.specCommits`：由 snapshot_controller **写入**（commit 解析结果）
- `Snapshot.status.phase`：由 snapshot_controller **写入**
- `Snapshot.status.conditions`：由 snapshot_controller **写入**

Snapshot 不支持 watch，应使用 `PollingSourceFactory` 创建 Source。事件处理器必须在 `Manager.Run` 之前注册；控制器启动后不得再注册 handler。Manager 完成全部 Source 的首次同步后才启动 worker，因此 `Sync` 可以依赖本地缓存已经完成初始 List。

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

// RepoStatus 对应 git-server 的 RepositoryResponse
type RepoStatus struct {
    Key        string            `json:"key"`
    CloneURL   string            `json:"clone_url,omitempty"`
    SyncTime   *time.Time        `json:"sync_time,omitempty"`
    RetryCount int               `json:"retry_count,omitempty"`
    Error      *RepositoryError  `json:"error,omitempty"`
}

// RepositoryError 对应 git-server 的 RepositoryError
type RepositoryError struct {
    Code      string `json:"code"`
    Message   string `json:"message"`
    Retryable bool   `json:"retryable"`
}

type GitServerClient interface {
    // PublishSyncTask 调用 POST /api/v1/repo/sync，注册仓库同步任务
    PublishSyncTask(ctx context.Context, originURL string) error
    // GetSyncStatus 调用 POST /api/v1/repo/status，查询同步状态
    // 返回 RepoStatus，SyncTime 为 nil 表示尚未成功同步
    GetSyncStatus(ctx context.Context, originURL string) (*RepoStatus, error)
    // ExecuteCommand 调用 POST /command，执行只读 Git 命令
    // originURL 为仓库完整 URL（如 https://gitee.com/src-openeuler/vim.git），command 为命令数组
    ExecuteCommand(ctx context.Context, originURL string, command []string) (stdout string, err error)
}
```

`UpdateSnapshotStatus` 必须验证成功响应非 nil、UID 与请求对象一致、resourceVersion 非空。任一成功响应异常都包装为 `WriteUnknown`。

所有方法返回的错误必须能通过下列方式读取 Outcome，并保留 `apierrors.IsConflict`、`IsNotFound`、`IsUnauthorized`、`IsForbidden` 和 `IsTooManyRequests` 判断能力：

```go
var writeErr *client.WriteError
if errors.As(err, &writeErr) {
    outcome := writeErr.Outcome
}
```

### git-server API 约定

git-server API 使用动作式 POST，仓库 URL 放在 JSON body 中，不出现在 query string 和常规访问日志中。

| 端点 | 方法 | 用途 | 本控制器调用参数 |
|------|------|------|------------------|
| `/api/v1/repo/sync` | POST | 注册仓库同步任务 | `{"origin_url": <specUrl>}` |
| `/api/v1/repo/status` | POST | 查询同步状态（是否已就绪） | `{"origin_url": <specUrl>}` |
| `/command` | POST | 执行只读 Git 命令 | `{"repo": <originURL>, "command": ["git-rev-parse", "<ref>"]}` |

**关键语义**：
- POST `/api/v1/repo/sync` 注册后，git-server 异步执行 clone/fetch，**首次查询可能返回 404**。
- POST `/api/v1/repo/status` 返回的 `RepoStatus.SyncTime` 仅表示该仓库在 git-server 本地已完成同步，**不代表已包含最新 commit**——必须通过 baseline 时间戳判断。
- `/command` 只允许特定的只读 Git 命令（`git-rev-parse`、`git-show`、`git-log`、`git-ls-tree`），从本地镜像获取，**不会访问远端**，避免限流。需要确保同步完成（`RepoStatus.SyncTime != nil && *RepoStatus.SyncTime >= baseline`）后再执行。
- 本控制器使用 `git-rev-parse` 获取本地镜像中分支/标签的 commit SHA，替代不在允许列表中的 `git-ls-remote`。必须使用完整 ref 路径（如 `refs/heads/master`、`refs/tags/v1.0`）。
- `/command` 接口的 `repo` 参数使用仓库完整 URL（如 `https://gitee.com/src-openeuler/vim.git`），git-server 内部会将其转换为仓库 key。
- 响应中的 `clone_url` 是 git daemon 提供的只读 clone 地址（由 `--clone-base-url` 配置生成），调用方可以直接 `git clone clone_url`。

**请求/响应格式**：
```json
// POST /api/v1/repo/sync - 创建同步任务
// 请求：
{"origin_url": "https://gitee.com/src-openeuler/vim.git"}
// 响应：202 Accepted
// 首次同步尚未成功时，响应中没有 sync_time 和 clone_url：
{
  "key": "gitee.com/src-openeuler/vim.git"
}
// 已成功同步过时，响应中包含 sync_time 和 clone_url：
{
  "key": "gitee.com/src-openeuler/vim.git",
  "clone_url": "git://git-server:9418/gitee.com/src-openeuler/vim.git",
  "sync_time": "2026-09-09T02:30:00Z"
}

// POST /api/v1/repo/status - 查询同步状态
// 请求：
{"origin_url": "https://gitee.com/src-openeuler/vim.git"}
// 场景 1：仓库从未出现、已删除或尚无可用本地副本 → 404 Not Found
// 场景 2：调用过 Sync 但尚未成功同步 → 200 OK（响应中没有 sync_time 和 clone_url）
{
  "key": "gitee.com/src-openeuler/vim.git"
}
// 场景 3：调用过 Sync 且成功同步过 → 200 OK（响应中有 sync_time 和 clone_url）
{
  "key": "gitee.com/src-openeuler/vim.git",
  "clone_url": "git://git-server:9418/gitee.com/src-openeuler/vim.git",
  "sync_time": "2026-09-09T02:30:00Z"
}
// 场景 4：最近一次同步失败 → 200 OK（响应中包含 error 和 retry_count）
{
  "key": "gitee.com/src-openeuler/vim.git",
  "clone_url": "git://git-server:9418/gitee.com/src-openeuler/vim.git",
  "sync_time": "2026-09-09T02:30:00Z",
  "retry_count": 2,
  "error": {
    "code": "FetchFailed",
    "message": "fetch 失败",
    "retryable": true
  }
}

// POST /command - 执行只读 Git 命令（从本地镜像获取，不访问远端）
// 请求：
{
  "repo": "https://gitee.com/src-openeuler/vim.git",
  "command": ["git-rev-parse", "refs/heads/master"]
}
// 成功响应：200 OK
{
  "stdout": "0123456789abcdef\n",
  "stderr": "",
  "exit_code": 0,
  "stdout_truncated": false,
  "stderr_truncated": false
}
// 命令失败（ref 不存在）：422 Unprocessable Entity（仍返回 CommandResponse）
{
  "stdout": "",
  "stderr": "fatal: ambiguous argument 'refs/heads/nonexistent': unknown revision\n",
  "exit_code": 128,
  "stdout_truncated": false,
  "stderr_truncated": false
}
// 命令超时：504 Gateway Timeout（仍返回 CommandResponse，exit_code=-1）
// 输出超限：413 Content Too Large（仍返回 CommandResponse，exit_code=-1）
```

**响应字段说明**：
- `key`：仓库 key，由 URL 转换算法生成
- `clone_url`：git daemon 提供的只读 clone 地址（由 `--clone-base-url` 配置生成）
- `sync_time`：RFC3339 格式的最近一次成功同步时间
- `retry_count`：当前重试次数（仅 status 响应）
- `error`：最近一次后台操作错误（仅 status 响应，包含 `code`、`message`、`retryable`）

**错误响应格式**：
```json
{
  "code": "RepositoryNotFound",
  "message": "仓库从未出现、已删除或尚无可用本地副本"
}
```

| HTTP 状态码 | code | 说明 |
|-------------|------|------|
| 400 | `InvalidRequest` | 请求参数错误（如 command 格式错误、URL 非法） |
| 404 | `RepositoryNotFound` | 仓库从未出现、已删除或尚无可用本地副本 |
| 422 | `CommandFailed` | Git 命令执行失败（仅 /command） |
| 413 | `OutputLimitExceeded` | 命令输出超过限制（仅 /command） |
| 500 | `InternalError` | 服务器内部错误 |
| 503 | `NotReady` | 服务尚未 ready |
| 504 | `CommandTimeout` | 命令超时（仅 /command） |

**不可信输入校验（客户端安全职责）**：`specUrl`、`branch`、`<ref>` 来自 Snapshot/Project 资源，属不可信输入。`pkg/gitserver` 在组装请求参数前统一校验：
- `specUrl`：仅允许 `http(s)://` 或 `git@` SCP-like 形式（`net/url` 解析 + 白名单校验）
- `branch`/`gitTag`：仅允许 `[a-zA-Z0-9._/-]` 且拒绝以 `-` 开头、含 `..`、空白与 shell 元字符
- `<ref>`：允许 `[a-zA-Z0-9._/^-{}]`（支持 `refs/tags/v1.0^{}` 语法用于 annotated tag deref），拒绝空白与 shell 元字符

违反即返回哨兵错误 `ErrGitValidation`（确定性失败，不重试）。防止仓库名/分支名注入（如 `--upload-pack=...`）。

## 三、字段所有权

多个组件会更新 Snapshot，必须以字段所有权限制写冲突。

| 组件 | 拥有的 Snapshot 字段 |
|------|---------------------|
| build_controller | 创建 Snapshot，写 `metadata`、`spec.packageRepos`（从 Project 继承） |
| snapshot_controller | 写 `status.specCommits`、`status.phase`、`status.conditions` |
| 其他组件 | 不写 Snapshot |

**禁止修改的字段**：
- `metadata.name`、`metadata.namespace`（创建后不可变）
- `metadata.labels`、`metadata.annotations`（由创建方管理）
- `spec.packageRepos`（从 Project.spec.packageRepos 继承，snapshot_controller **只读不修改**）

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
- `Update`：`phase` 变化、`resourceVersion` 变化或 `conditions` 变化时入队；仅有无关字段变化不重复入队。PollingSource resync 事件仍需入队，作为恢复兜底。
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

### 4.3 Project 事件（PollingSource）

Project 变化不直接入队 Snapshot，而是在 Snapshot reconcile 时按需读取最新 Project。这避免了 Project 更新导致所有关联 Snapshot 反复入队。

### 4.4 周期性重同步

PollingSource 默认 30s 轮询，作为丢事件后的兜底。`RequeueAfter` 用于延迟重试（如 git-server 不可用），不替代轮询。处于 git-server 重试期的 Snapshot 返回 `ReconcileResult{RequeueAfter: remaining}`，由 BaseController 在相应截止时间重新入队；进程重启后，初始 List 事件会重建计划。

### 4.5 跨轮失败计数

控制器维护并发安全的跨轮失败计数 map：

```go
type failureTracker struct {
    mu       sync.RWMutex
    counts   map[string]int  // key = snapshot UID + 包名
}
```

- 计数仅存在于进程内，控制器重启清零、重新给予预算；
- 包解析成功时清除该包计数；
- 计数 ≥ `failureRetryLimit` 时记 condition（`RetryExhausted`）并跳过；
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
| DR-2 | git-server 同步与 commit 获取 | **两阶段：先同步仓库，再从本地镜像获取 HEAD** | Phase 1: 发布同步任务（POST `/api/v1/repo/sync`）让 git-server 异步 clone/fetch 建立本地镜像；Phase 2: 等待同步完成（`RepoStatus.SyncTime != nil && *RepoStatus.SyncTime >= baseline`）后，通过 `/command` 执行 `git-rev-parse` 从本地镜像获取 commit SHA，避免直接访问远端造成限流。必须使用完整 ref 路径（如 `refs/heads/<branch>`、`refs/tags/<tag>`） |
| DR-3 | 本地镜像同步完成判定 | **baseline 时间戳比较** | POST `/api/v1/repo/status` 返回的 `RepoStatus.SyncTime`（`*time.Time`，RFC3339 解析）字段与 Snapshot 创建时间比较，仅当 `RepoStatus.SyncTime != nil && *RepoStatus.SyncTime >= baseline` 才认为镜像已同步到本次所需状态 |
| DR-4 | 失败原因记录位置 | **Snapshot.status.conditions**（type = repo.name） | 不污染 PackageRepo 的 name 字段，保留原始语义；condition.message 记录失败原因；`PackageRepo` 无 `status` 字段 |
| DR-5 | 部分失败处理 | **允许部分成功** | 成功的 repo 写入 specCommits；失败的 repo 按 DR-6 分类处理（确定性当轮跳过，非确定性未超限下轮仅重试失败项） |
| DR-6 | 失败分类与重试预算 | **确定性失败当轮跳过；非确定性失败跨轮重试，预算 `failureRetryLimit`（默认 3，可配），超限跳过** | 确定性失败（校验失败/分支不存在/commit 冲突/无 packageRepos 等）重试无意义，当轮记 condition 跳过；非确定性失败（5xx/网络/同步超时/预算耗尽等）跨轮重试，超限后记 condition（reason=`RetryExhausted`）跳过。全部包"成功或跳过"即推进 Active，Snapshot 不无限停留 Processing |
| DR-7 | Snapshot 命名 | `<build-name>` | 与所属 Build 同名，通过 `metadata.name` 直接定位 Snapshot 与反查父 Build，无需 label 关联；由 build_controller 创建时确定 |
| DR-8 | commitId 显式指定时跳过解析 | **跳过解析，直接采用** | `PackageRepo.commitId` 为"用户显式指定"语义，显式指定时跳过整个解析流程 |
| DR-9 | commit 冲突（同 repo 不同包） | **跳过 + condition 冲突告警** | 同一 specUrl+branch 已有 commitId 且与本次解析结果不同时，不覆盖写入，记录 condition（type=`<repo.name>`，reason=`CommitConflict`）提示冲突 |
| DR-10 | git-server 客户端 | **L1 TTL 进程内缓存 + 重试** | 共享包 `pkg/gitserver` 分两层：底层 `PublishSyncTask` / `IsSynced` / `ExecCommand`（重试 3 次）；L1 TTL 缓存包装层 `CheckSynced`（= IsSynced + 缓存）/ `GetRemoteHeadCommit`（= ExecCommand 执行 git-rev-parse + 输出解析 + 缓存），TTL 可配；不提供主动失效接口，依赖短 TTL 收敛。本控制器调用包装层 |
| DR-11 | `IsSynced` 语义 | **双阶段：存在性 → baseline 时效性** | 第一阶段判断本地镜像存在且可达；第二阶段判断 `RepoStatus.SyncTime != nil && *RepoStatus.SyncTime >= baseline`；对外经 `CheckSynced`（IsSynced 的 L1 TTL 缓存包装）提供，两阶段结果同键缓存 |
| DR-12 | build_tag 来源 | **build_env_macros 注入** | Build 的 `build_env_macros` 包含 build_tag 宏，由 build_controller 在创建 Build 时从 Project 继承 |
| DR-13 | 并发解析 | **goroutine 池 + 信号量限流（默认 8）+ 总预算检查** | 每包三阶段（发布任务 → 轮询就绪 → `rev-parse`）并发执行，总预算 `resolveBudget`（默认 120s）通过 `context.WithTimeout` 控制，预算耗尽时未完成包归入 failed set 计跨轮失败预算（DR-6） |
| DR-14 | 单包轮内重试上限 | **轮内内联重试 8 次**（1s/2s/4s/8s，之后按 8s 封顶，可被 ctx 中断） | 单轮内最多 8 次轮询，5xx/网络异常上限 5 次；轮内超限归入 failed set，计入跨轮失败预算（DR-6），预算未耗尽下轮续跑 |
| DR-15 | 单包构建 | **不关注其他包仓库，不解析其 commit** | single/specified 单包构建时，仅解析该包对应 spec 的 commit，其余包仓库跳过 |

### 5.3 幂等性

| 场景 | 幂等保证 |
|------|----------|
| 重复处理同一 Snapshot | 无副作用：`specCommits` 已有条目的包不重复解析（快照语义：分支移动不追溯），最终状态一致 |
| 部分 repo 解析失败 | 成功 repo 的 commitId 已写入 `specCommits`；确定性失败/重试超限的包记 condition 后跳过不再重试；未超限的可重试失败包下轮仅重试该项 |
| 控制器重启 | 从 `Pending` / `Processing` 状态恢复（HandlerFuncs 放行两者），继续未完成工作；`Processing` 为异常重入，保持不回退（DR-1） |

## 六、Reconcile 流程

### 6.1 状态机

`Snapshot.status.phase` 取值：`Pending` / `Processing` / `Active`。推进路径 `Pending → Processing → Active` 单向，**禁止回退**（`Active → 任何`、`Processing → Pending` 均非法）；`Processing → Processing` 为异常重入，保持继续填充（DR-1）。**不设 `Failed` 终态**——失败经 conditions 表达，确定性失败与重试超限的包被跳过后 Snapshot 仍推进 `Active`（DR-6）。

```
            build_controller 在 Prepared 阶段创建 Snapshot
                            ↓
                      ┌─────────┐
                      │ Pending │  ← 初始态，repo 输入待处理
                      └────┬────┘
                           │ snapshot_controller reconcile 取出后
                           │ advancePhase 单 PUT /status
                           ▼
            ┌───────── │ Processing │ ─────────────┐──────────┐
            │          │  填充进行中（异常重入     │          │
            │          │  继续填充，不回退 DR-1）  │          │
            │          └────┬──────────────────────┘          │
            │               │                                 │
            │               │ 全部目标包 Resolved 或 Skipped，│
            │               │ updateSnapshot PUT /status      │
            │               │ 写 specCommits + phase +        │
            │               │ conditions                      │
            │               ▼                                 │
            │          ┌────────┐                             │
            │          │ Active │  ← 终态（HandlerFuncs       │
            │          └────────┘    不放行，不再入队）       │
            │                                                 │
            └────── 保持 Processing（自循环）─────────────────┘
              · 存在未超限的可重试失败包（非确定性失败，跨轮预算未耗尽，下轮续跑，DR-6）
              · budgetCtx 总预算耗尽：未完成包计入失败预算，未超限下轮续跑（B-4）
```

| 状态 | 含义 | 进入条件 | 外出条件 |
|------|------|----------|----------|
| `Pending` | 初始态，repo 输入待处理 | build_controller 创建 Snapshot 时置入 | reconcile 取出后经 advancePhase 单 PUT /status → `Processing` |
| `Processing` | 填充进行中（异常重入保持，不回退） | `Pending → Processing` | 全部目标包 Resolved 或 Skipped（确定性失败/重试超限）→ `Active`；存在未超限可重试失败保持（自循环） |
| `Active` | 终态，目标包均已处理 | 单轮 reconcile 汇总全部包成功或跳过 | 无（终态；不再入队） |

### 6.2 单包 commit 解析子状态机（reconcile 内瞬时状态，不持久化）

单轮 reconcile 中每个 PackageRepo 的三阶段解析流程。瞬时状态仅存在于当轮内存，跨轮不保留——下轮从头部重放，靠幂等性保证无副作用累积。

```
┌────────────┐  PublishSyncTask 成功  ┌─────────┐  CheckSynced 达标     ┌───────────┐  rev-parse 成功  ┌──────────┐
│ PendingSync │ ───────────────────→ │ Syncing │ ───────────────────→ │ Resolving │ ──────────────→ │ Resolved │
└────────────┘                       └─────────┘  (SyncTime!=nil&&>=baseline) └───────────┘                 └──────────┘
      │失败（非确定）                       │未就绪/404：轮内内联重试 ≤8 次    │校验失败 / 分支不存在 / commit 冲突（确定性）
      ▼                                     ▼（异常上限 5 次，DR-14）        ▼
┌──────────────────────────────────┐   ┌───────────────────────────────────────────────────────────────┐
│ Failed（可重试，计跨轮预算）        │   │ Skipped（终态分流，不再重试）                                     │
│ 下轮重试；连续失败 ≥ failureRetryLimit│   │ · 确定性失败当轮进入（ValidationFailed/ResolveFailed/CommitConflict）│
│ → 记 condition（RetryExhausted）→ Skipped│   │ · 非确定性失败重试超限进入（RetryExhausted）                  │
└──────────────────────────────────┘   └───────────────────────────────────────────────────────────────┘
```

**注意**：status 接口返回 404 表示仓库从未出现、已删除或尚无可用本地副本，应调用 sync 接口后继续轮询；返回 200 但 `RepoStatus.SyncTime == nil` 表示尚未成功同步，继续轮询。

| 瞬时状态 | 含义 | 迁移条件 |
|----------|------|----------|
| `PendingSync` | 待发布同步任务 | `PublishSyncTask` 成功 → `Syncing`；失败（非确定）→ `Failed` |
| `Syncing` | git-server 本地镜像同步中 | status 返回 200 且 `RepoStatus.SyncTime != nil && *RepoStatus.SyncTime >= baseline` → `Resolving`；status 返回 404 或 200 但 `SyncTime == nil` → 轮内内联重试；轮内超限 → `Failed` |
| `Resolving` | 本地镜像 HEAD 解析中 | `rev-parse` 成功且无冲突 → `Resolved`；校验失败/分支不存在/commit 冲突（确定性）→ `Skipped`；解析 I/O 失败（非确定）→ `Failed` |
| `Resolved` | commit 已解析 | 组装 `SpecCommit`，清除该包历史 condition 与跨轮失败计数，进入当轮汇总 |
| `Failed` | 本轮失败（可重试） | 归入 failed set，跨轮连续失败计数 +1；计数 ≥ `failureRetryLimit` → 记 condition → `Skipped` |
| `Skipped` | 已跳过（不再重试） | 终态分流：condition 已持久化该包的确定性失败 / `RetryExhausted` 记录，后续轮次直接归入 skipped set |

### 6.3 Sync 流程

单个队列键的 `Sync(ctx, key)` 按以下顺序执行：

1. 通过 API Get 读取最新 Snapshot，404 时静默返回。检查是否为可处理 Snapshot；否则成功返回。
2. 仅当 `phase=Pending` 时，advancePhase 单 PUT `/status` 推进 `Pending→Processing`（先持久化再进填充，避免内存状态与存储状态不一致；`Processing→Processing` 异常重入跳过本步）。
3. parentAbortGuard：client.GetBuild（metadata.name，Snapshot 与 Build 同名）。
   - 查询失败（5xx）→ 返回 error 退避重试；连续失败轮次 ≥ `failureRetryLimit` → 记 condition `BuildQueryFailed` 后 fail-open 继续。
   - 父 Build 不存在（404）/ Aborted / Aborting → 幂等写入 condition `ParentAborted`（观测标记，不阻断），随后继续正常解析与推进。
   - 父 Build 正常 → 继续；若存在历史 `ParentAborted` / `BuildQueryFailed` condition 则清除。
4. 获取 PackageRepo 列表：从 `Snapshot.spec.packageRepos` 。
   - `Snapshot.spec.packageRepos` 为空 → 记 condition `NoPackageRepos`（确定性失败），无包可解析 → 直接推进 Active。
5. 单包构建判定：`Build.spec.buildType = single/specified` → 仅解析 `Build.spec.packages` 对应 repo；其他 → 解析全部 packageRepos。
6. 并发解析（goroutine 池：每包一个 goroutine + `chan struct{}` 信号量限流 `resolveWorkers=8` + `sync.WaitGroup` 汇合；总预算 `budgetCtx = context.WithTimeout(resolveBudget)`）。
   - 对每个 PackageRepo：
     - 已被跳过（conditions 中该包存在确定性失败 / RetryExhausted 记录）→ 不再重试，直接归入 skipped set。
     - `specCommits` 已有该包条目（前轮已解析成功）→ 不重复解析，直接归入 resolved set。
     - `commitId` 已显式指定 → 跳过整个解析流程，直接视为 Resolved。
     - Phase 1: 发布同步任务（POST `/api/v1/repo/sync`）→ 失败（5xx/网络）→ 归入 failed set。
     - Phase 2: 轮询同步状态（POST `/api/v1/repo/status`, L1 TTL 缓存）→
       - 404（仓库从未出现、已删除或尚无可用本地副本）→ 调用 Phase 1 后继续轮询
       - 200 但 `RepoStatus.SyncTime == nil`（尚未成功同步）→ 轮内内联重试（最多 8 次）
       - 200 且 `RepoStatus.SyncTime != nil`（已成功同步）→ 检查 `*RepoStatus.SyncTime >= baseline` → 进入 Phase 3
       - 轮内超限 → 归入 failed set
     - Phase 3: 获取本地镜像 HEAD（POST `/command`, L1 TTL 缓存）→ 解析成功且无冲突 → 组装 SpecCommit；输入校验失败/分支不存在/commit 冲突（确定性）→ 记 condition + skipped set；解析 I/O 失败（非确定）→ 归入 failed set。
   - budgetCtx 超时 → 未完成包归入 failed set，已完成的写入 specCommits。
7. 汇总结果（失败分类与重试预算）。
   - failed set 各包跨轮连续失败计数 +1（内存计数，key = snapshot UID + 包名）：计数 ≥ `failureRetryLimit` → 记 condition（RetryExhausted）→ 移入 skipped set；计数 < limit → 保持可重试，下轮续跑。
   - 全部目标包 Resolved 或 Skipped → 推进 Active：全部成功（无跳过）→ 清除全部历史失败 condition；存在跳过包 → 保留其 condition 作为 Active 快照的质量记录。
   - 仍有包可重试未超限 → 保持 Processing，下轮续跑（部分成功的 specCommits 已增量落盘）。
8. 回写（updateSnapshot，调用 PUT：PUT /status 写 specCommits + phase + conditions）。
   - 脏检查：reconcile 入口 DeepCopy 快照，回写前 `reflect.DeepEqual` 对比 status，无变化跳过 PUT。
   - 409 冲突 → 重新 GET 合并重试，上限 `updateConflictRetry`（默认 3）。
   - 404 → 静默返回（已被外部删除）。
   - 5xx → 保持 phase=Processing，返回 error 退避重试。
9. 返回 nil（等下一轮 poll，仅 phase ∈ {Pending, Processing} 会再次入队）。

### 6.4 condition 目录

`Snapshot.status.conditions` 采用 `metav1.Condition` 结构。`type` 使用失败 repo 的 `name`（如 `NoPackageRepos` 等系统级 type 用 PascalCase）；`status` 恒为 `True`（表示"该失败情形当前成立"）；`reason` 为机器可读分类；`message` 为消毒后的失败详情。

| type | reason | 失败分类 | 触发时机 |
|------|--------|----------|---------|
| `<repo.name>` | `SyncFailed` | 非确定性（计跨轮预算） | Phase 1 发布同步任务失败 |
| `<repo.name>` | `SyncTimeout` | 非确定性（计跨轮预算） | Phase 2 轮内轮询就绪超限（8 次内未就绪） |
| `<repo.name>` | `ResolveFailed` | **确定性（当轮跳过）** | Phase 3 `git-rev-parse` 失败：分支/标签不存在 |
| `<repo.name>` | `ValidationFailed` | **确定性（当轮跳过）** | 不可信输入校验失败（`ErrGitValidation`） |
| `<repo.name>` | `CommitConflict` | **确定性（当轮跳过）** | DR-9：同 specUrl+branch 已解析出不同 commitId |
| `<repo.name>` | `RetryExhausted` | 非确定性超限后转跳过 | 跨轮连续失败 ≥ `failureRetryLimit`（默认 3，可配） |
| `NoPackageRepos` | `NoPackageRepos` | **确定性（记后推进 Active）** | Snapshot 与 Project 兜底均无 packageRepos |
| `ParentAborted` | `ParentAborted` | 观测标记（不阻断） | 父 Build 不存在 / `Aborted` / `Aborting` |
| `BuildQueryFailed` | `RetryExhausted` | 观测标记（fail-open 继续） | 父 Build 查询失败（5xx）重试超限 |

**写入与清理规则**：
- 写入经 `meta.SetStatusCondition`（status 未变化保留 `lastTransitionTime`，幂等 upsert）。
- `message` 写入前消毒：去除换行/控制字符单行化，截断至 200 runes，防 condition 膨胀与日志注入。
- 某 repo 本轮解析成功 → `meta.RemoveStatusCondition` 清除其历史失败 condition（同 type），并重置其跨轮失败计数。
- 推进 Active 时：全部成功（无跳过）→ 清除全部历史失败 condition；存在跳过包 → 保留其 condition 作为 Active 快照的质量记录。
- 父 Build 恢复正常（存在且非 `Aborting`/`Aborted`）→ 清除 `ParentAborted` condition。
- 父 Build 查询恢复成功 → 分别清除 `BuildQueryFailed` condition。

### 6.5 边界情况处理

| 编号 | 边界情况 | 处理策略 |
|------|----------|----------|
| B-1 | **git-server 不可达** | 非确定性失败：全部包归入 failed set，计跨轮预算；git-server 客户端固定 3 次尝试后放弃本轮；连续失败 ≥ `failureRetryLimit` 后逐包记 condition（`RetryExhausted`）并跳过，Snapshot 推进 Active |
| B-2 | **分支/标签不存在** | 确定性失败：`rev-parse` 返回错误（exit code 非零）→ 记 condition（`ResolveFailed`）并跳过该包，不再重试 |
| B-3 | **部分 repo 成功部分失败** | 成功的写入 specCommits；失败的按 DR-6 分类：确定性当轮记 condition 跳过，非确定性未超限保持 Processing 下轮仅重试失败项；全部包成功或跳过后推进 Active |
| B-4 | **budgetCtx 总预算耗尽** | 未完成包归入 failed set（非确定性，计跨轮预算）；已完成的写入 specCommits |
| B-5 | **父 Build 不存在 / Aborted / Aborting** | 仅记录 condition `ParentAborted`（观测标记，不阻断），继续正常解析并推进 Active |
| B-6 | **Snapshot 在 reconcile 中被删除** | 回写时 404 → 静默返回 nil |
| B-7 | **同 repo 不同包 commit 冲突** | 确定性失败：不覆盖写入，记 condition（`CommitConflict`）并跳过该包 |
| B-8 | **commitId 已显式指定** | 跳过解析，直接采用 |
| B-9 | **Snapshot 无 packageRepos 且 Project 兜底也为空** | 确定性失败：记 condition `NoPackageRepos`，无包可解析 → 直接推进 Active |
| B-10 | **rev-parse 输出解析异常** | 输出为空/格式非法（非 40 字符十六进制 SHA）→ reason=`ResolveFailed` |
| B-11 | **标签为 annotated tag** | `rev-parse refs/tags/<tag>^{}` 优先获取 deref commit，失败时回退到 `rev-parse refs/tags/<tag>` |
| B-12 | **单包构建但 packages 指定的 repo 不在 packageRepos 中** | 确定性失败：记 condition（`ResolveFailed`）并跳过 → 推进 Active |
| B-13 | **控制器进程重启** | L1 缓存丢失无碍（TTL 短）；跨轮失败计数清零、重新给予预算；`Pending` 重新进入并推进 `Processing`；`Processing`（异常重入）保持继续填充，不回退 |
| B-14 | **单条资源数据异常导致整页 list 反序列化失败** | Go 反序列化以整页为单位，不进入 reconcile，由 PollingSource 退避重试 |

## 七、并发与一致性

### 7.1 单键串行与共享状态

BaseController 保证同一队列键在一个时刻只由一个 worker 调和；不同 Snapshot 可以并发。跨轮失败计数 map 仍可能被 PollingSource handler 与 worker 并发访问，必须由 `sync.Mutex` 保护，锁内不得执行 API 请求或队列阻塞操作。

### 7.2 乐观并发

Snapshot Controller 必须：
- 写入前 GET 最新 Snapshot；
- 携带最新 `resourceVersion` 更新 `/status`；
- 将 Conflict 视为最新状态优先，重新入队而不是覆盖；
- 404 视为对象已删除并成功结束；
- 401/403 视为本次调谐的永久错误，记录错误日志和鉴权失败指标；同一对象不做限速重试，等待认证配置修复后的 resync、新事件或进程重启；单次鉴权失败不改变 Controller 健康状态；
- 网络超时或结果未知时先 GET 确认。若 Snapshot 已是预期 Active 则视为成功，否则以最新对象重新判断。

### 7.3 与 git-server 的竞态

git-server 为外部服务，可能不可用或响应延迟。Controller 必须：
- git-server 超时归入 failed set，跨轮重试；
- 不依赖 git-server 的幂等性，靠 Snapshot 自身幂等保证；
- git-server status 接口返回 404 表示仓库从未出现、已删除或尚无可用本地副本，应调用 sync 接口后继续轮询；
- git-server status 接口返回 200 但响应中没有 `sync_time` 表示尚未成功同步，继续轮询；
- git-server status 接口返回 200 且响应中有 `sync_time` 表示已成功同步，可以执行 command；
- git-server 返回 5xx 归入 failed set，跨轮重试。

### 7.4 完整错误矩阵

#### 7.4.1 缓存读取和 GET

| 调用与结果 | 处理 |
|------------|------|
| Snapshot GET 404 | 目标 Snapshot 已不存在，本周期成功结束 |
| Build GET 404 | Build 不存在，记录 condition `ParentAborted` |
| Snapshot GET 2xx 且对象、UID、resourceVersion 合法 | 使用返回对象继续调谐 |
| GET 401/403 | PermanentError，记录鉴权失败指标，不改变健康状态 |
| GET 408、429 或 5xx | 临时错误，`AddRateLimited` |
| GET 网络超时、EOF、连接重置或 DNS/TLS 临时错误 | 临时错误 `AddRateLimited` |
| GET 2xx 但响应为空、解码失败、类型错误或缺少 UID/resourceVersion | 内部临时错误，记录 `unexpected-read-response` 并 `AddRateLimited` |
| Manager context 已取消 | 原样返回 context 错误，由 BaseController 停止 worker，不重入队 |
| Manager context 已取消 | 原样返回 context 错误，由 BaseController 停止 worker，不重入队 |

#### 7.4.2 明确收到写响应

下表适用于 `UpdateSnapshotStatus`。非 2xx 行对应 `WriteRejected`：

| HTTP 结果 | 处理 |
|-----------|------|
| 完整 2xx 且成功响应校验通过 | 清理跨轮失败计数（成功包），记录成功日志和指标，成功结束 |
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

若 Manager context 已取消，Controller 不在停止阶段启动确认请求，直接返回 context 错误；未确认的写入由进程重启后的初始 List 和正常调谐收敛。

status 更新结果未知后执行 Snapshot GET：

| 确认结果 | 处理 |
|----------|------|
| 404 | Snapshot 已消失，成功结束 |
| 同一 UID，phase 已为目标状态 | 目标状态已经收敛，成功结束 |
| UID 不同 | 原 Snapshot 已消失且同名对象已重建，成功结束，不更新新对象 |
| 同一 UID，但 phase 不同或 conditions 变化 | 按最新对象重新调谐 |
| GET 失败 | 严格按 7.4.1 的 GET 矩阵处理 |

## 八、错误与重试

| 结果 | 队列行为 |
|------|----------|
| 对象不存在、无需处理、已经终态 | `ReconcileResult{}, nil`，BaseController Forget |
| 状态推进成功或 GET 确认已达到预期 | `ReconcileResult{}, nil` |
| 存在未超限可重试失败包 | `ReconcileResult{Requeue: true}, nil` |
| resourceVersion Conflict | `ReconcileResult{Requeue: true}, nil`，清除旧退避后立即重读 |
| 429/503 且 WriteError 带合法 RetryAfter | 零值 Result 和原始错误，由 BaseController Forget 后 AddAfter |
| 读取或写入临时错误 | 零值 Result 和临时错误，由 BaseController AddRateLimited |
| WriteUnknown | 先按 7.4.4 确认；确认后成功结束、立即重入或按确认 GET 错误处理 |
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
| `--snapshot-update-conflict-retry` | 3 | update 乐观锁冲突重试上限 |
| `--snapshot-resolve-workers` | 8 | 单 Snapshot 内包解析并发数 |
| `--snapshot-resolve-budget` | 120s | 单轮解析总预算（budgetCtx） |
| `--snapshot-failure-retry-limit` | 3 | 非确定性失败跨轮重试预算，超限记 condition（`RetryExhausted`）并跳过 |
| `--git-server-addr` | `http://localhost:8080` | git-server 地址 |
| `--git-server-timeout` | 30s | 单次请求超时 |
| `--git-server-retry` | 3 | 客户端固定尝试次数 |
| `--git-server-cache-ttl` | 30s | L1 缓存 TTL（CheckSynced / GetRemoteHeadCommit） |

### 9.2 最小权限

Snapshot Controller 所需最小权限：

```text
snapshots:        get, list, update
snapshots/status: update
projects:         get
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

### 10.2 建议指标

```text
controller_reconcile_total{controller="snapshot",result}
controller_reconcile_duration_seconds{controller="snapshot"}
snapshot_controller_phase_transitions_total{from,to}
snapshot_controller_resolve_total{result}
snapshot_controller_resolve_duration_seconds
snapshot_controller_git_server_requests_total{method,result}
snapshot_controller_git_server_cache_hits_total
snapshot_controller_conditions_total{type,reason}
snapshot_controller_conflict_retries_total
```

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
| 父 Build 中止 | `ParentAborted` |

相同 reason 的日志必须限速或聚合，避免 git-server 不可用时产生日志风暴。

## 十一、测试要求

### 11.1 单元测试

- Active Snapshot 不处理；
- Pending Snapshot 推进到 Processing；
- Processing Snapshot 继续填充（异常重入）；
- 已被删除的 Snapshot 静默返回；
- PackageRepo 已有 specCommits 条目时不重复解析；
- commitId 显式指定时跳过解析；
- 确定性失败（校验失败/分支不存在/commit 冲突/无 packageRepos）记 condition 并跳过；
- 非确定性失败（5xx/网络/超时）跨轮重试，超限后跳过；
- 全部包成功或跳过后推进 Active；
- WriteError 三分类处理（WriteRejected/WriteUnknown/WriteNotSent）；
- PermanentError 清除退避；
- Conflict 重新入队（`Requeue=true`）；
- 404 静默返回；
- FakeClock 推进时间；
- `go test -race` 无竞态；
- 跨轮失败计数 map 并发安全；
- condition 消毒（去除换行、截断至 200 runes）；
- rev-parse 输出解析（B-10/B-11 规则）；
- L1 缓存命中减少 git-server 请求；
- budgetCtx 超时归入 failed set；

### 11.2 集成测试

- 初始 List 中已有 Pending Snapshot 时可以收敛；
- 初始 List 中已有 Processing Snapshot 时继续填充；
- git-server 不可达时跨轮重试；
- git-server 恢复后成功解析；
- 部分包成功部分失败时正确推进；
- Build 不存在时记录 condition 但不阻断；
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

2. **git-server 客户端**（`pkg/gitserver/`）：
   - `validate.go`：不可信输入白名单校验
   - `client.go`：PublishSyncTask、GetSyncStatus、ExecuteCommand（重试 3 次，30s 超时）
   - `revparse.go`：rev-parse 输出解析（提取 40 字符十六进制 SHA）
   - `cache.go`：L1 TTL 进程内缓存包装

3. **Snapshot Controller**（`pkg/controllers/snapshot/`）：
   - `conditions.go`：condition upsert/清理/消毒
   - `update.go`：advancePhase + updateSnapshot
   - `resolve.go`：两阶段并发解析（goroutine 池 + 信号量 + budgetCtx）
   - `reconcile.go`：reconcile 入口、parentAbortGuard、错误返回约定
   - `controller.go`：Initializer、事件注册、Client 接口定义

4. **集成与配置**：
   - 注册 `snapshot` initializer、命令行参数和指标
   - Snapshot Controller 没有独立关键后台循环，不实现自定义 HealthChecker，使用框架默认 ping checker

5. **测试**：
   - 单元测试（race、边界情况）
   - 集成测试（git-server 交互）
   - 完成后启用默认 `--controllers=*` 中的 Snapshot Controller

完成上述内容后，Snapshot Controller 可以独立开发；git-server 的部署和运维由基础设施层负责。
