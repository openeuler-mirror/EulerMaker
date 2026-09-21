# RpmRepo Controller 设计

## 一、定位与范围

RpmRepo Controller 是 controller-manager 中负责把构建产物物化为 RPM 仓库并驱动仓库正式发布的控制器。

职责：

- 消费 BuildInfo、Job、Build 的信息，维护 RpmRepo.status，推进构建产物物化与仓库正式发布；
- 支持重复调谐与重启恢复，提供结构化日志和指标。

职责边界：

- 不写 `Build.status`、`BuildInfo.status`、`Job.status`、`Project.status`；Build 侧由 Build Controller 自行复制发布结果；
- 不创建或删除 Job，不修改 `Build.spec`，不修改 `RpmRepo.spec`（首版 `RpmRepo.spec` 为空 `{}`）；
- 不直连 Artifact Manager 的 `${dataDir}`；所有物化与发布动作只经 `ArtifactManagerClient` 的 HTTP 接口；本控制器不解析、不持久化 RPM 元数据（单个 RPM 的元信息由 Artifact Manager 维护），也不访问 `Build.spec.bootstrapRepo` 等外部软件源；
- 不负责仓库签名、跨实例复制、镜像同步与 delta RPM。

## 二、依赖与组件边界

RpmRepo Controller 运行在现有 `controller-manager` 框架内，建议实现目录为：

```text
components/controller-manager/pkg/controllers/rpmrepo/
  controller.go            # Controller 定义与 initializer：依赖注入、Source handler 注册、BaseController 组装
  reconciler.go            # Sync(ctx, key) 主流程：键分派、确保对象、选批、恢复、发布驱动、收口
  batch.go                 # 候选 Job 过滤、稳定排序、批次上限、repositoryUID 计算
  transition.go            # transition 状态机：写入、恢复、提升、失败收口
  publish.go               # 发布驱动与发布状态机：提交、激活、结果收口
  publishpolicy.go         # PublishPolicy 接口、PublishPolicyInput、PublishDecision 与默认 DefaultPublishPolicy
  artifactclient.go        # ArtifactManagerClient 接口与 HTTP 实现
  artifactclient_fake.go   # ArtifactManagerClient 的内存 Fake
  client.go                # 类型化 Client 接口与共享 client 适配实现
  fake.go                  # Client 的内存 Fake
  conditions.go            # 条件常量与 MergeCondition
  metrics.go               # rpmrepo_controller_* 指标注册
```

依赖与注入：

- `Controller` 持有 `BaseController`（队列/Worker）、`Client`、`ArtifactManagerClient`、`PublishPolicy`、`clock.Clock` 与配置；
- 构造时注入 `clock.Clock`（`k8s.io/utils/clock`）：生产用 `clock.RealClock{}`，测试用 `k8s.io/utils/clock/testing.FakeClock`；业务代码不得直接调用 `time.Now()` / `time.Since()`；
- `const Name = "rpmrepo"`；initializer 负责构造 `Client`、`ArtifactManagerClient`、注入 `PublishPolicy`（首版固定 `DefaultPublishPolicy`）、只注册 `rpmrepos` PollingSource handler（不注册任何事件源），并创建 `BaseController`；
- reconcile 内所有 ebs-apiserver 访问只通过 `Client`，所有发布访问只通过 `ArtifactManagerClient`；单测注入 `fake.Client` 与 `fake.ArtifactManagerClient`。

### 2.1 装配与配置

- 队列 key 分两类：过程仓 `build/{project}/{buildName}`，发布 `release/{project}/{os}/{arch}`；`{project}` / `{buildName}` 分别等于 `RpmRepo.metadata.namespace` / `RpmRepo.metadata.name`，`{os}` / `{arch}` 取 `Build.spec.buildTarget.os` / `Build.spec.buildTarget.arch`；发布键由这两个字段构造，发布候选再按 RpmRepo 的同名标签 `ebs.io/target-os` / `ebs.io/target-arch` 做服务端过滤，键与过滤同源；
- 两类 key 都带 `{kind}/` 前缀（`build/`、`release/`），避免同 Project 下 `buildName` 与 `{os}/{arch}` 拼成的键撞名；两类 key 互不阻塞，同一 `release/{project}/{os}/{arch}` 内的发布由队列保证串行（与 Artifact Manager 的 `{project}/{os}/{arch}` 串行键一致）；
- 发布键由过程仓 reconcile 在观察到发布触发条件时通过 `BaseController.Enqueue` 入队；过程仓键每轮 resync 会重新观察该条件，因此发布键不依赖单独的事件源；
- `rpmrepos` 轮询周期沿用框架全局 `--poll-period`（默认 30s），是本控制器唯一的触发源：每轮为每个非终态 RpmRepo 发一次 Update 并按其名称入队 `build/{namespace}/{name}`，Job 完成、manifest 转终态、发布条件满足等外部变化最坏在一个轮询周期内被观察到；`jobs` 只用于 `ListJobs` 候选查询，不注册事件源；
- 控制器自有配置项（启动阶段校验，非法即启动失败）：
  | 配置 | 默认值 | 校验规则 |
  | --- | --- | --- |
  | `--rpmrepo-max-jobs-per-batch` | `20` | 必须 `> 0` |
  | `--rpmrepo-max-input-bytes` | `20GiB`（`21474836480` 字节） | 必须 > 0；按 Artifact Manager 的物化输入口径累计，属批次追加上限；首个候选自身超限时直接发布失败（见第六章「批次构成」） |
  | `--rpmrepo-materialize-retry-limit` | `3` | 必须 `> 0`；物化可重试失败的**重试次数**上限（首次提交不计入），判定以 Artifact Manager 响应的 `attempt` 为准：`attempt >= limit + 1` 即预算耗尽、按失败收口 |
  | `--artifact-manager-addr` | 必填，无默认 | 非空且可解析为带 host 的 `http` / `https` URL |
  | `--artifact-manager-timeout` | `30s` | 必须 `> 0` |
  - 命名沿用现有风格（对照 `--git-server-addr` / `--git-server-timeout`、`--snapshot-failure-retry-limit`）；这些 flag 由本控制器独占，将来若被别的控制器复用再提升为共享段；
- 复用框架全局配置、控制器不单独覆盖：`--apiserver` / `--apiserver-ca` / `--insecure-skip-verify`、`--controllers`（默认 `*`，注册后随默认集合启用；联调期可用 `--controllers=-rpmrepo` 关闭，验收通过后恢复默认）、`--workers`（6，框架默认）、`--controller-max-retries` 与慢速重试三项、`--poll-period`（30s）、`--poll-page-size`、`--cache-sync-timeout`、`--shutdown-timeout`、`--source-stale-threshold`（2m）、`--health-bind-address`（`:8080`）；`rpmrepo` 不单独覆盖 worker 数与轮询周期；
- 物化失败的退避曲线复用框架慢速重试参数 `--controller-slow-retry-initial-delay`（30s）/ `--controller-slow-retry-max-delay`（15m）/ `--controller-slow-retry-jitter`（0.2）：控制器按本批**已接受的执行次数**（取自 Artifact Manager 响应的 `attempt`，见 §8）计算退避间隔 `d = min(initial × 2^(attempt-1), max)` 并施加 jitter；**退避锚点取 Artifact Manager 响应的 `updatedAt`**（记录最近一次状态变更时间），只有已过 `d` 才重放，否则返回剩余等待时间。作为 `ReconcileResult{RequeueAfter}` 返回，**不叠加框架退避**；不新增退避 flag，也不在 `RpmRepo.status` 中自行维护计数或时间戳；
- 配置校验时机：所有静态配置在 `options.Parse` 阶段完成校验，非法时 controller-manager 直接启动失败，不得延迟到单个对象的 reconcile 中处理；
- `ArtifactManagerClient` 构造失败必须返回初始化错误，不得通过 HealthChecker 表达。

### 2.2 组件依赖与最小框架改动

**组件依赖与数据流**

```text
① 事件源                              ② RpmRepo Controller                 ③ 数据面 / 外部依赖
──────────────────────────          ──────────────────────────────        ──────────────────────────────
RpmRepo PollingSource ─key──▶   reconcile（经包内 Client）      ──────────▶  GET / PUT /status
                                        │                                  Artifact Manager
                                        └─ 物化 / 查询 / 发布 ───────────▶  /internal/v1/repositories
                                                                           /internal/v1/releases
                                                                           /artifacts/v1/.../manifest
```

**组件边界**

- 只写 `RpmRepo.status`；`Build.status` / `BuildInfo.status` / `Job.status` / `Snapshot.status` 由各自控制器维护；
- 只消费 Job 的状态与 labels、Build 的 `spec.buildTarget`、BuildInfo 的 `status.phase`；
- 只经 `ArtifactManagerClient` 访问 Artifact Manager；不出网访问任何外部软件源；
- 所有发布动作只经 `ArtifactManagerClient`；不感知 Artifact Manager 的本地存储布局。

**最小框架改动**

- 只复用 `PollingSourceFactory.ForResource(gvr, period, options)`（`rpmrepos` 只能使用 PollingSource）；本控制器不注册事件源，Job 侧只做 `ListJobs` 查询；
- 复用共享 client 的 `Get` / `ListProjectPage` / `UpdateStatus`；
- 不为 `Retry-After` 修改共享客户端或控制器框架：带 `Retry-After` 的 `429` / `503` 由本控制器换算为 `ReconcileResult{RequeueAfter}`（见第七章）；
- 在 `cmd/controller-manager/main.go` 的 `initializers` map 中加入 `rpmrepo.Initializer(...)`。

### 2.3 Source 装配语义

- `rpmrepos`：注册 `PollingSourceFactory.ForResource(RpmReposGVR, period, metav1.ListOptions{FieldSelector: "status.release.phase!=Ready,status.release.phase!=Failed,status.release.phase!=Aborted"})` 的 handler（`RpmReposGVR` 已由框架 `source` 包内置）。Add/Update 事件按 `build/{namespace}/{name}` 入队；Delete 事件仅记录日志；
  - 过滤语义：本源**只带** `fieldSelector`（驱动过程仓键、由单个对象重算），不带 `labelSelector`；只排除「发布终态（`release.phase ∈ {Ready, Failed, Aborted}`）」的 RpmRepo。过程仓不再有相位，apiserver 对 RpmRepo 的**字段**选择器只有 `metadata.name` / `metadata.namespace` / `status.release.phase`（labelSelector 另行支持，发布候选查询见 §7.3），因此不能按过程仓状态过滤。字段缺失（`release` 为 null）视为「不等于任何具体相位」，不会被排除。RpmRepo 的 `ebs.io/target-os` / `ebs.io/target-arch` 标签由 Build Controller 在创建时写入、apiserver 创建校验强制存在；
  - 成本控制：物化在途与失败收口的对象都会每轮入队，但**重试路径完全不写 status**（计数在 Artifact Manager 侧，`attempt` 只通过响应读取），等待路径在状态未变化时也不写 status（见 7.2）；对象收敛到 `release.phase ∈ {Ready, Failed, Aborted}` 后即被本过滤排除；
  - RpmRepo 的 `status` 变化只用于触发重算，reconcile 内仍以最新 GET 的对象为准。
- 首次同步：manager 等待全部 Source `HasSynced` 后才启动 Worker；重启后由 `rpmrepos` 首轮全量 List 为所有未收敛对象重建 `build/` 键（Job 侧不做全量 List：候选按对象名逐一 `ListJobs` 过滤）。

## 三、类型化客户端与 Fake

框架不新增完整类型化 CRUD；由 rpmrepo controller 包自行实现。

### 3.1 Client 接口

```go
package rpmrepo

type Client interface {
    // Project 级资源读取
    GetBuild(ctx context.Context, project, name string) (*v1.Build, error)
    GetBuildInfo(ctx context.Context, project, name string) (*v1.BuildInfo, error)
    GetRpmRepo(ctx context.Context, project, name string) (*v1.RpmRepo, error)
    // ListJobs 按项目作用域列出 Job；调用方以 labelSelector=ebs.io/build-name=<RpmRepo 名> 做服务端归属过滤，相位与其余三个 label 在控制器侧校验。
    ListJobs(ctx context.Context, project string, options metav1.ListOptions) ([]v1.Job, error)
    // ListRpmRepos 按项目作用域列出 RpmRepo；调用方以 labelSelector（ebs.io/target-os / ebs.io/target-arch）做服务端目标过滤，并按 fieldSelector 排除发布终态。
    ListRpmRepos(ctx context.Context, project string, options metav1.ListOptions) ([]v1.RpmRepo, error)

    // RpmRepo 写入（仅 status）
    // UpdateRpmRepoStatus 走 /status：写 RpmRepo.status，apiserver 保留旧 spec 与 metadata
    UpdateRpmRepoStatus(ctx context.Context, obj *v1.RpmRepo) (*v1.RpmRepo, error)
}
```

`ListJobs` / `ListRpmRepos` 通过共享 client 的 `ListProjectPage` 分别调用 `jobs` / `rpmrepos` 的项目级路径；Get 类方法在 404 时返回保留状态码的读取错误，不包装为写错误。本控制器不需要读取 Project 对象；若未来策略需要 Project 对象，再扩展 `PublishPolicyInput`。

### 3.2 路径约定

- Project：`GET /apis/ebs/v1/projects/{project}`（Project 为集群级资源，namespace 段为空）；
- 子资源单对象：`GET /apis/ebs/v1/projects/{project}/{resource}/{name}`，`{resource}` 取 `builds` / `buildinfos` / `rpmrepos`；
- Job 列表：`GET /apis/ebs/v1/projects/{project}/jobs?labelSelector=ebs.io/build-name=<buildName>`（`buildName` 等于 RpmRepo 名；仅按归属 label 服务端过滤，不附加 `status.phase` 字段选择器）；
- RpmRepo 列表（发布候选）：`GET /apis/ebs/v1/projects/{project}/rpmrepos?fieldSelector=status.release.phase!=Ready,status.release.phase!=Failed,status.release.phase!=Aborted&labelSelector=ebs.io/target-os=<os>,ebs.io/target-arch=<arch>`（两个选择器与 Project 路径隐含的 namespace 条件按 AND 组合）；
- RpmRepo status：`PUT /apis/ebs/v1/projects/{project}/rpmrepos/{name}/status`。

Artifact Manager 的路径与 `ArtifactManagerClient` 一一对应，统一列在 3.3。

写请求必须携带对象的 UID 与 `metadata.resourceVersion`。成功响应必须校验非 nil、UID 一致、`resourceVersion` 非空，`/status` 响应还要求保留旧 `spec`；任一不满足都按 `WriteUnknown` 处理。

### 3.3 ArtifactManagerClient 接口

```go
type ArtifactManagerClient interface {
    // SubmitRepository 幂等：相同 repositoryUID 与相同请求摘要是重试，不重复物化。
    SubmitRepository(ctx context.Context, req CreateRepositoryRequest) (RepositoryResponse, error)
    GetRepository(ctx context.Context, repositoryUID string) (RepositoryResponse, error)
    // GetJobManifest 查询 Job 的唯一上传清单；无清单时返回 NotFound 类错误。
    GetJobManifest(ctx context.Context, project, jobName, jobUID string) (JobUploadManifest, error)
    // SubmitRelease 幂等：相同 buildName 与相同请求摘要是重试，不重复发布。
    SubmitRelease(ctx context.Context, req CreateReleaseRequest) (ReleaseRecord, error)
    GetRelease(ctx context.Context, buildName string) (ReleaseRecord, error)
    // ActivateRelease 切换稳定入口；首次由 Prepared 激活，已 Ready 时幂等确认。
    ActivateRelease(ctx context.Context, buildName string) (ReleaseRecord, error)
}
```

- `SubmitRepository` 对应 `POST /internal/v1/repositories`；
- `GetRepository` 对应 `GET /internal/v1/repositories/{repositoryUID}`；
- `GetJobManifest` 对应 `GET /artifacts/v1/projects/{project}/jobs/{job}/manifest?jobUID={jobUID}`；
- `SubmitRelease` 对应 `POST /internal/v1/releases`；
- `GetRelease` 对应 `GET /internal/v1/releases/{buildName}`；
- `ActivateRelease` 对应 `POST /internal/v1/releases/{buildName}/activate`；
- 客户端必须保留 Artifact Manager 的稳定错误码与 `retryable` 语义，并把 HTTP 状态、`Retry-After` 与稳定错误码映射为可分类错误（见第八章）；分类错误必须携带 HTTP 状态与 `Retry-After`（>0 表示服务端要求的最短等待），控制器据此换算为 `RequeueAfter`；不得解析 `message` 做判定。
- 发布接口的响应语义：`202` 表示首次接受、`Creating`、`Prepared` 或可重试重放；`200` 表示已 `Ready` 的相同请求；`409 ReleaseIdentityConflict`（同 `buildName`、请求摘要不同；摘要由 Artifact Manager 服务端计算）与 `422`（非法集合或源仓不满足条件）为不可重试；`429` 为可重试并带 `Retry-After`。

`ArtifactManagerClient` 使用的类型与字段说明：

