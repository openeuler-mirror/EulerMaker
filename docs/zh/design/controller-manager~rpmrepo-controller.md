# RpmRepo Controller 设计

## 一、定位与范围

RpmRepo Controller 是 controller-manager 中负责把构建产物物化为 RPM 仓库并驱动仓库正式发布的控制器。

职责：

- 消费 BuildInfo、Job、Build 的信息，维护 RpmRepo.status，推进构建产物物化与仓库正式发布；
- 支持重复调谐与重启恢复，提供结构化日志和指标。

职责边界：

- 不写 `Build.status`、`BuildInfo.status`、`Job.status`、`Project.status`；Build 侧由 Build Controller 自行复制发布结果；
- 不创建或删除 Job，不修改 `Build.spec`，不修改 `RpmRepo.spec`（首版 `RpmRepo.spec` 为空 `{}`）；
- 不直连 Artifact Manager 的 `${dataDir}`；所有物化与发布动作只经 `ArtifactManagerClient` 的 HTTP 接口；本控制器不解析、不持久化 RPM 元数据（单个 RPM 的元信息由 Artifact Manager 维护）；
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
  conditions.go            # MergeCondition（条件 Type / Reason 常量复用公共 ebs/v1）
  metrics.go               # rpmrepo_controller_* 指标注册
```

依赖与注入：

- `Controller` 持有 `BaseController`（队列/Worker）、`Client`、`ArtifactManagerClient`、`PublishPolicy`、`clock.Clock` 与配置；
- 构造时注入 `clock.Clock`（`k8s.io/utils/clock`）：生产用 `clock.RealClock{}`，测试用 `k8s.io/utils/clock/testing.FakeClock`；业务代码不得直接调用 `time.Now()` / `time.Since()`；
- `const Name = "rpmrepo"`；initializer 负责构造 `Client`、`ArtifactManagerClient`、注入 `PublishPolicy`（首版固定 `DefaultPublishPolicy`）、只注册 `rpmrepos` PollingSource handler（不注册 Job Watch 源），并创建 `BaseController`；
- reconcile 内所有 ebs-apiserver 访问只通过 `Client`，所有发布访问只通过 `ArtifactManagerClient`；单测注入 `fake.Client` 与 `fake.ArtifactManagerClient`。

### 2.1 装配与配置

- 队列 key 分两类：过程仓 `build/{project}/{buildName}`，发布 `release/{project}/{os}/{arch}`；`{project}` / `{buildName}` 分别等于 `RpmRepo.metadata.namespace` / `RpmRepo.metadata.name`，`{os}` / `{arch}` 取 `Build.spec.buildTarget.os` / `Build.spec.buildTarget.arch`；发布键由这两个字段构造，发布候选再按 RpmRepo 的同名标签 `ebs.io/target-os` / `ebs.io/target-arch` 做服务端过滤，键与过滤同源；
- 两类 key 都带 `{kind}/` 前缀（`build/`、`release/`），避免同 Project 下 `buildName` 与 `{os}/{arch}` 拼成的键撞名；两类 key 互不阻塞，同一 `release/{project}/{os}/{arch}` 内的发布由队列保证串行（与 Artifact Manager 的 `{project}/{os}/{arch}` 串行键一致）；
- 控制器自有配置项（启动阶段校验，非法即启动失败）：
  | 配置 | 默认值 | 校验规则 |
  | --- | --- | --- |
  | `--rpmrepo-max-jobs-per-batch` | `20` | 必须 `> 0` |
  | `--rpmrepo-max-input-bytes` | `20GiB`（`21474836480` 字节） | 必须 > 0；按 Artifact Manager 的物化输入口径累计，属批次追加上限；首个候选自身超限时直接发布失败（见第六章「批次构成」） |
  | `--rpmrepo-materialize-retry-limit` | `3` | 必须 `> 0`；物化可重试失败的重试次数上限，首次提交不计入，判定见第八章 |
  | `--artifact-manager-addr` | 必填，无默认 | 非空且可解析为带 host 的 `http` / `https` URL |
  | `--artifact-manager-timeout` | `30s` | 必须 `> 0` |
  - 命名沿用现有风格（对照 `--git-server-addr` / `--git-server-timeout`、`--snapshot-failure-retry-limit`）；这些 flag 由本控制器独占，将来若被别的控制器复用再提升为共享段；
- 框架配置沿用 `controller-manager.md`，不单独覆盖 worker 数和轮询周期；`--controllers` 控制本控制器启停。
- 物化退避复用框架参数 `--controller-slow-retry-initial-delay`（30s）、`--controller-slow-retry-max-delay`（15m）、`--controller-slow-retry-jitter`（0.2）；计算、预算与等待规则统一见第八章。
- 配置校验时机：所有静态配置在 `options.Parse` 阶段完成校验，非法时 controller-manager 直接启动失败，不得延迟到单个对象的 reconcile 中处理；
- `ArtifactManagerClient` 构造失败必须返回初始化错误，不得通过 HealthChecker 表达。
- 慢速重试参数从框架 `init.Config` 读取并传入 `controller.WithSlowRetry(...)`，同时供物化退避计算使用；控制器自有 Config 不重复承载这三项。

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

**最小框架改动**

- Source 装配复用 `PollingSourceFactory.ForResource(gvr, period, options)`，具体规则见 2.3。
- 复用共享 client 的 `Get` / `ListProjectPage` / `UpdateStatus`；
- 不为 `Retry-After` 修改共享客户端或控制器框架：带 `Retry-After` 的 `429` / `503` 由本控制器换算为 `ReconcileResult{RequeueAfter}`（见第七章）；
- 在 `cmd/controller-manager/main.go` 的 `initializers` map 中加入 `rpmrepo.Initializer(...)`。

### 2.3 Source 装配语义

RpmRepo PollingSource 是唯一的外部变化触发源，周期沿用 `--poll-period`（默认 30s）。正常情况下，Job 完成和 Manifest 状态变化由下一轮轮询触发检查；实际处理时间还受队列等待、请求耗时及错误重试影响。

- `rpmrepos`：注册 `PollingSourceFactory.ForResource(RpmReposGVR, period, metav1.ListOptions{FieldSelector: "status.release.phase!=Ready,status.release.phase!=Failed,status.release.phase!=Skipped"})` 的 handler（`RpmReposGVR` 已由框架 `source` 包内置）。Add/Update 事件按 `build/{namespace}/{name}` 入队；Delete 事件仅记录日志；
  - 过滤语义：本源**只带** `fieldSelector`（驱动过程仓键、由单个对象重算），不带 `labelSelector`；只排除「发布终态（`release.phase ∈ {Ready, Failed, Skipped}`）」的 RpmRepo。过程仓不再有相位，apiserver 对 RpmRepo 的**字段**选择器只有 `metadata.name` / `metadata.namespace` / `status.release.phase`（labelSelector 另行支持，发布候选查询见 §7.3），因此不能按过程仓状态过滤。字段缺失（`release` 为 null）视为「不等于任何具体相位」，不会被排除。RpmRepo 的 `ebs.io/target-os` / `ebs.io/target-arch` 标签由 Build Controller 在创建时写入、apiserver 创建校验强制存在；
  - 成本控制：物化在途与失败收口的对象都会每轮入队，但**重试路径完全不写 status**（计数在 Artifact Manager 侧，`attempt` 只通过响应读取），等待路径在状态未变化时也不写 status（见 7.2）；对象收敛到 `release.phase ∈ {Ready, Failed, Skipped}` 后即被本过滤排除；
  - RpmRepo 的 `status` 变化只用于触发重算，reconcile 内仍以最新 GET 的对象为准。
- 本控制器不注册 Job Watch 源，不按 buildType 分支；single 的 RpmRepo 由 Build Controller 以 `release.phase=Skipped` 创建，通过终态过滤与入口守卫排除。
- 发布键由过程仓 reconcile 满足 7.2 的触发条件时通过 `BaseController.Enqueue` 入队；每轮 resync 重新检查，因此重启或入队丢失后可重新派发，不需要独立的发布事件源。
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
    // ListJobs 按项目作用域读取全部分页；成功返回完整集合，失败返回 nil、error。
    // 调用方以 labelSelector=ebs.io/build-name=<RpmRepo 名> 做服务端归属过滤，相位与其余三个 label 在控制器侧校验。
    ListJobs(ctx context.Context, project string, options metav1.ListOptions) ([]v1.Job, error)
    // ListRpmRepos 按项目作用域读取全部分页；成功返回完整集合，失败返回 nil、error。
    // 调用方以 labelSelector（ebs.io/target-os / ebs.io/target-arch）做服务端目标过滤，并按 fieldSelector 排除发布终态。
    ListRpmRepos(ctx context.Context, project string, options metav1.ListOptions) ([]v1.RpmRepo, error)

    // RpmRepo 写入（仅 status）
    // UpdateRpmRepoStatus 走 /status：写 RpmRepo.status，apiserver 保留旧 spec 与 metadata
    UpdateRpmRepoStatus(ctx context.Context, obj *v1.RpmRepo) (*v1.RpmRepo, error)
}
```

`ListJobs` / `ListRpmRepos` 通过共享 client 的 `ListProjectPage` 分别调用 `jobs` / `rpmrepos` 的项目级路径；Get 类方法在 404 时返回保留状态码的读取错误，不包装为写错误。本控制器不需要读取 Project 对象；若未来策略需要 Project 对象，再扩展 `PublishPolicyInput`。

**完整分页契约**（由类型化 Client 实现，不由 reconcile 手动翻页）：

- 每次调用从首页开始，复制 `ListOptions` 并清空 `Continue`，不修改调用方参数、不沿用上次调用的游标。`Limit > 0` 时作为每页大小，`Limit == 0` 时默认每页 500 条；负数为本地输入错误。`Limit` 不是结果总数上限，也不使用批次的 Job 数量上限截断列表。
- 循环调用 `ListProjectPage`，下一页 `Continue` 原样取上一页返回值。整个调用保持 resource、project、labelSelector、fieldSelector、Limit 及其它查询条件不变，不解析或自行构造游标。
- 仅当响应 `Continue` 为空时结束。空页或不足一页但仍有 Continue 时必须继续读取；不得根据页长度、当前已有候选数或找到在途对象提前返回。
- 所有页面读取成功后才返回完整集合；无对象返回空集合与 `nil`。完整结果的排序、候选选择与批次截断由 reconcile 执行；列表读取期间不得基于已读页面写 status、提交物化或启动发布。
- 任意页请求、解码或对象转换失败，丢弃本次已累计数据，返回 `nil, error`，保留读取错误的状态码、原因与重试提示，不包装为 `WriteError`。临时错误按读取错误规则重试；游标过期（410）同样结束本次调用并作为可重试读取错误返回，下一周期从首页重新读取，不拼接新旧扫描结果，也不在本次调用中无限重新扫描。
- 全部分页共用调用方 context；取消或超时立即结束并丢弃部分结果。响应重复返回已使用的非空游标时终止扫描，作为响应契约错误返回 `PermanentError`，不得无限翻页或返回部分集合。
- 只有完整成功的列表才能参与“没有剩余 Job”“没有在途发布”等判断。完整分页不等于跨资源事务快照：首次发布仍须遵守 BuildInfo Completed 契约、最新对象复核与 RpmRepo CAS 写入规则。

### 3.2 路径约定

- 子资源单对象：`GET /apis/ebs/v1/projects/{project}/{resource}/{name}`，`{resource}` 取 `builds` / `buildinfos` / `rpmrepos`；
- Job 列表：`GET /apis/ebs/v1/projects/{project}/jobs?labelSelector=ebs.io/build-name=<buildName>`（`buildName` 等于 RpmRepo 名；仅按归属 label 服务端过滤，不附加 `status.phase` 字段选择器）；
- RpmRepo 列表（发布候选）：`GET /apis/ebs/v1/projects/{project}/rpmrepos?fieldSelector=status.release.phase!=Ready,status.release.phase!=Failed,status.release.phase!=Skipped&labelSelector=ebs.io/target-os=<os>,ebs.io/target-arch=<arch>`（两个选择器与 Project 路径隐含的 namespace 条件按 AND 组合）；
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
| `RepositoryResponse` | 物化响应：`repositoryUID`、`state`、`attempt`（重试预算的权威计数）、可选的 `pollAfterSeconds`、`contentURL`、`failure` 与 `createdAt` / `updatedAt`（退避窗口锚点，见 §8）/ `completedAt`；RPM 元数据和内容摘要只保留在 Artifact Manager 内部，不在响应中返回 |
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

