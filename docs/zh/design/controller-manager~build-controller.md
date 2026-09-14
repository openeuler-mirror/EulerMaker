# Build Controller 设计

## 一、定位与范围

Build Controller 是 controller-manager 中负责 Build 生命周期编排收敛的控制器，作为 `ebs/v1` `Build` 资源的编排中枢，位于 ebs-apiserver 与各执行 Controller 之间。

首版职责：

- 周期 List 轮询 Build，将 `Build.status.phase=Pending` 收敛到终态 `{Success, Failed, Aborted, Skipped}`；
- 按 `Build.status.phase` 所处阶段幂等 ensure 子资源：`Snapshot`、`RpmRepo`、`BuildInfo`；
- 消费 Snapshot / BuildInfo / RpmRepo 子资源状态，按 `RpmRepo.status.release.phase`（`Ready` / `Failed`）推进 `Build.status` 终态；
- 只向 `Build.status.conditions` 中写入 `BuildSucceed` 与 `PublishSucceed`，分别表示构建是否成功与发布是否成功；
- 保证 reconcile 幂等、可重入、进程重启后可恢复：重复执行不重复创建资源、不状态回退，终态只写一次；
- 在 build controller 包内实现类型化 Project-scoped 客户端与 fake，供 reconcile 与单元测试使用；
- 暴露必要的结构化日志和指标，保证状态可观测、可定位。

首版不负责：

- 不直接参与依赖解析、Job 执行或 repo 发布实现（repo 文件生成、createrepo 等由 RpmRepo Controller 负责）；
- 不直连 etcd、Elasticsearch 或 Artifact Manager；所有资源访问只经包内 `Client` 到达 ebs-apiserver；
- 不推进 `Project.status`、`Snapshot.status`、`BuildInfo.status`、`RpmRepo.status`；不填充 `BuildInfo.spec.specDepends` 与 `BuildInfo.status.specStatus`；
- 不修改 `Build.spec`；不创建或维护 `Build.metadata.labels`；
- 不将非单包构建的包级构建失败聚合为 `Build` 终态；`BuildSucceed` 仅作为包构建聚合结果记录；未就绪超时、删除级联（对删除中的 Build 在 reconcile 入口直接跳过，不做级联删除）、Scheduler / Runner / Job 内部细节不在首版范围；
- 不负责把任何状态推进为 `Aborted`；`Aborted` 是外部中止流程写入的终态（触发中止不在本版本范围），Build Controller 只将其识别为终态并停止处理；
- 不给 `Snapshot` / `BuildInfo` / `RpmRepo` 注册轮询或 Watch 源，其状态只在 reconcile 内按确定性名称读取；
- 不写中间等待条件：运行/等待阶段 `Build.status.conditions` 不维护占位值，进度以 `phase` / `stage` 表达；
- 不重复实现 controller-manager 公共框架的其他扩展。

## 二、依赖与组件边界

Build Controller 运行在现有 `controller-manager` 框架内，建议实现目录为：

```text
components/controller-manager/pkg/controllers/build/
  controller.go         # Controller 定义与 initializer：依赖注入、PollingSource handler 注册、BaseController 组装
  reconciler.go         # Sync(ctx, key) 主流程：按 Build.status.phase/stage 分发各阶段
  pending.go            # Pending：定位 base Build、ensure Snapshot/RpmRepo、等待 Active、转 Prepared
  prepared.go           # Prepared：ensure BuildInfo、转 Processing/build
  buildstage.go         # Processing/build：等待 BuildInfo Completed、判定 single 构建失败、写 BuildSucceed；跳过发布写 Skipped/publish，否则推进 stage=publish
  publish.go            # Processing/publish：ensure RpmRepo、等待发布收口（release.phase 为 Ready/Failed），写终态与 repo
  ensure.go             # ensure <Object>：确定性名称 GET/POST、AlreadyExists 沿用、结果未知确认
  prevbuildrepo.go      # 上一次发布成功 Build 定位（Client.GetLastPublishedBuild，项目级 List）
  client.go             # Client 接口与共享 client 适配实现（Get/ListProjectPage/Create/UpdateStatus + 写错误三分类）
  fake.go               # 内存 fake Client（单测用）
  conditions.go         # conditions 常量与 MergeCondition（observedGeneration=metadata.generation、固定 message）
  metrics.go            # build_controller_* 指标注册
```

依赖与注入：

- `Controller` 持有 `BaseController`（队列/Worker）、`Client`、`clock.Clock` 与配置；
- 构造时注入 `clock.Clock`（`k8s.io/utils/clock`）：生产用 `clock.RealClock{}`，测试用 `k8s.io/utils/clock/testing.FakeClock`；业务代码不得直接调用 `time.Now()` / `time.Since()`；
- initializer 负责构造 `Client`、注册 `builds` PollingSource handler 并创建 `BaseController`；
- reconcile 内所有资源访问只通过 `Client`，单测注入 `fake.Client`。

### 2.1 装配与配置

- 队列 key 为 `{Build.metadata.namespace}/{Build.metadata.name}`，`Build.metadata.namespace` 即 Project 名。
- Controller 配置项：轮询周期沿用框架全局 `--poll-period`（默认 30s）；build 专用轮询周期参数（`--build-poll-period`，期望 15s）待框架支持，当前不可配置。worker 数沿用框架配置。
- 配置校验时机：所有静态配置（apiserver 地址与 CA、请求超时）必须在 initializer 阶段完成校验；配置非法时 controller-manager 直接启动失败，不得延迟到单个 Build 的 reconcile 中处理，也不通过 HealthChecker 表达。

### 2.2 组件依赖与最小框架改动

**组件依赖与数据流**

依赖关系与数据流向如下，箭头表示数据/依赖方向；reconcile 内所有对 ebs-apiserver 的访问都经过包内 `Client`：

```text
① 事件 / 上游状态                       ② Build Controller                    ③ 数据面
────────────────────────────          ────────────────────────────────        ────────────────────────────────
Build PollingSource ──key 事件──▶   key 队列 {namespace}/{name}
 （周期分页 List 非终态 Build）           Add/Update 入队；Delete 仅日志
                                             │
                                             ▼
Snapshot/BuildInfo/RpmRepo            reconcile（仅经包内 Client）────────▶ ebs-apiserver
 Controller                                   │                            ├ GET 最新 Build / 子资源状态
 （写子资源 status ▶ ebs-apiserver）           │                            ├ POST 创建子资源
                                             └─ 只读 RpmRepo 发布收口结果      └ PUT /status
```

图例说明：

- ① 上游只产出状态与事件，不反向依赖 Build Controller；Snapshot/BuildInfo/RpmRepo 的 status 由各自子 Controller 推进。
- ② reconcile 每次访问 apiserver 都经由包内 `Client`，且只对 ebs-apiserver 读写，不直连 etcd、Elasticsearch 或 Artifact Manager。
- ③ Build Controller 不调用 repo 发布后端，发布收口结果只从 `RpmRepo.status` 读取。

**组件边界**

- 只消费 Build、Snapshot、BuildInfo、RpmRepo 的状态；`Snapshot.status` / `BuildInfo.status` / `RpmRepo.status` 由各自子 Controller 维护，本 Controller 不越权推进。
- 所有资源访问只经包内 `Client` 到达 ebs-apiserver（GET/创建/PUT `/status`），不在 Controller 内直连数据存储或 Artifact Manager。
- 不触发发布：`Processing/publish` 只读取 RpmRepo 的发布收口结果（`status.release.phase` 为 `Ready` / `Failed`）；子资源未就绪时保持等待/重试，不写中间条件。