| 类型 | 字段说明 |
| --- | --- |
| `RepositoryState` | 过程仓状态：`Creating` / `Ready` / `Failed` / `Deleting` |
| `CreateRepositoryRequest` / `ManifestReference` | 物化请求：`repositoryUID`、`repositoryName`、`project`、`buildName`、`targetOS`、`targetArch`、可选的 `baseRepositoryUID`，以及按 `jobUID` 排序的 `manifests`（`jobName` + `jobUID`） |
| `RepositoryResponse` | 物化响应：`repositoryUID`、`state`、`attempt`（重试预算的权威计数）、可选的 `pollAfterSeconds`、`contentURL`、`failure` 与 `createdAt` / `updatedAt`（退避窗口锚点，见 §8）/ `completedAt`；响应体可能携带 `rpms`（Artifact Manager 侧字段，控制器不解析、不持久化）。内容摘要只保留在 Artifact Manager 内部，不在响应中返回 |
| `JobUploadManifest` / `ManifestFile` | Job 上传清单及其文件项：`state` ∈ `Open` / `Completing` / `Completed` / `Failed`、`digest`、`files[]`（相对路径、大小、SHA-256、是否必需等） |
| `FailureInfo` | 失败详情：稳定错误码 `code`、诊断信息 `message`、是否可重试 `retryable` 与发生时间 |
| `ReleaseState` | 发布状态：`Creating` / `Prepared` / `Ready` / `Failed` / `Deleting` |
| `CreateReleaseRequest` | 发布请求：`buildName`、`project`、`targetOS`、`targetArch`、`sourceRepositoryUID`、`excludeSpecs` |
| `ReleaseRecord` | 发布记录：`state`、`attempt`、可选的 `pollAfterSeconds`、`contentURL`、`failure` 与 `createdAt` / `updatedAt` / `completedAt`（请求摘要与发布摘要由 Artifact Manager 服务端持有，不在响应中返回） |
| `Timestamp` | 统一的时间字段类型 |

"是否需要发布"与"排除哪些 spec"都由策略决定，控制器不内置业务判定：

```go
type PublishPolicyInput struct {
    Project             string
    Build               *v1.Build
    BuildInfo           *v1.BuildInfo
    SourceRepositoryUID string
    TargetOS            string
    TargetArch          string
}

type PublishDecision struct {
    Publish      bool
    ExcludeSpecs []string
}

type PublishPolicy interface {
    Decide(ctx context.Context, input PublishPolicyInput) (PublishDecision, error)
}

// DefaultPublishPolicy 是首版默认实现：不排除任何 spec，并按内置规则决定是否发布。
type DefaultPublishPolicy struct{}
```

- `PublishPolicyInput` 覆盖当前可得的全部依据（Project、Build、BuildInfo、源过程仓 UID、目标 OS/Arch），后续策略调整只需替换实现；
- `Decide` 一次返回两项决策：`Publish`（是否需要正式发布）与 `ExcludeSpecs`（发布时排除的 spec 集合），避免两次调用之间依据不一致；
- 首版默认实现 `DefaultPublishPolicy`：`Publish=false` **当且仅当** `buildTarget.publishFlag=false`；其余情形 `Publish=true`；`ExcludeSpecs` 恒为空；`buildType` 不参与判定；
- `ExcludeSpecs` 在构造请求前**去重并按字典序排序**，与 Artifact Manager §9.13.2 中 `requestDigest`（服务端计算）的规范化口径一致；
- 策略返回 error 时按**可重试错误**处理：不提交发布、不写 `PublishSucceed`，记录日志与指标；
- 策略在 initializer 注入；首版固定 `DefaultPublishPolicy`。

### 3.4 Fake

两个 Fake 都是对应接口的内存替身，供单元与集成测试使用：

- `fake.Client`：模拟 NotFound、AlreadyExists、409 Conflict、resourceVersion 自增、`/status` 保留 spec、写错误三分类（NotSent / Rejected / Unknown）；`ListRpmRepos` / `ListJobs` 可预设对象集合，并断言请求的 label selector 与 field selector 原样透传（`ListJobs` 断言 labelSelector 恰为 `ebs.io/build-name=<name>` 且 FieldSelector 为空）；
- `fake.ArtifactManagerClient`：
  - 可预设 `SubmitRepository` 的返回：`202 Creating`、`200 Ready`、`Failed{retryable=true|false}`、`409 RepositoryIdentityConflict`、`409 RepositoryDeleting`、`410 MaterializationInputExpired`、`422`（非法请求、`ManifestNotReady`、`BaseRepositoryNotReady`）、`429 RepositoryQueueFull`（带 `Retry-After`）、`503`；每个成功/失败响应都可指定 `attempt` 与 `updatedAt`，用于断言重试预算与退避窗口；
  - 可预设 `GetRepository` 的返回状态、`attempt`、`updatedAt` 与 `pollAfterSeconds`，并可返回 404（`attempt` 与 `updatedAt` 用于断言重试预算判定与退避窗口）；
  - 可预设 `GetJobManifest` 返回 `Completed` / `Open` / `Completing` / `Failed` / 404；
  - 可预设 `SubmitRelease` 的返回：`202 Creating`、`202 Prepared`、`200 Ready`、`Failed{retryable=true|false}`、`409 ReleaseIdentityConflict`、`422`、`429`（带 `Retry-After`）、`503`；
  - 可预设 `GetRelease` 的四态与 `pollAfterSeconds`，`ActivateRelease` 的 `200 Ready` / 不可重试失败；
  - 记录每个方法的调用次数与最后一次请求体，支持重置，用于断言幂等与"未重复提交不同请求"。

## 四、对象样例与字段来源

`RpmRepo` 与 `Build` 同名同 namespace，一对一。首版 `RpmRepo.spec` 为空 `{}`，由 Build Controller 在 Pending 阶段 ensure。

初始对象：

```yaml
apiVersion: ebs/v1
kind: RpmRepo
metadata:
  name: ${build.metadata.name}
  namespace: ${build.metadata.namespace}
spec: {}
status:
  repository:
    contentURL: /repositories/v1/<base-repositoryUID>/  # Build Controller 创建时预置：上一个发布成功 Build 同名 RpmRepo 的 contentURL
    repositoryUID: <base-repositoryUID>                                       # Build Controller 创建时预置：同上
    sourceJobUIDs: []       # 为空表示本对象尚未产出过版本；仅有继承基线时不可发布
    transition: null
  release: null             # 尚未进入发布阶段
  conditions: []
```

推进中与发布后示例：

```yaml
# 物化进行中（存在 repository.transition，尚无新版本）
status:
  repository:
    contentURL: /repositories/v1/<previous-repositoryUID>/
    repositoryUID: <previous-repositoryUID>
    sourceJobUIDs: ["uid-job-0"]
    transition:
      inputs:
        - jobName: job-gcc
          jobUID: uid-job-1
          specName: gcc
      baseRepositoryUID: <previous-repositoryUID>
      repositoryUID: <next-repositoryUID>

# 重试耗尽（或不可重试失败）：transition 原样保留，发布终局同次写入
status:
  repository:
    contentURL: /repositories/v1/<previous-repositoryUID>/
    repositoryUID: <previous-repositoryUID>
    sourceJobUIDs: ["uid-job-0"]
    transition:
      inputs:
        - jobName: job-gcc
          jobUID: uid-job-1
          specName: gcc
      baseRepositoryUID: <previous-repositoryUID>
      repositoryUID: <next-repositoryUID>
    updatedAt: "2026-09-14T09:30:00Z"
  release:
    phase: Failed
    transition: null
    updatedAt: "2026-09-14T09:30:00Z"
  conditions:
    - type: RepositoryReady
      status: "False"
      reason: RepositoryFailed
    - type: PublishSucceed
      status: "False"
      reason: RepositoryPublishFailed

# 发布后
status:
  repository:
    contentURL: /repositories/v1/<repositoryUID>/
    repositoryUID: <repositoryUID>
    sourceJobUIDs: ["uid-job-1"]
    transition: null
    updatedAt: "2026-09-14T10:00:00Z"
  release:
    phase: Ready            # Pending / Creating / Prepared / Ready / Failed
    sourceRepositoryUID: <repositoryUID>
    contentURL: /repositories/<project>/<os>/<arch>/
    transition: null
    updatedAt: "2026-09-14T10:00:00Z"
  conditions:
    - type: PublishSucceed
      status: "True"
      reason: RepositoryPublished
```

字段来源：

| 字段 | 来源/写入方 | 说明 |
| --- | --- | --- |
| `RpmRepo.metadata.name` / `namespace` | Build Controller | 与 Build 同名同 namespace |
| `RpmRepo.metadata.labels` | Build Controller（创建时写入） | 写入 `ebs.io/target-os` / `ebs.io/target-arch`，值取 `Build.spec.buildTarget.os` / `arch` ；本控制器只读、不补写、不改写，用于发布候选的服务端 `(os, arch)` 过滤 |
| `RpmRepo.spec` | Build Controller | 空 `{}`，本控制器不修改 |
| `RpmRepo.status.repository.contentURL` | Build Controller（创建时预置基础仓）/ RpmRepo Controller（物化提升时覆盖） | 创建时预置为上一个发布成功 Build 同名 RpmRepo 的 `contentURL`，即本对象的基础仓；提升后为本对象当前可读版本的地址；失败路径不清空。取 Artifact Manager 返回的相对路径，形如 `/repositories/v1/{repositoryUID}/`；非空只表示存在可读仓库指针，可能是 Build Controller 预置的继承基线，**不代表本对象已产出过版本** |
| `RpmRepo.status.repository.repositoryUID` | Build Controller（创建时预置基础仓）/ RpmRepo Controller（物化提升时覆盖） | 创建时预置为基础仓的 `repositoryUID`（`SubmitRepository` 的 `baseRepositoryUID` 来源）；提升后为本对象当前版本的物理标识。非空只表示存在可读仓库指针，可能是 Build Controller 预置的继承基线，**不代表本对象已产出过版本** |
| `RpmRepo.status.repository.sourceJobUIDs` | RpmRepo Controller | 已成功纳入当前版本的 Job UID；写入时去重并按字典序排序；不记录失败 Job。为空表示本对象尚未产出过版本（Build Controller 预置的继承基线不计入），是「本对象已产出可发布版本」的唯一判据 |
| `RpmRepo.status.repository.transition` | RpmRepo Controller | 在途批次或已放弃批次的固定输入（`inputs`）、`baseRepositoryUID` 与 `repositoryUID`；不承载重试计数（计数以 Artifact Manager 的 `attempt` 为准，见 §8）。成功提升时随 `transition=nil` 一起清除，失败收口时原样保留 |
| `RpmRepo.status.repository.updatedAt` | RpmRepo Controller | 过程仓 status 最近一次有效写入时间（提交批次、失败收口、提升版本时更新）；**不用作退避锚点**（退避以 Artifact Manager 响应的 `updatedAt` 为准，见 §8） |
| `RpmRepo.status.release` | RpmRepo Controller | 发布状态：`phase`（`Pending` / `Creating` / `Prepared` / `Ready` / `Failed` / `Aborted`）/ `sourceRepositoryUID` / `contentURL`（Project / OS / 架构的稳定仓库入口，取 Artifact Manager 返回的相对路径，形如 `/repositories/{project}/{os}/{arch}/`）/ `transition`（`sourceRepositoryUID` + 规范化 `excludeSpecs`）/ `updatedAt`；提交前先写 `release.transition`，它即请求的全部可变输入，重启后据此重放同一请求（请求摘要由 Artifact Manager 服务端计算，不落本状态） |
| `RpmRepo.status.conditions` | RpmRepo Controller | 过程仓与正式发布两类错误条件；type 必须区分二者（见第九章） |

发布相位 `release.phase`（`release` 缺失表示尚未进入发布阶段）；在途与恢复判据是 `release.transition` 非空，`release.phase` 用于候选过滤（`∈ {Ready, Failed, Aborted}` 为终态）与对外提供发布结论：

| phase | 含义 | 允许的下一步 |
| --- | --- | --- |
| `Pending` | 检查点已写，尚未提交或提交结果未确认 | `Creating` / `Prepared` / `Ready` / `Failed` / `Aborted` |
| `Creating` | 已提交 `SubmitRelease`，Artifact Manager 处理中 | `Prepared` / `Ready` / `Failed` / `Aborted` |
| `Prepared` | 正文就绪，待激活切换稳定入口 | `Ready` / `Failed` / `Aborted` |
| `Ready` | 已激活，稳定入口可读（`release.contentURL` 非空） | 终局；不再重复提交 |
| `Failed` | 发布不可重试失败，或过程仓重试耗尽 / 不可重试失败时的终局登记（由 §7.2 失败收口同次写入） | 终局；修正内容需新建 Build |
| `Aborted` | 同名 Build 已被中止（`Build.status.phase=Aborted`），发布不再进行；由过程仓流程或发布流程直接写入 | 终局；不再提交、激活或重放 |

## 五、状态机

### 5.1 发布状态机（`release.phase`）

发布子状态机与过程仓推进 **相互独立**：`release.phase` 取 `Pending` / `Creating` / `Prepared` / `Ready` / `Failed` / `Aborted`，发布推进不改动过程仓字段；`release` 缺失表示尚未进入发布阶段。

| 当前状态 | 触发 | 动作 | 结果 |
| --- | --- | --- | --- |
| `release` 缺失或 `Pending`（无检查点） | `BuildInfo` 完成、无 `repository.transition`、存在本对象已产出的可发布版本（`repository.sourceJobUIDs` 非空）、本轮候选扫描为空且无未就绪输入（manifest `Open` / `Completing`） | 按发布策略决定是否发布：发布则一次 CAS 写检查点（`release.phase=Pending`、`release.transition.sourceRepositoryUID` / `excludeSpecs`、`release.updatedAt`）后提交 `SubmitRelease`，再按响应写 `release.phase=Creating` / `Prepared`；不发布则仅在存在残留 `release` 时清空并跳过该候选 | `Creating` / `Prepared`（发布）；不发布时不落持久结果 |
| `Pending` / `Creating` / `Prepared`（检查点非空） | 在途确认：`GetRelease` 返回 `Creating` / `Prepared`，或 `Failed{retryable=true}` / `404` 需要重放 | `Creating` 等待、`Prepared` 调 `ActivateRelease`、可重试失败或 `404` 用同一请求重放 | 仍 `Creating` / `Prepared` |
| `Pending` / `Creating` / `Prepared`（检查点非空） | 发布完成（`SubmitRelease` 返回 `200`、`GetRelease` 返回 `Ready` 或 `ActivateRelease` 返回 `200`） | 一次 CAS 写 `release.phase=Ready`、`release.sourceRepositoryUID` / `release.contentURL` / `release.updatedAt`、清 `release.transition` 与 `PublishSucceed=True` | `Ready`（发布终局） |
| `Pending` / `Creating` / `Prepared`（检查点非空） | 发布不可重试失败（`409` / `422` / `Failed{retryable=false}`） | 一次 CAS 写 `release.phase=Failed`、`release.updatedAt`、清 `release.transition` 与 `PublishSucceed=False` | `Failed`（发布终局） |
| `release` 缺失 | 过程仓重试耗尽或遭遇不可重试失败（失败收口） | 与过程仓失败收口同一次 CAS 写 `release.phase=Failed`、`release.updatedAt`、`release.transition=nil` 与 `PublishSucceed=False` | `Failed`（发布终局，不调用发布接口） |
| `release` 缺失 | **无可用版本**：`BuildInfo=Completed`、候选扫描为空、无未就绪输入、`repository.transition=nil`，但 `repository.sourceJobUIDs` 为空（本对象从未产出任何版本；含仅预置了继承基线的对象） | 一次 CAS 写 `release.phase=Failed`、`release.updatedAt`、`release.transition=nil` 与 `PublishSucceed=False`（reason `RepositoryPublishFailed`）；**不写 `repository.*`**、不调用 Artifact Manager、不入队发布键；计一次 `rpmrepo_controller_release_failed_total` | `Failed`（发布终局） |
| `release` 缺失或 `Pending` / `Creating` / `Prepared` | 读到同名 `Build.status.phase=Aborted`（过程仓流程或发布流程任一先读到） | 一次 CAS 写 `release.phase=Aborted`、`release.updatedAt`、`release.transition=nil` 与 `PublishSucceed=False`（reason `RepositoryPublishAborted`）；**不写 `repository.*`**、不调用 Artifact Manager、不做策略判定、不入队发布键；不计入 `rpmrepo_controller_release_failed_total` | `Aborted`（发布终局） |

规则：

- 发布只在过程仓批次全部处理完（无 `repository.transition`、候选扫描为空且本对象已产出可发布版本，即 `repository.sourceJobUIDs` 非空）后开始；`release.phase ∈ {Ready, Failed, Aborted}` 的终态对象不进入发布候选；
- 检查点 `release.transition`（`sourceRepositoryUID` + 规范化 `excludeSpecs`）是唯一的恢复依据，它即发布请求的全部可变输入；有检查点时只重放或查询，无检查点且非终态时才重新决策（摘要一致性由 Artifact Manager 服务端保证）；
- 同一 `{project}/{os}/{arch}` 已有在途发布（`release.transition` 非空）时，更早创建但尚未开始的候选要等在途发布收口后才开始；
- 发布收口必须清空 `release.transition`（成功、失败与中止三条路径都清），否则对象会在后续轮次被当作在途反复驱动；
- 策略判定为不发布时不落任何持久结果：仅在存在残留 `release` 时清空一次，并继续评估同目标的下一个候选；
- 发布候选必须存在本对象版本（`repository.transition=nil` 且 `repository.sourceJobUIDs` 非空；Build Controller 预置的继承基线不计入）；从未产出本对象版本的对象不进入发布流程，其发布终局有两种来源——过程仓失败收口（同次 CAS 写入）或"无可用版本"的发布失败终局（§7.2 第 5 步），两者都不调用发布接口；

## 六、输入选择与批次

**入选条件**

候选来自 `ListJobs(project, metav1.ListOptions{LabelSelector: labels.Set{ebsv1.JobBuildNameLabel: rpmRepoName}.String()})`，即 `labelSelector=ebs.io/build-name=<RpmRepo 名>`：只有 `ebs.io/build-name` 参与服务端过滤，归属键与 §2.3 的轮询触发同源；`Job.status.phase` 与 `ebs.io/spec-name` / `ebs.io/target-os` / `ebs.io/target-arch` 一律在控制器侧按下列规则校验，不附加 `status.phase` 字段选择器，因此同 Build 的非 `Succeeded` Job 也会被列出、并按规则 1 显式跳过。`ebs.io/build-name` 缺失的 Job 不会出现在任何列表、也不会被任何键看到（该 label 由 BuildInfo Controller 负责写入，缺失属上游契约问题，本控制器不额外告警）；取值不等于本 RpmRepo 名的 Job 归属到其 label 指向的键，不在本键处理。项目级路径已保证 `metadata.namespace` 等于 Project；它与 `ebs.io/build-name` 同属一次列表调用保证的条件，规则中不重复校验。