- 类型化 Client Fake 支持预设对象、读取错误、写错误 Outcome 与 resourceVersion 冲突，模拟成功写入，并记录查询条件和写入请求。
- ArtifactManagerClient Fake 支持预设每个方法的响应序列（状态、attempt、时间、Retry-After 与错误），记录调用次数及请求体，支持重置。
- 完整分页使用共享 client 的 HTTP 测试服务或单页接口替身验证，不能仅用直接返回完整切片的 Fake 替代。具体响应组合和断言见第十四章。

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
      reason: RepositoryCreationFailed
    - type: PublishSucceed
      status: "False"
      reason: RepositoryCreationFailed

# 发布后
status:
  repository:
    contentURL: /repositories/v1/<repositoryUID>/
    repositoryUID: <repositoryUID>
    sourceJobUIDs: ["uid-job-1"]
    transition: null
    updatedAt: "2026-09-14T10:00:00Z"
  release:
    phase: Ready            # 非 single 发布结果；single 创建时为 Skipped
    sourceRepositoryUID: <repositoryUID>
    contentURL: /repositories/<project>/<os>/<arch>/
    transition: null
    updatedAt: "2026-09-14T10:00:00Z"
  conditions:
    - type: PublishSucceed
      status: "True"
      reason: ReleaseActivated
```

字段来源：

| 字段 | 来源/写入方 | 说明 |
| --- | --- | --- |
| `RpmRepo.metadata.name` / `namespace` | Build Controller | 与 Build 同名同 namespace |
| `RpmRepo.metadata.labels` | Build Controller（创建时写入） | 写入 `ebs.io/target-os` / `ebs.io/target-arch`，值取 `Build.spec.buildTarget.os` / `arch` ；本控制器只读、不补写、不改写，用于发布候选的服务端 `(os, arch)` 过滤 |
| `RpmRepo.spec` | Build Controller | 空 `{}`，本控制器不修改 |
| `RpmRepo.status.repository.contentURL` | Build Controller（创建时预置基础仓）/ RpmRepo Controller（物化提升时覆盖） | 创建时预置为上一个发布成功 Build 同名 RpmRepo 的 `contentURL`，即本对象的基础仓；提升后为本对象当前可读版本的地址；失败路径不清空。取 Artifact Manager 返回的相对路径，形如 `/repositories/v1/{repositoryUID}/`；产出判据见 `sourceJobUIDs` |
| `RpmRepo.status.repository.repositoryUID` | Build Controller（创建时预置基础仓）/ RpmRepo Controller（物化提升时覆盖） | 创建时预置为基础仓的 `repositoryUID`（`SubmitRepository` 的 `baseRepositoryUID` 来源）；提升后为本对象当前版本的物理标识。产出判据见 `sourceJobUIDs` |
| `RpmRepo.status.repository.sourceJobUIDs` | RpmRepo Controller | 已成功纳入当前版本的 Job UID；写入时去重并按字典序排序；不记录失败 Job。为空表示本对象尚未产出过版本（Build Controller 预置的继承基线不计入），其非空是「本对象已产出可发布版本」的唯一判据，不以 repositoryUID / contentURL 或 RepositoryReady 条件代替 |
| `RpmRepo.status.repository.transition` | RpmRepo Controller | 在途批次或已放弃批次的固定输入（`inputs`）、`baseRepositoryUID` 与 `repositoryUID`；不承载重试计数（计数以 Artifact Manager 的 `attempt` 为准，见 §8）。成功提升时随 `transition=nil` 一起清除，失败收口时原样保留 |
| `RpmRepo.status.repository.updatedAt` | RpmRepo Controller | 过程仓 status 最近一次有效写入时间（提交批次、失败收口、提升版本时更新）；**不用作退避锚点**（退避以 Artifact Manager 响应的 `updatedAt` 为准，见 §8） |
| `RpmRepo.status.release` | Build Controller（single 创建时）/ RpmRepo Controller（非 single） | 发布状态：`phase`（`Pending` / `Creating` / `Prepared` / `Ready` / `Failed` / `Skipped`；single 初始为 Skipped）/ `sourceRepositoryUID` / `contentURL`（Project / OS / 架构的稳定仓库入口，取 Artifact Manager 返回的相对路径，形如 `/repositories/{project}/{os}/{arch}/`）/ `transition`（`sourceRepositoryUID` + 规范化 `excludeSpecs`）/ `updatedAt`；提交前先写 `release.transition`，它即请求的全部可变输入，重启后据此重放同一请求（请求摘要由 Artifact Manager 服务端计算，不落本状态） |
| `RpmRepo.status.conditions` | RpmRepo Controller | 过程仓与正式发布两类错误条件；type 必须区分二者（见第九章） |

## 五、状态机

### 5.1 发布状态机（`release.phase`）

`Skipped` 由 Build Controller 创建 single 的 RpmRepo 时写入，表示该对象只保存供 BuildInfo 使用的继承过程仓，不生成新版本、不正式发布。它与 Ready、Failed 同属本控制器的终态；终态检查先于 Build 中止，即使 Build 随后 Aborted，也不得将 Skipped 改为 Failed。地址仍保存在 `repository.contentURL` 并与 repositoryUID 成对，release.contentURL 不填写；无基线时 repository 为 nil。Skipped 不写成功或失败条件、不计发布成功/失败指标，也不表示 Build 已完成。


发布相位 `release.phase`（`release` 缺失表示尚未进入发布阶段）；在途与恢复判据是 `release.transition` 非空，`release.phase` 用于候选过滤（`∈ {Ready, Failed, Skipped}` 为终态）与对外提供发布结论：

| phase | 含义 | 允许的下一步 |
| --- | --- | --- |
| `Pending` | 检查点已写，尚未提交或提交结果未确认 | `Creating` / `Prepared` / `Ready` / `Failed` |
| `Creating` | 已提交 `SubmitRelease`，Artifact Manager 处理中 | `Prepared` / `Ready` / `Failed` |
| `Prepared` | 正文就绪，待激活切换稳定入口 | `Ready` / `Failed` |
| `Ready` | 已激活，稳定入口可读（`release.contentURL` 非空） | 终局；不再重复提交 |
| `Skipped` | Build Controller 在 single 创建时设置 | 终态；本控制器不修改 |
| `Failed` | 发布不可重试失败、过程仓失败或 Build 主动中止时的终局登记（中止 reason 为 BuildAborted）（由 §7.2 失败收口同次写入） | 终局；修正内容需新建 Build |

发布不修改过程仓字段；所有收口路径清空 `release.transition`。发布门禁、策略、候选扫描及在途恢复以 7.3 为准，过程仓失败与无可用版本导致的发布失败以 7.2 为准；完整写入字段在相应收口动作中定义。

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

Job 主动中止以 apiserver 持久化的终态为准：Aborted 即使已有 Completed Manifest，也不入候选、不加入 `sourceJobUIDs`，继续扫描其它成功 Job；单个 Job 中止不等同于父 Build 中止，不触发 `BuildAborted` 发布失败。Succeeded 先写入时 `/abort` 不改变其结果，仍按正常规则归档。终态 status 不可变，因此正常 `/abort` 不会使已冻结的成功输入变为 Aborted，也不新增批次回滚路径。最终发布仍遵循 BuildInfo Completed 和发布策略。

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
  - 非空基础仓必须是 Ready 版本，不能引用本批 repositoryUID；不得以空版本冒充基础仓。基础仓有效性由 Artifact Manager 校验；
  - 若 Artifact Manager 因基础仓的 Project / OS / 架构与请求不一致（或基础仓不是 `Ready`）返回 `422 BaseRepositoryNotReady`，按**不可重试失败**收口（本设计不自行校验该一致性），并记录 `reason=RepositoryCreationFailed` 告警日志；
- `repositoryUID` 是物化请求的幂等键，计算必须与 Artifact Manager 的计算保持一致。
- `SubmitRepository` 请求字段推导固定为：`repositoryName` 必须等于 `buildName`（Artifact Manager 校验两者相等，否则返回 `422 InvalidRepositoryRequest`）；`project` / `buildName` 取 RpmRepo 的 `metadata.namespace` / `metadata.name`；`targetOS` / `targetArch` 取 `Build.spec.buildTarget.os` / `arch`；`baseRepositoryUID` 取本批冻结的基础仓（无基础仓时为空串）；`manifests` 由 `repository.transition.inputs` 映射，只含 `jobName` 与 `jobUID`，并按 `jobUID` 升序提交（Artifact Manager 会服务端排序并校验 `jobUID` 不重复、标识符合法，否则返回 `422 InvalidManifestReference`）。
- 相同批次重试自然复用同一 `repositoryUID`；基础仓或批次成员变化必然产生不同的不可变版本。
- 恢复不依赖 Job 对象仍然存在：`SubmitRepository` 按 `jobUID` 引用 Artifact Manager 侧已封账的 manifest，Job 被 history GC 回收后仍可按 `repository.transition` 重放；若 Artifact Manager 侧输入已过期，重放返回 `410 MaterializationInputExpired`，按**不可重试失败**处理（直接失败收口，不消耗重试预算，见 7.2）。

## 七、Reconcile 流程

**状态写入原子性**：每次 status 迁移先构造完整目标 `RpmRepo.status`，再调用一次 `UpdateRpmRepoStatus`——`repository.*`、`release.*` 与 `conditions` 的变更合并到同一次写入；写入冲突（Rejected / 409）立即结束本周期，返回 `ReconcileResult{RequeueAfter: 1s}` + `nil`；下一周期重新 GET 对象与依赖后计算。不得在本周期重做该步骤、继续提交或激活，也不得仅替换 resourceVersion 重放旧目标。

**状态写错误分类**：仅适用于 `UpdateRpmRepoStatus`。先按共享 apiserver 客户端返回的 `WriteError.Outcome` 分流，不根据错误文本或“是否超时”覆盖 Outcome；Artifact Manager 错误按第八章处理，其 409 不是 apiserver CAS Conflict。成功响应违反 3.2 的契约时由适配层归为 Unknown。

| Outcome / 原因 | 返回与处理 |
| --- | --- |
| `NotSent`：本地输入、对象类型、目标路径或 metadata 校验错误 | 零值 + `controller.NewPermanentError(err)` |
| `NotSent`：临时网络错误或请求超时 | 零值 + 原始错误，框架退避重入 |
| `Rejected / 409` | 结束本周期，`ReconcileResult{RequeueAfter: 1s}` + `nil`；下一周期重新读取并计算 |
| `Rejected / 404` | 对象已不存在，结束旧周期，零值 + `nil`；不创建替代对象 |
| `Rejected / 408、429、5xx` | 零值 + 原始 `WriteError`，保留 Retry-After 交由框架处理 |
| `Rejected` 的其它拒绝响应（含 `400、401、403、422`） | 零值 + `controller.NewPermanentError(err)` |
| `Unknown` | 执行下述完整写入意图确认，不重放旧 PUT |

Manager context 已取消时返回零值 + `ctx.Err()`，停止后续请求，不启动后台确认。NotSent 与 Rejected 都不执行 Unknown 确认，不因写入错误额外写业务失败条件或终态。任何未确认成功的写入均停止当前推进链；尤其检查点未确认时不得提交外部操作，相位推进未确认时不得继续激活。

写入结果未知（`WriteError.Outcome=Unknown`，如已发送请求后超时、连接中断、响应无法解析）时不重放原 PUT，先 GET 当前对象。仅在 UID 与原对象一致、且本次写入意图中的全部目标字段均已达成时确认成功；比较范围包括本次变更的版本字段、完整检查点、相位、条件与时间字段，不要求未修改的字段保持旧值。下表列出各类写入必须覆盖的业务字段，不能只比较相位或单个标识：

| 写入类别 | 达成判据 |
| --- | --- |
| 提交批次（写 `repository.transition`） | 完整 `repository.transition`（`repositoryUID` / `baseRepositoryUID` / `inputs`）与目标一致 |
| 过程仓成功收口 | `repository.repositoryUID` / `contentURL` / `sourceJobUIDs` 与目标一致、`repository.transition=nil`，且 `RepositoryReady` 条件与目标一致 |
| 过程仓失败收口 | 完整 `repository.transition` 与目标一致、`RepositoryReady` 条件与目标一致，且发布失败终局的全部目标字段一致 |
| 发布检查点（写 `release.phase=Pending` 与 `release.transition`） | `release.phase=Pending`，完整 `release.transition`（`sourceRepositoryUID` / `excludeSpecs`）与目标一致 |
| 发布收口 | 本次变更的 `release` 字段（含相位、版本标识、地址和清空的 transition）及 `PublishSucceed` 条件与目标一致 |

- 判定“已达成”：本次写入确认成功；后续步骤必须使用 GET 返回的最新对象重新检查前置条件，不继续使用旧目标对象。
- 同 UID 但目标未完全达成：放弃旧决策，返回 `ReconcileResult{RequeueAfter: 1s}` + `nil`；下一周期重新读取对象及依赖、计算目标。即使 resourceVersion 未变化，也不得直接重放旧 PUT；尤其禁止只替换成新的 resourceVersion 后提交旧目标。
- NotFound 或 UID 不同：结束旧周期，返回零值 + `nil`，不对替代对象执行旧写入。
- GET 失败：按读取错误分类返回，不继续写入。最新对象已终态或删除中时结束旧周期，不尝试恢复旧目标。

以上规则也适用于发布相位推进、清空 `release` 和中止收口。Artifact Manager 基于已持久化检查点的幂等请求重放不属于旧 `/status` 决策重放；调用前仍须通过最新对象的状态与身份检查。

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

**入口**：先 GET 同名 RpmRepo，完成存在性、删除与终态检查，再读取同名 Build 并处理中止；按以下顺序执行：

- RpmRepo 读取失败（非 NotFound）→ 按读取错误分类返回，不写 status；
- RpmRepo NotFound → 仅 GET 同名 Build 判断缺失场景：Build 也 NotFound 时本键收敛；Build 可读时返回可重试错误（前置创建尚未完成或对象被异常删除）；Build 读取失败按统一读取错误分类返回。即使 Build 已 Aborted，也不创建替代对象、不执行中止状态写入；
- `RpmRepo.metadata.deletionTimestamp` 非空 → 返回零值 + `nil`；
- `RpmRepo.status.release.phase ∈ {Ready, Failed, Skipped}` → 返回零值 + `nil`，不读取 Build、不重复写入，也不入队发布键；
- GET 同名 Build：NotFound 表示孤儿 RpmRepo，记录 `reason=BuildMissing` 告警并计入 `rpmrepo_controller_build_missing_total`，返回零值 + `nil`，由后续轮询复查；其它错误按统一读取错误分类返回；
- `Build.status.phase=Aborted` → 一次 CAS 写 `release.phase=Failed`、`release.transition=nil`、`release.updatedAt` 与 `PublishSucceed=False`（reason `BuildAborted`）；`repository.*` 不变，输出 `reason=BuildAborted` 日志。写入成功返回零值 + `nil`，失败按 status 写错误分类处理；不调用 Artifact Manager、不入队发布键。此分支仅处理存在、未删除且非终态的 RpmRepo，优先于后续在途恢复；
- 同名 `BuildInfo` 读取失败或不存在 → 按章首统一语义处理（`NotFound` 视为未就绪：本轮等待，仅输出 `reason=BuildInfoNotReady` 告警日志）。对象可读时，`phase != Completed` **不阻止过程仓推进**：仍按已成功 Job 的封账 manifest 选批、提交、恢复在途批次并提升版本；`Completed` 仅作为第 5 步正式发布触发及“无可用版本”失败判定的前置条件；
- `status.release` 非空 → 通过 `BaseController.Enqueue` 入队 `release/{project}/{os}/{arch}`（目标取 `Build.spec.buildTarget`），返回零值 + `nil`；`status.release` 为空 → 执行下方过程仓推进。

上述等待与收敛分支不写 status、不调用 Artifact Manager；中止收口的唯一例外已在对应分支明确。

过程仓推进（无相位，按顺序执行）：

1. **在途判断**（`repository.transition != nil`；`status.release` 非空时入口已提前返回，因此"已放弃批次"分支只在 `release` 被外部清空后可达）：
   - 在途批次（`RepositoryReady` 不是 `False/reason=RepositoryCreationFailed`）→ 跳至第 3 步；
   - 已放弃批次（`transition != nil` 且 `RepositoryReady=False/reason=RepositoryCreationFailed`）→ 视为发布终局被外部清空：一次 CAS 重写发布终局 `release.phase=Failed` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False`（reason `RepositoryCreationFailed`），不写 `repository.*`、不调用 Artifact Manager、**不重复计** `rpmrepo_controller_repository_failed_total`，记录 `reason=RepositoryCreationFailed` 告警日志，返回零值 + `nil`；
   - 无在途批次 → 第 2 步。