**最小框架改动**

- 分页 List 按 GVR 注入 fieldSelector——已由 `PollingSourceFactory.ForResource(gvr, period, options)` 提供；
- 项目级 List 与子资源创建——已由共享 client 的 `ListProjectPage` 与 `Create` 提供，无需暴露 `RESTClient()`；
- 在 `components/controller-manager/cmd/controller-manager/main.go` 的 `initializers` map 中加入 `build.Initializer(...)`。

### 2.3 PollingSource 装配语义

- 只对 `builds` 通过 `PollingSourceFactory.ForResource(gvr, period, metav1.ListOptions{FieldSelector: ...})` 获取 PollingSource；非终态 fieldSelector 通过该 options 注入，周期固定，Controller 在 Source 启动前完成 handler 注册。
- 每个 period 执行一轮 scan：对 `builds` 发起分页 List，请求固定携带非终态 fieldSelector，通过 `continue` 翻页。
- List 返回对象必须含 `metadata.name`、`metadata.uid`、`metadata.resourceVersion`，按 `{metadata.namespace}/{metadata.name}` 建内部快照。
- 与上一轮快照 diff：key 消失或 `metadata.uid` 改变先发 Delete、再发 Add；同 key 且同 `metadata.uid` 每轮都发 Update，Update 兼作周期 resync。
- 任一页失败则整轮失败，不替换旧快照、不产生 Delete，按退避重试；成功后原子替换快照并更新 lastSuccess。
- 事件处理规则：
  - `Build` Add/Update → 入队 `{Build.metadata.namespace}/{Build.metadata.name}`；
  - 进入终态的 `Build` 不再出现在后续 List，旧快照 diff 产生 Delete 事件 → handler 只记录日志，不将其作为 reconcile 目标入队。
  - `metadata.deletionTimestamp` 的变化不单独产生事件；由于同 key 每轮都发 Update，删除中的对象自然会被重新入队，由 7.1 的入口守卫短路。
- 内部快照保存完整对象（DeepCopy）与 `metadata.uid`、`metadata.resourceVersion`，用于变化检测与 resync；不是 reconcile 数据源。

### 2.4 PollingSource API 交互样例

每轮 scan 只对 `builds` 发起分页 List（`status.phase` 只支持 `=`/`==`/`!=`，故用四个 `!=` AND）：

```text
GET /apis/ebs/v1/builds?fieldSelector=status.phase!=Success,status.phase!=Failed,status.phase!=Aborted,status.phase!=Skipped&limit=100
```

说明：`continue` 为空时不发送该参数。
响应 200 示例（只含非终态对象，`metadata.continue` 非空表示继续下一页）：

```json
{
  "apiVersion": "ebs/v1",
  "kind": "BuildList",
  "metadata": { "continue": "token-0001", "resourceVersion": "rv-1000" },
  "items": [
    {
      "apiVersion": "ebs/v1",
      "kind": "Build",
      "metadata": {
        "name": "build-001",
        "namespace": "project-a",
        "uid": "uid-build-001",
        "resourceVersion": "17",
        "labels": {
          "ebs.io/target-os": "openEuler-22.03-LTS",
          "ebs.io/target-arch": "aarch64",
          "ebs.io/build-type": "full"
        }
      },
      "spec": {
        "buildType": "full",
        "bootstrapRepo": [
          { "name": "everything", "repo": "https://example.com/repo/everything" }
        ],
        "packages": ["gcc"],
        "buildTarget": {
          "os": "openEuler-22.03-LTS",
          "arch": "aarch64",
          "buildFlag": true,
          "publishFlag": true
        }
      },
      "status": {
        "phase": "Processing",
        "stage": "build",
        "startTime": "2026-09-03T12:00:00Z"
      }
    }
  ]
}
```

携带 `metadata.continue` 翻页，直到响应中 `metadata.continue` 为空。Worker 收到 key 后先 GET 最新对象，再在 reconcile 中按确定性名称 GET 子资源：

```text
GET /apis/ebs/v1/projects/project-a/builds/build-001
GET /apis/ebs/v1/projects/project-a/snapshots/build-001
GET /apis/ebs/v1/projects/project-a/buildinfos/build-001
GET /apis/ebs/v1/projects/project-a/rpmrepos/build-001
```

## 三、类型化客户端与 Fake（在 build controller 内实现）

框架不新增完整类型化 CRUD；由 build controller 包自行实现。

### 3.1 接口定义

```go
package build

type Client interface {
    // Project 级资源读取
    // GetProject 读取 Project；用于构造 Snapshot.spec.packageRepos。
    GetProject(ctx context.Context, project string) (*v1.Project, error)
    GetBuild(ctx context.Context, project, name string) (*v1.Build, error)
    // GetLastPublishedBuild 返回最后一个发布成功的 Build。
    // 过滤条件：target-os / target-arch 匹配、status.phase=Success、status.stage=publish。
    // 项目作用域由项目级 List 的路径承担，不使用 metadata.namespace fieldSelector。
    // single 的成功终态是 Skipped，因此 phase=Success 已隐含“非 single 且实际发布成功”，无需 ebs.io/build-type 过滤。
    GetLastPublishedBuild(ctx context.Context, project, os, arch string) (*v1.Build, error)
    GetSnapshot(ctx context.Context, project, name string) (*v1.Snapshot, error)
    GetRpmRepo(ctx context.Context, project, name string) (*v1.RpmRepo, error)
    GetBuildInfo(ctx context.Context, project, name string) (*v1.BuildInfo, error)

    // 子资源创建（create-only）
    CreateSnapshot(ctx context.Context, project string, obj *v1.Snapshot) (*v1.Snapshot, error)
    CreateRpmRepo(ctx context.Context, project string, obj *v1.RpmRepo) (*v1.RpmRepo, error)
    CreateBuildInfo(ctx context.Context, project string, obj *v1.BuildInfo) (*v1.BuildInfo, error)

    // Build 写操作
    // UpdateBuildStatus 走 /status：写 Build.status，apiserver 保留旧 Build.spec
    UpdateBuildStatus(ctx context.Context, obj *v1.Build) (*v1.Build, error)
}
```

### 3.2 路径约定

- Project：`GET /apis/ebs/v1/projects/{project}`（Project 为集群级资源，namespace 段为空；共享 client 的 `Get` 对 `projects` 要求空 namespace）；
- 最后一个发布成功的 Build：`GET /apis/ebs/v1/projects/{project}/builds?labelSelector=ebs.io/target-os={os},ebs.io/target-arch={arch}&fieldSelector=status.phase=Success,status.stage=publish&limit=1`（apiserver 默认降序，路径即项目作用域）；
  - `os` / `arch` 取 `Build.spec.buildTarget.os` / `Build.spec.buildTarget.arch`，`{project}` 取 `Build.metadata.namespace`；
  - 通过共享 client 的项目级 List 发起，由路径把查询限定在该 Project 内；否则 `limit=1` 会取到其他 Project 的最新 Build，导致 base 误判。
- 子资源单对象：`GET /apis/ebs/v1/projects/{project}/{resource}/{name}`；
- 子资源创建：`POST /apis/ebs/v1/projects/{project}/{resource}`；
- Build status：`PUT /apis/ebs/v1/projects/{project}/builds/{name}/status`。

HTTP 实现复用共享 client 的 `Get` / `ListProjectPage` / `Create` / `UpdateStatus` 等能力；写请求使用对象 `metadata.resourceVersion` 触发乐观锁。

### 3.3 Fake

Fake 实现同一 `Client` 接口，内部用内存 map 保存对象，并模拟：