候选 Job 必须同时满足：

1. `Job.status.phase=Succeeded`；
2. `ebs.io/spec-name` 必须存在且非空；`ebs.io/target-os` / `ebs.io/target-arch` 必须分别等于 `Build.spec.buildTarget.os` / `arch`（防御性校验：Job 的 label 无服务端校验，用于拦截 BuildInfo 契约被破坏导致的跨目标 Job 混入）；`ebs.io/build-name` 已由服务端选择器保证，不重复校验。`ebs.io/spec-name` 缺失或为空、`ebs.io/target-os` / `ebs.io/target-arch` 缺失或取值不一致的 Job 不进入候选、不入队、记录 `reason=InputLabelMismatch` 告警日志，且**不阻塞发布触发**；
3. `metadata.uid` 不在 `status.repository.sourceJobUIDs` 中、`metadata.name` 也不是当前 `status.repository.transition.inputs` 的成员；
4. Artifact Manager 的 `GetJobManifest(project, jobName, jobUID)` 返回 `state=Completed`：
   - 返回 404：该 Job 无归档产物（`Job.status.phase=Succeeded` 已隐含清单处理收口——需归档时清单必已封账，无需归档时 Artifact Manager 不创建 manifest）。本轮跳过、不写任何 status、不计入候选、**不阻塞发布触发**，输出 `reason=InputManifestMissing` 告警日志，并在后续轮询中复查；
     - 护栏：若该 Job 的 `status.phase` 不是 `Succeeded` 却拿到 404（相位漂移或读到了错误对象），返回可重试错误，不得当作无产物静默跳过；该护栏为防御性断言——规则 1 已在前面拦截非 `Succeeded` 对象，只有对象在候选校验与 manifest 查询之间发生相位漂移或读到错误对象时才会触发；
   - 返回 `Open` / `Completing`：本轮跳过并输出 `reason=InputManifestNotReady` 告警日志；属**未就绪输入**，在清单终态前阻塞发布触发；
   - 返回 `Failed`：本轮跳过（不写任何 status、输出 `reason=InputManifestFailed` 告警日志）；清单已终态、**不阻塞发布触发**（产物不可用）；该 Job 每轮都会被重新判定，确定失败的批次由 Artifact Manager 以同一 `repositoryUID` 幂等返回同一结果，不产生额外副作用。

未就绪输入（manifest 为 `Open` / `Completing`）既不是候选 Job，也不允许被跳过成「无候选」：只要存在未就绪输入，就不满足发布触发条件（见 7.2）。首版不引入超时，长期停留在 `Open` / `Completing` 的 Job 会一直阻塞该 Build 的发布，需人工介入。

候选校验期间若 `Job` 对象本身 NotFound（List 与按 UID / 名称校验之间被删除或 GC），按第七章开篇「依赖读取的统一语义」处理：跳过该输入、不计入候选、不写 status、不阻塞发布触发，输出 `reason=InputObjectMissing` 告警日志，并在后续轮询中复查；`Job` 的其它读取错误与 manifest 的 `404` / `Open` / `Completing` / `Failed` 语义分别按统一语义与本节前述规则处理。

**批次构成**

- 候选按 `creationTimestamp`、`metadata.name`、`metadata.uid` 升序稳定排序；
- 同一批次内每个 `specName` 至多选择一个 Job，其余留到下一批；
- 每个候选的物化输入字节数按 Artifact Manager 的物化输入口径计算，与其保持一致；
- 形成批次：批次为空时取排序最前的候选——若其自身输入超过 `--rpmrepo-max-input-bytes`，**不形成批次**，输出一次 `reason=InputTooLarge`（附 `bytes` 与 `limit`）并按发布失败终局收口（只写发布侧条件，见 §7.2 第 2 步）；否则无条件加入该候选；
- 继续追加：下一个候选会使累计输入超过 `--rpmrepo-max-input-bytes` 时，该候选留待下一批并结束本批；达到 `--rpmrepo-max-jobs-per-batch` 或候选集取完同样结束本批；
- 被留待下一批的候选在成为首个候选时按同一规则判定，因此任何自身超限的候选最终都会触发该失败终局，不会造成静默丢包；
- 该判定位于批次提交路径，不受 `BuildInfo.status.phase=Completed` 门禁约束；
- 不使用时间窗口等待更多 Job；存在可入选 Job 即组批（首个候选自身超限的情形除外）。

**基础仓与幂等键**

- 基础仓 `baseRepositoryUID`：
  - `status.repository.repositoryUID` 非空时使用它——该值由 Build Controller 在创建 RpmRepo 时预置为基础仓（上一个发布成功 Build 同名 RpmRepo 的 `repositoryUID`），物化提升后为本对象当前版本的 UID；基础仓由 Build Controller 按 `GetLastPublishedBuild(project, os, arch)` 选择，因此与本次请求的 Project / OS / 架构一致；
  - 为空时表示无基础仓，由 Artifact Manager 从零物化；
  - 若 Artifact Manager 因基础仓的 Project / OS / 架构与请求不一致（或基础仓不是 `Ready`）返回 `422 BaseRepositoryNotReady`，按**不可重试失败**收口（本设计不自行校验该一致性），并记录 `reason=RepositoryFailed` 告警日志；
- `repositoryUID` 是物化请求的幂等键，计算必须与 Artifact Manager 的计算保持一致。
- `SubmitRepository` 请求字段推导固定为：`repositoryName` 必须等于 `buildName`（Artifact Manager 校验两者相等，否则返回 `422 InvalidRepositoryRequest`）；`project` / `buildName` 取 RpmRepo 的 `metadata.namespace` / `metadata.name`；`targetOS` / `targetArch` 取 `Build.spec.buildTarget.os` / `arch`；`baseRepositoryUID` 取本批冻结的基础仓（无基础仓时为空串）；`manifests` 由 `repository.transition.inputs` 映射，只含 `jobName` 与 `jobUID`，并按 `jobUID` 升序提交（Artifact Manager 会服务端排序并校验 `jobUID` 不重复、标识符合法，否则返回 `422 InvalidManifestReference`）。
- 相同批次重试自然复用同一 `repositoryUID`；基础仓或批次成员变化必然产生不同的不可变版本。
- 恢复不依赖 Job 对象仍然存在：`SubmitRepository` 按 `jobUID` 引用 Artifact Manager 侧已封账的 manifest，Job 被 history GC 回收后仍可按 `repository.transition` 重放；若 Artifact Manager 侧输入已过期，重放返回 `410 MaterializationInputExpired`，按**不可重试失败**处理（直接失败收口，不消耗重试预算，见 7.2）。

**及时性**

- `rpmrepos` 的 PollingSource 是本控制器唯一的触发源（每轮为每个非终态对象发一次 Update，并入队对应的 `build/` 键）；
- Artifact Manager 返回 `Creating` 时按其 `pollAfterSeconds`（固定 5）用 `RequeueAfter` 延迟重入（不叠加框架退避），而不是等下一个轮询周期；
- 可重试失败按退避重试：退避间隔 `d = min(--controller-slow-retry-initial-delay × 2^(attempt-1), --controller-slow-retry-max-delay)`（`attempt` 取自 Artifact Manager 响应）并施加 `--controller-slow-retry-jitter`；以响应 `updatedAt` 为锚点，只有已过 `d` 才重放，否则只延迟重入；同样不叠加框架退避；
- 发布接口返回 `202`（`Creating` / `Prepared`）时同样按 `pollAfterSeconds` 用 `RequeueAfter` 延迟重入（不叠加框架退避）；`Prepared` 随后调用 `ActivateRelease` 并继续等待 `Ready`；
- 不引入额外超时机制：重试预算（`--rpmrepo-materialize-retry-limit`）是唯一的过程仓失败收口判据；`Creating` 长期不返回由 Artifact Manager 侧 `pollAfterSeconds` 与日志、指标暴露，由人工处理。

## 七、Reconcile 流程

发布键的触发与补偿：发布键只由过程仓 reconcile 在满足触发条件时入队；过程仓键每轮 resync 都会重新观察该条件，因此进程重启后仍会在一个轮询周期内重新入队，不依赖额外的发布事件源。

**状态写入原子性**：每次 status 迁移先构造完整目标 `RpmRepo.status`，再调用一次 `UpdateRpmRepoStatus`——`repository.*`、`release.*` 与 `conditions` 的变更合并到同一次写入；写入冲突（409）必须重新 GET 后重算，禁止用旧对象重放。

写入结果未知（超时、连接中断、响应无法解析）时不重放原 PUT，先 GET 当前对象判断"目标 status 是否已达成"，判据按写入类别固定：

| 写入类别 | 达成判据 |
| --- | --- |
| 提交批次（写 `repository.transition`） | `repository.transition.repositoryUID` 与本次目标一致 |
| 过程仓成功收口 | `repository.transition=nil`、`repository.sourceJobUIDs` 已包含本批全部 Job UID 且 `RepositoryReady=True` |
| 过程仓失败收口 | `repository.transition` 原样保留（`repositoryUID` / `inputs` 与目标一致）、`RepositoryReady=False/reason=RepositoryFailed` 且 `release.phase=Failed` |
| 发布检查点（写 `release.phase=Pending` 与 `release.transition`） | `release.transition.sourceRepositoryUID`、`excludeSpecs` 与目标一致 |
| 发布收口 | `release.phase` 为目标相位、`release.transition=nil` 且 `PublishSucceed` 的 `status`/`reason` 与目标一致 |

判定"已达成"则跳过本步、按目标状态的后续步骤继续；判定"未达成"则重新 GET 取最新 `resourceVersion` 后按原目标重做本步。

**依赖读取的统一语义**

本节只适用于本章各阶段对依赖对象的读取：同名 `Build`（`spec.buildTarget`）、同名 `BuildInfo`（`status.phase` 门禁与策略输入）与 `Job`（候选输入与 manifest）。以下三者**不适用**本节：本键驱动对象 `RpmRepo` 的缺失（按 7.2 的分支处理）；Artifact Manager 的 `GetRepository` / `GetRelease` 返回（含 `404` 与 `Creating` / `Prepared` / `Failed`，按 7.2 / 7.3 / 第八章处理）；`Project`（只作为字符串在 `PublishPolicyInput` 中透传、不读取对象，见 3.3）。除 `NotFound`（不进入通用分类，单独按下方角色表处理）外，读取错误按下列分类：

- 其它读取错误（网络、超时、`408` / `429` / `5xx`）→ 返回零值 + 原始错误（框架退避重试）；例外：若 `apierrors.SuggestsClientDelay(err) > 0`（仅 `429` / `503`，防御性分支——ebs-apiserver 当前不返回该提示），返回 `ReconcileResult{RequeueAfter: <提示值>}` + `nil`，不写 status、不叠加框架退避；
- 永久错误（`401` / `403` / `400` / `422` 或响应身份契约错误）→ 返回零值 + `controller.NewPermanentError`；Manager / round context 取消 → 返回 `ctx.Err()`；以上分支一律不写 `RpmRepo.status`、不写条件、不调用 Artifact Manager、不使用 `RequeueAfter`（返回形态见 7.4）。该分支**仅通过日志暴露**（框架 `result=permanent-error` 日志行已含 `key` 与错误内容；控制器不新增固定 reason、不新增计数、不写 status），RpmRepo 保留在轮询集合中，控制器权限 / apiserver 配置修复后下一轮自动继续；持续出现时按运维手册排查控制器权限与 apiserver 配置。

| 依赖 `NotFound` 的处置 | 计数 |
| --- | --- |
| `Build`（归属与目标来源）：过程仓键按 7.2——`RpmRepo` 不存在 → 零值 + `nil` 收敛；`RpmRepo` 存在（孤儿）→ 告警 + 零值 + `nil`、不推进。发布流程按 7.3 在读取选中对象的同名 Build 时 NotFound → 跳过该对象、告警，同目标其它对象照常推进；标签与 Build 目标不一致同样跳过该对象 | `rpmrepo_controller_build_missing_total`（仅过程仓键的孤儿收敛；发布侧读取 Build 失败或标签不一致的跳过只记 `reason=ReleaseGroupBuildMissing` / `reason=RpmRepoLabelMismatch` 告警日志，都不重复计数） |
| `BuildInfo`（Job 集合固定的门禁与策略输入）：视为未就绪——本轮不推进、零值 + `nil`、不计错，仅输出 `reason=BuildInfoNotReady` 告警日志 | — |
| `Job` 对象（候选输入）：该输入已不存在——跳过、不计入候选、不阻塞发布触发（与 manifest `404` 同级），后续轮询复查 | —（仅告警日志） |
| `Job` manifest（输入就绪度）：`404` / `Open` / `Completing` / `Failed` 的语义沿用第六章，不套用本节的读取错误分类 | 见第六章 |
| `Project`（策略输入，字符串）：不读取对象，无 `NotFound` 语义 | — |

后续小节只描述各自特有规则（例如过程仓键的收敛 / 孤儿计数、发布流程读取选中对象 Build 失败或标签不一致时的跳过与 `reason=ReleaseGroupBuildMissing` / `reason=RpmRepoLabelMismatch` 告警），读取错误的通用分类不再重复。

### 7.1 主流程

主流程只做 key 派分与入口校验，两个分支的流程分别见 7.2 与 7.3：解析 key（`kind/{project}/{...}`）并校验——`kind` 不是 `build` / `release`、或 Project 与后续键段为空时，记录错误并返回零值 + `controller.NewPermanentError`；`build/{project}/{buildName}` 执行 7.2 过程仓流程；`release/{project}/{os}/{arch}` 执行 7.3 发布流程。

### 7.2 过程仓流程（`build/{project}/{buildName}`）

**入口**：先按确定性名称 GET 同名 `RpmRepo`，再 GET 同名 `Build`，两次读取都完成后才进入判定。`Build` 的可用性**优先**于 `RpmRepo` 分支——`Build.spec.buildTarget.os` / `arch` 是发布键入队与后续目标标签过滤的唯一来源。

- `Build` NotFound 且 `RpmRepo` 也不存在 → 本键收敛；
- `Build` NotFound 但 `RpmRepo` 存在（该对象已失去归属：同名 Build 被删除）→ 记录 `reason=BuildMissing` 告警并计入 `rpmrepo_controller_build_missing_total`；不创建或删除对象，孤儿 RpmRepo 由人工清理，本控制器不做级联删除，后续每轮由 `rpmrepos` 轮询复查；
- `Build` 的其它读取错误与永久错误 → 按章首「依赖读取的统一语义」处理，本轮不推进；
- `Build.status.phase=Aborted`（用户已中止本轮构建）→ 不再物化、不入队发布键、不调用 Artifact Manager、不做策略判定：一次 CAS 写发布终局 `release.phase=Aborted` + `release.transition=nil` + `release.updatedAt` 与 `PublishSucceed=False`（reason `RepositoryPublishAborted`），输出 `reason=BuildAborted` 告警日志，返回零值 + `nil`；`repository.*` 不写（在途 `repository.transition` 原样保留为已放弃批次）。该分支优先于 `RpmRepo` 判定与下方过程仓推进，`release` 已非空时同样直接收口；
- 同名 `BuildInfo` 不可用 → 同样按章首统一语义处理（`NotFound` 视为未就绪：本轮等待，仅输出 `reason=BuildInfoNotReady` 告警日志）；本节第 5 步的发布触发检查使用 `BuildInfo.status.phase=Completed` 作门禁时同样适用；
- 本节的收敛与等待类分支一律不写 status、不调用 Artifact Manager、不入队发布键，返回形态见 7.4；例外是上一条 `Aborted` 中止收口，它会写一次发布终局（`release.*`）后直接返回。

`RpmRepo` 判定（使用上面已读到的 `Build`）：

- `RpmRepo` NotFound：返回可重试错误并退避重试（视为 Build Controller 的前置创建尚未完成或对象被异常删除，不创建替代对象）；
- `RpmRepo.metadata.deletionTimestamp` 非空 → 本键收敛；
- 成功且 `status.release` 不为空 → 通过 `BaseController.Enqueue` 入队 `release/{project}/{os}/{arch}`（`{os}` / `{arch}` 取 `Build.spec.buildTarget.os` / `Build.spec.buildTarget.arch`），返回零值 + `nil`；`status.release` 为空 → 按下方「过程仓推进」执行（`Build` 用于读取 `spec.buildTarget`）。

过程仓推进（无相位，按顺序执行）：

1. **在途判断**（`repository.transition != nil`；`status.release` 非空时入口已提前返回，因此"已放弃批次"分支只在 `release` 被外部清空后可达）：
   - 在途批次（`RepositoryReady` 不是 `False/reason=RepositoryFailed`）→ 跳至第 3 步；
   - 已放弃批次（`transition != nil` 且 `RepositoryReady=False/reason=RepositoryFailed`）→ 视为发布终局被外部清空：一次 CAS 重写发布终局 `release.phase=Failed` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False`（reason `RepositoryPublishFailed`），不写 `repository.*`、不调用 Artifact Manager、**不重复计** `rpmrepo_controller_repository_failed_total`，记录 `reason=RepositoryFailed` 告警日志，返回零值 + `nil`；
   - 无在途批次 → 第 2 步。
2. **提交候选批次**（候选规则见第六章）：有候选 Job → 若排序最前的候选自身输入超过 `--rpmrepo-max-input-bytes`，**不形成批次**、不写 `repository.transition`，输出一次 `reason=InputTooLarge` 并按发布失败终局收口（一次 CAS 写 `release.phase=Failed` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False`（reason `RepositoryPublishFailed`）；不写 `repository.*`、不调用 Artifact Manager；计一次 `rpmrepo_controller_repository_failed_total`；返回零值 + `nil`）；否则按第六章选出 `inputs` / `baseRepositoryUID` / `repositoryUID`，以最新 `resourceVersion` 一次 CAS 写 `repository.transition` 与 `repository.updatedAt`（409 时重新 GET 重算后重做本步），随后调 `SubmitRepository` 并把响应交给第 3 步；无候选 Job → 第 5 步。控制器不写任何重试计数（计数以响应 `attempt` 为准）。
3. **结果分流**（`SubmitRepository` 的响应，或 `GetRepository(repository.transition.repositoryUID)` 的响应；冻结 `repository.transition`，沿用 `inputs` / `baseRepositoryUID` / `repositoryUID`，不得重新选批、更换基础仓或重算）：