2. **提交候选批次**（候选规则见第六章）：有候选 Job → 若排序最前的候选自身输入超过 `--rpmrepo-max-input-bytes`，**不形成批次**、不写 `repository.transition`，输出一次 `reason=InputTooLarge` 并按发布失败终局收口（一次 CAS 写 `release.phase=Failed` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False`（reason `RepositoryCreationFailed`）；不写 `repository.*`、不调用 Artifact Manager；计一次 `rpmrepo_controller_repository_failed_total`；返回零值 + `nil`）；否则按第六章选出 `inputs` / `baseRepositoryUID` / `repositoryUID`，以最新 `resourceVersion` 一次 CAS 写 `repository.transition` 与 `repository.updatedAt`（409 时结束本周期并延迟 1s 重新入队，下轮重算；本轮不调用 SubmitRepository），随后调 `SubmitRepository` 并把响应交给第 3 步；无候选 Job → 第 5 步。控制器不写任何重试计数（计数以响应 `attempt` 为准）。
3. **结果分流**（`SubmitRepository` 的响应，或 `GetRepository(repository.transition.repositoryUID)` 的响应；冻结 `repository.transition`，沿用 `inputs` / `baseRepositoryUID` / `repositoryUID`，不得重新选批、更换基础仓或重算）：

| 响应 | 处理 |
| --- | --- |
| `202` / `200 Creating` | 保留 `repository.transition`，按 `ReconcileResult{RequeueAfter: pollAfterSeconds}` + `nil` 延迟重入（不叠加框架退避），本轮不写 status；`202` 覆盖三种子情形：首次接受（`Creating, attempt=1`）；相同请求正在执行或排队（返回原 `Creating`，不重复入队、不增加 attempt）；相同请求处于可重试 `Failed`（服务端原子增加 attempt、清空旧 Failure、写为 `Creating` 后重新入队） |
| `404`（Artifact Manager 无该 `repositoryUID` 记录） | 用完全相同的 `repositoryUID` / `Manifests` / `baseRepositoryUID` 重放 `SubmitRepository`，再按本表分流；这是"`repository.transition` 已写、`SubmitRepository` 尚未被接受"崩溃窗口的恢复路径，不得直接失败收口。注意重放会新建记录并把 `attempt` 重置为 `1`（重试预算随之从零开始，见 §8） |
| `200 Ready` | 执行「成功收口」（第 4 步） |
| 可重试失败（已落到记录）：`200 Failed{retryable=true}` | 按第 4 步的「重试」处理：响应 `attempt < limit + 1` 且在退避窗口（锚点为响应 `updatedAt`）之后则重放同一请求（不写 status）；窗口未过只返回剩余等待；`attempt >= limit + 1` 则按第 4 步的「失败收口」处理 |
| 可重试失败（未落到记录）：`429`、`503`、网络错误 / 超时 / 响应无法解析 | 服务端未接受新的执行尝试、`attempt` 不变，因此**不消耗重试预算**；保留 `repository.transition`，按 `Retry-After`（或退避）重放同一请求，不写 status；同一退避窗口内的重复入队不触发重放 |
| 不可重试失败：`200 Failed{retryable=false}`、`409 RepositoryIdentityConflict`、`409 RepositoryDeleting`、`410 MaterializationInputExpired`、`422 ManifestNotReady` / `BaseRepositoryNotReady` / `ManifestInvalid` / `ManifestContainsNoPackages` / `InvalidRepositoryRequest` / `RepositoryUIDMismatch` / `InvalidManifestReference` | 直接进入「失败收口」（不重试、不消耗预算；`409 RepositoryIdentityConflict` 额外记录告警） |
| `200 Deleting`（该 `repositoryUID` 的记录正在删除） | 直接进入「失败收口」（不等待删除收尾，与 `409 RepositoryDeleting` 同口径），并记录 `reason=RepositoryCreationFailed` 告警 |

4. **收口**（写入字段清单见第四章字段来源表，本节只列特有判定）：
   - **成功收口**：把本批 Job UID 并入 `repository.sourceJobUIDs`（去重排序），一次 CAS 提交目标 status——`repository.contentURL` 取 Artifact Manager 的 Ready 响应、`repository.repositoryUID` 取本批 `repository.transition.repositoryUID`、`repository.transition=nil`、`repository.updatedAt` 与 `RepositoryReady=True/reason=RepositoryCreated` 条件；随后若仍有可入选 Job 则重新入队本键组下一批，否则进入第 5 步；
   - **重试**（`200 Failed{retryable=true}` 且响应 `attempt < --rpmrepo-materialize-retry-limit + 1` 且已过退避窗口，或 `429` / `503` / 网络 / 超时 / 未解析）：**不写 status**（`transition` 与版本字段原样保留），计一次 `rpmrepo_controller_materialize_retries_total`，按退避 `RequeueAfter` 用同一请求重放 `SubmitRepository`（`429`/`503` 优先按其 `Retry-After`；未落到记录的错误不影响预算）。退避窗口未过（锚点为响应 `updatedAt`）时本轮只返回剩余等待（`RequeueAfter`）+ `nil`：不调用 `SubmitRepository`、不写 status、不计 `rpmrepo_controller_materialize_retries_total`；
   - **失败收口**（可重试失败且响应 `attempt >= --rpmrepo-materialize-retry-limit + 1`，或遭遇不可重试失败）：一次 CAS 提交目标 status——`repository.transition` **原样保留**（`inputs` / `baseRepositoryUID` / `repositoryUID` 均为失败时刻取值）、`repository.updatedAt`、`RepositoryReady=False` 条件（reason `RepositoryCreationFailed`），并同次写发布失败终局 `release.phase=Failed`、`release.transition=nil`、`release.updatedAt` 与 `PublishSucceed=False` 条件（reason `RepositoryCreationFailed`）；计一次 `rpmrepo_controller_repository_failed_total`；不清空 `repository.repositoryUID` / `contentURL` 等版本字段；不调用 `SubmitRelease` / `ActivateRelease`、不额外入队发布键；本轮返回零值 + `nil`。
5. **发布触发**（三项前置）：`BuildInfo.status.phase=Completed` 且本轮候选扫描为空、① 无未就绪输入（无 manifest 为 `Open` / `Completing` 的 Job）、② `repository.transition=nil` 时——③ 若 `repository.sourceJobUIDs` 非空（本对象已产出可发布版本）→ 通过 `BaseController.Enqueue` 入队 `release/{project}/{os}/{arch}`（`{os}` / `{arch}` 取 `Build.spec.buildTarget`）并返回零值 + `nil`；**③ 不成立（`sourceJobUIDs` 为空，本对象从未产出任何版本、不存在可发布内容，无论 `repositoryUID` / `contentURL` 是否被预置了继承基线）→ 直接写发布失败终局**：一次 CAS 写 `release.phase=Failed`、`release.transition=nil`、`release.updatedAt` 与 `PublishSucceed=False` 条件（reason `NoPublishableArtifacts`），计一次 `rpmrepo_controller_release_failed_total`，不写 `repository.*`、不调用 Artifact Manager、不入队发布键，返回零值 + `nil`；①② 任一不满足 → 等待（不写 status，返回零值 + `nil`）。

第 5 步观察到 `BuildInfo.status.phase != Completed` 时，仅跳过发布触发与“无可用版本”判定，不回退或撤销本轮已经完成的过程仓推进；无下一批可处理时返回零值 + `nil`，等待后续轮询。

本键的等待、收敛与失败收口类分支统一返回零值 + `nil`（例外：带 `Retry-After` 的 `429` / `503` 返回 `ReconcileResult{RequeueAfter}` + `nil`）；错误分类与返回形态见 7.4。

### 7.3 发布流程（`release/{project}/{os}/{arch}`）

`List rpmrepos`（项目级 List）：两个选择器同时下发、与 Project 路径隐含的 namespace 条件按 AND 组合：

- `fieldSelector=status.release.phase!=Ready,status.release.phase!=Failed,status.release.phase!=Skipped`：该字段只支持 `Equals` / `NotEquals`、不支持 `NotIn`，因此用三个 `!=` 表达「非发布终态或无 `release`」；字段缺失（`release` 为 null）的对象会保留；
- `labelSelector=ebs.io/target-os={os},ebs.io/target-arch={arch}`：`{os}` / `{arch}` 取自发布键（由过程仓键按 `Build.spec.buildTarget` 构造，与 §7.2 入队同源）；标签由 Build Controller 在创建 RpmRepo 时写入、apiserver 创建校验强制存在。

返回集合即本键候选集、顺序不保证：集合内先取 `release.transition` 非空的在途对象（在途判据不受候选谓词约束），无在途对象时才用候选谓词筛出发布候选——`repository.transition==nil`、`repository.sourceJobUIDs` 非空（产出含义见第四章）且 `release.phase` 不属于 `{Ready, Failed, Skipped}`。

选对象与前置判定（本键候选集内一律按 `creationTimestamp`、`metadata.name` 升序取第一个）：

**选对象**（每轮最多推进一个对象的发布，但允许按稳定顺序检查并跳过多个未开始发布的候选）：

本节的“跳过”对无检查点候选表示继续当前循环，不能直接返回 Sync；只有扫描完全部候选才按等待返回零值 + `nil`。在途对象优先且不适用该跳过规则：其依赖缺失或目标不匹配时告警并停止本轮，保留检查点，等待恢复或人工处理，不启动同目标的新发布。7.4 的等待类返回值以此区分为准。

- **优先处理在途**：在途判据为 `release.transition` 非空（相位可能是 `Pending` / `Creating` / `Prepared`），该判据不受候选谓词约束（避免版本指针或 `sourceJobUIDs` 异常影响在途恢复）；存在在途对象时取之为本次对象；`metadata.deletionTimestamp` 非空（含在途对象）→ 本轮不推进、不驱动激活或收口、不启动新发布，返回零值 + `nil`；存在多个在途 → 记录 `reason=MultipleReleaseInFlight` 告警日志，其余在途对象等待本次对象收口；
- **发布候选**：无在途对象时，在候选集内按序逐个检查对象，跳过删除中的对象；对当前对象执行下面的前置判定与完整复核，满足可跳过条件时继续检查下一个，通过全部检查后才作为本次推进对象（非发布终态、过程仓不在途、本对象已产出可发布版本已由上面的候选谓词保证）。BuildInfo 的读取与就绪检查严格区分：
  - NotFound，或成功读取但 `phase != Completed`：记录 `reason=BuildInfoNotReady`，跳过当前候选并继续扫描；后续轮询会重新检查该候选，全部候选均跳过时才返回零值 + `nil`；
  - 网络、超时、`5xx` 等临时读取错误：按统一读取错误分类返回可重试错误，不能当作未就绪吞掉；带有效 `Retry-After` 的 `429` / `503` 沿用统一延迟重入规则；
  - 权限、参数或响应契约错误（包括 `401` / `403` / `400` / `422`、对象身份不符）：返回 `controller.NewPermanentError`；
  - context 取消：返回 `ctx.Err()`。上述未通过分支均不写 status、不调用 Artifact Manager、不执行 `PublishPolicy`；相同分类适用于首次发布完整复核中的 BuildInfo 读取。
- 无候选或全部候选均跳过 → 本键本轮结束（零值 + `nil`）。不持久化跳过标记，后续轮询重新检查。该规则允许暂时不可用的旧候选被后续候选越过，不保证严格按 Build 创建时间发布；旧候选恢复后仍可参与发布，本版本不新增过期候选淘汰策略。

**选中后的前置判定**（在途对象与发布候选同等适用；未通过者不得进入「有检查点」/「无检查点」）：

- **先检查最新 RpmRepo**：GET 选中对象，NotFound、UID 改变、删除中或 `release.phase ∈ {Ready, Failed, Skipped}` 时结束当前对象处理，不写 status、不调用 Artifact Manager；无检查点候选继续扫描下一个，在途对象则结束本轮、不启动其他发布；其它读取错误按读取错误分类返回。通过后才读取 Build 并判断中止，所有后续写入以该最新对象为基础。
- **读取并校验同名 `Build`（按当前扫描对象读取）**：对本次对象 GET 一次同名 `Build`，它同时是 `PublishPolicyInput.Build` 与 `TargetOS` / `TargetArch` 的来源——`Build` NotFound → 跳过该对象、记录 `reason=ReleaseGroupBuildMissing` 告警、不计数；无检查点候选继续扫描，在途对象结束本轮并阻塞新发布；`Build.spec.buildTarget.os` / `arch` 与对象 `metadata.labels[ebs.io/target-os]` / `[ebs.io/target-arch]` 不一致 → 跳过该对象、记录 `reason=RpmRepoLabelMismatch` 告警、不计数、不调用该对象的 Artifact Manager 接口；无检查点候选继续扫描，在途对象结束本轮并阻塞新发布；其它读取错误按章首「依赖读取的统一语义」处理（本轮不推进）；`Build.status.phase=Aborted` → 走下一行的「Build 中止收口」。标签由 apiserver 创建校验强制存在、`Build.spec` 创建后不可变，正常路径不会漂移，该校验只兜人工改坏标签；
- **Build 中止收口（先于「有检查点」与「无检查点」两个分支；在途对象不因存在 `release.transition` 检查点而例外）**：本次对象（在途或发布候选）的同名 `Build.status.phase=Aborted` 时——一次 CAS 写 `release.phase=Failed`、`release.transition=nil`、`release.updatedAt` 与 `PublishSucceed=False`（reason `BuildAborted`）；**不调用** Artifact Manager（不 `GetRelease`、不重放 `SubmitRelease`、不 `ActivateRelease`）、不做策略判定；`repository.*` 不写（在途 `repository.transition` 原样保留为已放弃批次）；输出 `reason=BuildAborted` 告警日志，返回零值 + `nil`；该对象随即被轮询过滤与发布候选排除，同目标其它候选不受阻塞；早中止（`BuildInfo` 未完成、`repository.sourceJobUIDs` 为空）的对象由过程仓键的同一分支收口；

**首次发布的完整复核**（仅无 `release.transition` 时执行，在策略判定与检查点写入之前）：

- GET 最新 RpmRepo，确认与选中对象 UID 一致、未删除且未进入发布终态；对象不存在、UID 改变、删除中或已终态时跳过该候选并继续扫描。以该对象重新检查 `repository.transition=nil`、`repository.sourceJobUIDs` 非空；不满足时跳过该候选并继续扫描；若已有发布检查点则转入在途恢复，不重新决策。
- 读取并校验同名 Build 与 BuildInfo（复用本轮读取结果时，必须在上述最新 RpmRepo 读取之后读取），要求 Build 未中止、BuildInfo 为 `Completed`。`Completed` 表示所有 Job 已收敛，完整契约见第十三章第 2 条。
- 复用第六章的候选扫描规则，完整读取 Job 列表所有分页，基于最新 `sourceJobUIDs` 排除已消费 Job，并检查 manifest。存在未消费候选时入队 `build/{project}/{buildName}`、跳过当前发布候选并继续扫描；存在 `Open` / `Completing` 时同样跳过当前候选并继续扫描；任意页或依赖读取失败按错误分类返回，不得把部分结果当作扫描为空。`404` / `Failed` 等跳过规则保持不变。
- 只有候选为空且没有未就绪输入时，才能执行发布策略并写检查点；CAS 必须使用上述最新 RpmRepo 的 resourceVersion，不得在扫描后仅刷新版本号。冲突则延迟重新入队，下一轮完整复核。过程仓键若先写批次检查点，发布 CAS 将冲突；发布检查点若先写入，旧过程仓 CAS 将冲突，下一轮按 `release` 分支停止组批。

已有 `release.transition` 的对象按持久化检查点恢复，不重新扫描选版本或计算发布策略；仍须遵守身份、删除与 Build 中止检查。

`推进状态`：

对上面已选定、并通过前置判定的本次对象，按检查点状态进入下面两个分支推进：

- 无检查点（条件：`release.transition` 为空且 `release.phase` 不属于 `{Ready, Failed, Skipped}`）：构造 `PublishPolicyInput`（`Project`、`Build`、`BuildInfo`、`SourceRepositoryUID=repository.repositoryUID`、`TargetOS`/`TargetArch` 取 `Build.spec.buildTarget`）调 `PublishPolicy.Decide`（`Build` 已在上一步读取并校验，`BuildInfo` 的读取失败按章首统一语义处理，此处不再重复分类）——策略返回 error → 返回可重试错误，不写 status、不提交发布；不发布 → 记录 `reason=ReleasePolicySkipped` 告警日志，仅在 `release` 非 nil 时一次 CAS 写 `status.release=nil`（不写 condition），本轮继续评估下一个候选、全部候选都不发布则返回零值 + `nil`；发布 → 一次 CAS 写检查点（`release.phase=Pending`、`release.transition.sourceRepositoryUID` 取 `repository.repositoryUID`、`release.transition.excludeSpecs` 为本次策略结果（去重并按字典序排序，不二次调用策略）、`release.updatedAt`；409 时结束本周期并延迟 1s 重新入队，下轮重算；本轮不调用 SubmitRelease），随后调 `SubmitRelease`（同 `buildName`、同 `sourceRepositoryUID`、同 `excludeSpecs`），按响应分流，返回形态见下表：

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
| `Deleting`（发布记录正在删除） | 执行「失败收口」（不等待删除收尾），记录 `reason=ReleaseFailed` 告警 |
| `429` / `503` 且 Artifact Manager 返回 `Retry-After` | 保留检查点，返回 `ReconcileResult{RequeueAfter: <Retry-After>}` + `nil` |
| 网络错误 / `5xx` / 响应无法解析 / 响应契约错误（`attempt < 1`、`updatedAt` 缺失、响应身份不一致）/ 无 `Retry-After` 的 `429`、`503` | 保留检查点，返回原始可重试错误（框架退避） |

- 激活（条件：Artifact Manager 返回 `Prepared`）：调 `ActivateRelease(buildName)`——`200`（切换成功或已是当前版本）→ 执行「成功收口」；可重试错误 → 保持 `release.phase=Prepared` 与检查点：带 `Retry-After` 的 `429` / `503` 返回 `ReconcileResult{RequeueAfter: <Retry-After>}` + `nil`，其余（网络 / 超时 / 响应无法解析 / 响应契约错误）返回原始可重试错误（框架退避），下一轮重新激活；不可重试错误 → 执行「失败收口」。
- 收尾动作（本节定义完整写入字段；字段含义见第四章）：
  - 成功收口（条件：`SubmitRelease` 返回 `200`+`Ready`、`GetRelease` 返回 `Ready` 或 `ActivateRelease` 返回 `200`）：一次 CAS 提交目标 status——`release.phase=Ready`、`release.sourceRepositoryUID`（取 `release.transition.sourceRepositoryUID`）、`release.contentURL`、`release.updatedAt`、`release.transition=nil` 与 `PublishSucceed=True` 条件（reason `ReleaseActivated`）；返回 `ReconcileResult{Requeue: true}` + `nil`，立即处理同目标下一个候选；
  - 失败收口（条件：不可重试失败）：一次 CAS 提交目标 status——`release.phase=Failed`、`release.updatedAt`、`release.transition=nil`（检查点即「在途」判据，收口必须清空，否则会被反复驱动）与 `PublishSucceed=False` 条件（reason `ReleaseFailed`）；失败详情只进日志（`reason=ReleaseFailed`）；终态写入成功后返回零值 + `nil`，不因原始业务失败触发框架重试——发布终局已持久化，该对象随即被发布候选与轮询过滤排除，失败不会阻塞同 Project/OS/Arch 的其他 Build（后续候选由过程仓键 resync 重新入队）。

重启恢复（任意阶段）：

| 崩溃点 | 恢复路径 |
| --- | --- |
| 无检查点且非终态 | 基于最新输入重新判定策略，`excludeSpecs` 去重排序；尚未持久化检查点，不保证决策与崩溃前一致 |
| 检查点已写、`SubmitRelease` 未被接受 | `GetRelease` 返回 `404`，用同一请求重放 |
| `SubmitRelease` 已接受、相位未补齐 | `GetRelease` 返回 `Creating` / `Prepared`，补齐相位后继续等待或激活 |
| 激活已成功、收口写入前 | `GetRelease` 返回 `Ready`，直接成功收口 |
| 失败收口写入前 | `GetRelease` 仍返回 `Failed{retryable=false}`（或不可重试错误码），重新收口 |
| 收口写入结果未知 | 按第七章统一 WriteUnknown 规则确认完整写入意图；未达成则结束本周期、延迟重新调谐，不仅凭相位确认成功或重放旧收口 |
| `Requeue: true` 丢失 | 发布键由过程仓 reconcile 每轮 resync 重新入队 |

约定：

- 发布只写 `status.release.*` 与顶层 `conditions` 中的发布条目，不改动 `status.repository.*`；
- 是否需要发布由 `PublishPolicy.Decide` 决定（控制器不内置该判定；判定为不发布的 RpmRepo 不落任何持久结果，等价于每轮重新判定，因此策略必须是同输入同结果的纯函数）；发布请求摘要不落 `RpmRepo.status`——请求的全部可变输入已冻结在 `release.transition`，摘要在 Artifact Manager 侧计算并用于幂等，控制器不做本地摘要比对，`release.transition` 被外部改动导致的漂移由 AM 的 `409` 兜住并按失败收口；
- 同一 `{project}/{os}/{arch}` 同时只允许一个在途发布（`release.transition` 非空）；在途对象优先于尚未开始的对象，由 `release/` 键串行与「优先处理在途」共同保证；过程仓键与发布键可并行推进；单活动实例与首版不引入超时（在途检查点会一直阻塞该目标的新发布，长期停留由日志与指标暴露、人工处理）见第十章。

### 7.4 队列结果映射

`Sync` 返回 `(ReconcileResult, error)`。业务判定与写入字段以 7.2 / 7.3 为准，本表只规定队列行为：

| 场景 | 返回 |
| --- | --- |
| 7.2 的等待或收敛分支；7.3 全部候选跳过或在途对象阻塞；无状态变化 | 零值 + `nil`。无检查点候选的“跳过”只继续扫描，不提前返回 Sync |
| BuildInfo 可读但未 Completed | 不阻止过程仓推进；只阻止该对象发布，按所在流程等待或继续候选扫描 |
| 过程仓键 RpmRepo NotFound 且 Build 可读 | 零值 + 可重试错误 |
| 依赖读取失败 | 按本章“依赖读取的统一语义”；列表额外遵循 3.1 的完整分页契约 |
| 等待 Artifact Manager 的 Creating | `ReconcileResult{RequeueAfter: pollAfterSeconds}` + `nil`；缺失或非正值默认 5s |
| 过程仓可重试失败、预算未耗尽或响应契约错误 | `ReconcileResult{RequeueAfter: <退避或剩余等待>}` + `nil`，预算与退避规则见第八章 |
| 发布侧临时错误或响应契约错误 | 有效 Retry-After 转为 `RequeueAfter` + `nil`；其余返回零值 + 原始可重试错误 |
| 过程仓提升后仍有候选 Job；发布 Ready 后继续检查后续候选 | `ReconcileResult{Requeue: true}` + `nil` |
| 策略不发布 | 必要的状态清理成功后继续扫描；全部候选处理完返回零值 + `nil` |
| 检查点存在但 Artifact Manager 查询返回 404 | 按检查点重放对应提交，返回值由提交结果决定，不按 404 写业务失败 |
| 过程仓失败、发布失败、无可用版本、输入超限、恢复被清空的失败终局或 Build 中止 | 终态写入成功后返回零值 + `nil`，不返回原始业务错误、不主动请求重入 |
| status 写入 Conflict | `ReconcileResult{RequeueAfter: 1s}` + `nil`，下一周期重新计算 |
| status 写入 Unknown | 按本章 WriteUnknown 确认规则处理，不重放旧决策 |
| 其它 status 写入错误 | 按本章“状态写错误分类”返回；不得被原始业务错误覆盖或误报为成功 |

`err != nil` 时不返回非零 `ReconcileResult`。正常等待不使用 error；显式 `RequeueAfter` 不叠加框架错误退避。依赖读取与 Artifact Manager 的有效 Retry-After 由控制器转换；status 写入的 `429/503` 则保留 `WriteError` 交由框架处理。

## 八、Artifact Manager 契约

接口路径与类型统一见 3.3，Artifact Manager 完整协议见 `artifact-manager.md` 第九章；本节规定响应、幂等与重试语义。控制器不删除仓库或直接读取仓库文件。

提交响应处理：

| 响应 | 含义 | 控制器动作 |
| --- | --- | --- |
| `202` | 首次接受；相同请求正在执行或排队（返回原 `Creating`，不重复入队、不增加 attempt）；相同请求处于可重试 `Failed`（原子增加 attempt、清空旧 Failure、写为 `Creating` 后重新入队） | 保留 `repository.transition`，按 `pollAfterSeconds` 延迟重入 |
| `200` + `Ready` | 相同请求已经 Ready | 直接提升 |
| `200` + `Failed{retryable=false}` | 相同请求处于不可重试 `Failed` | 不可重试失败收口 |
| `409 RepositoryIdentityConflict` | 同 `repositoryUID` 不同请求摘要 | 不可重试失败收口，并记录 `reason=RepositoryCreationFailed` 告警（程序错误或需人工介入） |
| `409 RepositoryDeleting` | 该 `repositoryUID` 的记录正在删除（`Deleting`） | 不可重试失败收口（`FailureInfo.retryable=false`）；删除完成后重放会新建记录，但本设计不用重放等待删除收尾 |
| `410 MaterializationInputExpired` | Manifest、Artifact 或基础仓已过期 | 不可重试失败收口 |
| `422` | `InvalidRepositoryRequest` / `RepositoryUIDMismatch` / `InvalidManifestReference`（请求字段非法、UID 计算不一致、Manifest 引用重复）、`ManifestNotReady`、`BaseRepositoryNotReady`、`ManifestInvalid`、`ManifestContainsNoPackages` | 全部视为不可重试失败收口 |
| `429 RepositoryQueueFull` | 服务端队列已满 | 保留 `repository.transition`，按 `Retry-After` 重入；服务端未接受新执行尝试，因此**不消耗**重试预算 |
| `503 RepositoryStorageUnavailable` | 服务不可用或停机中 | 保留 `repository.transition`，按 `Retry-After` 或指数退避重入；服务端未接受新执行尝试，因此**不消耗**重试预算 |

约定：

- `repositoryUID` 就是幂等键，不额外使用 `Idempotency-Key`；
- 发布依赖 Artifact Manager 保证：同一 buildName 与相同请求摘要幂等，不同摘要返回 409；Prepared 不会自行激活，只由控制器调用激活接口。
- 可重试执行失败的退避间隔为 `d = min(initial × 2^(attempt-1), max)`，施加配置的 jitter；返回 `ReconcileResult{RequeueAfter}`，不叠加框架退避。可重试执行失败受重试预算限制，不可重试错误和输入超限按各自规则直接收口；`Creating` 不设超时，长期停留需人工处理。
- 重试预算以 Artifact Manager 响应的 `attempt` 为**权威计数**（`attempt` 是"服务端已接受的执行尝试次数"，首次提交为 `1`，相同请求处于可重试 `Failed` 被重放时 `+1`，查询与 Ready 重放不递增）；控制器不写任何计数，判定式为 `attempt >= --rpmrepo-materialize-retry-limit + 1` 即预算耗尽；
- 预算边界：`429` / `503` / 网络超时（服务端未接受新尝试）不递增 `attempt`，因此不消耗预算；`404 RepositoryNotFound`（记录不存在）走"用同一请求重放 `SubmitRepository`"的恢复路径，重放会新建记录并把 `attempt` 重置为 `1`，即计数器从零开始；
- 退避锚点：退避窗口以 `RepositoryResponse.updatedAt`（服务端记录最近一次状态变更时间）为锚点；窗口未过时，任何重复入队（`rpmrepos` 轮询 resync、框架重入）都只返回剩余等待时间（`RequeueAfter`）+ `nil`，**不重放** `SubmitRepository`、不写 status、不消耗预算。未落到记录的错误若响应带 `Retry-After` 则按其等待；无 `Retry-After`（例如连接中断、响应无法解析）时以上一次成功 `GetRepository` 响应的 `updatedAt` 为锚点，仍无可用锚点则按初始退避（`--controller-slow-retry-initial-delay`，默认 30s）等待。该比较跨进程，允许由 jitter 覆盖的时钟偏差；偏差只会让重放略微提前或推迟，不影响预算与终局判定；
- 响应契约：`RepositoryResponse.attempt < 1`（首次提交必为 `1` 起）与 `updatedAt` 缺失均视为 Artifact Manager 响应契约错误，**按可重试处理**（与"响应无法解析"同类，服务端恢复后自动继续），不据此推进预算或收口、不写 status、不消耗重试预算，也不得用本地时间臆造退避锚点：
  - 过程仓路径（`SubmitRepository` / `GetRepository`）：按 `RequeueAfter` 表达（无可用锚点时用初始退避 `--controller-slow-retry-initial-delay`，默认 30s），保留 `repository.transition`，不进入 error 路径；
  - 发布路径（`SubmitRelease` / `GetRelease` / `ActivateRelease`）：返回零值 + 原始错误（框架退避），保留 `release.transition` 与相位；
- 只依赖稳定错误码与 `retryable`，不解析 `message`；
- 提交响应里不会出现 `200 + Failed{retryable=true}`：可重试失败被重放时服务端返回 `202` 且 `attempt` +1；`Failed{retryable=true}` 只通过 `GetRepository` 观察（查询不改变 `state` 与 `attempt`）；
- 结果未知一律按"先 GET 确认、不重放不同请求"处理；
- `Ready` 响应必须包含 `contentURL`；服务端在需要客户端轮询的响应中应返回 `pollAfterSeconds`，客户端在该字段缺失或非正值时按 5 秒兜底，不因此判定响应契约错误。该规则统一适用于过程仓与发布流程。响应体可能携带 `rpms`，控制器不解析、不持久化该字段；内容与发布摘要只由 Artifact Manager 服务端持有，不在响应中返回，控制器不做本地摘要比对。
- `GetRepository` 返回 `404 RepositoryNotFound` 表示 Artifact Manager 侧不存在该 `repositoryUID` 记录，控制器按"提交未被接受"用同一请求重放 `SubmitRepository`（与 `artifact-manager.md` §9.3.3 第 2 步"查询或重新提交"一致），不视为不可重试失败。

## 九、条件与时间

条件只允许以下两个 type；condition type 必须区分过程仓错误与正式发布错误（对齐 `docs/zh/design/data-models.md`）：

| type | 含义 | True 时机 | False 时机 |
| --- | --- | --- | --- |
| `RepositoryReady` | **最近一次批次的结果**：`True` = 最近一次批次成功提升；`False/reason=RepositoryCreationFailed` = 最近一次批次失败收口；未设置 = 尚无批次结果（含仅继承基线、从未物化的对象） | 每次成功提升当前版本 | 过程仓失败收口（重试耗尽或不可重试失败） |
| `PublishSucceed` | 正式发布是否成功 | 发布 `Ready` 后写入 | 发布不可重试失败（`409` / `422` / `Failed{retryable=false}`）、过程仓失败收口（重试耗尽 / 不可重试失败）时的终局登记（由 §7.2 失败收口同次写入），"无可用版本"的发布失败终局（§7.2 第 5 步），首个候选自身超限的发布失败终局（§7.2 第 2 步），已放弃批次被外部清空后的重新收口（§7.2 第 1 步），或同名 Build 被中止（reason `BuildAborted`，§7.2 入口与 §7.3 中止收口） |

condition merge helper：

```go
func MergeCondition(conditions []metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string, observedGeneration int64, now time.Time) ([]metav1.Condition, bool)
```

- `now` 由调用方传入本轮 `Sync` 唯一一次 `clock.Now()` 的结果；helper 内部不读取系统时间或时钟。首次插入条件或 `status` 变化时，将 `lastTransitionTime` 设置为 `metav1.NewTime(now.UTC())`；仅 reason / message / observedGeneration 变化时保留原转换时间；
- `observedGeneration` 取 `RpmRepo.metadata.generation`；结果按 `type` 排序；所有字段无变化时返回 `changed=false` 并跳过 `/status` 写入，不能仅因传入的 `now` 不同而更新条件；
- Type / Reason 常量复用 `api/ebs/v1/types_status.go` 中的 `RpmRepoCondition*` / `RpmRepoReason*`；Status 复用 `metav1.ConditionTrue` / `ConditionFalse`，控制器不重复定义。
- Type、Status、Reason 的合法组合统一见 `data-models.md` 第五章条件表；过程仓成功使用 `RepositoryCreated`，正式发布成功使用 `ReleaseActivated`。动态错误详情只进日志。
- 时间来源：每次 `Sync` 只调用一次 `clock.Now()`，写入统一为 `metav1.NewTime(now.UTC())`；`status.repository.updatedAt` 与 `status.release.updatedAt` 仅在对应字段有效写入时更新。

中止只表示控制器停止推进，不能取消 Artifact Manager 已接受的操作，也不会回滚已经发生的激活；Build 保留 `Aborted` 作为用户中止记录，RpmRepo 以 `Failed + BuildAborted` 表达原因，中止不计入发布失败指标。

## 十、并发与一致性

- 队列保证同一 key 串行；不同 Build 的过程仓可并行，同一对象的过程仓键与发布键也可能并发，靠 status CAS 协调。Conflict 与 Unknown 的恢复以第七章为准。
- 同一 `{project}/{os}/{arch}` 的发布由 release 键串行驱动，同一时刻只允许一个在途发布；在途优先及候选跳过规则见 7.3。不同目标组可并发。
- 批次检查点冻结 inputs、baseRepositoryUID 与 repositoryUID，发布检查点冻结 sourceRepositoryUID 与 excludeSpecs；只在明确的收口路径清理，恢复请求不得改变输入。Artifact Manager 幂等契约见第八章。
- 过程仓提升将版本指针、去重排序后的 sourceJobUIDs、条件与检查点清理合并为一次 CAS，避免出现部分提升。
- 首版仅允许单活动实例，不保证多副本正确性；故障切换依靠持久化检查点、CAS 与 Artifact Manager 幂等恢复。

## 十一、可观测性

框架日志行已包含 `controller` / `key` / `result` / `duration` / `error`（部分分支还含 `requeue` / `requeue-after` / `panic`）；本控制器在其之上补充：`reason`、`repositoryUID`、Artifact Manager 响应里的 `attempt`、本次退避时长、Project、Build name、本批输入 Job UID、Artifact Manager 响应状态与稳定错误码。不记录 Artifact Manager 的内部路径与凭据。

日志 reason 取值固定（新增日志不改变任何判定与状态写入）：

| reason | 触发点 |
| --- | --- |
| `BuildInfoNotReady` | 同名 `BuildInfo` NotFound，或正式发布门禁检查时 `phase != Completed`；不用于阻止未完成 BuildInfo 的过程仓推进 |
| `BuildMissing` | 过程仓键遇到孤儿 RpmRepo（同名 Build NotFound） |
| `ReleaseGroupBuildMissing` | 发布流程读取选中对象的同名 Build 时 NotFound，跳过该对象 |
| `InputLabelMismatch` | 输入 Job 的 `ebs.io/spec-name` 缺失或为空，或 `ebs.io/target-os` / `ebs.io/target-arch` 缺失或取值与 `Build.spec.buildTarget` 不一致（`ebs.io/build-name` 已由服务端选择器过滤，不产生该告警） |
| `InputManifestMissing` / `InputManifestNotReady` / `InputManifestFailed` | 输入 manifest 为 `404` / `Open` / `Completing` / `Failed` |
| `InputObjectMissing` | 候选校验期间 `Job` 对象 NotFound |
| `InputTooLarge` | 排序最前的候选自身的物化输入超过 `--rpmrepo-max-input-bytes`，本批不形成并转入发布失败终局 |
| `MultipleReleaseInFlight` | 同一 `{project}/{os}/{arch}` 的候选集内出现多个在途发布 |
| `ReleasePolicySkipped` | 发布策略判定该候选不发布 |
| `BuildAborted` | 过程仓流程或发布流程读到同名 `Build.status.phase=Aborted`，把 `release.phase` 收口为 `Failed`，条件 reason 为 `BuildAborted` |
| `RpmRepoLabelMismatch` | 选中对象的 `ebs.io/target-os` / `ebs.io/target-arch` 标签与同名 Build 的 `spec.buildTarget` 不一致，跳过该对象 |

失败收口日志复用条件 reason：`RepositoryCreationFailed`（过程仓失败或首个候选超限）、`ReleaseFailed`（正式发布失败）、`NoPublishableArtifacts`（无可发布产物）、`BuildAborted`（Build 中止），重试日志附 `attempt` 与本次退避时长。

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

过程仓：`rpmrepo_controller_repository_ready_total` 在成功收口写入（`RepositoryReady=True/reason=RepositoryCreated`）时计一次；`rpmrepo_controller_repository_failed_total` 在每次**过程仓失败收口**（重试耗尽、不可重试失败，或 `GetRepository` 返回 `Deleting`）时计一次（不随轮次累加），并包含首个候选自身超限而直接写入的发布失败终局（该终局只写发布侧，见 §7.2 第 2 步）；**不含**已放弃批次被外部清空后的重新收口；`rpmrepo_controller_materialize_retries_total` 在每次因可重试失败而重放 `SubmitRepository` 时计一次——其中 `200 Failed{retryable=true}` 的重放与 Artifact Manager 响应里 `attempt` 的递增同步，`429` / `503` / 网络 / 超时 / 未解析这类未落到记录的重放不递增 `attempt`、仍计一次。

输入与归属：`rpmrepo_controller_build_missing_total` 统计过程仓键在 RpmRepo 已存在、但同名 Build NotFound 导致本键收敛（结束本轮、不写 status）的轮次；发布流程因选中对象的同名 Build 缺失（`reason=ReleaseGroupBuildMissing`）或标签与 Build 目标不一致（`reason=RpmRepoLabelMismatch`）而跳过对象只记告警日志、**不计入本计数**（由同一条告警定位，不重复计数），`Build` 读取的其它失败按可重试 / 永久错误分类、不计入本计数。输入侧被跳过的 Job（manifest 为 `Failed` / 404 / `Open` / `Completing`、候选校验期间 `Job` 对象 NotFound，或 `ebs.io/spec-name` 缺失/为空、`ebs.io/target-os` / `ebs.io/target-arch` 缺失或取值不一致——`ebs.io/build-name` 已由服务端选择器过滤，不属该告警集）一律不写 status、不加计数，只按上表输出 `reason` 告警日志；其中 `Open` / `Completing` 属未就绪输入、阻塞发布触发；首个候选自身超限不属该跳过路径，按 §7.2 第 2 步直接收口为发布失败。

发布：`rpmrepo_controller_release_ready_total` 在发布成功收口写入（`release.phase=Ready`）时计一次；`rpmrepo_controller_release_failed_total` 统计发布失败终局——发布流程自身的失败收口（含 `GetRelease` 返回 `Deleting`），以及"无可用版本"（`repository.sourceJobUIDs` 为空，本对象从未产出任何版本，含仅预置了继承基线的对象）时由 §7.2 第 5 步直接写入的发布失败终局，**不含**过程仓失败收口同次写入的发布失败终局登记（那一次计入 `rpmrepo_controller_repository_failed_total`），**也不含**首个候选自身超限的发布失败终局（同样计入 `rpmrepo_controller_repository_failed_total`），**更不含**同名 Build 被中止的 Failed 中止终局（它是用户中止而非发布失败，只由相位与 `reason=BuildAborted` 日志体现）。

状态写入：`rpmrepo_controller_status_update_conflicts_total` 统计 `/status` 写入因乐观并发被拒（409，结束本周期并延迟重新入队）的轮次；`rpmrepo_controller_status_update_unknown_total` 统计写入结果未知（超时 / 连接中断 / 响应无法解析，按第七章 GET 确认写入意图）的轮次；两者沿用 job / runner / build 控制器体例。

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

## 十三、上游契约

1. **Build Controller**：所有构建类型均创建与 Build 同项目、同名的 RpmRepo；single 创建时设置 `release.phase=Skipped`，继承基线保存到 `repository.repositoryUID/contentURL`，并写入与 `Build.spec.buildTarget` 一致的 `ebs.io/target-os` / `ebs.io/target-arch` 标签。
2. **BuildInfo Controller**：创建 Job 时写入 `ebs.io/build-name`、`ebs.io/spec-name`、`ebs.io/target-os`、`ebs.io/target-arch`；`BuildInfo.status.phase=Completed` 表示不再创建 Job，所有 Job 均已终态且结果不再变化。发布复核与“无可发布产物”判定依赖此保证，过程仓推进不以 Completed 为前提。
3. **Runner**：需要归档的 Job 在 Manifest 成功封账后才写 `Succeeded`；无需归档的 Job 可以没有 Manifest。每个 Job 只能成功封账一次，封账后内容不可变。
4. **Artifact Manager**：提供检查点恢复所需的幂等提交、显式激活及 attempt 计数保证，具体契约统一见第八章。

## 十四、测试计划

### 14.1 单元测试

- Job 中止与成功并发：仅已持久化为 Succeeded 的 Job 可入选；Aborted + Completed Manifest 不查询产物、不入选、不加入 `sourceJobUIDs`，不阻止扫描其它成功 Job，也不写 `BuildAborted`。

- Skipped：轮询与发布列表选择器均包含 `status.release.phase!=Skipped`；残留队列键直接调谐时只读取 RpmRepo 即结束，不读取 Build/BuildInfo/Job、不调用 Artifact Manager、不写 status、不入队发布键。发布列表的旧快照在最新 GET 返回 Skipped 时跳过，不进行激活。覆盖有基线、无基线、Build 已 Aborted、重启与重复入队，断言版本指针不变、条件与指标不变。


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
- 首个候选超限：单候选自身输入为上限 1.5 倍时，断言不写 `repository.transition`、不调用 Artifact Manager、一次 CAS 写 `release.phase=Failed` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False`（reason `RepositoryCreationFailed`）、计一次 `rpmrepo_controller_repository_failed_total`（且 `release_failed_total` 不增）、输出一次 `reason=InputTooLarge`、返回零值 + `nil`，且对象此后被轮询与发布候选排除。
- 字节上限累加：上限 X、候选 A=0.6X 且 B=0.6X 时断言首批为 `[A]`、B 留待下一批；A=0.6X、B=0.3X、C=0.3X 时断言首批为 `[A,B]`、C 留待下一批；B 留待后在下一批成为首个候选且自身超限时断言走同一发布失败终局（不截断、不跳过）。
- 计数口径：manifest 同时包含 RPM 与日志时，断言日志不参与累计（口径与 Artifact Manager 的物化输入一致）。
- 零输入与未列 422：物化输入为 0 的候选断言正常参与批次（只占 Job 数上限）；Artifact Manager 返回 `422 ManifestContainsNoPackages` / `ManifestInvalid` 时断言按不可重试失败收口（`RepositoryReady=False` + `release.phase=Failed` + `PublishSucceed=False`，`repository.transition` 原样保留）。