- NotFound；
- AlreadyExists（同名重复 Create 返回 409）；
- 版本过期 Conflict（旧 resourceVersion 写返回 409）；
- resourceVersion 自增；
- `/status` 保留旧 spec。
- `GetProject` 返回成功对象或 NotFound。

## 四、对象样例与字段来源

`Snapshot`、`RpmRepo`、`BuildInfo` 通过同名的 `metadata.name` 与 Build 关联。

### 4.1 Build

初始对象：

```yaml
apiVersion: ebs/v1
kind: Build
metadata:
  name: uuid
  namespace: ${project.metadata.name}
  labels:
    ebs.io/target-os: openEuler-22.03-LTS
    ebs.io/target-arch: aarch64
    ebs.io/build-type: full
spec:
  buildType: full
  bootstrapRepo:
    - name: everything
      repo: https://example.com/repo/everything
  packages: [gcc]
  buildTarget:
    os: openEuler-22.03-LTS
    arch: aarch64
    buildFlag: true
    publishFlag: true
status:
  phase: Pending
```

构建中与终态示例：

```yaml
# 构建中
status:
  phase: Processing
  stage: build
  startTime: "2026-09-03T12:00:00Z"

# 最终（依据 RpmRepo release.phase=Ready 判定）
status:
  phase: Success
  stage: publish
  endTime: "2026-09-03T13:30:00Z"
  repo: https://repo.example/repo/final/build-001
  conditions:
    - type: BuildSucceed
      status: "True"
      reason: BuildSucceeded
    - type: PublishSucceed
      status: "True"
      reason: PublishSucceeded

# 最终（依据 RpmRepo release.phase=Failed 判定）
status:
  phase: Failed
  stage: publish
  endTime: "2026-09-03T13:10:00Z"
  repo: ""
  conditions:
    - type: BuildSucceed
      status: "True"
      reason: BuildSucceeded
    - type: PublishSucceed
      status: "False"
      reason: PublishFailed

# 最终（跳过发布，由本控制器判定：buildType=single 单包成功 或 buildTarget.publishFlag=false）
status:
  phase: Skipped
  stage: publish
  endTime: "2026-09-03T13:30:00Z"
  repo: ""
  conditions:
    - type: BuildSucceed
      status: "True"
      reason: BuildSucceeded
```

字段来源：

| 字段                                                                                                     | 来源/写入方           | 说明                                             |
| ------------------------------------------------------------------------------------------------------ | ---------------- | ---------------------------------------------- |
| `Build.metadata.name` / `Build.metadata.namespace`                                                     | 用户提交 / API 路径    | uuid，不复用同名                                     |
| `Build.metadata.labels`                                                                                | 用户或调用方           | 创建 Build 时写入                               |
| `Build.spec.buildType`                                                                                 | 用户提交             | Controller 只读；允许值 `full` / `incremental` / `specified` / `single`，控制器只对 `single` 特判，其余取值（含未枚举值）一律按非 single 处理 |
| `Build.spec.bootstrapRepo` / `Build.spec.packages` / `Build.spec.buildTarget`                           | 用户提交             | Controller 只读                                  |
| `Build.status.phase` / `Build.status.stage` | Build Controller | Pending → Prepared → Processing 由本控制器状态机推进；publish 终态由本控制器依据 `RpmRepo.status.release.phase` 判定，`stage` 由本控制器写 `publish` |
| `Build.status.repo` | Build Controller | 复制自 `RpmRepo.status.release.contentURL`（正式发布稳定入口），仅在 `RpmRepo.status.release.phase=Ready` 时写入 |
| `Build.status.startTime` / `Build.status.endTime` / `Build.status.baseBuildRef` | Build Controller | 进入 Processing 时写 `startTime`，进入终态时写 `endTime`；`baseBuildRef` 在 Pending 阶段写入，`nil` 表示未解析，`{}` 表示无上一个发布成功的 Build |
| `BuildSucceed` / `PublishSucceed`（`Build.status.conditions`） | Build Controller | `BuildSucceed` 由 `BuildInfo.status.specStatus` 计算；`PublishSucceed` 由 `RpmRepo.status.release.phase` 推导（`Ready`→`True`、`Failed`→`False`），不再从 `RpmRepo.status.conditions` 复制 |

`Build.status` 更新样例（PUT `/status`）：

```yaml
PUT /apis/ebs/v1/projects/project-a/builds/build-001/status

apiVersion: ebs/v1
kind: Build
metadata:
  name: build-001
  namespace: project-a
  resourceVersion: "18"
status:
  phase: Success
  stage: publish
  endTime: "2026-09-03T13:30:00Z"
  repo: https://repo.example/repo/final/build-001
  conditions:
    - type: BuildSucceed
      status: "True"
      reason: BuildSucceeded
    - type: PublishSucceed
      status: "True"
      reason: PublishSucceeded
```

### 4.2 Snapshot

Build Controller 创建 Snapshot 时写入 `Snapshot.spec.packageRepos`（逐项复制 `Project.spec.packageRepos`，Project 名取 `Build.metadata.namespace`）；不写 `Snapshot.status.packageRepoStatuses`，该字段由 Snapshot Controller 按 `spec.packageRepos` 逐项解析后写入。Build Controller 只等待 `Snapshot.status.phase=Active`；已存在的 Snapshot 不读 Project、不覆盖 spec。

```yaml
apiVersion: ebs/v1
kind: Snapshot
metadata:
  name: ${build.metadata.name}
  namespace: ${build.metadata.namespace}
spec:
  packageRepos:   # 逐项复制 Project.spec.packageRepos
    - name: gcc
      url: https://example.com/src-openeuler/gcc.git
      ref:
        type: Branch
        value: master
status:
  phase: Pending
```

字段来源：

| 字段                                     | 来源/写入方              | 说明                                            |
| -------------------------------------- | ------------------- | --------------------------------------------- |
| `Snapshot.metadata.name` / `namespace` | Build Controller    | 与 `Build.metadata.name` 同名，namespace 来自 Build |
| `Snapshot.spec.packageRepos`           | Build Controller    | 创建时逐项复制 `Project.spec.packageRepos`；已存在对象不覆盖；`Project.spec.packageRepos` 为空时写入空列表 |
| `Snapshot.status.packageRepoStatuses`  | Snapshot Controller | 按 `Snapshot.spec.packageRepos` 逐项解析写入 |
| `Snapshot.status.phase`                | Snapshot Controller | Pending → Processing → Active                              |

### 4.3 RpmRepo

RpmRepo 的相位与发布收口语义以 RpmRepo-Controller 为准，RpmRepo 没有对象终态；Build Controller 只消费下表字段。`RpmRepoStatus` 的结构（`repository` / `release` / `conditions`）与 `docs/zh/design/data-models.md` 一致。

骨架形态（Build Controller 在 Pending 阶段 ensure）：

```yaml
apiVersion: ebs/v1
kind: RpmRepo
metadata:
  name: ${build.metadata.name}
  namespace: ${build.metadata.namespace}
spec: {}
status:
  repository:
    phase: Pending          # Pending / Processing / Ready / Failed，均非对象终态
    rpmDepends: {}
    sourceJobUIDs: []
    contentURL: ""          # 过程仓地址；发布前为空
    transition: null        # 非空表示存在尚未收口的批次
  release: null             # 尚未进入发布阶段
  conditions: []
```

骨架形态见上；发布收口后的形态见下（仅列发布相关字段）：