| 响应 | 处理 |
| --- | --- |
| `202` / `200 Creating` | 保留 `repository.transition`，按 `ReconcileResult{RequeueAfter: pollAfterSeconds}` + `nil` 延迟重入（不叠加框架退避），本轮不写 status；`202` 覆盖三种子情形：首次接受（`Creating, attempt=1`）；相同请求正在执行或排队（返回原 `Creating`，不重复入队、不增加 attempt）；相同请求处于可重试 `Failed`（服务端原子增加 attempt、清空旧 Failure、写为 `Creating` 后重新入队） |
| `404`（Artifact Manager 无该 `repositoryUID` 记录） | 用完全相同的 `repositoryUID` / `Manifests` / `baseRepositoryUID` 重放 `SubmitRepository`，再按本表分流；这是"`repository.transition` 已写、`SubmitRepository` 尚未被接受"崩溃窗口的恢复路径，不得直接失败收口。注意重放会新建记录并把 `attempt` 重置为 `1`（重试预算随之从零开始，见 §8） |
| `200 Ready` | 执行「成功收口」（第 4 步） |
| 可重试失败（已落到记录）：`200 Failed{retryable=true}` | 按第 4 步的「重试」处理：响应 `attempt < limit + 1` 且在退避窗口（锚点为响应 `updatedAt`）之后则重放同一请求（不写 status）；窗口未过只返回剩余等待；`attempt >= limit + 1` 则按第 4 步的「失败收口」处理 |
| 可重试失败（未落到记录）：`429`、`503`、网络错误 / 超时 / 响应无法解析 | 服务端未接受新的执行尝试、`attempt` 不变，因此**不消耗重试预算**；保留 `repository.transition`，按 `Retry-After`（或退避）重放同一请求，不写 status；同一退避窗口内的重复入队不触发重放 |
| 不可重试失败：`200 Failed{retryable=false}`、`409 RepositoryIdentityConflict`、`409 RepositoryDeleting`、`410 MaterializationInputExpired`、`422 ManifestNotReady` / `BaseRepositoryNotReady` / `ManifestInvalid` / `ManifestContainsNoPackages` / `InvalidRepositoryRequest` / `RepositoryUIDMismatch` / `InvalidManifestReference` | 直接进入「失败收口」（不重试、不消耗预算；`409 RepositoryIdentityConflict` 额外记录告警） |
| `200 Deleting`（该 `repositoryUID` 的记录正在删除） | 直接进入「失败收口」（不等待删除收尾，与 `409 RepositoryDeleting` 同口径），并记录 `reason=RepositoryFailed` 告警 |

4. **收口**（写入字段清单见第四章字段来源表，本节只列特有判定）：
   - **成功收口**：把本批 Job UID 并入 `repository.sourceJobUIDs`（去重排序），一次 CAS 提交目标 status——`repository.contentURL` 取 Artifact Manager 的 Ready 响应、`repository.repositoryUID` 取本批 `repository.transition.repositoryUID`、`repository.transition=nil`、`repository.updatedAt` 与 `RepositoryReady=True` 条件；随后若仍有可入选 Job 则重新入队本键组下一批，否则进入第 5 步；
   - **重试**（`200 Failed{retryable=true}` 且响应 `attempt < --rpmrepo-materialize-retry-limit + 1` 且已过退避窗口，或 `429` / `503` / 网络 / 超时 / 未解析）：**不写 status**（`transition` 与版本字段原样保留），计一次 `rpmrepo_controller_materialize_retries_total`，按退避 `RequeueAfter` 用同一请求重放 `SubmitRepository`（`429`/`503` 优先按其 `Retry-After`；未落到记录的错误不影响预算）。退避窗口未过（锚点为响应 `updatedAt`）时本轮只返回剩余等待（`RequeueAfter`）+ `nil`：不调用 `SubmitRepository`、不写 status、不计 `rpmrepo_controller_materialize_retries_total`；
   - **失败收口**（可重试失败且响应 `attempt >= --rpmrepo-materialize-retry-limit + 1`，或遭遇不可重试失败）：一次 CAS 提交目标 status——`repository.transition` **原样保留**（`inputs` / `baseRepositoryUID` / `repositoryUID` 均为失败时刻取值）、`repository.updatedAt`、`RepositoryReady=False` 条件（reason `RepositoryFailed`），并同次写发布失败终局 `release.phase=Failed`、`release.transition=nil`、`release.updatedAt` 与 `PublishSucceed=False` 条件（reason `RepositoryPublishFailed`）；计一次 `rpmrepo_controller_repository_failed_total`；不清空 `repository.repositoryUID` / `contentURL` 等版本字段；不调用 `SubmitRelease` / `ActivateRelease`、不额外入队发布键；本轮返回零值 + `nil`。
5. **发布触发**（三项前置）：`BuildInfo.status.phase=Completed` 且本轮候选扫描为空、① 无未就绪输入（无 manifest 为 `Open` / `Completing` 的 Job）、② `repository.transition=nil` 时——③ 若 `repository.sourceJobUIDs` 非空（本对象已产出可发布版本）→ 通过 `BaseController.Enqueue` 入队 `release/{project}/{os}/{arch}`（`{os}` / `{arch}` 取 `Build.spec.buildTarget`）并返回零值 + `nil`；**③ 不成立（`sourceJobUIDs` 为空，本对象从未产出任何版本、不存在可发布内容，无论 `repositoryUID` / `contentURL` 是否被预置了继承基线）→ 直接写发布失败终局**：一次 CAS 写 `release.phase=Failed`、`release.transition=nil`、`release.updatedAt` 与 `PublishSucceed=False` 条件（reason `RepositoryPublishFailed`），计一次 `rpmrepo_controller_release_failed_total`，不写 `repository.*`、不调用 Artifact Manager、不入队发布键，返回零值 + `nil`；①② 任一不满足 → 等待（不写 status，返回零值 + `nil`）。

本键的等待、收敛与失败收口类分支统一返回零值 + `nil`（例外：带 `Retry-After` 的 `429` / `503` 返回 `ReconcileResult{RequeueAfter}` + `nil`）；错误分类与返回形态见 7.4。

### 7.3 发布流程（`release/{project}/{os}/{arch}`）

`List rpmrepos`（项目级 List）：两个选择器同时下发、与 Project 路径隐含的 namespace 条件按 AND 组合：

- `fieldSelector=status.release.phase!=Ready,status.release.phase!=Failed,status.release.phase!=Aborted`：该字段只支持 `Equals` / `NotEquals`、不支持 `NotIn`，因此用三个 `!=` 表达「非发布终态或无 `release`」；字段缺失（`release` 为 null）的对象会保留；
- `labelSelector=ebs.io/target-os={os},ebs.io/target-arch={arch}`：`{os}` / `{arch}` 取自发布键（由过程仓键按 `Build.spec.buildTarget` 构造，与 §7.2 入队同源）；标签由 Build Controller 在创建 RpmRepo 时写入、apiserver 创建校验强制存在。

返回集合即本键候选集、顺序不保证：集合内先取 `release.transition` 非空的在途对象（在途判据不受候选谓词约束），无在途对象时才用候选谓词筛出发布候选——`repository.transition==nil`、`repository.sourceJobUIDs` 非空（本对象已产出的版本；`repositoryUID` / `contentURL` 非空只代表存在可读仓库指针，可能来自 Build Controller 预置的继承基线，不能作为候选依据）且 `release.phase` 不属于 `{Ready, Failed, Aborted}`。

选对象与前置判定（本键候选集内一律按 `creationTimestamp`、`metadata.name` 升序取第一个）：

**选对象**（每轮只处理一个对象，其余留给后续轮次）：

- **优先处理在途**：在途判据为 `release.transition` 非空（相位可能是 `Pending` / `Creating` / `Prepared`），该判据不受候选谓词约束（避免版本指针或 `sourceJobUIDs` 异常影响在途恢复）；存在在途对象时取之为本次对象；`metadata.deletionTimestamp` 非空（含在途对象）→ 本轮不推进、不驱动激活或收口、不启动新发布，返回零值 + `nil`；存在多个在途 → 记录 `reason=MultipleReleaseInFlight` 告警日志，其余在途对象等待本次对象收口；
- **发布候选**：无在途对象时，在候选集内按序取第一个 `metadata.deletionTimestamp` 为空且 `BuildInfo.status.phase=Completed` 的对象作为本次对象（非发布终态、过程仓不在途、本对象已产出可发布版本已由上面的候选谓词保证）；`BuildInfo` 读取失败 / `NotFound` / `phase != Completed` → 本轮不推进、不使用 `PublishPolicy` 判定、返回零值 + `nil`，由过程仓键 resync 重新入队发布键；
- 无候选 → 本键收敛（零值 + `nil`）。

**选中后的前置判定**（在途对象与发布候选同等适用；未通过者不得进入「有检查点」/「无检查点」）：

- **读取并校验同名 `Build`（每轮一次）**：对本次对象 GET 一次同名 `Build`，它同时是 `PublishPolicyInput.Build` 与 `TargetOS` / `TargetArch` 的来源——`Build` NotFound → 跳过该对象、记录 `reason=ReleaseGroupBuildMissing` 告警、不计数、本轮结束；`Build.spec.buildTarget.os` / `arch` 与对象 `metadata.labels[ebs.io/target-os]` / `[ebs.io/target-arch]` 不一致 → 跳过该对象、记录 `reason=RpmRepoLabelMismatch` 告警、不计数、本轮结束（不调用 Artifact Manager）；其它读取错误按章首「依赖读取的统一语义」处理（本轮不推进）；`Build.status.phase=Aborted` → 走下一行的「Build 中止收口」。标签由 apiserver 创建校验强制存在、`Build.spec` 创建后不可变，正常路径不会漂移，该校验只兜人工改坏标签；
- **Build 中止收口（先于「有检查点」与「无检查点」两个分支；在途对象不因存在 `release.transition` 检查点而例外）**：本次对象（在途或发布候选）的同名 `Build.status.phase=Aborted` 时——一次 CAS 写 `release.phase=Aborted`、`release.transition=nil`、`release.updatedAt` 与 `PublishSucceed=False`（reason `RepositoryPublishAborted`）；**不调用** Artifact Manager（不 `GetRelease`、不重放 `SubmitRelease`、不 `ActivateRelease`）、不做策略判定；`repository.*` 不写（在途 `repository.transition` 原样保留为已放弃批次）；输出 `reason=BuildAborted` 告警日志，返回零值 + `nil`；该对象随即被轮询过滤与发布候选排除，同目标其它候选不受阻塞；早中止（`BuildInfo` 未完成、`repository.sourceJobUIDs` 为空）的对象由过程仓键的同一分支收口；

`推进状态`：

对上面已选定、并通过前置判定的本次对象，按检查点状态进入下面两个分支推进：

- 无检查点（条件：`release.transition` 为空且 `release.phase` 不属于 `{Ready, Failed, Aborted}`）：构造 `PublishPolicyInput`（`Project`、`Build`、`BuildInfo`、`SourceRepositoryUID=repository.repositoryUID`、`TargetOS`/`TargetArch` 取 `Build.spec.buildTarget`）调 `PublishPolicy.Decide`（`Build` 已在上一步读取并校验，`BuildInfo` 的读取失败按章首统一语义处理，此处不再重复分类）——策略返回 error → 返回可重试错误，不写 status、不提交发布；不发布 → 记录 `reason=ReleasePolicySkipped` 告警日志，仅在 `release` 非 nil 时一次 CAS 写 `status.release=nil`（不写 condition），本轮继续评估下一个候选、全部候选都不发布则返回零值 + `nil`；发布 → 一次 CAS 写检查点（`release.phase=Pending`、`release.transition.sourceRepositoryUID` 取 `repository.repositoryUID`、`release.transition.excludeSpecs` 为本次策略结果（去重并按字典序排序，不二次调用策略）、`release.updatedAt`；409 重新 GET 重算后重做本步），随后调 `SubmitRelease`（同 `buildName`、同 `sourceRepositoryUID`、同 `excludeSpecs`），按响应分流，返回形态见下表：

| 提交响应 | 处理 |
| --- | --- |
| `202` + `state=Creating` | 写 `release.phase=Creating` 与 `release.updatedAt`，按 `ReconcileResult{RequeueAfter: pollAfterSeconds}` + `nil` 延迟重入（不叠加框架退避） |
| `202` + `state=Prepared` | 写 `release.phase=Prepared` 与 `release.updatedAt`，同样延迟重入 |
| `200` + `Ready` | 执行「成功收口」 |
| `200` + `Failed{retryable=false}` / `409 ReleaseIdentityConflict` / `422` | 执行「失败收口」 |
| `429` / `503` 且 Artifact Manager 返回 `Retry-After` | 保留检查点（相位保持 `Pending`），返回 `ReconcileResult{RequeueAfter: <Retry-After>}` + `nil`；不提交与检查点不同的请求（不重新决策 `excludeSpecs`） |
| 网络错误 / 超时 / 响应无法解析 / 响应契约错误（`attempt < 1`、`updatedAt` 缺失、响应身份不一致）/ 无 `Retry-After` 的 `429`、`503` | 保留检查点（相位保持 `Pending`），返回原始可重试错误（框架退避）；不提交与检查点不同的请求（不重新决策 `excludeSpecs`），结果未知时下一轮走「有检查点」分支 |

- 有检查点（条件：`release.transition` 非空）：冻结检查点——沿用 `sourceRepositoryUID` 与 `excludeSpecs`，不按最新 Project / BuildInfo 重算；`release.transition` 即请求的全部可变输入，摘要一致性由 Artifact Manager 保证（相同摘要幂等、不同摘要返回 `409`）。调 `GetRelease(buildName)`，按响应分流：

| 查询响应 | 处理 |
| --- | --- |
| `Creating` | 相位不一致时一次 CAS 补齐 `release.phase=Creating` 与 `release.updatedAt`；按 `ReconcileResult{RequeueAfter: pollAfterSeconds}` + `nil` 延迟重入（不叠加框架退避） |
| `Prepared` | 相位不一致时一次 CAS 补齐 `release.phase=Prepared` 与 `release.updatedAt`；执行「激活」 |
| `Ready` | 执行「成功收口」 |
| `Failed{retryable=true}` | 用完全相同的请求重放 `SubmitRelease`，按提交响应分流 |
| `Failed{retryable=false}` | 执行「失败收口」 |
| `404`（提交未被确认或记录不存在） | 用完全相同的请求重放 `SubmitRelease`，按提交响应分流 |
| `Deleting`（发布记录正在删除） | 执行「失败收口」（不等待删除收尾），记录 `reason=RepositoryPublishFailed` 告警 |
| `429` / `503` 且 Artifact Manager 返回 `Retry-After` | 保留检查点，返回 `ReconcileResult{RequeueAfter: <Retry-After>}` + `nil` |
| 网络错误 / `5xx` / 响应无法解析 / 响应契约错误（`attempt < 1`、`updatedAt` 缺失、响应身份不一致）/ 无 `Retry-After` 的 `429`、`503` | 保留检查点，返回原始可重试错误（框架退避） |

- 激活（条件：Artifact Manager 返回 `Prepared`）：调 `ActivateRelease(buildName)`——`200`（切换成功或已是当前版本）→ 执行「成功收口」；可重试错误 → 保持 `release.phase=Prepared` 与检查点：带 `Retry-After` 的 `429` / `503` 返回 `ReconcileResult{RequeueAfter: <Retry-After>}` + `nil`，其余（网络 / 超时 / 响应无法解析 / 响应契约错误）返回原始可重试错误（框架退避），下一轮重新激活；不可重试错误 → 执行「失败收口」。
- 收尾动作（写入字段清单见 §5.1 与 §4 字段来源表，本节只列特有项）：
  - 成功收口（条件：`SubmitRelease` 返回 `200`+`Ready`、`GetRelease` 返回 `Ready` 或 `ActivateRelease` 返回 `200`）：一次 CAS 提交目标 status——`release.phase=Ready`、`release.sourceRepositoryUID`（取 `release.transition.sourceRepositoryUID`）、`release.contentURL`、`release.updatedAt`、`release.transition=nil` 与 `PublishSucceed=True` 条件（reason `RepositoryPublished`）；返回 `ReconcileResult{Requeue: true}` + `nil`，立即处理同目标下一个候选；
  - 失败收口（条件：不可重试失败）：一次 CAS 提交目标 status——`release.phase=Failed`、`release.updatedAt`、`release.transition=nil`（检查点即「在途」判据，收口必须清空，否则会被反复驱动）与 `PublishSucceed=False` 条件（reason `RepositoryPublishFailed`）；失败详情只进日志（`reason=RepositoryPublishFailed`）；写完后返回零值 + 原始错误，**不进入 `Requeue`**——发布终局已持久化，该对象随即被发布候选与轮询过滤排除，失败不会阻塞同 Project/OS/Arch 的其他 Build（后续候选由过程仓键 resync 重新入队）。

重启恢复（任意阶段）：