**过程仓：在途结果、重试预算与收口**

- `repository.transition` 路径分流：非空时断言不重新选批、不重算 `repositoryUID`，只先调用 `GetRepository`。
- `GetRepository` 响应分流：`200 Ready` 提升；`200 Creating` 断言 `RequeueAfter` 等于 `pollAfterSeconds`（缺失或 ≤ 0 时缺省 5s）且不写 status；`200 Failed{retryable=true}` 用完全相同的请求重放；`200 Failed{retryable=false}` / `200 Deleting` / `409 RepositoryIdentityConflict` / `409 RepositoryDeleting` / `410 MaterializationInputExpired` / `422 ManifestNotReady`|`BaseRepositoryNotReady` 失败收口（写入内容见「收口写入与保留」条，`Deleting` 与 `BaseRepositoryNotReady` 额外断言输出 `reason=RepositoryCreationFailed` 告警）；`404` 用完全相同的请求重放 `SubmitRepository`（断言不写失败条件、不重新选批、不重算 `repositoryUID`）；响应未知先 GET 比对再决定。
- 提交响应契约：断言 `SubmitRepository` **不返回** `200 + Failed{retryable=true}`——可重试失败被重放时返回 `202` 且 `attempt` +1；`Failed{retryable=true}` 只能通过 `GetRepository` 观察。
- 重试预算与退避：可重试失败断言**重试期间零 status 写入**、计一次 `rpmrepo_controller_materialize_retries_total`，返回 `ReconcileResult{RequeueAfter: <退避>}` + `nil`，且不与框架退避叠加；`200 Failed{retryable=true}` 按响应 `attempt` 判定——`attempt < --rpmrepo-materialize-retry-limit + 1` 时重放、`attempt >= limit + 1` 时直接失败收口；`429` / `503` / 网络 / 超时 / 未解析断言**不消耗预算**（`attempt` 不变）且按 `Retry-After`（有则优先）/ 退避重放；**退避窗口**以响应 `updatedAt` 为锚点——同一窗口内连续多轮轮询 resync 断言 `attempt` 不变、零 status 写入、不调用 `SubmitRepository`、只返回剩余等待；无 `Retry-After` 且无可用锚点时按初始退避（`--controller-slow-retry-initial-delay`，30s）等待；响应 `attempt < 1` 或 `updatedAt` 缺失断言按响应契约错误（**可重试**）处理：过程仓路径返回 `ReconcileResult{RequeueAfter: <初始退避或剩余等待>}` + `nil`、不写 status、不推进预算也不收口、不进入 error 路径，发布路径返回零值 + 原始错误（框架退避）并保留检查点与相位；不可重试失败断言不重试、不消耗预算、直接失败收口；重启后预算按 Artifact Manager 记录的 `attempt` 继续且仍需遵守剩余退避窗口。
- 字段级 `422` 收敛：`SubmitRepository` 返回 `422 InvalidRepositoryRequest` / `RepositoryUIDMismatch` / `InvalidManifestReference` 时断言按不可重试失败收口（`repository.transition` 原样保留、`RepositoryReady=False`、`release.phase=Failed`、`PublishSucceed=False`、计一次 `repository_failed_total`），不再返回 `controller.NewPermanentError`。
- 已放弃批次恢复：预置 `repository.transition` 非空 + `RepositoryReady=False/reason=RepositoryCreationFailed` + `release` 为空，断言一次 CAS 重写发布终局（`release.phase=Failed` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False/reason=RepositoryCreationFailed`）、不写 `repository.*`、不调用 Artifact Manager、`repository_failed_total` 不增、输出 `reason=RepositoryCreationFailed` 告警、返回零值 + `nil`，且对象此后被轮询与发布候选排除。
- 收口写入与保留：成功收口同一次 `/status` 写入包含 `repository.contentURL` 与 `repository.repositoryUID`（分别取 Ready 响应与本批 `transition.repositoryUID`）、`repository.sourceJobUIDs`、`repository.transition=nil`、`repository.updatedAt` 与 `RepositoryReady=True/reason=RepositoryCreated`；失败收口同一次写入包含 `repository.transition` **原样保留**（`inputs` / `baseRepositoryUID` / `repositoryUID` 与失败时刻一致）、`repository.updatedAt`、`RepositoryReady=False/reason=RepositoryCreationFailed`，以及发布终局 `release.phase=Failed`、`release.transition=nil`、`release.updatedAt` 与 `PublishSucceed=False/reason=RepositoryCreationFailed`（断言全程不调用 `SubmitRelease` / `ActivateRelease`、不额外入队发布键），并计一次 `rpmrepo_controller_repository_failed_total`；两条路径都不得清空 `repository.contentURL` / `repositoryUID` 等版本字段。
- `repository.updatedAt` 更新时机：断言只在提交批次、失败收口与提升版本时更新，重试期间不更新（与"重试零 status 写入"一致）。

**入口与依赖读取**

- 入口守卫：`RpmRepo` 缺失时断言返回可重试错误、不调用 Artifact Manager、不写 status；同名 RpmRepo 已存在时断言按普通对象继续推进。`Build` 不可用时——NotFound 且 RpmRepo 不存在断言返回零值 + `nil`、不写 status、不调用 Artifact Manager、不入队发布键、不删除任何对象且 `rpmrepo_controller_build_missing_total` 不增；NotFound 且 RpmRepo 存在断言返回零值 + `nil`、输出 `reason=BuildMissing` 告警日志、该计数每次 +1、`status`（含 `repository.*` 与 `release.*`）不变、不调用 Artifact Manager、不入队发布键、不删除孤儿对象，并在下一轮轮询重复同一收敛。
- 依赖读取的统一语义（通用分类只在此断言）：`BuildInfo` NotFound 时，过程仓键断言返回零值 + `nil`；发布键断言跳过当前无检查点候选并继续扫描，全部候选均跳过后才返回零值 + `nil`。两者均不写该对象 status、不调用该对象的 Artifact Manager 接口、不为该对象入队发布键、不产生终局或条件，输出 `reason=BuildInfoNotReady` 日志，且 `rpmrepo_controller_build_missing_total` 不增；候选校验期间 `Job` 对象 NotFound 断言跳过该输入、不计候选、不阻塞发布触发、不写 status、输出 `reason=InputObjectMissing` 日志且不计数；`Build` / `BuildInfo` / `Job` 读取返回 `5xx` / 超时 / 无重试提示的 `429`、`503` 断言可重试（零值 + 原始错误）、不写 status、不推进，带 `apierrors.SuggestsClientDelay` 提示的 `429` / `503` 断言返回 `ReconcileResult{RequeueAfter: <提示值>}` + `nil`、不写 status、不推进；返回 `401` / `403` / `422` 或响应身份契约错误断言 `controller.NewPermanentError`；对照断言 Artifact Manager 的 `GetRepository` / `GetRelease` 返回 `404` 仍按各自状态机的"同请求重放"规则，不受本章统一读取语义影响。

**写错误分类（apiserver 侧）**

- status 写入 Rejected / 409 断言立即返回 `ReconcileResult{RequeueAfter: 1s}` + `nil`，本周期不再次 GET 重算或 PUT、不继续提交或激活，下一周期重新读取并计算，不用旧对象重放；status 写入返回字段非法（Rejected / 422）断言返回 `controller.NewPermanentError` 且不再写 status；status 写入返回 `429` / `503` 断言返回可重试错误并保留 `Retry-After`（由框架的 `WriteError` 路径识别；**与带 `Retry-After` 的依赖读取、Artifact Manager `429` / `503` 不同**：后者由控制器换算为 `RequeueAfter` 且不消耗重试预算，见「重试预算与退避」条）。

**发布**

- 发布触发入队：**RpmRepo 存在、未删除且非终态，同名 Build 未中止时**，`status.release` 非空则入口即入队发布键并返回零值 + `nil`（不推进过程仓字段、不调用 Artifact Manager、不写 status）；`release` 为空且 `BuildInfo=Completed`、`repository.transition==nil`、本轮候选扫描为空、无未就绪输入、`repository.sourceJobUIDs` 非空（本对象已产出可发布版本）时才走首次入队；`sourceJobUIDs` 为空时即使 `repositoryUID` / `contentURL` 被预置了继承基线也不入队；候选非空、`repository.transition` 非空或存在未就绪输入时不入队（等待）；符合上述 RpmRepo 前置条件且 Build 已 `Aborted` 时两条分支都不走，改为直接写 `release.phase=Failed` 中止终局（见「Build 被中止」断言）；
- 无可用版本的发布失败终局：`release` 为空且 `BuildInfo=Completed`、`repository.transition==nil`、候选扫描为空、无未就绪输入，但 `repository.sourceJobUIDs` 为空时（覆盖无基线与仅有继承基线两种形态），断言**只发生一次** CAS 写入 `release.phase=Failed` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False/reason=NoPublishableArtifacts`，计一次 `rpmrepo_controller_release_failed_total`；断言不写 `repository.*`、不调用 Artifact Manager（含不调用 `GetRepository` / `SubmitRepository` / `SubmitRelease`）、不入队发布键，返回零值 + `nil`，且该对象之后被轮询过滤与发布候选同时排除；
- 发布候选查询与 Build 校验：断言 `ListRpmRepos` 透传的 `FieldSelector` 恰为 `status.release.phase!=Ready,status.release.phase!=Failed,status.release.phase!=Skipped`、`LabelSelector` 恰为 `ebs.io/target-os=<os>,ebs.io/target-arch=<arch>`（取自发布键，同 arch 不同 os 的对象互不阻塞、也不会出现在本键候选集中）；断言按扫描顺序读取当前候选的 Build，允许同轮检查多个对象，但最多推进一个对象的发布；选中对象的同名 Build `NotFound` 时断言跳过该对象、输出 `reason=ReleaseGroupBuildMissing` 告警且不重复计数（同名 Build 缺失由过程仓键的 `rpmrepo_controller_build_missing_total` 暴露）、其它对象照常推进；选中对象的标签与 `Build.spec.buildTarget` 不一致时断言跳过该对象、输出 `reason=RpmRepoLabelMismatch` 告警、不计数、不调用 Artifact Manager；Build 读取返回 `5xx` / 超时断言返回可重试错误且本轮不推进；构造一个不带目标标签的 RpmRepo（存量形态）断言它不进候选、不产生发布动作；
- 发布策略：注入 stub 策略断言 `Publish=false` 的候选不调发布接口、不写 condition、输出 `reason=ReleasePolicySkipped` 告警，仅在存在残留 `release` 时写一次 `release=nil`，并继续评估下一候选（全部不发布 → 零值 + `nil`）；`DefaultPublishPolicy` 在 `Build.spec.buildTarget.publishFlag=false` 时返回 `Publish=false`，其余返回 `Publish=true`（不特判 `buildType`）；`ExcludeSpecs` 去重并按字典序排序；策略返回 error 时返回可重试错误且不提交发布；
- 发布候选过滤：`repository.transition` 非空、`repository.sourceJobUIDs` 为空（无论 `repositoryUID` / `contentURL` 是否被继承基线预置）、`release.phase ∈ {Ready, Failed, Skipped}`、`metadata.deletionTimestamp` 非空（非在途对象）的对象不进入候选、不调用 `SubmitRelease`；
- 发布状态机（无检查点）：`SubmitRelease` 的 `202 Creating` / `202 Prepared` / `200 Ready` / `409` / `422` / `429` / `503` 分支；断言一次 CAS 写检查点（`release.phase=Pending`、`release.transition.sourceRepositoryUID` / `excludeSpecs`、`release.updatedAt`）后才调用 `SubmitRelease`；`202` 后按响应体写 `release.phase=Creating` / `Prepared`；可重试错误保留检查点并把相位留在 `Pending`；
- 发布状态机（有检查点）：`GetRelease` 的 `Creating`（补齐相位并延迟重入）/ `Prepared`（补齐相位后 `ActivateRelease`）/ `Ready`（直接收口）/ `Failed{retryable=true}`（重放同一请求）/ `Failed{retryable=false}`（失败收口）/ `Deleting`（失败收口并输出 `reason=ReleaseFailed` 告警）/ `404`（重放同一请求）七条分支；断言有检查点时不再重新调用策略、不重算 `sourceRepositoryUID` / `excludeSpecs`；
- Retry-After 换算：Artifact Manager 发布提交 / 有检查点查询 / 激活返回带 `Retry-After` 的 `429` / `503` 时，断言保留检查点与相位、返回 `ReconcileResult{RequeueAfter: <Retry-After>}` + `nil`、不消耗重试预算；无 `Retry-After` 时断言返回原始可重试错误；
- 发布收口写入：成功收口同一次 `/status` 写入包含 `release.phase=Ready`、`sourceRepositoryUID` / `contentURL` / `updatedAt`、`release.transition=nil` 与 `PublishSucceed=True/reason=ReleaseActivated`，并返回 `ReconcileResult{Requeue: true}` + `nil`；失败收口同一次写入包含 `release.phase=Failed`、`release.updatedAt`、`release.transition=nil` 与 `PublishSucceed=False/reason=ReleaseFailed`（失败详情不落 status），断言写入成功后返回**零值 + `nil`**、不返回 `Requeue`，且 BaseController 不因该业务失败调用限速重试或慢速重入；注入收口写入 Conflict、Unknown 和其它写错误时，断言按 status 写错误分类处理，不返回原始发布错误；
- 构建中生成过程仓：`BuildInfo.status.phase=Processing` 且存在成功 Job 的封账 manifest 时，断言正常选批、提交并在 Ready 后提升版本；有在途批次时正常恢复。没有候选 Job 时只等待，不启动正式发布、不因 `sourceJobUIDs` 为空写“无可用版本”失败；批次重试耗尽或不可重试失败仍正常失败收口，不等待 BuildInfo Completed；
- 等待与跳过返回形态：同名 BuildInfo NotFound 时过程仓键等待；无检查点发布候选的 BuildInfo NotFound、未 Completed 或候选删除中时，断言不写该对象 status、不调用该对象发布接口，并继续扫描后续候选，全部跳过才返回零值 + `nil`；在途对象删除中时结束本轮，不越过它启动新发布；
- 发布候选与首次发布完整复核分别覆盖 BuildInfo 错误分类：网络、超时、5xx 返回可重试错误（有效 Retry-After 按统一规则处理），权限、参数及响应契约错误返回 `PermanentError`，context 取消返回 `ctx.Err()`；均不写 status、不调用 Artifact Manager、不执行发布策略，且不能误记为正常的 `BuildInfoNotReady`；
- 基础仓不匹配：Artifact Manager 返回 `422 BaseRepositoryNotReady`（基础仓不存在、非 `Ready`，或 Project / OS / 架构与请求不一致）时断言按不可重试失败收口（`RepositoryReady=False` + `release.phase=Failed` + `PublishSucceed=False`、`repository.transition` 原样保留）、输出 `reason=RepositoryCreationFailed` 告警，且本控制器不自行做该一致性校验；
- Build 被中止：RpmRepo 存在、未删除且非终态，同名 `Build.status.phase=Aborted` 时，分两组断言——① 过程仓键：断言不写 `repository.transition`、不调用 Artifact Manager、不入队发布键，只发生一次 CAS 写 `release.phase=Failed` + `release.transition=nil` + `release.updatedAt` + `PublishSucceed=False/reason=BuildAborted`，输出 `reason=BuildAborted` 日志，返回零值 + `nil`；② 发布键：在途对象（`release.transition` 非空）与发布候选对象都断言同一次 CAS 收口、不调用 `GetRelease` / `SubmitRelease` / `ActivateRelease`、不做策略判定（即中止判定先于在途恢复：在途对象不得进入「有检查点」流程）；两组都断言 `repository.*` 不被改动（在途 `repository.transition` 原样保留）、`rpmrepo_controller_release_failed_total` 不增、返回零值 + `nil`；早中止（`BuildInfo` 未完成、`repository.sourceJobUIDs` 为空）的对象由过程仓键的同一分支收口，不依赖发布键的候选扫描；
- 发布幂等：按冻结的 `release.transition` 重放同一请求不产生第二个 release 提交（断言 Fake 的调用次数与最后请求体一致）；`release.transition` 被改动导致重放不同请求时断言按 AM 的 `409` 失败收口（`release.phase=Failed` + `PublishSucceed=False`）。