```yaml
# 发布后（release.phase=Ready）
status:
  repository:
    phase: Ready
    repositoryUID: <repositoryUID>
    contentURL: <过程仓不可变地址>
    repositoryDigest: <repositoryDigest>
    packageCount: <packageCount>
    rpmDepends: {}
    sourceJobUIDs: ["<jobUID>"]
    transition: null
    updatedAt: "2026-09-14T10:00:00Z"
  release:
    phase: Ready            # Pending / Creating / Prepared / Ready / Failed
    sourceRepositoryUID: <repositoryUID>
    contentURL: <稳定入口地址>
    releaseDigest: <releaseDigest>
    packageCount: <packageCount>
    transition: null
    updatedAt: "2026-09-14T10:00:00Z"
  conditions: []            # 失败原因记录在此；Build 不读取
```

字段来源：

| 字段                                    | 来源/写入方             | 说明                                             |
| ------------------------------------- | ------------------ | ---------------------------------------------- |
| `RpmRepo.metadata.name` / `namespace` | Build Controller | 与 `Build.metadata.name` 同名，namespace 来自 Build |
| `RpmRepo.spec`                        | Build Controller   | 空 `{}`                                         |
| `RpmRepo.status.release.phase`        | RpmRepo Controller | 发布收口门禁与结论来源：`Ready` → `Success/publish`、`Failed` → `Failed/publish`；其余取值（含 `release` 缺失）表示等待 |
| `RpmRepo.status.release.contentURL`   | RpmRepo Controller | 正式发布稳定入口地址；仅在 `release.phase=Ready` 时复制到 `Build.status.repo` |

上表仅列 Build Controller 消费的字段；其余字段（`status.repository.*`、`status.release` 的其它字段、`status.conditions`）由 RpmRepo Controller 维护，Build 不读取。

### 4.4 BuildInfo

Build Controller 只创建骨架：`BuildInfo.spec.specDepends` 为空，不写 `BuildInfo.status.specStatus`；由独立 BuildInfo Controller 填充并推进。

```yaml
apiVersion: ebs/v1
kind: BuildInfo
metadata:
  name: ${build.metadata.name}
  namespace: ${build.metadata.namespace}
spec:
  specDepends: {}
status:
  phase: Pending
```

字段来源：

| 字段                                                                                       | 来源/写入方               | 说明                                   |
| ---------------------------------------------------------------------------------------- | -------------------- | ------------------------------------ |
| `BuildInfo.metadata.name` / `namespace` | Build Controller | 与 `Build.metadata.name` 同名，namespace 来自 Build |
| `BuildInfo.spec.specDepends`                                                             | BuildInfo Controller | 由 BuildInfo Controller 解析生成 |
| `BuildInfo.status.phase` / `BuildInfo.status.specStatus` / `BuildInfo.status.conditions` | BuildInfo Controller | Pending → Processing → Completed，并回写各包状态          |

## 五、状态机

非终态 Build 指 `Build.status.phase` 不在终态集合 `{Success, Failed, Aborted, Skipped}` 中。

| 当前 phase / stage | 动作与分支 | 下一 phase / stage |
| --- | --- | --- |
| `Pending` / 空 | 若 `Build.status.baseBuildRef` 为 nil：定位上一个发布成功的 Build 并写 `baseBuildRef`（结果为空写 `{}`），本轮即返回 | 仍 `Pending` / 空（下一轮继续） |
| `Pending` / 空 | ensure Snapshot；ensure RpmRepo；等待 `Snapshot.status.phase=Active` | `Snapshot.status.phase=Active` 后：`Prepared` / 空 |
| `Prepared` / 空 | ensure BuildInfo | `Processing` / `build` |
| `Processing` / `build` | 等待 `BuildInfo.status.phase=Completed`；完成后按以下分支处理 | — |
|  | `Build.spec.buildType=single` 且首个 `BuildInfo.status.specStatus` 的 `value.build.status` 非 `Succeeded`（含缺失、未完成、`Failed`/`Aborted` 等），或 `specStatus` 为空 | `Failed` / `build` |
|  | `buildType=single` 且单包构建成功，或非 single 且 `Build.spec.buildTarget.publishFlag=false`（由本控制器判定跳过发布） | `Skipped` / `publish` |
|  | 其余情况（非 single 且 `Build.spec.buildTarget.publishFlag=true`） | `Processing` / `publish` |
| `Processing` / `publish` | 等待 RpmRepo 发布收口：`RpmRepo.status.release` 缺失或 `release.phase` 不是 `Ready` / `Failed`；未收口时等待 | — |
|  | `release.phase=Ready` | `Success` / `publish`（`repo` 复制自 `RpmRepo.status.release.contentURL`） |
|  | `release.phase=Failed` | `Failed` / `publish` |

规则：

- `Build.status.stage`：
  - Pending/Prepared 阶段为空；
  - 进入 Processing 时首次写 `build`；
  - 只允许从 `build` 推进到 `publish`；
- `Build.spec.buildType=single` 分支：沿用 Pending 阶段已 ensure 的 RpmRepo；单包构建失败进入 `Failed` / `build`；单包成功由本控制器直接判定跳过发布，写 `Skipped` / `publish`（不写 `PublishSucceed`、不等待 RpmRepo）。
- 非 single 分支：`Build.spec.buildTarget.publishFlag=false` 时由本控制器直接判定跳过发布，写 `Skipped` / `publish`（不写 `PublishSucceed`、不等待 RpmRepo）；其余情况进入 `Processing` / `publish` 等待 RpmRepo 发布收口。
- 发布门禁：`RpmRepo.status.release.phase` ∈ `{Ready, Failed}`。
- 发布结论：`release.phase=Ready` → `Success` / `publish`（`repo` 复制自 `RpmRepo.status.release.contentURL`）；`release.phase=Failed` → `Failed` / `publish`。Build 不校验 `release.contentURL` 是否非空，按原值复制。

## 六、子资源 ensure

`ensure <Object>` 用于幂等创建或获取 Snapshot / RpmRepo / BuildInfo：

1. **按确定性名称 GET**
   - 成功且 `metadata.deletionTimestamp` 为空：沿用现有对象，不修改 spec。
   - 成功但 `metadata.deletionTimestamp` 非空：视为该子资源不可用；本轮不推进（调用方按 7.6 「队列结果映射」的等待类返回零值 + `nil`），不重建、不覆盖、不删除，并记录日志与 `build_controller_ensure_terminating_total` 指标。
   - NotFound：进入创建流程。
2. **创建对象**
   - 按约定 spec 执行 POST。
   - POST 409（AlreadyExists）：GET 后沿用现有对象，不修改 spec。
3. **结果未知处理**
   - 网络错误、5xx、超时导致结果未知时：
     - 先 GET 确认实际结果；
     - 存在则沿用，不存在则返回可重试错误。

ensure 不推进子资源状态、不等待子资源就绪、不覆盖已存在 spec、不删除其他 Build 的对象。

Snapshot 的创建需要先读 Project：仅当 Snapshot NotFound 时才 `GetProject(Build.metadata.namespace)`，并以返回对象的 `spec.packageRepos` 构造 Snapshot spec 后创建；已有 Snapshot 时不读 Project、不覆盖 spec。`GetProject` 返回 NotFound 时按永久错误处理，不创建子资源、不写 `Build.status`。

## 七、Reconcile 流程

### 7.1 主流程

统一 reconcile 主循环按以下顺序执行：