| 崩溃点 | 恢复路径 |
| --- | --- |
| 无检查点且非终态 | 重新判定策略（纯函数、`excludeSpecs` 去重排序），结果与崩溃前一致 |
| 检查点已写、`SubmitRelease` 未被接受 | `GetRelease` 返回 `404`，用同一请求重放 |
| `SubmitRelease` 已接受、相位未补齐 | `GetRelease` 返回 `Creating` / `Prepared`，补齐相位后继续等待或激活 |
| 激活已成功、收口写入前 | `GetRelease` 返回 `Ready`，直接成功收口 |
| 失败收口写入前 | `GetRelease` 仍返回 `Failed{retryable=false}`（或不可重试错误码），重新收口 |
| 收口写入结果未知 | 先 GET 与目标 status 比对：已是 `Ready` / `Failed` / `Aborted` 则本轮结束，仍是 `Pending` / `Creating` / `Prepared` 则按「有检查点」续跑 |
| `Requeue: true` 丢失 | 发布键由过程仓 reconcile 每轮 resync 重新入队 |

任何时点都不存在「只存在于内存的进度」，也不存在需要人工清理的半成品检查点。

约定：

- 发布只写 `status.release.*` 与顶层 `conditions` 中的发布条目，不改动 `status.repository.*`；
- 是否需要发布由 `PublishPolicy.Decide` 决定（控制器不内置该判定；判定为不发布的 RpmRepo 不落任何持久结果，等价于每轮重新判定，因此策略必须是同输入同结果的纯函数）；发布请求摘要不落 `RpmRepo.status`——请求的全部可变输入已冻结在 `release.transition`，摘要在 Artifact Manager 侧计算并用于幂等，控制器不做本地摘要比对，`release.transition` 被外部改动导致的漂移由 AM 的 `409` 兜住并按失败收口；
- 同一 `{project}/{os}/{arch}` 同时只允许一个在途发布（`release.transition` 非空）；在途对象优先于尚未开始的对象，由 `release/` 键串行与「优先处理在途」共同保证；过程仓键与发布键可并行推进；单活动实例与首版不引入超时（在途检查点会一直阻塞该目标的新发布，长期停留由日志与指标暴露、人工处理）见第十章。

### 7.4 队列结果映射

`Sync` 返回 `(ReconcileResult, error)`，BaseController 据此决定该 key 的重入方式：

| 场景 | 返回 |
| --- | --- |
| RpmRepo 与 Build 都不存在（两者都缺失）、RpmRepo 删除中、无候选 Job、发布候选 `metadata.deletionTimestamp` 非空（在途对象不驱动激活/收口）、无发布候选（含策略判定不发布且候选扫描结束）、发布无变化 | 零值 + `nil` |
| RpmRepo 存在但同名 Build NotFound（孤儿 RpmRepo） | 零值 + `nil`；记录 `reason=BuildMissing` 告警并计入 `rpmrepo_controller_build_missing_total`；不写 status、不创建/删除对象、不入队发布键 |
| 同名 BuildInfo 不可用（两种情况：同名 BuildInfo 还没创建，即 NotFound；或已创建但 status.phase 还没到 Completed；都属未就绪）| 零值 + `nil`（不写 status、不调用 Artifact Manager、不计错，仅输出 `reason=BuildInfoNotReady` 告警日志；由过程仓键 resync 重新入队） |
| 输入 Job 的 manifest 为 `404` / `Open` / `Completing` / `Failed` | 跳过该输入：不写 status、不计数，分别记录 `reason=InputManifestMissing` / `InputManifestNotReady` / `InputManifestFailed` 告警；`Open` / `Completing` 阻塞发布触发，其余不阻塞；本轮若无候选 Job 则返回零值 + `nil` |
| 候选校验期间 `Job` 对象 NotFound | 跳过该输入：不写 status、不计数、不阻塞发布触发，记录 `reason=InputObjectMissing` 告警；本轮返回按所在步骤（无候选 Job → 零值 + `nil`） |
| 依赖（`Build` / `BuildInfo` / `Job`）读取失败（网络、超时、`408` / `429` / `5xx`） | 零值 + 原始错误（框架退避）；例外：`apierrors.SuggestsClientDelay(err) > 0` 的 `429` / `503` → `ReconcileResult{RequeueAfter: <提示值>}` + `nil`；`401` / `403` / `400` / `422` 与响应身份契约错误 → 零值 + `controller.NewPermanentError` |
| 发布流程读取选中对象的同名 `Build` 返回 NotFound | 跳过该对象：不写 status、不计数（**不计入** `rpmrepo_controller_build_missing_total`），记录 `reason=ReleaseGroupBuildMissing` 告警，返回零值 + `nil`（其余候选留给后续轮次） |
| 选中对象的 `ebs.io/target-os` / `ebs.io/target-arch` 标签与同名 `Build.spec.buildTarget` 不一致 | 跳过该对象：不写 status、不计数、不调用 Artifact Manager、不做策略判定，记录 `reason=RpmRepoLabelMismatch` 告警，返回零值 + `nil` |
| RpmRepo NotFound（`Build` 可读） | 可重试错误（视为前置创建尚未完成或对象被异常删除，不创建替代对象） |
| 等待 Artifact Manager 完成（过程仓物化 `Creating`、发布 `Creating`） | `ReconcileResult{RequeueAfter: pollAfterSeconds}` + `nil` |
| 过程仓可重试失败且重试预算未用满（`Failed{retryable=true}` 且响应 `attempt < limit + 1`；或 `429` / `503` / 网络/超时/未解析；或响应契约错误 `attempt < 1` / `updatedAt` 缺失） | `ReconcileResult{RequeueAfter: <退避，窗口未过时为剩余等待；无可用锚点时用初始退避>}` + `nil`（**不写 status**；已过退避窗口才重放，未过则只等待；`429`/`503` 优先 `Retry-After`；未落到记录的错误不消耗预算） |
| 过程仓提升成功后仍有候选 Job / 发布 `Ready` 后仍有发布候选 | `ReconcileResult{Requeue: true}` + `nil` |
| 策略判定不发布且 `status.release` 有残留（写一次 `release=nil`） | 记录 `reason=ReleasePolicySkipped` 告警；写入按 status 冲突规则处理，随后继续评估同目标的下一个候选；全部候选都不发布 → 零值 + `nil` |
| 检查点已写但 `GetRelease` / `GetRepository` 返回 `404`（提交未被接受） | 用同一请求重放 `SubmitRelease` / `SubmitRepository`，按提交响应分流，不写失败条件（过程仓 404 见 7.2、发布 404 见 7.3） |
| status 写入返回 409 Conflict | `ReconcileResult{RequeueAfter: 1s}` + `nil`（重新 GET 后重算，不使用立即重入） |
| Artifact Manager 发布侧 / 读取侧可重试错误（发布提交、查询、激活的 `429`、`503`、网络、超时、未知结果） | 带 `Retry-After` 的 `429` / `503` → `ReconcileResult{RequeueAfter: <Retry-After>}` + `nil`；其余 → 零值 + 原始错误（框架退避） |
| 过程仓失败收口（重试耗尽，或不可重试失败：`409`/`410`/`422`/`Failed{retryable=false}`） | 一次 CAS 写 `RepositoryReady=False` 与发布终局 `release.phase=Failed` + `PublishSucceed=False`（`repository.transition` 原样保留、版本字段不清空），返回零值 + `nil` |
| 过程仓 `GetRepository` 返回 `200 Deleting` | 同「过程仓失败收口」：一次 CAS 写 `RepositoryReady=False` 与发布终局 `release.phase=Failed` + `PublishSucceed=False`（不等待删除收尾），记录 `reason=RepositoryFailed` 告警，返回零值 + `nil` |
| 已放弃批次遭遇 `release` 被外部清空（`repository.transition` 非空 + `RepositoryReady=False` + `release` 为空） | 一次 CAS 重写发布终局 `release.phase=Failed` + `release.transition=nil` + `PublishSucceed=False`（不写 `repository.*`、不调用 Artifact Manager、**不重复计** `rpmrepo_controller_repository_failed_total`），记录 `reason=RepositoryFailed` 告警，返回零值 + `nil` |
| 无可用版本的发布失败终局（`BuildInfo=Completed`、无候选、无未就绪输入、`transition=nil`，但 `repository.sourceJobUIDs` 为空，含仅预置了继承基线的对象） | 一次 CAS 写 `release.phase=Failed` + `PublishSucceed=False` 并清 `release.transition`（不写 `repository.*`、不调用 Artifact Manager），返回零值 + `nil` |
| 首个候选自身超限（第六章「批次构成」；不受 `BuildInfo=Completed` 门禁约束） | 一次 CAS 写 `release.phase=Failed` + `release.transition=nil` + `PublishSucceed=False`（不写 `repository.*`、不调用 Artifact Manager），记录 `reason=InputTooLarge` 告警，计一次 `rpmrepo_controller_repository_failed_total`，返回零值 + `nil` |
| 发布不可重试失败（发布 `409`/`422`、`Failed{retryable=false}`、`GetRelease` 返回 `Deleting`） | 一次 CAS 写 `release.phase=Failed` + `PublishSucceed=False` 并清 `release.transition`，记录 `reason=RepositoryPublishFailed` 告警，再返回零值 + 原始错误（**不进入 `Requeue`**） |
| 同名 `Build.status.phase=Aborted`（过程仓键或发布键任一处先读到；含在途发布） | 一次 CAS 写 `release.phase=Aborted` + `PublishSucceed=False`（reason `RepositoryPublishAborted`）并清 `release.transition`，记录 `reason=BuildAborted` 告警；不调用 Artifact Manager、不做策略判定、`repository.*` 不写，返回零值 + `nil`（不计入 `release_failed_total`） |
| Artifact Manager 响应契约错误——发布路径（发布提交 / 查询 / 激活的 `attempt < 1`、`updatedAt` 缺失、响应身份不一致） | 保留检查点与相位，返回零值 + 原始可重试错误（框架退避）；过程仓路径的同类错误按上表「过程仓可重试失败」行以 `RequeueAfter` 表达 |

约定：`err != nil` 时不返回非零 `ReconcileResult`；等待类用零值或 `RequeueAfter` 表达，不用 error 表达正常等待；过程仓重试类一律用 `RequeueAfter` 表达（退避随 Artifact Manager 响应的 `attempt` 指数增长，不叠加框架退避、不写 status），不返回 error；过程仓失败收口与发布不可重试失败收口都在写完成后返回（过程仓返回零值 + `nil`，发布侧返回零值 + 原始错误），`Aborted` 中止收口同样返回零值 + `nil`（它是终局而非错误），三者都不进入 `Requeue`、避免重复写条件；带 `Retry-After` 的 `429` / `503`（依赖读取与 Artifact Manager 侧）由控制器直接以 `RequeueAfter` 表达、不进入 error 路径，框架的 rate limiter / slow retry 只作用于其余 `err != nil` 路径，status 写入的 `429` / `503` 仍由框架按 `WriteError` 的 `Retry-After` 处理；`pollAfterSeconds` 缺失或 ≤ 0 时按 5s 缺省。

## 八、Artifact Manager 契约

接口与状态（详见 `docs/zh/design/artifact-manager.md` 第九章；Artifact Manager 侧的发布接口已落地，联调前核对其稳定错误码与本节一致）：

| 用途 | 请求 | 关键响应 |
| --- | --- | --- |
| 提交物化 | `POST /internal/v1/repositories` | 见下表 |
| 查询状态 | `GET /internal/v1/repositories/{repositoryUID}` | `200` + `RepositoryResponse`；不存在返回 `404 RepositoryNotFound` |
| 删除仓库 | `DELETE /internal/v1/repositories/{repositoryUID}` | `202`（进入 `Deleting`）或 `204`（已删除）；**本控制器不调用该接口**（过程仓记录删除由运维或上层流程执行） |
| 仓库内容 | `GET /repositories/v1/{repositoryUID}/{path}` | 仅 `Ready` 可用；控制器只使用 `contentURL`，不直接读取内容 |
| Job 清单 | `GET /artifacts/v1/projects/{project}/jobs/{job}/manifest?jobUID={uid}` | `JobUploadManifest`：`state` ∈ `Open`/`Completing`/`Completed`/`Failed` |

提交响应处理：

| 响应 | 含义 | 控制器动作 |
| --- | --- | --- |
| `202` | 首次接受；相同请求正在执行或排队（返回原 `Creating`，不重复入队、不增加 attempt）；相同请求处于可重试 `Failed`（原子增加 attempt、清空旧 Failure、写为 `Creating` 后重新入队） | 保留 `repository.transition`，按 `pollAfterSeconds` 延迟重入 |
| `200` + `Ready` | 相同请求已经 Ready | 直接提升 |
| `200` + `Failed{retryable=false}` | 相同请求处于不可重试 `Failed` | 不可重试失败收口 |
| `409 RepositoryIdentityConflict` | 同 `repositoryUID` 不同请求摘要 | 不可重试失败收口，并记录 `reason=RepositoryFailed` 告警（程序错误或需人工介入） |
| `409 RepositoryDeleting` | 该 `repositoryUID` 的记录正在删除（`Deleting`） | 不可重试失败收口（`FailureInfo.retryable=false`）；删除完成后重放会新建记录，但本设计不用重放等待删除收尾 |
| `410 MaterializationInputExpired` | Manifest、Artifact 或基础仓已过期 | 不可重试失败收口 |
| `422` | `InvalidRepositoryRequest` / `RepositoryUIDMismatch` / `InvalidManifestReference`（请求字段非法、UID 计算不一致、Manifest 引用重复）、`ManifestNotReady`、`BaseRepositoryNotReady`、`ManifestInvalid`、`ManifestContainsNoPackages` | 全部视为不可重试失败收口 |
| `429 RepositoryQueueFull` | 服务端队列已满 | 保留 `repository.transition`，按 `Retry-After` 重入；服务端未接受新执行尝试，因此**不消耗**重试预算 |
| `503 RepositoryStorageUnavailable` | 服务不可用或停机中 | 保留 `repository.transition`，按 `Retry-After` 或指数退避重入；服务端未接受新执行尝试，因此**不消耗**重试预算 |

约定：

- `repositoryUID` 就是幂等键，不额外使用 `Idempotency-Key`；`attempt` 只在服务端接受新的执行尝试时增加，查询与 Ready 重放不增加；
- 重试预算以 Artifact Manager 响应的 `attempt` 为**权威计数**（`attempt` 是"服务端已接受的执行尝试次数"，首次提交为 `1`，相同请求处于可重试 `Failed` 被重放时 `+1`，查询与 Ready 重放不递增）；控制器不写任何计数，判定式为 `attempt >= --rpmrepo-materialize-retry-limit + 1` 即预算耗尽；
- 预算边界：`429` / `503` / 网络超时（服务端未接受新尝试）不递增 `attempt`，因此不消耗预算；`404 RepositoryNotFound`（记录不存在）走"用同一请求重放 `SubmitRepository`"的恢复路径，重放会新建记录并把 `attempt` 重置为 `1`，即计数器从零开始；
- 退避锚点：退避窗口以 `RepositoryResponse.updatedAt`（服务端记录最近一次状态变更时间）为锚点；窗口未过时，任何重复入队（`rpmrepos` 轮询 resync、框架重入）都只返回剩余等待时间（`RequeueAfter`）+ `nil`，**不重放** `SubmitRepository`、不写 status、不消耗预算。未落到记录的错误若响应带 `Retry-After` 则按其等待；无 `Retry-After`（例如连接中断、响应无法解析）时以上一次成功 `GetRepository` 响应的 `updatedAt` 为锚点，仍无可用锚点则按初始退避（`--controller-slow-retry-initial-delay`，默认 30s）等待。该比较跨进程，允许由 jitter 覆盖的时钟偏差；偏差只会让重放略微提前或推迟，不影响预算与终局判定；
- 响应契约：`RepositoryResponse.attempt < 1`（首次提交必为 `1` 起）与 `updatedAt` 缺失均视为 Artifact Manager 响应契约错误，**按可重试处理**（与"响应无法解析"同类，服务端恢复后自动继续），不据此推进预算或收口、不写 status、不消耗重试预算，也不得用本地时间臆造退避锚点：
  - 过程仓路径（`SubmitRepository` / `GetRepository`）：按 `RequeueAfter` 表达（无可用锚点时用初始退避 `--controller-slow-retry-initial-delay`，默认 30s），保留 `repository.transition`，不进入 error 路径；
  - 发布路径（`SubmitRelease` / `GetRelease` / `ActivateRelease`）：返回零值 + 原始错误（框架退避），保留 `release.transition` 与相位；
- 只依赖稳定错误码与 `retryable`，不解析 `message`；
- 提交响应里不会出现 `200 + Failed{retryable=true}`：可重试失败被重放时服务端返回 `202` 且 `attempt` +1；`Failed{retryable=true}` 只通过 `GetRepository` 观察（查询不改变 `state` 与 `attempt`）；
- 结果未知一律按"先 GET 确认、不重放不同请求"处理；
- `Ready` 响应必须包含 `contentURL`；`Creating` 必须包含 `pollAfterSeconds`。响应体可能携带 `rpms`，控制器不解析、不持久化该字段；内容与发布摘要只由 Artifact Manager 服务端持有，不在响应中返回，控制器不做本地摘要比对。
- `GetRepository` 返回 `404 RepositoryNotFound` 表示 Artifact Manager 侧不存在该 `repositoryUID` 记录，控制器按"提交未被接受"用同一请求重放 `SubmitRepository`（与 `artifact-manager.md` §9.3.3 第 2 步"查询或重新提交"一致），不视为不可重试失败。

## 九、条件与时间

条件只允许以下两个 type；condition type 必须区分过程仓错误与正式发布错误（对齐 `docs/zh/design/data-models.md`）：