**条件、时间与观测**

- 条件与时间：`RepositoryReady` / `PublishSucceed` 的 reason 固定短语；`repository.updatedAt` 与 `release.updatedAt` 仅在对应字段有效写入时更新，且重试期间不更新 `repository.updatedAt`（`observedGeneration`、FakeClock / UTC 与 `MergeCondition` 的断言见「时间、退避与条件纯函数」条）。
- 指标：断言 §11 列出的指标均已注册；过程仓失败收口（含首个候选自身超限的发布失败终局）、每次重放、发布失败终局（发布不可重试失败与"无可用版本"终局）分别使 `rpmrepo_controller_repository_failed_total` / `rpmrepo_controller_materialize_retries_total` / `rpmrepo_controller_release_failed_total` 各递增一次。

- status 写错误 Outcome：覆盖 NotSent 的本地校验错误与临时网络错误，分别断言 PermanentError 与框架重试；覆盖 Rejected 的 404（结束）、408/429/5xx（重试）、400/401/403/422（PermanentError），均不执行 Unknown 确认 GET、不额外写业务终态。检查点或相位写入未确认时，断言不继续 SubmitRepository、SubmitRelease 或 ActivateRelease；Manager context 取消时不发起后续请求。

### 14.2 集成测试

- 首版生成：无基础仓时由多个 Completed Manifest 生成自身的首个可读版本——同一轮 CAS 写 `repository.repositoryUID` / `repository.contentURL`、把本批 Job UID 并入 `repository.sourceJobUIDs`、`repository.transition=nil` 与 `RepositoryReady=True/reason=RepositoryCreated`；发布收口同一次写入 `release.phase=Ready` 与 `PublishSucceed=True`；
- 多批推进：提升成功后仍有候选 Job 时立即重入，并以前一版本为基础仓；
- WriteUnknown 确认：覆盖同 UID 且全部目标字段一致、仅相位或 repositoryUID 一致但其它目标字段不一致、resourceVersion 已改变、resourceVersion 未变但目标未达成、NotFound、UID 不同、删除中、其它终态与 GET 失败；仅完整意图一致确认成功，后续使用最新对象；未达成时断言本轮无第二次 PUT，下一周期重新计算目标。并发写入 Aborted 或推进新版本后，断言不会被旧决策覆盖；
- 列表完整分页：分别覆盖 `ListJobs` / `ListRpmRepos` 的多页聚合、空页带 Continue、短页带 Continue、首页即空、Limit 默认值与自定义值、调用方 Continue 被清空且原参数未变；断言每页选择器与项目作用域不变。第二页失败、410、解码失败或 context 取消时返回 nil 集合与错误，下一次调用从首页开始；重复游标终止扫描。后续页含未消费 Job 或在途发布时不得遗漏，第一页成功但后续页失败不得触发任何物化、发布或 status 写入；
- 首次发布完整复核：发布键选中的对象即使满足粗筛条件，只要还有未消费 Job（含后续分页中的 Job）或 `Open` / `Completing` manifest，就不得写发布检查点或提交发布；任意页读取失败不得按空候选处理。有未消费候选时断言入队过程仓键；扫描期间过程仓版本变化时断言发布 CAS 冲突并重新调谐，不刷新 resourceVersion 重放。已有发布检查点时断言恢复原检查点、不重新选版本或调用发布策略；
- 中止入口保护：过程仓键与发布键分别覆盖 RpmRepo 不存在、删除中及 `Ready/Failed/Skipped` 已终态，断言不执行中止写入、不改变时间或条件、不调用 Artifact Manager；终态与删除中对象不读取 Build。过程仓键对象缺失时仅按 Build 是否存在决定收敛或重试，不创建对象；重复处理已 Failed 对象不得再次写入。
- 候选跳过不阻塞：按时间排序构造多个无检查点候选，首个分别为 Build 缺失、标签不匹配、BuildInfo 未就绪、有未消费 Job 或 manifest 未就绪，后一个已就绪；断言同轮跳过前者并推进后者，不记录持久跳过标记。全部跳过时返回零值 + nil；临时读取错误或 PermanentError 不继续扫描；首个为异常在途对象时保留检查点并阻塞新发布。
- 幂等：相同 UID 相同请求并发提交只产生一个版本；相同 UID 不同摘要按 `409` 收口；
- 失败与保留：失败收口后旧版本仍可读（`repository.contentURL` 与 `repository.repositoryUID` 不变，`repository.transition` 原样保留为失败批次）；
- 账本：`repository.sourceJobUIDs` 去重排序、只增不减；
- 过程仓重启（逐崩溃窗口）：① 批次选定后未写 `repository.transition` → 重启后按确定性排序重选同一批、`repositoryUID` 一致；② `repository.transition` 写入结果未知后重启 → 不假设仍持有旧写入意图；读取最新对象，有持久化检查点则按检查点恢复，无检查点则基于最新候选与基线重新选批；③ `repository.transition` 已写、`SubmitRepository` 未被接受 → `GetRepository` 返回 `404` 时用同一请求重放，不得失败收口、不得丢批次；④ 提交已受理、物化中 → `GetRepository` 返回 `Creating` 时等待，不重复提交；⑤ `Ready` 后、提升 CAS 前 → 重启后提升，`repositoryUID` 与崩溃前一致；⑥ 提升 CAS 结果未知后重启 → 以最新对象恢复：已消费 Job 不再选批；仍有在途检查点则重新查询 Artifact Manager 并计算提升目标，不替换 resourceVersion 重放旧提升；⑦ 提升后、发布键入队前 → 过程仓 resync 重新入队发布键；⑧ 重试期间重启 → 预算与退避都不丢：重启后仍遵守剩余退避窗口（锚点为 Artifact Manager 记录的 `updatedAt`），窗口过后由响应的 `attempt` 决定是否继续重试；重启不重置计数（除非该记录不存在、重放新建了记录）；⑨ 失败收口写入结果未知后重启 → 最新对象已终态则结束，否则按最新检查点与 Artifact Manager 结果重新调谐；进程未重启且仍持有写入意图时，按第七章完整字段规则确认，未达成则延迟重新入队；全部窗口均不产生分叉版本，也不丢批次、不重复消耗重试预算。
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
- Build 中止端到端：同一 `{project}/{os}/{arch}` 的对象在发布在途或待发布时对应 Build 被中止（`POST …/builds/{name}/abort`）——断言该对象一次 CAS 收敛为 `release.phase=Failed` 且 `release.transition` 清空、`PublishSucceed=False/reason=BuildAborted`，`repository.*` 不变、日志含 `reason=BuildAborted`；收敛后该对象不再出现在轮询与发布候选中，同目标其它候选照常发布，且 Artifact Manager 侧不再收到该对象的任何提交或激活调用；
- 孤儿 RpmRepo：删除同名 Build 但保留 RpmRepo 时，断言控制器不再推进该对象（`status` 不变、不调用 Artifact Manager、不入队发布键）、输出 `reason=BuildMissing` 告警并递增 `rpmrepo_controller_build_missing_total`；删除该 Build 的同时 RpmRepo 也已不存在时断言不计数、不产生任何动作；
- Artifact Manager 侧记录被删除：在途 `GetRepository` 返回 `200 Deleting` 时断言一次 CAS 失败收口（`RepositoryReady=False` + `release.phase=Failed` + `PublishSucceed=False`）并输出 `reason=RepositoryCreationFailed` 告警；`GetRelease` 返回 `Deleting` 时断言发布失败收口并输出 `reason=ReleaseFailed` 告警；
- 运行兜底：外部清空 `release`（`repository.transition` 保留 + `RepositoryReady=False`）后，断言对象在下一轮轮询重新收敛为发布失败终局、不再零进展；`RepositoryUIDMismatch` 端到端断言对象按不可重试失败收口、Build 收敛 `Failed/publish`，而非持续空转。
- 永久错误仅日志暴露：撤销控制器读取权限（或注入 `401` / `403`）后断言对象不丢、`RpmRepo.status` 不变、每轮仅产生 `result=permanent-error` 日志且不新增计数；恢复权限后断言下一轮自动继续推进（自愈）。
- 发布重启（逐崩溃窗口）：① 策略判定后未写检查点 → 重启后重新判定得到相同 `excludeSpecs`，不产生第二个 release；② 检查点已写、`SubmitRelease` 未被接受 → `GetRelease` 返回 `404` 时用同一请求重放；③ 提交已受理、相位未补齐 → `GetRelease` 返回 `Creating` / `Prepared` 时补齐相位后继续等待或激活；④ 激活成功后收口写入前 → `GetRelease` 返回 `Ready` 时直接成功收口；⑤ 失败收口写入前 → `GetRelease` 仍不可重试失败时重新收口；⑥ 收口写入结果未知后重启 → 基于最新对象恢复，终态或删除中则结束，否则重新读取依赖与发布结果计算决策；不沿用旧收口目标；⑦ `Requeue: true` 丢失 → 发布键由过程仓 resync 重新入队；全部窗口均不产生第二个 release 版本。

验收场景沿用 `docs/zh/design/artifact-manager.md` §9.12 的 11 条并改写为可断言用例。

## 十五、实施顺序

1. 按第十三章上游契约准备客户端 Fake 与联调数据。
2. 实现类型化客户端及 Fake，包括完整分页、写错误分类和 Artifact Manager 响应契约。
3. 实现批次选择、UID 计算、条件合并与重试判定等纯函数。
4. 实现过程仓调谐、发布策略与发布调谐，覆盖检查点恢复和候选扫描。
5. 按第二章接入配置、时钟、指标与 initializer；权限按第十二章配置。
6. 联调期用 `--controllers=-rpmrepo` 关闭；第十四章测试通过后恢复 `--controllers=*` 启用。