1. 读取最新 Build。
2. 若 Build 已处于 Success、Failed、Aborted 或 Skipped 终态，本轮直接成功返回、不再处理（`Aborted` 由外部中止流程写入，reconcile 不负责推进）。
3. 若 `Build.metadata.deletionTimestamp` 非空（对象正在删除），本轮直接返回零值 + `nil`：不写 `Build.status`、不 ensure 子资源、不读取 RpmRepo 发布收口结果；已创建的子资源不做级联删除。服务端没有 `metadata.deletionTimestamp` 的 fieldSelector 映射，删除中的非终态 Build 仍会出现在 List，因此该守卫必须在 reconcile 入口判断。
4. 校验 `(phase, stage)` 组合，合法集合为 `{(Pending,""), (Prepared,""), (Processing,"build"), (Processing,"publish")}`：
   - 非终态 phase 但不在 Pending / Prepared / Processing 内：记录未知 phase 错误，返回永久错误。
   - phase 合法但组合不在合法集合内（例如 `Processing` + 空 stage、`Pending` + `publish`）：记录错误，返回永久错误，**不写 status、不推进终态**。这是第 7 步“确定性错误进入对应终态”的例外，非法组合只能由外部或人工修正；每轮 resync 会重新入队，因此该错误会周期性重复记录。
5. 根据状态机确定当前 phase/stage 对应的处理阶段。
6. 执行对应阶段处理逻辑。
7. 阶段处理成功则本轮结束；临时错误返回可重试错误，由队列退避重试；确定性错误进入对应终态、在结果确定时写入对应条件后返回永久错误。
8. 若状态没有变化，不调用状态更新 API。

状态写入原子性：

- 每次状态迁移先构造完整的目标 `Build.status`，再调用一次 `UpdateBuildStatus` 提交。
- `7.2` 至 `7.5` 中“写：”块列出的字段是本次写入需要合并的字段清单，不是多次独立 API 调用。
- `Build.status.conditions` 的变更也合并到同一次 `/status` 写入，不单独更新 condition。
- 写入结果未知（超时、连接中断、响应无法解析）时不重放原 PUT，按第九章「写入结果未知确认」处理。

全局网络错误语义：

- 任何 apiserver API 调用因网络、超时、连接中断、5xx、结果未知导致失败时，一律返回可重试错误。
- 只有明确的 4xx 业务拒绝、参数错误、资源不存在且不可恢复等确定性错误，才返回永久错误并进入对应终态（结果确定时写对应条件）。

后续各小节只描述阶段内部逻辑，不重复分派规则。

### 7.2 Pending

- 若 `Build.status.baseBuildRef` 为 nil：
  - 调用 `GetLastPublishedBuild(project, os, arch)`：
    - 若结果为空，写 `Build.status.baseBuildRef={}`；
    - 否则写：
      - `Build.status.baseBuildRef.name`（上一个发布成功 Build 的 `Build.metadata.name`）
      - `Build.status.baseBuildRef.repo`（上一个发布成功 Build 的 `Build.status.repo`）
  - 写入后立即返回零值 + `nil`，`ensure Snapshot` / `ensure RpmRepo` 与后续等待从下一轮继续；`{}` 与实际值同样视为已处理。
- ensure Snapshot：GET Snapshot；
  - 已存在且未处于删除中：沿用，不读 Project、不覆盖 spec；
  - 已存在但处于删除中：按未就绪等待；
  - NotFound：`GetProject(project)` → 以 `spec.packageRepos` 构造 Snapshot spec 后创建；
    - `GetProject` NotFound：返回零值 + `controller.NewPermanentError`，不写 status、不创建子资源；
    - `GetProject` 其他错误：按 7.1「全局网络错误语义」分类（临时→可重试错误，确定性 4xx→永久错误）。
- ensure RpmRepo。
- GET Snapshot。
- 等待 `Snapshot.status.phase=Active`。
- 满足后写 `Build.status.phase=Prepared`。

### 7.3 Prepared

- ensure BuildInfo，创建空 `BuildInfo.spec.specDepends`。
- 写：
  - `Build.status.phase=Processing`
  - `Build.status.stage=build`
  - `Build.status.startTime`

### 7.4 Processing/build

- GET BuildInfo。
- 等待 `BuildInfo.status.phase=Completed`。
- 若 `Build.spec.buildType=single`：
  - 读取 `BuildInfo.status.specStatus` 的 values；空 map 视为失败；非空时取第一个 value。
  - 构建失败：
    - 触发条件：`BuildInfo.status.specStatus` 为空，或首个 value 的 `build.status` 非 `Succeeded`（含缺失、未完成、`Failed`/`Aborted` 等）；
    - 写：
      - `Build.status.phase=Failed`
      - `Build.status.stage=build`
      - `Build.status.endTime`
      - `BuildSucceed=False/BuildFailed`
  - 构建成功（跳过发布）：
    - 触发条件：首个 value 的 `build.status == Succeeded`；
    - 写：
      - `Build.status.phase=Skipped`
      - `Build.status.stage=publish`
      - `Build.status.endTime`
      - `BuildSucceed=True/BuildSucceeded`
    - `Build.status.repo` 保持为空；本控制器直接判定跳过发布，不写 `PublishSucceed`、不读取 RpmRepo。
- 否则：
  - 依据 `BuildInfo.status.specStatus` 的所有 value 计算 `BuildSucceed`：
    - `BuildInfo.status.specStatus` 为空/缺失，或存在任一 `value.build.status` 非 `Succeeded`：写 `BuildSucceed=False/BuildFailed`；
    - 所有 `value.build.status` 均为 `Succeeded`：写 `BuildSucceed=True/BuildSucceeded`。
  - `BuildSucceed` 仅记录所有包是否完全构建成功，不作为发布门禁。
  - 若 `Build.spec.buildTarget.publishFlag=false`（跳过发布），写：
    - `Build.status.phase=Skipped`
    - `Build.status.stage=publish`
    - `Build.status.endTime`
    - `BuildSucceed`（取上一步计算结果）
    - 本控制器直接判定跳过发布，不写 `PublishSucceed`、不读取 RpmRepo。
  - 否则写：
    - `Build.status.stage=publish`
    - `BuildSucceed`（取上一步计算结果）
    - `Build.status.phase` 保持 `Processing`，等待 RpmRepo 发布收口。

### 7.5 Processing/publish

- GET RpmRepo（确定性名称，project 与 namespace 取自 Build）。
- 等待 RpmRepo 发布收口：`RpmRepo.status.release` 缺失，或 `release.phase` 不是 `Ready` / `Failed` 时，返回零值 + `nil`，下一轮由周期 resync 复查。
- 收口后（`release.phase` ∈ `{Ready, Failed}`）一次 `/status` 写：
  - `release.phase=Ready`：
    - `Build.status.phase=Success`
    - `Build.status.stage=publish`
    - `Build.status.repo=RpmRepo.status.release.contentURL`
    - `Build.status.endTime`
    - `PublishSucceed=True/PublishSucceeded`（reason 与 message 由本控制器固定，不从 RpmRepo 复制）
  - `release.phase=Failed`：
    - `Build.status.phase=Failed`
    - `Build.status.stage=publish`
    - `Build.status.endTime`
    - `PublishSucceed=False/PublishFailed`
    - 不写 `Build.status.repo`（保持为空）。
- 本轮返回值：`release.phase=Ready` 返回零值 + `nil`；`release.phase=Failed` 返回零值 + `controller.NewPermanentError`。


### 7.6 队列结果映射

`Sync` 返回 `(ReconcileResult, error)`，BaseController 据此决定该 key 的重入方式；`Requeue` 与 `RequeueAfter` 不得同时非零。各分支的映射如下：