| type | 含义 | True 时机 | False 时机 |
| --- | --- | --- | --- |
| `RepositoryReady` | **最近一次批次的结果**：`True` = 最近一次批次成功提升；`False/reason=RepositoryFailed` = 最近一次批次失败收口；未设置 = 尚无批次结果（含仅继承基线、从未物化的对象） | 每次成功提升当前版本 | 过程仓失败收口（重试耗尽或不可重试失败） |
| `PublishSucceed` | 正式发布是否成功 | 发布 `Ready` 后写入 | 发布不可重试失败（`409` / `422` / `Failed{retryable=false}`）、过程仓失败收口（重试耗尽 / 不可重试失败）时的终局登记（由 §7.2 失败收口同次写入），"无可用版本"的发布失败终局（§7.2 第 5 步），首个候选自身超限的发布失败终局（§7.2 第 2 步），已放弃批次被外部清空后的重新收口（§7.2 第 1 步），或同名 Build 被中止（reason `RepositoryPublishAborted`，§7.2 入口与 §7.3 中止收口） |

condition merge helper：

```go
func MergeCondition(conditions []metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string, observedGeneration int64) ([]metav1.Condition, bool)
```

- `observedGeneration` 取 `RpmRepo.metadata.generation`；仅有 `status` 变化时更新 `lastTransitionTime`；结果按 `type` 排序；所有字段无变化时跳过 `/status` 写入；
- 「本对象已产出可发布版本」只看 `repository.sourceJobUIDs` 是否非空（成功提升时与 `repositoryUID` / `contentURL` 同一次 CAS 写入），**不看** `RepositoryReady`：由 Build Controller 预置基线（`sourceJobUIDs` 为空但 `repositoryUID` / `contentURL` 非空）的对象在首批收口前没有该 condition，版本可读但不能作为发布依据；
- `repository.repositoryUID` 与 `repository.contentURL` 非空只表示存在可读仓库指针（可能是继承基线）；发布门禁与发布候选一律以 `repository.sourceJobUIDs` 非空为准；
- reason 取值固定：`RepositoryPublished`（发布 `Ready`）/ `RepositoryPublishFailed`（发布不可重试失败、过程仓失败收口时的发布终局登记，以及"无可用版本"的发布失败终局——后两者由 §7.2 同次写入）/ `RepositoryPublishAborted`（同名 Build 被中止，`release.phase=Aborted`）/ `RepositoryFailed`（过程仓失败收口：重试耗尽或不可重试失败）；动态错误详情只进日志；
- 时间来源：每次 `Sync` 只调用一次 `clock.Now()`，写入统一为 `metav1.NewTime(now.UTC())`；`status.repository.updatedAt` 与 `status.release.updatedAt` 仅在对应字段有效写入时更新。

## 十、并发与一致性

- 队列保证同一 key 串行，不同 Build 可并行推进；Artifact Manager 侧不同仓库可并发物化；
- 所有 status 写入携带最新 `resourceVersion`；409 后必须重新 GET 重算，禁止用旧对象重放；
- `repository.transition` 一旦写入即冻结输入、基础仓与 `repositoryUID`；它不承载重试计数（计数在 Artifact Manager 侧），提升内容完全由 `(repository.transition, Ready 响应)` 决定，因此重复提升幂等；
- `repository.sourceJobUIDs` 的合并使用 CAS 写入、去重并按字典序排序；稳定失败的 Job 不落任何持久标记；
- 失败路径不清空已发布版本字段；`baseRepositoryUID` 必须指向已 `Ready` 的版本，禁止引用 `Creating`、`Failed` 或自身；
- 首个版本之前的批次不允许以本对象的空版本作为基础仓；
- 发布串行化：同一 `{project}/{os}/{arch}` 的发布由 `release/` 键保证串行，且同时只允许一个在途发布（在途判据 = `release.transition` 非空，见 7.3「优先处理在途」）；已有在途发布时不启动新发布，即使存在 `creationTimestamp` 更早的候选；不同 Project 或不同 `{os}/{arch}` 可并发（与 Artifact Manager 的 `{project}/{os}/{arch}` 串行键一致）；过程仓键（`build/`）与发布键（`release/`）互不阻塞；
- 发布恢复以检查点为中心：`release.transition`（`sourceRepositoryUID` + 规范化 `excludeSpecs`）即请求的全部可变输入，重启后按 7.3「重启恢复」用同一请求重放；AM 侧以 `buildName` + 服务端计算的请求摘要做幂等键，相同摘要返回原记录、不同摘要按 `409` 拒绝并收口为发布失败；
- 多副本部署不保证正确性：首版只允许单活动实例，故障切换依赖 `resourceVersion` CAS、持久化 `repository.transition` / `release.transition` 与 Artifact Manager 的 `repositoryUID` 幂等约束。

## 十一、可观测性

框架日志行已包含 `controller` / `key` / `result` / `duration` / `error`（部分分支还含 `requeue` / `requeue-after` / `panic`）；本控制器在其之上补充：`reason`、`repositoryUID`、Artifact Manager 响应里的 `attempt`、本次退避时长、Project、Build name、本批输入 Job UID、Artifact Manager 响应状态与稳定错误码。不记录 Artifact Manager 的内部路径与凭据。

日志 reason 取值固定（新增日志不改变任何判定与状态写入）：

| reason | 触发点 |
| --- | --- |
| `BuildInfoNotReady` | 同名 `BuildInfo` NotFound 或 `phase != Completed` |
| `BuildMissing` | 过程仓键遇到孤儿 RpmRepo（同名 Build NotFound） |
| `ReleaseGroupBuildMissing` | 发布流程读取选中对象的同名 Build 时 NotFound，跳过该对象 |
| `InputLabelMismatch` | 输入 Job 的 `ebs.io/spec-name` 缺失或为空，或 `ebs.io/target-os` / `ebs.io/target-arch` 缺失或取值与 `Build.spec.buildTarget` 不一致（`ebs.io/build-name` 已由服务端选择器过滤，不产生该告警） |
| `InputManifestMissing` / `InputManifestNotReady` / `InputManifestFailed` | 输入 manifest 为 `404` / `Open` / `Completing` / `Failed` |
| `InputObjectMissing` | 候选校验期间 `Job` 对象 NotFound |
| `InputTooLarge` | 排序最前的候选自身的物化输入超过 `--rpmrepo-max-input-bytes`，本批不形成并转入发布失败终局 |
| `MultipleReleaseInFlight` | 同一 `{project}/{os}/{arch}` 的候选集内出现多个在途发布 |
| `ReleasePolicySkipped` | 发布策略判定该候选不发布 |
| `BuildAborted` | 过程仓流程或发布流程读到同名 `Build.status.phase=Aborted`，把 `release.phase` 收口为 `Aborted` |
| `RpmRepoLabelMismatch` | 选中对象的 `ebs.io/target-os` / `ebs.io/target-arch` 标签与同名 Build 的 `spec.buildTarget` 不一致，跳过该对象 |

重试与失败收口日志复用条件 reason：`RepositoryFailed`（不可重试失败、重试耗尽、`GetRepository` 返回 `Deleting`）、`RepositoryPublishFailed`（发布不可重试失败、`GetRelease` 返回 `Deleting`、"无可用版本"终局、首个候选自身超限终局）与 `RepositoryPublishAborted`（同名 Build 被中止的中止收口），并附 `attempt` 与本次退避时长。

指标通过 `pkg/metrics.NewCounter` 注册，只保留有独立递增点、用于速率与告警的口径；`pkg/metrics.NewCounter` 不带标签，对象级维度（key / `repositoryUID` / Build 名）只出现在日志中：

```text
rpmrepo_controller_repository_ready_total
rpmrepo_controller_repository_failed_total
rpmrepo_controller_materialize_retries_total
rpmrepo_controller_build_missing_total
rpmrepo_controller_release_ready_total
rpmrepo_controller_release_failed_total
rpmrepo_controller_status_update_conflicts_total
rpmrepo_controller_status_update_unknown_total
```

过程仓：`rpmrepo_controller_repository_ready_total` 在成功收口写入（`RepositoryReady=True`）时计一次；`rpmrepo_controller_repository_failed_total` 在每次**过程仓失败收口**（重试耗尽、不可重试失败，或 `GetRepository` 返回 `Deleting`）时计一次（不随轮次累加），并包含首个候选自身超限而直接写入的发布失败终局（该终局只写发布侧，见 §7.2 第 2 步）；**不含**已放弃批次被外部清空后的重新收口；`rpmrepo_controller_materialize_retries_total` 在每次因可重试失败而重放 `SubmitRepository` 时计一次——其中 `200 Failed{retryable=true}` 的重放与 Artifact Manager 响应里 `attempt` 的递增同步，`429` / `503` / 网络 / 超时 / 未解析这类未落到记录的重放不递增 `attempt`、仍计一次。

输入与归属：`rpmrepo_controller_build_missing_total` 统计过程仓键在 RpmRepo 已存在、但同名 Build NotFound 导致本键收敛（结束本轮、不写 status）的轮次；发布流程因选中对象的同名 Build 缺失（`reason=ReleaseGroupBuildMissing`）或标签与 Build 目标不一致（`reason=RpmRepoLabelMismatch`）而跳过对象只记告警日志、**不计入本计数**（由同一条告警定位，不重复计数），`Build` 读取的其它失败按可重试 / 永久错误分类、不计入本计数。输入侧被跳过的 Job（manifest 为 `Failed` / 404 / `Open` / `Completing`、候选校验期间 `Job` 对象 NotFound，或 `ebs.io/spec-name` 缺失/为空、`ebs.io/target-os` / `ebs.io/target-arch` 缺失或取值不一致——`ebs.io/build-name` 已由服务端选择器过滤，不属该告警集）一律不写 status、不加计数，只按上表输出 `reason` 告警日志；其中 `Open` / `Completing` 属未就绪输入、阻塞发布触发；首个候选自身超限不属该跳过路径，按 §7.2 第 2 步直接收口为发布失败。

发布：`rpmrepo_controller_release_ready_total` 在发布成功收口写入（`release.phase=Ready`）时计一次；`rpmrepo_controller_release_failed_total` 统计发布失败终局——发布流程自身的失败收口（含 `GetRelease` 返回 `Deleting`），以及"无可用版本"（`repository.sourceJobUIDs` 为空，本对象从未产出任何版本，含仅预置了继承基线的对象）时由 §7.2 第 5 步直接写入的发布失败终局，**不含**过程仓失败收口同次写入的发布失败终局登记（那一次计入 `rpmrepo_controller_repository_failed_total`），**也不含**首个候选自身超限的发布失败终局（同样计入 `rpmrepo_controller_repository_failed_total`），**更不含**同名 Build 被中止的 `Aborted` 中止终局（它是用户中止而非发布失败，只由相位与 `reason=BuildAborted` 日志体现）。

状态写入：`rpmrepo_controller_status_update_conflicts_total` 统计 `/status` 写入因乐观并发被拒（409，需重新 GET 重算）的轮次；`rpmrepo_controller_status_update_unknown_total` 统计写入结果未知（超时 / 连接中断 / 响应无法解析，需下一轮 GET 比对确认）的轮次；两者沿用 job / runner / build 控制器体例。

`BuildInfo` 未就绪只输出 `reason=BuildInfoNotReady` 告警日志、不新增计数；依赖读取失败的分类不计入任何计数、**仅通过框架日志暴露**（见第七章开篇「依赖读取的统一语义」）；持续出现需人工排查控制器权限 / apiserver 配置。在途批次或发布检查点长期未收口时只通过日志与 Artifact Manager 侧指标反映，不写额外收敛状态；输入 Job 长期停留在 manifest `Open` / `Completing` 会持续阻塞该键的发布触发（见第六章「入选条件」），同样只通过每轮 `reason=InputManifestNotReady` 日志反映、需人工介入。

可观测性扩展（可选）：若需要"批次等待时长直方图"，需扩展 `pkg/metrics`（当前只提供 Counter）。

## 十二、权限与身份

所需最小权限：

```text
rpmrepos:        get, list
rpmrepos/status: update
jobs:            get, list
builds:          get
buildinfos:      get
```

- 不能 create / update / delete Job，不能写 Build / BuildInfo / Job status，不能 create / delete RpmRepo（RpmRepo 由 Build Controller ensure，见第四章）；
- Artifact Manager 内部接口当前不校验 Token，只允许部署在受信任网络；控制器不得把该地址暴露给其他组件或用户；
- 本轮不配置部署侧 mTLS 身份与 apiserver 授权，联调与投产前再补。

## 十三、前置条件与同步清单

以下条目是 RpmRepo Controller 可完整实现的前提，本轮只登记，不在本设计范围内修改其它文档或代码：

1. API 状态校验：apiserver 的 `ValidateRpmRepoStatusUpdate` 目前仍是空实现，需按下述清单补 status 校验（本设计控制器侧不新增自检）：
   - `release.phase` 必须在 `Pending` / `Creating` / `Prepared` / `Ready` / `Failed` / `Aborted` 枚举内；过程仓**没有**相位字段（`repository.phase` 已删除）；
   - `release.transition` 只允许出现在 `release.phase` 为 `Pending` / `Creating` / `Prepared` 时，`Ready` / `Failed` / `Aborted` 时必须为空；
   - `conditions` 只允许 `RepositoryReady` / `PublishSucceed` 两个 type，`status` ∈ `True` / `False`，`reason` ∈ `RepositoryPublished` / `RepositoryPublishFailed` / `RepositoryPublishAborted` / `RepositoryFailed`；
   - `release.phase=Ready` 时 `release.contentURL` 必须非空；
   - `repository.repositoryUID` 与 `repository.contentURL` 必须成对出现（要么都为空、要么都非空）；`repository.transition` 非空时其 `inputs` / `repositoryUID` 必须非空（成对约束已在创建路径由 apiserver 创建策略实现，本条针对 `/status` 更新路径）；
   - `repository.sourceJobUIDs` 非空时，`repository.repositoryUID` 与 `repository.contentURL` 必须都非空（保证「本对象已产出可发布版本」与可读仓库指针一致；由 apiserver 校验阻断，控制器不再重复校验）；
   - 校验不得拒绝创建时预置的基础仓（`sourceJobUIDs` 为空但 `repositoryUID` / `contentURL` 非空），也不得拒绝失败收口后的形态（`transition` 保留 + `RepositoryReady=False`）；
   - `/status` 更新不得修改 `spec` 与 `metadata`（由 status 子资源保证，需在实现中确认）；
2. BuildInfo Controller 契约：
   - Job 业务 labels：`ebs.io/build-name`、`ebs.io/spec-name`、`ebs.io/target-os`、`ebs.io/target-arch` 必须由 BuildInfo Controller 在创建 Job 时写入；缺失任一 label 的 Job 不进入物化队列（`ebs.io/build-name` 缺失的 Job 不会被任何列表返回、本控制器不感知；其余三个缺失或取值不符由候选校验输出 `reason=InputLabelMismatch`）；
   - `BuildInfo.status.phase=Completed` 必须表示不再新增候选 Job（Job 集合自此固定）——7.2 的发布触发与失败收口的发布终局登记都依赖该前提；BuildInfo Controller 设计文档不在本仓库，落地前需与上游确认并在该文档写明。
3. 重试预算契约与 `transition` 失败保留语义：本设计**不新增任何字段**，重试预算以 Artifact Manager 响应的 `attempt` 为权威计数（见 §8），因此需要 Artifact Manager 保证"`attempt` 仅在服务端接受新的执行尝试时递增、查询与 Ready 重放不递增"；同时把 `repository.transition` 的语义扩展为「在途批次**或**已放弃的失败批次」，需同步 `docs/zh/design/data-models.md` 的 `transition` 行（补「失败收口时原样保留」）。`attempt` 语义的权威定义在 Artifact Manager 侧，联调前需与其文档核对。
4. 入口依赖同名 Build 与 RpmRepo 目标标签：过程仓键需要 `Build.spec.buildTarget.os` / `arch`（发布键入队与 `(os, arch)` 串行键，与 Artifact Manager §9.13.1 的 `{project}/{os}/{arch}` 一致）；发布候选过滤还要求 RpmRepo 携带 `ebs.io/target-os` / `ebs.io/target-arch` 标签（Build Controller 创建时写入，apiserver 创建校验强制存在、缺失或为空返回 422）。**标签能力上线前创建的存量 RpmRepo 没有这两个标签，因此不会进入发布候选**，处置方式为删除后由 Build Controller 重建、或运维一次性补标签——本设计不做兜底查询、不补写 metadata。此外，删除 Build 前应先处理同名 RpmRepo，否则该对象成为孤儿、由 `rpmrepo_controller_build_missing_total` 与 `reason=BuildMissing` 日志暴露；本控制器不级联删除、不补建 Build，删除顺序需写入运维流程。
5. 待对齐欠账（只登记，本轮不改其它文档）：
   - `docs/zh/design/artifact-manager.md` §9.3.3 仍要求 Job 携带 `status.artifactState=Completed`、JobStatus 新增 `artifactState` / `artifactCount` / `repositoryState` / `repositoryUID`、提升时写"摘要、RPM 元数据"，并把结果**写回 `Job.status`** 且逐项幂等；本设计与 `api/ebs/v1` 相反：manifest 是唯一事实来源、控制器不写 `Job.status`、`RpmRepo.status` 无 digest / packageCount。需按本设计同步 AM 文档（参照 build-controller 文档的同类欠账条目）；
   - `docs/zh/design/data-models.md` §五需对齐两处：`repository.updatedAt` 统一为"过程仓 status 最近一次有效写入时间（提交批次 / 失败收口 / 提升版本）"，`transition` 补"失败收口时原样保留（记录已放弃的失败批次）"；同时清理 `release` 行中"不与过程仓 phase 混用"的过期措辞（过程仓相位已删除）。
   - `api/ebs/v1` 的 `RpmRepoReleasePhase` 需新增 `Aborted`（常量 + `RpmRepoReleasePhaseValues()` + `IsValid()`），并同步 `types_status_test.go` 的枚举用例与第 1 条的 apiserver 状态校验；在补上之前，`release.phase=Aborted` 会通过现有空实现校验，但无法由类型化客户端用常量表达。
   - `docs/zh/design/artifact-manager.md` §9.5 的 `RpmRepoReleasePhase` 枚举描述与 `docs/zh/design/controller-manager~build-controller.md` §4.3 的 `release.phase` 消费行需同步加入 `Aborted`：Build 被中止时其自身已是终态 `Aborted`、Build Controller 不会消费该相位；若未来出现非终态 Build 读到 `release.phase=Aborted`，按发布终止处理（不得当作等待）。