| 场景 | 返回 |
| --- | --- |
| Build 不存在、已终态、状态无变化 | 零值 + `nil` |
| 写入 `baseBuildRef` 后本轮返回、等待 `Snapshot.status.phase=Active`、`BuildInfo.status.phase=Completed`、RpmRepo 发布收口（`RpmRepo.status.release.phase` 为 `Ready` / `Failed`） | 零值 + `nil`（依赖 PollingSource 周期 resync 复查） |
| `/status` 或子资源写入返回 409 Conflict | `ReconcileResult{RequeueAfter: 1s}` + `nil`（重新 GET 并按第七章重算目标 status，不使用立即重入，避免持续冲突形成热循环） |
| API 临时错误（超时、连接中断、5xx、结果未知） | 零值 + 原始错误（BaseController 走 `AddRateLimited`；超过 `--controller-max-retries` 后转慢速指数退避并持续重入） |
| 429 / 503 且响应带 `Retry-After` | 零值 + 携带 RetryAfter 的 `client.WriteError`（框架按该延时重入） |
| 永久 4xx、非法 `(phase, stage)`、输入或客户端错误 | 零值 + `controller.NewPermanentError(err)`（框架 Forget 仅清除本次队列退避；后续仍会被 PollingSource 周期 resync 重新入队） |
| 构建或发布确定性失败，且终态与条件已通过一次 `/status` 写入 | 零值 + `controller.NewPermanentError(err)`（终态即该 key 的终点） |
| 写入结果未知 | 先按 9.4「写入结果未知确认」确认，再按确认结果落到上述对应行返回 |

约定：

- `err != nil` 时不返回非零 `ReconcileResult`，避免框架记录 `invalid-result-with-error`；
- "等待"类分支统一用零值 + `nil`，不用 error 表达正常等待，避免污染错误指标并叠加额外退避；
- 只有需要比轮询周期更快复查时才使用 `RequeueAfter`。


## 八、条件与时间

### 8.1 Conditions 生命周期

`Build.status.conditions` 只记录构建与发布结果，不维护快照、构建信息、RPM 仓库等中间就绪状态；运行中的进度由 `Build.status.phase` / `Build.status.stage` 表达。只允许以下两个 type，结果确定前对应条件不出现，每轮 reconcile 内容变化时才通过 `/status` 写入：

| type             | 含义           | `status="True"` 的写入时机                                     | `status="False"` 的写入时机                                    |
| ---------------- | -------------- | ----------------------------------------------------------- | ------------------------------------------------------------ |
| `BuildSucceed`   | 构建是否成功   | 非 single：所有 `BuildInfo.status.specStatus` 的 `value.build.status == Succeeded`；single：首个 `value.build.status == Succeeded` | `BuildInfo.status.specStatus` 为空/缺失，或存在任一 `value.build.status` 非 `Succeeded` |
| `PublishSucceed` | 发布是否成功   | `RpmRepo.status.release.phase=Ready`（本控制器写 `reason/message=PublishSucceeded`） | `RpmRepo.status.release.phase=Failed`（本控制器写 `reason/message=PublishFailed`）；`release.phase` 未到终态时不写 |

各阶段写入规则：

- Pending/Prepared、Processing/build 与 Processing/publish 的等待/运行阶段：不写任何条件。
- 构建结果确定：写 `BuildSucceed`；`single` 分支仅当首个 `BuildInfo.status.specStatus` 的 `value.build.status == Succeeded` 写 `BuildSucceed=True/BuildSucceeded`，`specStatus` 为空或首个 value 非 `Succeeded` 写 `BuildSucceed=False/BuildFailed`；非 single 分支遍历所有 value，全部为 `Succeeded` 写 `BuildSucceed=True/BuildSucceeded`，否则写 `BuildSucceed=False/BuildFailed`。
- 非 `single` 场景 `BuildSucceed=False` 只表示包未完全构建成功，不改变 Build 终态，也不阻断发布。
- 发布结果确定：仅在 `RpmRepo.status.release.phase` 为 `Ready` 或 `Failed` 后，由本控制器直接写入 `PublishSucceed`（`Ready` → `True/PublishSucceeded`；`Failed` → `False/PublishFailed`）；不再从 `RpmRepo.status.conditions` 复制，`release.phase` 未到终态时不写。
- 跳过发布（由本控制器判定：`buildType=single` 单包成功 或 `buildTarget.publishFlag=false`）只写 `BuildSucceed`，不写 `PublishSucceed`。
- 条件一旦写入即保留到终态，不因后续阶段变化删除或改写；`reason` 固定为上述四个值，不再细分中间原因。

condition merge helper：

```go
func MergeCondition(
    conditions []metav1.Condition,
    condType string,
    status metav1.ConditionStatus,
    reason, message string,
    observedGeneration int64,
) ([]metav1.Condition, bool)
```

- 按 `type` 定位并更新 `status`、`reason`、`message`、`observedGeneration`；
- `observedGeneration` 取 `Build.metadata.generation`（apiserver 仅在 spec 变化时递增该值）；
- `message` 写与 `reason` 同族的固定短语：`BuildSucceeded` / `BuildFailed` / `PublishSucceeded` / `PublishFailed`；动态错误详情只进日志，不写入 condition；
- 仅在 `status` 变化时更新 `lastTransitionTime`；
- 结果按 `type` 排序；
- 所有字段无变化时返回 `changed=false`，Controller 跳过 `/status` 写入。

### 8.2 时间来源

本控制器所有主动时间来源都是构造时注入的 `clock.Clock`：

- `Build.status.startTime` 在进入 Processing 时写入，取本轮 `now`；
- `Build.status.endTime` 在进入终态时写入，取同一次调谐的 `now`；
- 已有的 `Build.status.startTime` / `endTime` 是 apiserver 中的业务数据，Controller 只读取，不使用本地时间覆盖或重写。

每次 `Sync` 开始时只调用一次 `clock.Now()` 并保存为 `now`，本周期内所有新时间戳复用该值；写入 API 消耗的时间不重新计入本周期，下一次调谐再取新的 `now`。写入 `metav1.Time` 时统一使用 `metav1.NewTime(now.UTC())`，持久化时间一律 UTC。

本控制器不做基于时间的等待判断（未就绪超时见第十六章的显式取舍），因此不涉及墙上时间的截止计算；若后续引入超时，必须沿用同一次 `now` 计算剩余时间。

测试必须通过 FakeClock 推进时间，不使用真实 `sleep`，也不依赖真实时钟。

## 九、并发与一致性

### 9.1 单键串行与共享状态

BaseController 保证同一队列键 `{namespace}/{name}` 在任一时刻只由一个 worker 调和；不同 Build 可以并发。本控制器不维护跨 key 的共享业务状态，队列键之外没有需要加锁的内存结构。

### 9.2 乐观并发

Build、子控制器与外部中止流程都可能更新同一对象。本控制器必须：

- 写入前 GET 最新 Build，基于最新对象的 `resourceVersion` 构造目标 status；
- `/status` 返回 409 Conflict 时重新 GET，并按第七章重新计算目标 status 后重试，禁止用旧对象重放；重试使用固定 1s 退避（常量，不新增配置项）——`Requeue: true` 会立即重入且不受 `--controller-max-retries` 约束，持续冲突会退化为热循环；
- 不直接修改 `Build.spec`，也不写 `Build.metadata`（标签由创建方或 apiserver 维护）。

### 9.3 与子控制器的竞态

- 本控制器只写 `Build.status`；`Snapshot.status` / `BuildInfo.status` / `RpmRepo.status` 由各自子控制器写入。
- `ensure` 按确定性名称 GET 后创建，`AlreadyExists` 视为沿用现有对象，不覆盖已存在 spec（见第六章）。
- Build 进入终态后本轮直接返回，不再产生新的子资源写入。

### 9.4 写入结果未知确认

写入失败必须区分三种结果，不能一律重试：

- **NotSent**（请求未发出）：直接返回可重试错误。
- **Rejected**（收到明确响应）：409 Conflict 重新 GET 后重算重试；确定性 4xx 返回永久错误，并按 7.4 / 7.5 写对应终态、按 8.1 写对应条件。
- **Unknown**（超时、连接中断、响应无法解析）：不得重放原 PUT；先 GET Build，再按第七章重新构造目标 status 并与最新对象比对：
  - 已与目标 status 一致：视为写入成功，继续后续步骤；
  - 不一致：按最新对象重新计算后重试。

该流程同时适用于 `Build.status` 写入与子资源创建；子资源创建的结果未知确认另见第六章。

## 十、可观测性

日志在框架已输出的 `controller=<name> key=<key> duration=<duration> error=<error>` 之外，补充：`uid`、`resourceVersion`、`phase`、`stage`、`reason`、`result`，以及 ensure 与发布结果复制的具体原因。不记录 spec 全量与凭据。

进程内首次 reconcile 成功时输出一条启动打点：`controller=build event=reconciled-after-start key=<ns/name> phase=<phase> stage=<stage>`，用于确认恢复已开始处理；每条进程只输出一次。

指标通过 `pkg/metrics.NewCounter` 注册，命名沿用 job / runner 控制器体例：

```text
build_controller_status_update_conflicts_total
build_controller_status_update_unknown_total
build_controller_ensure_conflicts_total
build_controller_ensure_terminating_total
```

## 十一、权限与身份

Build Controller 所需最小权限：

```text
builds:         get, list
builds/status:  update
projects:       get
snapshots:      get, create
rpmrepos:       get, create
buildinfos:     get, create
```

权限严格限制在上述操作：只读 Project（`projects: get`）；不能 create / update / delete Build（不做删除级联），不能写子资源的 status，不能访问 Job / Runner。

身份侧：本轮不配置部署侧 mTLS 身份与 apiserver 授权；Build Controller 不调用发布后端、不持有发布凭据，发布后端的调用身份由 RpmRepo Controller 负责。

## 十二、重启与可用性

本控制器不保存进程内的业务状态，全部进度都持久化在 `Build.status` 与子资源对象中，因此：

- 启动时 manager 先运行 PollingSource 并通过 `waitForSync` 后才启动 worker，未完成首次同步前不产生 reconcile；
- 恢复依赖 PollingSource 的周期全量 List：重启后首轮扫描对每个非终态 Build 发 Add，之后每轮对同 key 也发 Update（周期 resync），故一个 poll 周期内即可恢复收敛；
- 该恢复能力的前提是 apiserver 支持按 `status.phase` 过滤的非终态 List（已就绪）；
- 同一 key 的重复 reconcile 由第九章的单键串行与注入幂等保证不会重复创建子资源。
- 重启会清空框架的快速重试计数与慢速指数退避状态（BaseController 的内存态）；业务状态与终态判定不受影响，但同一 key 的退避会从初始值重新开始，因此重启后可能出现短暂的更密集重试。
- 重启首轮扫描会把所有非终态 Build 一次性入队（惊群）；本轮接受该行为，规模由工作队列与 worker 数约束，不做分批或额外限速。若后续单实例承载的 Build 规模显著增大，再考虑按 resourceVersion 分批或引入入队限速。

崩溃点与重启后的结论：

| 崩溃点 | 重启后行为 |
| --- | --- |
| 崩在 ensure 之间（子资源已创建但未记录） | 首轮 reconcile GET 到已存在对象并沿用，不重复创建（见第六章） |
| 崩在 `/status` 写入之前 | 状态未变化，按当前 `Build.status` 重算并写入同一目标状态 |
| 崩在 `/status` 结果未知之后 | 按 9.4 先 GET 与目标 status 比对：一致视为成功继续，不一致重算重试；不重放原 PUT |
| 崩在读 RpmRepo 发布收口结果前后 | 重启后重新 GET RpmRepo：`release.phase` 未到 `Ready` / `Failed` 则继续等待，已到终态则按 7.5 判定并写入与未崩溃时相同的 `Build.status`；终态只写一次 |
| 崩在终态写入之前 | 按当前 phase/stage 重算，写入与未崩溃时相同的终态；终态只写一次 |

可用性：本轮仅运行单个 active controller-manager 实例，未实现 leader election。多副本不会破坏 Build 状态（依赖 `resourceVersion` 乐观并发），但会产生重复 GET、Conflict 与指标噪声，不等价于完整高可用方案。

本控制器不实现自定义 `HealthChecker`：对象是否就绪属于业务状态，不应令进程 `/healthz` 失败；Source 同步与陈旧状态由 manager 的 `/readyz` 负责。

## 十三、前置条件

- Build 校验：`Build.spec.buildType=single` 时 `Build.spec.packages` 必须且只能包含一个包，并确保该包存在于 `Project.spec.packageRepos`。
  当前 apiserver 的 validation 是无 ctx/client 的纯函数，无法读取 Project；需先把 Project 查询能力接入 Build 校验（`CreateAPIGroupInfo` 中已有 `projectES`，可注入 build storage），否则该规则无法实现。
- RpmRepo Controller：负责 repo 解析、发布与 `RpmRepo.status` 推进。Build 依赖其提供 `RpmRepo.status.release.phase` 与 `RpmRepo.status.release.contentURL`；`RpmRepoStatus` 的 `repository` / `release` / `conditions` 结构已在公共 API 落地（见 `api/ebs/v1/types.go` 与 `docs/zh/design/data-models.md`）。在该 Controller 就绪前 Build 会停在 `Processing/publish` 等待。

## 十四、测试计划

### 14.1 单元测试