## 十四、测试计划

### 14.1 单元测试

**纯函数与客户端替身**

- 幂等键：`repositoryUID` 对相同输入稳定、对基础仓或批次成员变化敏感。
- 物化请求构造：断言 `SubmitRepository` 请求满足 `repositoryName == buildName`、`targetOS` / `targetArch` 等于 `Build.spec.buildTarget`、`baseRepositoryUID` 等于批次冻结值、`manifests` 按 `jobUID` 升序且与 `transition.inputs` 的 `jobName` / `jobUID` 一一对应；重放时逐字段（含 `jobName`）与首次请求一致。
- 时间、退避与条件纯函数：每次 `Sync` 只取一次 `clock.Now()`；退避间隔按 Artifact Manager 响应的 `attempt` 计算（2 倍递增、上限 `--controller-slow-retry-max-delay`、含 jitter），并判定"退避窗口是否已过"（锚点为响应 `updatedAt`）；`MergeCondition` 首次插入与 status 变化使用传入 `now`、同值且 `now` 变化仍返回 `changed=false`、仅 reason / message / observedGeneration 变化返回 `changed=true` 但保留原转换时间、`observedGeneration` 取 `metadata.generation`；无变化时跳过 `/status` 写入。
- Fake：两个 Fake 的注入能力、调用次数、最后一次请求体与重置；`fake.ArtifactManagerClient` 可预设 `SubmitRepository` / `GetRepository` 的返回（含 `attempt`、`updatedAt`、`pollAfterSeconds` 与 `404`），并断言 `GetRepository` 不改变 `attempt`。

**过程仓：批次选择与提交**

- 候选扫描查询：断言 `ListJobs` 的 labelSelector 恰为 `ebs.io/build-name=<RpmRepo 名>` 且 FieldSelector 为空，且选择器取自 RpmRepo/Build 名而非 RpmRepo labels 或其它字段。
- 批次选择：`ebs.io/spec-name` 缺失或为空、`ebs.io/target-os` / `ebs.io/target-arch` 缺失或取值与 `Build.spec.buildTarget` 不一致、UID 已在 `repository.sourceJobUIDs`、Manifest 非 `Completed`、Manifest 404、Manifest `Failed` 的过滤；稳定排序；同 `specName` 去重；两个上限参数；Manifest `Failed` 场景断言不写任何 status 字段、输出 `reason=InputManifestFailed` 告警且不计数；上述 label 校验失败断言不入候选、不入队、输出 `reason=InputLabelMismatch` 告警且不阻塞发布触发（`ebs.io/build-name` 已由服务端选择器保证，不在本用例断言）；Fake 预置 phase 为 `Pending` / `Running` / `Failed` 的 Job 时断言它们被列出但不进入候选、不调用 `GetJobManifest`（规则 1 先行拦截）。
- manifest 404 与未就绪输入：404 断言既不算候选、也不阻塞发布触发（不写 status、输出 `reason=InputManifestMissing` 告警、不计数），且该 Job 相位不是 `Succeeded` 时返回可重试错误；`Open` / `Completing` 断言不算候选、阻塞发布触发（不计入「无候选」）、输出 `reason=InputManifestNotReady` 告警且不计数。
- 提交批次：`repository.transition` 为空且存在可入选 Job 时，断言一次 CAS 写 `repository.transition`（`inputs` / `baseRepositoryUID` / `repositoryUID`）与 `repository.updatedAt` 之后才调用 `SubmitRepository`；无候选 Job 时断言不写 status；两者都不产生只写中间状态的额外写入。
- 首个候选超限：单候选自身输入为上限 1.5 倍时，断言不写 `repository.transition`、不调用 Artifact Manager、一次 CAS 写 `release.phase=Failed` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False`（reason `RepositoryPublishFailed`）、计一次 `rpmrepo_controller_repository_failed_total`（且 `release_failed_total` 不增）、输出一次 `reason=InputTooLarge`、返回零值 + `nil`，且对象此后被轮询与发布候选排除。
- 字节上限累加：上限 X、候选 A=0.6X 且 B=0.6X 时断言首批为 `[A]`、B 留待下一批；A=0.6X、B=0.3X、C=0.3X 时断言首批为 `[A,B]`、C 留待下一批；B 留待后在下一批成为首个候选且自身超限时断言走同一发布失败终局（不截断、不跳过）。
- 计数口径：manifest 同时包含 RPM 与日志时，断言日志不参与累计（口径与 Artifact Manager 的物化输入一致）。
- 零输入与未列 422：物化输入为 0 的候选断言正常参与批次（只占 Job 数上限）；Artifact Manager 返回 `422 ManifestContainsNoPackages` / `ManifestInvalid` 时断言按不可重试失败收口（`RepositoryReady=False` + `release.phase=Failed` + `PublishSucceed=False`，`repository.transition` 原样保留）。

**过程仓：在途结果、重试预算与收口**

- `repository.transition` 路径分流：非空时断言不重新选批、不重算 `repositoryUID`，只先调用 `GetRepository`。
- `GetRepository` 响应分流：`200 Ready` 提升；`200 Creating` 断言 `RequeueAfter` 等于 `pollAfterSeconds`（缺失或 ≤ 0 时缺省 5s）且不写 status；`200 Failed{retryable=true}` 用完全相同的请求重放；`200 Failed{retryable=false}` / `200 Deleting` / `409 RepositoryIdentityConflict` / `409 RepositoryDeleting` / `410 MaterializationInputExpired` / `422 ManifestNotReady`|`BaseRepositoryNotReady` 失败收口（写入内容见「收口写入与保留」条，`Deleting` 与 `BaseRepositoryNotReady` 额外断言输出 `reason=RepositoryFailed` 告警）；`404` 用完全相同的请求重放 `SubmitRepository`（断言不写失败条件、不重新选批、不重算 `repositoryUID`）；响应未知先 GET 比对再决定。
- 提交响应契约：断言 `SubmitRepository` **不返回** `200 + Failed{retryable=true}`——可重试失败被重放时返回 `202` 且 `attempt` +1；`Failed{retryable=true}` 只能通过 `GetRepository` 观察。
- 重试预算与退避：可重试失败断言**重试期间零 status 写入**、计一次 `rpmrepo_controller_materialize_retries_total`，返回 `ReconcileResult{RequeueAfter: <退避>}` + `nil`，且不与框架退避叠加；`200 Failed{retryable=true}` 按响应 `attempt` 判定——`attempt < --rpmrepo-materialize-retry-limit + 1` 时重放、`attempt >= limit + 1` 时直接失败收口；`429` / `503` / 网络 / 超时 / 未解析断言**不消耗预算**（`attempt` 不变）且按 `Retry-After`（有则优先）/ 退避重放；**退避窗口**以响应 `updatedAt` 为锚点——同一窗口内连续多轮轮询 resync 断言 `attempt` 不变、零 status 写入、不调用 `SubmitRepository`、只返回剩余等待；无 `Retry-After` 且无可用锚点时按初始退避（`--controller-slow-retry-initial-delay`，30s）等待；响应 `attempt < 1` 或 `updatedAt` 缺失断言按响应契约错误（**可重试**）处理：过程仓路径返回 `ReconcileResult{RequeueAfter: <初始退避或剩余等待>}` + `nil`、不写 status、不推进预算也不收口、不进入 error 路径，发布路径返回零值 + 原始错误（框架退避）并保留检查点与相位；不可重试失败断言不重试、不消耗预算、直接失败收口；重启后预算按 Artifact Manager 记录的 `attempt` 继续且仍需遵守剩余退避窗口。
- 字段级 `422` 收敛：`SubmitRepository` 返回 `422 InvalidRepositoryRequest` / `RepositoryUIDMismatch` / `InvalidManifestReference` 时断言按不可重试失败收口（`repository.transition` 原样保留、`RepositoryReady=False`、`release.phase=Failed`、`PublishSucceed=False`、计一次 `repository_failed_total`），不再返回 `controller.NewPermanentError`。
- 已放弃批次恢复：预置 `repository.transition` 非空 + `RepositoryReady=False/reason=RepositoryFailed` + `release` 为空，断言一次 CAS 重写发布终局（`release.phase=Failed` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False/reason=RepositoryPublishFailed`）、不写 `repository.*`、不调用 Artifact Manager、`repository_failed_total` 不增、输出 `reason=RepositoryFailed` 告警、返回零值 + `nil`，且对象此后被轮询与发布候选排除。
- 收口写入与保留：成功收口同一次 `/status` 写入包含 `repository.contentURL` 与 `repository.repositoryUID`（分别取 Ready 响应与本批 `transition.repositoryUID`）、`repository.sourceJobUIDs`、`repository.transition=nil`、`repository.updatedAt` 与 `RepositoryReady=True`；失败收口同一次写入包含 `repository.transition` **原样保留**（`inputs` / `baseRepositoryUID` / `repositoryUID` 与失败时刻一致）、`repository.updatedAt`、`RepositoryReady=False/reason=RepositoryFailed`，以及发布终局 `release.phase=Failed`、`release.transition=nil`、`release.updatedAt` 与 `PublishSucceed=False/reason=RepositoryPublishFailed`（断言全程不调用 `SubmitRelease` / `ActivateRelease`、不额外入队发布键），并计一次 `rpmrepo_controller_repository_failed_total`；两条路径都不得清空 `repository.contentURL` / `repositoryUID` 等版本字段。
- `repository.updatedAt` 更新时机：断言只在提交批次、失败收口与提升版本时更新，重试期间不更新（与"重试零 status 写入"一致）。

**入口与依赖读取**

- 入口守卫：`RpmRepo` 缺失时断言返回可重试错误、不调用 Artifact Manager、不写 status；同名 RpmRepo 已存在时断言按普通对象继续推进。`Build` 不可用时——NotFound 且 RpmRepo 不存在断言返回零值 + `nil`、不写 status、不调用 Artifact Manager、不入队发布键、不删除任何对象且 `rpmrepo_controller_build_missing_total` 不增；NotFound 且 RpmRepo 存在断言返回零值 + `nil`、输出 `reason=BuildMissing` 告警日志、该计数每次 +1、`status`（含 `repository.*` 与 `release.*`）不变、不调用 Artifact Manager、不入队发布键、不删除孤儿对象，并在下一轮轮询重复同一收敛。
- 依赖读取的统一语义（通用分类只在此断言）：`BuildInfo` NotFound（过程仓键与发布候选两处）断言零值 + `nil`、不写 status、不调用 Artifact Manager、不入队发布键、不产生终局或条件、输出 `reason=BuildInfoNotReady` 日志，且 `rpmrepo_controller_build_missing_total` 不增；候选校验期间 `Job` 对象 NotFound 断言跳过该输入、不计候选、不阻塞发布触发、不写 status、输出 `reason=InputObjectMissing` 日志且不计数；`Build` / `BuildInfo` / `Job` 读取返回 `5xx` / 超时 / 无重试提示的 `429`、`503` 断言可重试（零值 + 原始错误）、不写 status、不推进，带 `apierrors.SuggestsClientDelay` 提示的 `429` / `503` 断言返回 `ReconcileResult{RequeueAfter: <提示值>}` + `nil`、不写 status、不推进；返回 `401` / `403` / `422` 或响应身份契约错误断言 `controller.NewPermanentError`；对照断言 Artifact Manager 的 `GetRepository` / `GetRelease` 返回 `404` 仍按各自状态机的"同请求重放"规则，不受本章统一读取语义影响。

**写错误分类（apiserver 侧）**

- status 写入 409 断言重新 GET 重算后重做本步、不用旧对象重放；status 写入返回字段非法（422）断言返回 `controller.NewPermanentError` 且不写 status；status 写入返回 `429` / `503` 断言返回可重试错误并保留 `Retry-After`（由框架的 `WriteError` 路径识别；**与带 `Retry-After` 的依赖读取、Artifact Manager `429` / `503` 不同**：后者由控制器换算为 `RequeueAfter` 且不消耗重试预算，见「重试预算与退避」条）。

**发布**

- 发布触发入队：**同名 Build 未中止时**，`status.release` 非空则入口即入队发布键并返回零值 + `nil`（不推进过程仓字段、不调用 Artifact Manager、不写 status）；`release` 为空且 `BuildInfo=Completed`、`repository.transition==nil`、本轮候选扫描为空、无未就绪输入、`repository.sourceJobUIDs` 非空（本对象已产出可发布版本）时才走首次入队；`sourceJobUIDs` 为空时即使 `repositoryUID` / `contentURL` 被预置了继承基线也不入队；候选非空、`repository.transition` 非空或存在未就绪输入时不入队（等待）；Build 已 `Aborted` 时两条分支都不走，改为直接写 `release.phase=Aborted` 中止终局（见「Build 被中止」断言）；
- 无可用版本的发布失败终局：`release` 为空且 `BuildInfo=Completed`、`repository.transition==nil`、候选扫描为空、无未就绪输入，但 `repository.sourceJobUIDs` 为空时（覆盖无基线与仅有继承基线两种形态），断言**只发生一次** CAS 写入 `release.phase=Failed` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False/reason=RepositoryPublishFailed`，计一次 `rpmrepo_controller_release_failed_total`；断言不写 `repository.*`、不调用 Artifact Manager（含不调用 `GetRepository` / `SubmitRepository` / `SubmitRelease`）、不入队发布键，返回零值 + `nil`，且该对象之后被轮询过滤与发布候选同时排除；
- 发布候选查询与 Build 校验：断言 `ListRpmRepos` 透传的 `FieldSelector` 恰为 `status.release.phase!=Ready,status.release.phase!=Failed,status.release.phase!=Aborted`、`LabelSelector` 恰为 `ebs.io/target-os=<os>,ebs.io/target-arch=<arch>`（取自发布键，同 arch 不同 os 的对象互不阻塞、也不会出现在本键候选集中）；断言每轮**至多一次** Build GET 且只对本次对象执行，不做逐对象读取；选中对象的同名 Build `NotFound` 时断言跳过该对象、输出 `reason=ReleaseGroupBuildMissing` 告警且不重复计数（同名 Build 缺失由过程仓键的 `rpmrepo_controller_build_missing_total` 暴露）、其它对象照常推进；选中对象的标签与 `Build.spec.buildTarget` 不一致时断言跳过该对象、输出 `reason=RpmRepoLabelMismatch` 告警、不计数、不调用 Artifact Manager；Build 读取返回 `5xx` / 超时断言返回可重试错误且本轮不推进；构造一个不带目标标签的 RpmRepo（存量形态）断言它不进候选、不产生发布动作；
- 发布策略：注入 stub 策略断言 `Publish=false` 的候选不调发布接口、不写 condition、输出 `reason=ReleasePolicySkipped` 告警，仅在存在残留 `release` 时写一次 `release=nil`，并继续评估下一候选（全部不发布 → 零值 + `nil`）；`DefaultPublishPolicy` 在 `Build.spec.buildTarget.publishFlag=false` 时返回 `Publish=false`，其余返回 `Publish=true`（不特判 `buildType`）；`ExcludeSpecs` 去重并按字典序排序；策略返回 error 时返回可重试错误且不提交发布；
- 发布候选过滤：`repository.transition` 非空、`repository.sourceJobUIDs` 为空（无论 `repositoryUID` / `contentURL` 是否被继承基线预置）、`release.phase ∈ {Ready, Failed, Aborted}`、`metadata.deletionTimestamp` 非空（非在途对象）的对象不进入候选、不调用 `SubmitRelease`；
- 发布状态机（无检查点）：`SubmitRelease` 的 `202 Creating` / `202 Prepared` / `200 Ready` / `409` / `422` / `429` / `503` 分支；断言一次 CAS 写检查点（`release.phase=Pending`、`release.transition.sourceRepositoryUID` / `excludeSpecs`、`release.updatedAt`）后才调用 `SubmitRelease`；`202` 后按响应体写 `release.phase=Creating` / `Prepared`；可重试错误保留检查点并把相位留在 `Pending`；
- 发布状态机（有检查点）：`GetRelease` 的 `Creating`（补齐相位并延迟重入）/ `Prepared`（补齐相位后 `ActivateRelease`）/ `Ready`（直接收口）/ `Failed{retryable=true}`（重放同一请求）/ `Failed{retryable=false}`（失败收口）/ `Deleting`（失败收口并输出 `reason=RepositoryPublishFailed` 告警）/ `404`（重放同一请求）七条分支；断言有检查点时不再重新调用策略、不重算 `sourceRepositoryUID` / `excludeSpecs`；
- Retry-After 换算：Artifact Manager 发布提交 / 有检查点查询 / 激活返回带 `Retry-After` 的 `429` / `503` 时，断言保留检查点与相位、返回 `ReconcileResult{RequeueAfter: <Retry-After>}` + `nil`、不消耗重试预算；无 `Retry-After` 时断言返回原始可重试错误；
- 发布收口写入：成功收口同一次 `/status` 写入包含 `release.phase=Ready`、`sourceRepositoryUID` / `contentURL` / `updatedAt`、`release.transition=nil` 与 `PublishSucceed=True/reason=RepositoryPublished`，并返回 `ReconcileResult{Requeue: true}` + `nil`；失败收口同一次写入包含 `release.phase=Failed`、`release.updatedAt`、`release.transition=nil` 与 `PublishSucceed=False/reason=RepositoryPublishFailed`（失败详情不落 status），断言写完后返回**零值 + 原始错误**、不返回 `Requeue`；
- 等待类返回形态：发布候选的 `BuildInfo` 可读但 `phase != Completed`、以及候选 `metadata.deletionTimestamp` 非空（含在途对象）时，断言不写 status、不调用发布接口、返回零值 + `nil`（由过程仓键 resync 重新入队发布键）；
- 基础仓不匹配：Artifact Manager 返回 `422 BaseRepositoryNotReady`（基础仓不存在、非 `Ready`，或 Project / OS / 架构与请求不一致）时断言按不可重试失败收口（`RepositoryReady=False` + `release.phase=Failed` + `PublishSucceed=False`、`repository.transition` 原样保留）、输出 `reason=RepositoryFailed` 告警，且本控制器不自行做该一致性校验；
- Build 被中止：同名 `Build.status.phase=Aborted` 时，分两组断言——① 过程仓键：断言不写 `repository.transition`、不调用 Artifact Manager、不入队发布键，只发生一次 CAS 写 `release.phase=Aborted` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False/reason=RepositoryPublishAborted`，输出 `reason=BuildAborted` 日志，返回零值 + `nil`；② 发布键：在途对象（`release.transition` 非空）与发布候选对象都断言同一次 CAS 收口、不调用 `GetRelease` / `SubmitRelease` / `ActivateRelease`、不做策略判定（即中止判定先于在途恢复：在途对象不得进入「有检查点」流程）；两组都断言 `repository.*` 不被改动（在途 `repository.transition` 原样保留）、`rpmrepo_controller_release_failed_total` 不增、返回零值 + `nil`；早中止（`BuildInfo` 未完成、`repository.sourceJobUIDs` 为空）的对象由过程仓键的同一分支收口，不依赖发布键的候选扫描；
- 发布幂等：按冻结的 `release.transition` 重放同一请求不产生第二个 release 提交（断言 Fake 的调用次数与最后请求体一致）；`release.transition` 被改动导致重放不同请求时断言按 AM 的 `409` 失败收口（`release.phase=Failed` + `PublishSucceed=False`）。