- 状态机：Pending → Prepared → Processing/build → Processing/publish → 终态，覆盖各终态分支与等待态；
- 状态机守卫：非法 `(phase, stage)` 组合（如 `Processing` + 空 stage、`Pending` + `publish`）断言不调用 `UpdateBuildStatus`、对象保持原 phase/stage，并返回零值 + `controller.NewPermanentError`；
- 纯函数：`MergeCondition`（同值返回 `changed=false`、status 变化才更新 `lastTransitionTime`、reason/message 变化不误改转换时间、`observedGeneration` 取 `metadata.generation`）、buildType 分支判定；
- 时间来源：断言 `startTime` / `endTime` 取自注入的 FakeClock 且为 UTC，同一轮 `/status` 写入的两个时间戳取自同一次 `now`；断言业务代码不调用 `time.Now()`；
- 类型化客户端与 fake：NotFound、AlreadyExists、409 Conflict、resourceVersion 自增、`/status` 保留 spec、写错误三分类（NotSent / Rejected / Unknown）；
- 写入结果未知确认：模拟 Unknown 后 GET 返回"与目标一致"和"不一致"两种情况，断言分别按"视为成功继续"与"重算重试"处理，且不重放原 PUT；
- 队列结果映射（见 7.6）：逐分支断言返回值形态——Build 不存在、已终态、状态无变化与全部等待类分支（等待 Snapshot Active / BuildInfo Completed / RpmRepo 发布收口）返回零值 + `nil`；409 Conflict 返回 `ReconcileResult{RequeueAfter: 1s}` + `nil`；临时错误返回零值 + 原始错误；永久错误返回零值 + `controller.NewPermanentError`；并断言任何 `err != nil` 的分支都不带非零 `ReconcileResult`；
- ensure 幂等：GET 命中直接沿用、创建返回 409 沿用现有对象、创建结果未知先 GET 确认；
- ensure 命中删除中子资源：子资源 GET 返回带 `deletionTimestamp` 的对象时，断言不调用 `Create`、不写 `Build.status`，且返回零值 + `nil`；对象消失后下一轮重新创建；
- `baseBuildRef` 写入即返回：`Build.status.baseBuildRef` 为 nil 时断言本轮只发生一次 `/status` 写入（写 `baseBuildRef`）、不调用 `CreateSnapshot` / `CreateRpmRepo`，且返回零值 + `nil`；下一轮以已写入的 `baseBuildRef` 继续 ensure 子资源；
- Snapshot 创建写入 packageRepos：`GetProject` 返回 N 个 `PackageRepo` 时，断言 `CreateSnapshot` 收到的对象 `spec.packageRepos` 与之一致；已有 Snapshot 存在时断言不调用 `GetProject`、不调用 `CreateSnapshot`；
- Project 读取失败：`GetProject` 返回 NotFound 时断言不调用 `CreateSnapshot`、不写 `Build.status`，且返回零值 + `controller.NewPermanentError`；返回临时错误时断言零值 + 原始错误；
- 删除中守卫：`deletionTimestamp` 非空时断言不调用 `UpdateBuildStatus`、不调用 `Create`，且返回零值 + `nil`；
- PollingSource：非终态 fieldSelector 生效、Add/Update/Delete 事件映射正确、List 失败时退避重试且不替换旧快照；
- conditions 生命周期：只写 `BuildSucceed` 与 `PublishSucceed`，未确定时不出现，等待/重试阶段不写条件，`message` 取固定短语。

### 14.2 集成测试

- 发布收口映射（成功）：`RpmRepo.status.release.phase=Ready` 时，断言同一次 `/status` 写入 `Build.status.phase=Success`、`stage=publish`、`repo=RpmRepo.status.release.contentURL`、`endTime` 非空与 `PublishSucceed=True/PublishSucceeded`；
- 发布收口映射（失败）：`release.phase=Failed` 时，断言同一次 `/status` 写入 `phase=Failed`、`stage=publish`、`endTime` 非空与 `PublishSucceed=False/PublishFailed`，不写 `repo`，且不覆盖已写入的 `BuildSucceed`；
- 等待语义：`RpmRepo.status.release` 缺失或 `release.phase` 为 `Pending` / `Creating` / `Prepared` 时断言不写 `Build.status`、返回零值 + `nil`；收口完成后下一轮收敛到同一终态；
- 跳过发布：`buildType=single` 单包成功、非 single 的 `buildTarget.publishFlag=false` 两种输入都断言直接写 `Skipped` / `publish` + `endTime` + `BuildSucceed`、不写 `PublishSucceed`、不读取 RpmRepo；
- `Build.spec.buildType=single` 分支：验证创建 RpmRepo；`specStatus` 为空或首个 value 非 `Succeeded` 进入 `Failed/build`，并写 `BuildSucceed=False/BuildFailed`；
- API 交互：`/status` 保留 spec、AlreadyExists 与 Conflict 的处理、`GetLastPublishedBuild` 通过项目级 List 路径限定 Project；
- 崩溃恢复：分别模拟崩在 ensure 之间、崩在 `/status` 之前、崩在 `/status` 结果未知之后、崩在读 RpmRepo 发布收口结果前后、崩在终态写入前，断言重启后收敛到同一终态，且不产生重复子资源；
- 启动打点：断言进程内首次 reconcile 成功时只输出一次 `reconciled-after-start` 日志；
- 可观测性：断言第十章列出的指标已注册，且 Conflict、Unknown 等场景对应计数递增。

## 十五、实施顺序

1. 客户端：在 build controller 包内实现类型化 `Client` 与 fake，复用共享 client 的 `Get` / `ListProjectPage` / `Create` / `UpdateStatus`，并实现写错误三分类。
2. 纯函数：状态机判定与 `MergeCondition`。
3. Reconcile：注册 PollingSource handler，实现 Pending / Prepared / Processing-build / Processing-publish 四条分支与队列结果映射。
4. 装配：注册 `build` initializer（注入 `clock.RealClock{}`）、在 initializer 阶段校验静态配置、轮询周期沿用全局 `--poll-period`、指标与权限就位。
5. 测试：完成单元、集成与崩溃恢复测试后，再纳入默认 `--controllers=*`。

## 十六、假设

- `Build.metadata.name` 为 uuid，不存在同名重建。
- `Build.spec.buildType` 允许值为 `full` / `incremental` / `specified` / `single`；控制器只对 `single` 特判，其余取值（含未枚举值）按非 single 宽容处理，不进入失败终态。
- `Build.spec.buildType=single` 时，`Build.spec.packages[]` 恰好一个包；单包构建失败进 `Failed/build`，构建成功由本控制器直接判定跳过发布、写 `Skipped/publish`（不写 `PublishSucceed`）。
- 终态集合固定为 `Success` / `Failed` / `Aborted` / `Skipped`。
- RpmRepo 无论 single 与否都在 Pending 阶段 ensure；是否跳过发布由 Build 依据 `buildType=single`（单包成功）与 `buildTarget.publishFlag=false` 判定，不由 RpmRepo 决定。
- RpmRepo 无对象终态（`repository.phase` 与 `release.phase` 均可继续变化）；Build 依据 `RpmRepo.status.release.phase` 为 `Ready` / `Failed` 判定发布已收口，并据此写入 `Success/publish` / `Failed/publish`；未收口期间 Build 停在 `Processing/publish` 等待。
- BuildInfo 由独立 BuildInfo Controller 填充与推进。
- 本轮不将软件包构建失败作为 Build 终态触发（`Build.spec.buildType=single` 分支除外）；非 `single` 场景 `BuildSucceed=False` 只表示包未完全构建成功，不阻断发布。
- `Build.status.conditions` 只承载构建/发布结果：运行中条件允许为空或仅含已确定结果，不代表失败；阶段进度以 `Build.status.phase` / `Build.status.stage` 为准。
- `BuildSucceed=False` 在 `BuildInfo.status.specStatus` 为空/缺失或存在 `build.status` 非 `Succeeded` 时写入；但非 single 包级构建失败仍不触发 Build 终态（见上一条）。
- `ebs.io/target-os`、`ebs.io/target-arch`、`ebs.io/build-type` 由创建方或 apiserver 补齐，且必须与 spec 一致；`GetLastPublishedBuild` 依赖 `ebs.io/target-os` / `ebs.io/target-arch` 两个 label 与 `status.phase` / `status.stage` 字段，`PollingSource` 只依赖 `status.phase` 字段，不依赖任何 label。
- 非法 `(phase, stage)` 组合不写 status、不进入终态，只能由外部或人工修正；每轮 resync 会重新入队，该错误会周期性重复记录；与「`Aborted` 由外部中止流程写入」的假设一致。
- 控制器不级联删除子资源；命中正在删除的子资源时按未就绪等待（不重建、不覆盖、不删除），待该对象消失后由 PollingSource 周期 resync 触发重建。
- 重启首轮扫描的全量入队（惊群）被显式接受：不做分批或限速，规模由工作队列与 worker 数约束；限速/分批属后续优化项。
- 本轮不配置部署侧 mTLS 身份与 apiserver 授权，联调与投产前再补。
- 本轮不设未就绪超时。
- 本轮部署仅单副本运行，多副本选举与故障转移延后至后续版本。