**条件、时间与观测**

- 条件与时间：`RepositoryReady` / `PublishSucceed` 的 reason 固定短语；`repository.updatedAt` 与 `release.updatedAt` 仅在对应字段有效写入时更新，且重试期间不更新 `repository.updatedAt`（`observedGeneration`、FakeClock / UTC 与 `MergeCondition` 的断言见「时间、退避与条件纯函数」条）。
- 指标：断言 §11 列出的指标均已注册；过程仓失败收口（含首个候选自身超限的发布失败终局）、每次重放、发布失败终局（发布不可重试失败与"无可用版本"终局）分别使 `rpmrepo_controller_repository_failed_total` / `rpmrepo_controller_materialize_retries_total` / `rpmrepo_controller_release_failed_total` 各递增一次。

### 14.2 集成测试

- 首版生成：无基础仓时由多个 Completed Manifest 生成自身的首个可读版本——同一轮 CAS 写 `repository.repositoryUID` / `repository.contentURL`、把本批 Job UID 并入 `repository.sourceJobUIDs`、`repository.transition=nil` 与 `RepositoryReady=True`；发布收口同一次写入 `release.phase=Ready` 与 `PublishSucceed=True`；
- 多批推进：提升成功后仍有候选 Job 时立即重入，并以前一版本为基础仓；
- 幂等：相同 UID 相同请求并发提交只产生一个版本；相同 UID 不同摘要按 `409` 收口；
- 失败与保留：失败收口后旧版本仍可读（`repository.contentURL` 与 `repository.repositoryUID` 不变，`repository.transition` 原样保留为失败批次）；
- 账本：`repository.sourceJobUIDs` 去重排序、只增不减；
- 过程仓重启（逐崩溃窗口）：① 批次选定后未写 `repository.transition` → 重启后按确定性排序重选同一批、`repositoryUID` 一致；② `repository.transition` 写入结果未知 → 重启后先 GET 比对：已写入（`repositoryUID` / `inputs` 一致）走第 3 步「结果分流」（查询物化结果），未写入重做本步；③ `repository.transition` 已写、`SubmitRepository` 未被接受 → `GetRepository` 返回 `404` 时用同一请求重放，不得失败收口、不得丢批次；④ 提交已受理、物化中 → `GetRepository` 返回 `Creating` 时等待，不重复提交；⑤ `Ready` 后、提升 CAS 前 → 重启后提升，`repositoryUID` 与崩溃前一致；⑥ 提升 CAS 结果未知 → 已写入则本批 UID 已在 `repository.sourceJobUIDs`（不会被重新选批），未写入则重放同一提升；⑦ 提升后、发布键入队前 → 过程仓 resync 重新入队发布键；⑧ 重试期间重启 → 预算与退避都不丢：重启后仍遵守剩余退避窗口（锚点为 Artifact Manager 记录的 `updatedAt`），窗口过后由响应的 `attempt` 决定是否继续重试；重启不重置计数（除非该记录不存在、重放新建了记录）；⑨ 失败收口写入结果未知 → 先 GET 比对：`repository.transition` 仍为失败批次、`RepositoryReady=False` 且 `release.phase=Failed` 则本轮结束，未达成则重做收口；全部窗口均不产生分叉版本，也不丢批次、不重复消耗重试预算。
- 重试耗尽端到端：可重试失败（`200 Failed{retryable=true}`）逐轮按响应 `attempt` 退避重放、过程仓**零 status 写入**；`attempt >= --rpmrepo-materialize-retry-limit + 1` 时一次 CAS 失败收口（`repository.transition` 原样保留 + `RepositoryReady=False` + `release.phase=Failed` + `PublishSucceed=False`），Build 收敛 `Failed/publish`；断言重启后 `attempt` 不重置（除非记录不存在、重放新建）；
- 不可重试失败端到端：`409 RepositoryIdentityConflict` / `409 RepositoryDeleting` / `410 MaterializationInputExpired` / `422 ManifestNotReady`|`BaseRepositoryNotReady` 断言**不重放**、`attempt` 不变、同一轮完成失败收口，Build 收敛 `Failed/publish`；已存在旧版本时 `repository.contentURL` / `repositoryUID` 保持不变；
- 发布：`BuildInfo` 完成、无候选（无 `repository.transition`、扫描为空）、本对象已产出可发布版本（`repository.sourceJobUIDs` 非空）且无未就绪输入（末批 manifest 已终态）后入队发布键，先写检查点再提交，`release.phase=Ready` 写 `PublishSucceed=True`、不可重试失败写 `release.phase=Failed` 与 `PublishSucceed=False` 并清空 `release.transition`；
- 发布门禁与解锁：存在未就绪输入（manifest `Open` / `Completing`）时断言**不入队发布键**、不写 status；该输入转为终态后由下一轮 `rpmrepos` resync 重新入队发布键并完成发布；
- 发布侧不可重试失败端到端：`409 ReleaseIdentityConflict` / `422` / `Failed{retryable=false}` 断言一次 CAS 写 `release.phase=Failed`、`PublishSucceed=False` 并清空 `release.transition`，`repository.*` 不变，Build 收敛 `Failed/publish`；
- 无产出场景（批次失败）：重试耗尽或遭遇不可重试失败、从未产出本对象版本（`repository.sourceJobUIDs` 为空）时，过程仓失败收口的同一次 CAS 写 `release.phase=Failed` 与 `PublishSucceed=False`，`repository.transition` 原样保留，不调用发布接口、不产生 release 记录；
- 无产出场景（无可发布内容）：从未组成过可物化批次（所有 Job 的 manifest 为 404 / `Failed`，或该 Build 无 Job）且 `BuildInfo=Completed` 时，断言由 §7.2 第 5 步直接写 `release.phase=Failed` 与 `PublishSucceed=False`（清 `release.transition`），不写 `repository.*`、不调用 Artifact Manager、不产生过程仓记录；对象收敛后不再被轮询或发布候选驱动；该用例同时覆盖两种形态——无基线的对象（`repositoryUID` / `contentURL` 为空）与仅预置了继承基线的对象（指针非空但 `sourceJobUIDs` 为空），两者收口一致；
- 继承基线与首批提升：RpmRepo 创建时预置了继承基线（`repositoryUID` / `contentURL` 非空、`sourceJobUIDs` 为空），首批候选 Job 成功提升后断言 `repository.repositoryUID` 被替换为本批 `transition.repositoryUID`（与基线 UID 不同）、`sourceJobUIDs` 含本批全部 Job UID，且随后 `SubmitRelease` 的 `sourceRepositoryUID` 为本批 UID 而非基线 UID；
- 继承基线不参与发布门禁：仅有继承基线（`sourceJobUIDs` 为空）的对象断言不被选为发布候选、不入队发布键、不调用 `SubmitRelease`，最终按无产出场景收口为发布失败终局；
- 发布串行与在途优先：同一 `{project}/{os}/{arch}` 的两个 Build，先进入在途（`release.transition` 非空）者收口前后一个不提交（即使后一个的 `creationTimestamp` 更早），在途者收口后后者才开始；在途优先不受 `sourceJobUIDs` 候选谓词影响；同 arch 但不同 os 的两个 Build 互不阻塞、各自按序发布；在途对象 `metadata.deletionTimestamp` 非空时本轮不驱动任何发布（零值 + `nil`）；
- 目标标签隔离：同一 Project 下两个不同 `(os, arch)` 的 Build 各自只看到本目标的 RpmRepo（断言两次 `ListRpmRepos` 的 `LabelSelector` 分别为各自目标、返回集合不含对方对象），交叉目标对象既不阻塞发布、也不会被误提交；人工把某个 RpmRepo 的标签改成另一个目标后，断言它不再出现在原目标的候选集中，且新目标键在读取其同名 Build 时以 `reason=RpmRepoLabelMismatch` 跳过（不调用 Artifact Manager）；
- 发布策略不发布：同一 `{project}/{os}/{arch}` 中更早创建的 Build 被策略判定为不发布时，输出 `reason=ReleasePolicySkipped` 日志且不阻塞后续候选——本轮继续提交后面的 Build；
- Build 中止端到端：同一 `{project}/{os}/{arch}` 的对象在发布在途或待发布时对应 Build 被中止（`POST …/builds/{name}/abort`）——断言该对象一次 CAS 收敛为 `release.phase=Aborted` 且 `release.transition` 清空、`PublishSucceed=False/reason=RepositoryPublishAborted`，`repository.*` 不变、日志含 `reason=BuildAborted`；收敛后该对象不再出现在轮询与发布候选中，同目标其它候选照常发布，且 Artifact Manager 侧不再收到该对象的任何提交或激活调用；
- 孤儿 RpmRepo：删除同名 Build 但保留 RpmRepo 时，断言控制器不再推进该对象（`status` 不变、不调用 Artifact Manager、不入队发布键）、输出 `reason=BuildMissing` 告警并递增 `rpmrepo_controller_build_missing_total`；删除该 Build 的同时 RpmRepo 也已不存在时断言不计数、不产生任何动作；
- Artifact Manager 侧记录被删除：在途 `GetRepository` 返回 `200 Deleting` 时断言一次 CAS 失败收口（`RepositoryReady=False` + `release.phase=Failed` + `PublishSucceed=False`）并输出 `reason=RepositoryFailed` 告警；`GetRelease` 返回 `Deleting` 时断言发布失败收口并输出 `reason=RepositoryPublishFailed` 告警；
- 运行兜底：外部清空 `release`（`repository.transition` 保留 + `RepositoryReady=False`）后，断言对象在下一轮轮询重新收敛为发布失败终局、不再零进展；`RepositoryUIDMismatch` 端到端断言对象按不可重试失败收口、Build 收敛 `Failed/publish`，而非持续空转。
- 永久错误仅日志暴露：撤销控制器读取权限（或注入 `401` / `403`）后断言对象不丢、`RpmRepo.status` 不变、每轮仅产生 `result=permanent-error` 日志且不新增计数；恢复权限后断言下一轮自动继续推进（自愈）。
- 发布重启（逐崩溃窗口）：① 策略判定后未写检查点 → 重启后重新判定得到相同 `excludeSpecs`，不产生第二个 release；② 检查点已写、`SubmitRelease` 未被接受 → `GetRelease` 返回 `404` 时用同一请求重放；③ 提交已受理、相位未补齐 → `GetRelease` 返回 `Creating` / `Prepared` 时补齐相位后继续等待或激活；④ 激活成功后收口写入前 → `GetRelease` 返回 `Ready` 时直接成功收口；⑤ 失败收口写入前 → `GetRelease` 仍不可重试失败时重新收口；⑥ 收口写入结果未知 → 先 GET 比对目标 status，已终态则本轮结束；⑦ `Requeue: true` 丢失 → 发布键由过程仓 resync 重新入队；全部窗口均不产生第二个 release 版本。

验收场景沿用 `docs/zh/design/artifact-manager.md` §9.12 的 11 条并改写为可断言用例。

## 十五、实施顺序

1. 客户端与 Fake：实现类型化 `Client`、`ArtifactManagerClient` 及两个 Fake，复用共享 client 的 `Get` / `ListProjectPage` / `UpdateStatus`，并保留写错误三分类；
2. 纯函数：批次选择与排序、`repositoryUID` 计算、`repository.transition` 状态机（含基于 Artifact Manager `attempt` 的重试预算判定、退避时长计算与"退避窗口是否已过"判定——锚点为响应 `updatedAt`）、`MergeCondition`；
3. 发布：实现 `PublishPolicy`（`Decide` + `PublishDecision`）与默认 `DefaultPublishPolicy`、`release.transition` / `release.phase` 持久化、发布状态机（提交 / 激活 / 收口）；
4. Reconcile：只注册 `rpmrepos` PollingSource handler，实现 `build/` 键的选批、提交、transition 恢复，`release/` 键的候选查询（`labelSelector=ebs.io/target-os` / `ebs.io/target-arch` + `fieldSelector` 排除发布终态）、每轮一次同名 Build 读取与校验、在途优先、候选选取、检查点式发布驱动与重启恢复、发布键入队与队列结果映射；
5. 装配：注册 `rpmrepo.Initializer(Config{ArtifactManagerAddr, ArtifactManagerTimeout, MaxJobsPerBatch, MaxInputBytes, MaterializeRetryLimit, PollPeriod, MaxRetries})`（注入 `clock.RealClock{}` 与 `DefaultPublishPolicy`）；物化退避曲线沿用框架慢速重试三项，由 initializer 从 `init.Config` 读取并传给 `controller.WithSlowRetry(...)`（与 build / snapshot 控制器一致），控制器自有 `Config` 不承载这三项。在 initializer 阶段校验静态配置（含 `--rpmrepo-materialize-retry-limit > 0`）与 Artifact Manager 配置，指标与权限就位，并注册到 `initializers` 集合（flag 名与 2.1 表一致；启用/关闭按第 6 步）；
6. 测试与启用：联调期用 `--controllers=-rpmrepo` 保持关闭；单元、集成与重启恢复测试全部通过后，恢复默认 `--controllers=*` 启用（与 2.1 的表述一致）。

## 十六、假设

- 上游契约前提：`BuildInfo.status.phase=Completed` 表示不再新增候选 Job（Job 集合自此固定）；7.2 的发布触发与失败收口写入的 `release.phase=Failed` 终局都依赖它。该契约的权威定义属 BuildInfo Controller，不在本仓库，见第十三章第 2 条；
- 物化失败策略：可重试失败按退避最多重放 `--rpmrepo-materialize-retry-limit`（默认 3）次，**重试次数不在 `RpmRepo.status` 中持久化，而是以 Artifact Manager 响应的 `attempt` 为权威计数**（重启不丢）；退避参数复用框架慢速重试三项；重试耗尽或遭遇不可重试失败即失败收口，`repository.transition` 原样保留；
- 无可用版本的处理：`BuildInfo=Completed` 且无候选、无未就绪输入，但 `repository.sourceJobUIDs` 为空（本对象从未产出任何版本）时，按「不存在可发布内容」直接写发布失败终局（不等待人工介入）；「本对象已产出可发布版本」的唯一判据是 `sourceJobUIDs` 非空，Build Controller 预置的继承基线只提供可读仓库指针、不计入本对象版本，因此仅有继承基线的对象同样走该终局；该场景在按「版本指针非空」判定的旧设计下会把继承基线误当成本对象产物并尝试发布，本设计显式收口；
- 取舍：本对象已产出过版本（`repository.sourceJobUIDs` 非空）的对象在失败收口后同样被登记为发布失败（`release.phase=Failed`），该版本仍可读但不再发布新版本；仅有继承基线（`sourceJobUIDs` 为空）的对象从不进入发布流程；同一失败批次不会重新组批（Artifact Manager 对同一 `repositoryUID` 的不可重试失败幂等返回、永不重跑）；
- 外部服务前提：Artifact Manager 满足 `artifact-manager.md` §9.13 的发布语义——同一 `buildName` + 相同请求摘要（摘要由 AM 服务端计算）的提交幂等（不同摘要返回 `409`），且不会自行激活 `Prepared` 记录；7.3「重启恢复」建立在这两条行为上。
- 发布候选依赖 RpmRepo 目标标签：候选集合由 `labelSelector=ebs.io/target-os` / `ebs.io/target-arch` 服务端过滤确定（键由过程仓键按 `Build.spec.buildTarget` 构造）；标签由 Build Controller 在创建 RpmRepo 时写入、apiserver 创建校验强制存在。标签能力上线前创建的存量对象没有标签，本设计不做兼容兜底（不兜底查询、不补写 metadata），需删除后重建或运维一次性补标签；标签与同名 Build 目标不一致按数据异常跳过并告警（`reason=RpmRepoLabelMismatch`）。
- 单一触发源前提：键只从 `rpmrepos` 轮询派发（不注册 Job 侧事件源），故本设计不设 `buildType=single` 特判；若将来增加 Job 侧事件源，需要重新评估该前提。
