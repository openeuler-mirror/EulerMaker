# Build Controller 设计

## 一、定位与范围

Build Controller 根据 Snapshot、BuildInfo 和 RpmRepo 的状态推进 Build 生命周期。

职责：

- 周期 List 轮询 Build，将 `Build.status.phase=Pending` 收敛到终态 `{Success, Failed, Aborted, Skipped}`；
- 按 `Build.status.phase` 所处阶段幂等 ensure 子资源：`Snapshot`、`RpmRepo`、`BuildInfo`；
- 消费 Snapshot / BuildInfo / RpmRepo 子资源状态，按 `RpmRepo.status.release.phase`（`Ready` / `Failed`）推进 `Build.status` 终态；
- 只向 `Build.status.conditions` 中写入 `BuildSucceed` 与 `PublishSucceed`，分别表示构建是否成功与发布是否成功；
- reconcile 可重入并支持重启恢复：沿用已存在的子资源，不回退状态；观察到终态后不再更新。
- 在 build controller 包内实现类型化 Project-scoped 客户端与 fake，供 reconcile 与单元测试使用；
- 提供结构化日志和指标。

职责边界：

- 不直接参与依赖解析、Job 执行或 repo 发布实现（repo 文件生成、createrepo 等由 RpmRepo Controller 负责）；
- 不直连 etcd、Elasticsearch 或 Artifact Manager；所有资源访问只经包内 `Client` 到达 ebs-apiserver；
- 不推进 `Project.status`、`Snapshot.status`、`BuildInfo.status`、`RpmRepo.status`；不填充 `BuildInfo.spec.specDepends` 与 `BuildInfo.status.specStatus`；
- 不修改 `Build.spec`；不创建或维护 `Build.metadata.labels`；
- 非单包构建的包级失败仅记录为 `BuildSucceed=False`，不直接触发 Build 终态；未就绪超时见第十六章，删除处理见 7.1。
- 不负责把任何状态推进为 `Aborted`；`Aborted` 是外部中止流程写入的终态（触发中止不在本版本范围），Build Controller 只将其识别为终态并停止处理；
- 不给 `Snapshot` / `BuildInfo` / `RpmRepo` 注册轮询或 Watch 源，其状态只在 reconcile 内按确定性名称读取；
- 不写中间等待条件：运行/等待阶段 `Build.status.conditions` 不维护占位值，进度以 `phase` / `stage` 表达；

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
- Controller 配置项：轮询周期沿用框架全局 `--poll-period`（默认 30s），worker 数沿用框架配置。
- 配置校验时机：所有静态配置（apiserver 地址与 CA、请求超时）必须在 initializer 阶段完成校验；配置非法时 controller-manager 直接启动失败，不得延迟到单个 Build 的 reconcile 中处理，也不通过 HealthChecker 表达。

### 2.2 组件依赖与框架接口

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

组件边界遵循第一章：所有资源访问经包内 Client 到达 ebs-apiserver，子资源状态由各自 Controller 推进；Build Controller 只读取发布结果，不调用发布后端。

**框架接口与注册**

- 分页 List 的 fieldSelector 通过 `PollingSourceFactory.ForResource(gvr, period, options)` 注入；
- 项目级 List 与子资源创建使用共享 client 的 `ListProjectPage` 与 `Create`；
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
    // GetProject 读取 Project；用于构造 Snapshot.spec.defaultRef 与 packageRepos。
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

仓库选择规则：`Build.spec.buildType=single` 时，以 `Build.spec.packages` 的去重名称集匹配 `Project.spec.packageRepos[].name`，只复制匹配条目，保持 Project 列表中的相对顺序；不补入其他仓库，也不为未找到的名称伪造 PackageRepo。所选条目完整 DeepCopy，包括 URL、ref 和 buildTargets。其他 buildType 复制完整仓库列表。Snapshot.spec.defaultRef 始终从同一次 Project GET 复制，包 ref 为空时仍保持为空。初次创建与 NotFound 后重建使用同一选择规则；已存在对象不重新筛选或覆盖。

正常 single 输入应满足第十三章前置校验；若创建 Build 后 Project 变化导致目标缺失，只复制实际命中的条目，缺失目标沿用 Snapshot Controller 的整体 condition 处理规则，不替换成全量仓库。

Build Controller 创建 Snapshot 时，从同一次 `GetProject` 的返回对象复制 `Project.spec.defaultRef`，并按下述规则选择 `Project.spec.packageRepos`，分别写入 `Snapshot.spec.defaultRef` 和 `Snapshot.spec.packageRepos`（Project 名取 `Build.metadata.namespace`），不能分别读取两次 Project 混合不同版本的输入。复制 packageRepos 时使用 DeepCopy，原样保留仓库 ref（包括空 ref），由 Snapshot Controller 解析时回退到 Snapshot.spec.defaultRef；不写 `Snapshot.status.packageRepoStatuses`，该字段由 Snapshot Controller 按仓库 ref 逐项解析后写入。Build Controller 只等待 `Snapshot.status.phase=Active`；已存在的 Snapshot 不读 Project、不覆盖 spec，也不补写 defaultRef。

```yaml
apiVersion: ebs/v1
kind: Snapshot
metadata:
  name: ${build.metadata.name}
  namespace: ${build.metadata.namespace}
spec:
  defaultRef:    # 复制同一 Project 的 spec.defaultRef
    type: Branch
    value: master
  packageRepos:   # single 仅复制 Build.spec.packages 命中的仓库；其他类型复制全量
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
| `Snapshot.spec.defaultRef`             | Build Controller    | 创建时复制 `Project.spec.defaultRef`；与 packageRepos 来自同一次 Project GET；已有 Snapshot 不覆盖 |
| `Snapshot.spec.packageRepos`           | Build Controller    | 创建时 single 按 `Build.spec.packages` 筛选并复制；其他类型复制全部 `Project.spec.packageRepos`；已存在对象不覆盖 |
| `Snapshot.status.packageRepoStatuses`  | Snapshot Controller | 按 `Snapshot.spec.packageRepos` 逐项解析写入 |
| `Snapshot.status.phase`                | Snapshot Controller | Pending → Processing → Active                              |

### 4.3 RpmRepo

RpmRepo 的相位与发布收口语义以 RpmRepo-Controller 为准，RpmRepo 没有对象终态；Build Controller 只消费下表字段。`RpmRepoStatus` 的结构（`repository` / `release` / `conditions`）与 `docs/zh/design/data-models.md` 一致。

骨架形态（仅非 single，Build Controller 在 Pending 阶段 ensure）：

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

single 的 BuildInfo 只包含目标仓库解析出的 spec，不包含其他仓库或依赖仓库的 spec；一个目标仓库可以包含多个 spec，不能假设 specStatus 只有一个条目。BuildInfo Controller 保证进入 Completed 时目标 spec 的结果齐全，不得只记录已成功的部分 spec 就宣布完成。Build Controller 在 Completed 后按 7.4 的统一规则汇总结果。

**single 的执行契约**：Build Controller 仅创建 Snapshot 和 BuildInfo，不创建、读取或恢复本轮同名 RpmRepo。BuildInfo Controller 在 Snapshot Active 后解析目标包，使用 `Build.spec.bootstrapRepo` 和 `Build.status.baseBuildRef.name` 指向的历史 Build 的过程仓下发 Job，然后直接汇总 Job 的构建结果。single 的 `BuildInfo.status.phase=Completed` 不以生成本轮过程仓、Job 被 RpmRepo Controller 消费或正式发布完成为前提；产物仍通过 Runner / Artifact Manager 上传与保存。

历史 Build 按 3.1 的查询规则选择。BuildInfo Controller 读取该历史 Build 同名 RpmRepo 中已经可用的过程仓版本及不可变 contentURL，不使用会随正式发布切换的 `Build.status.repo` 稳定入口。下发 Job 时固化该次使用的仓库 URL，后续历史仓版本推进不修改已创建的 Job；不生成新的过程仓。baseBuildRef 为 `{}` 时仅使用 bootstrapRepo；baseBuildRef 非空但历史仓缺失或不可读时，不重建历史对象、不静默改用其他版本，按依赖读取错误处理，不下发 Job。

状态推进见第五章；非法阶段组合由 7.1 的入口守卫处理。

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
| `Pending` / 空 | ensure Snapshot；非 single 才 ensure RpmRepo；等待 `Snapshot.status.phase=Active` | `Snapshot.status.phase=Active` 后：`Prepared` / 空 |
| `Prepared` / 空 | ensure BuildInfo | `Processing` / `build` |
| `Processing` / `build` | 等待 `BuildInfo.status.phase=Completed`；完成后按以下分支处理 | — |
|  | `Build.spec.buildType=single` 且构建失败（汇总规则见 7.4） | `Failed` / `build` |
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
- Build 不校验 `release.contentURL` 是否非空，发布成功时按原值复制。

## 六、子资源 ensure

`ensure <Object>` 用于幂等创建或获取 Snapshot / RpmRepo / BuildInfo：

1. **按确定性名称 GET**
   - 成功且 `metadata.deletionTimestamp` 为空：沿用现有对象，不修改 spec。
   - 成功但 `metadata.deletionTimestamp` 非空：视为该子资源不可用；本轮不推进（调用方按 7.6 「队列结果映射」的等待类返回零值 + `nil`），不重建、不覆盖、不删除，并记录日志与 `build_controller_ensure_terminating_total` 指标。
   - NotFound：进入创建流程。
2. **创建对象**
   - 按约定 spec 执行 POST。
   - POST 409（AlreadyExists）：优先按创建冲突处理，GET 后沿用现有对象，不修改 spec；不直接返回通用 Conflict 的 `RequeueAfter: 1s`。GET 成功后仍检查 deletionTimestamp：未删除则沿用并继续本阶段，删除中则按第 1 步等待；GET NotFound 表示并发删除，返回 `RequeueAfter: 1s`，下轮重新 ensure，本轮不再次 POST；其他 GET 错误按读取错误分类返回。
3. **结果未知处理**
   - 网络错误、5xx、超时导致结果未知时：
     - 先 GET 确认实际结果；
     - 存在则沿用，不存在则返回可重试错误。

ensure 不推进子资源状态、不等待子资源就绪、不覆盖已存在 spec、不删除其他 Build 的对象。

**子对象缺失统一重建**：所有阶段读取本轮需要的同名子对象均通过 ensure（single 不包含 RpmRepo；历史基础 RpmRepo 只读，不重建），GET 返回 NotFound 不作为业务失败或永久错误，而是按第四章重新创建初始骨架。重建不回退 Build.phase/stage，不清空 baseBuildRef、时间或已确定的 conditions，也不复制已删除对象的旧 status；新对象的解析、构建与仓库恢复由对应子 Controller 负责。阶段判断使用 ensure 返回的服务端对象，不在 ensure 后无条件重复 GET；若确需再次读取且返回 NotFound，仍进入 ensure，不沿用先前读取的状态。

| 阶段 | ensure 顺序与就绪判断 |
| --- | --- |
| Pending（baseBuildRef 已固化） | Snapshot → RpmRepo（仅非 single）；Snapshot Active 后进入 Prepared |
| Prepared | Snapshot → RpmRepo（仅非 single）；Snapshot Active 后 ensure BuildInfo，再进入 Processing/build |
| Processing/build | Snapshot → RpmRepo（仅非 single）；Snapshot Active 后 ensure BuildInfo，再等待 BuildInfo Completed |
| Processing/publish（仅非 single） | Snapshot → RpmRepo（仅非 single）；Snapshot Active 后 ensure BuildInfo；BuildInfo Completed 后读取本轮 ensure 得到的 RpmRepo 发布结果 |

前序依赖同样按上述顺序恢复；任一对象仍在删除中或前序依赖未就绪时，本轮不推进 Build，返回零值 + nil，下一轮周期 resync 继续。Processing/publish 恢复 BuildInfo 后不重新计算或改写已确定的 BuildSucceed。第六章 POST AlreadyExists / Unknown 后的确认 GET 若为 NotFound，仍按各自规则结束本轮，下轮重建，不在一个周期内循环 POST。

Snapshot 的创建需要先读 Project：仅当 Snapshot NotFound 时才 `GetProject(Build.metadata.namespace)`，并以同一返回对象的 `spec.defaultRef` 和按 4.2 筛选后的 `spec.packageRepos` 构造 Snapshot spec 后创建；已有 Snapshot 时不读 Project、不覆盖 spec。`GetProject` 返回 NotFound 时按永久错误处理，不创建子资源、不写 `Build.status`。

Snapshot 删除后无法从本控制器恢复原先固化的输入，重建沿用上述创建规则，复制当时读取的 Project（single 仍只复制 Build.spec.packages 指定的仓库）；若 Project 已变化，新 Snapshot 的输入和解析结果可能与删除前不同。重建是恢复资源及继续调和，不保证复原被删除对象的历史内容。

## 七、Reconcile 流程

### 7.1 主流程

统一 reconcile 主循环按以下顺序执行：

1. 读取最新 Build。
2. 若 Build 已处于 Success、Failed、Aborted 或 Skipped 终态，本轮直接成功返回、不再处理（`Aborted` 由外部中止流程写入，reconcile 不负责推进）。
3. 若 `Build.metadata.deletionTimestamp` 非空（对象正在删除），本轮直接返回零值 + `nil`：不写 `Build.status`、不 ensure 子资源、不读取 RpmRepo 发布收口结果；已创建的子资源不做级联删除。服务端没有 `metadata.deletionTimestamp` 的 fieldSelector 映射，删除中的非终态 Build 仍会出现在 List，因此该守卫必须在 reconcile 入口判断。
4. 校验 `(phase, stage)` 组合，合法集合为 `{(Pending,""), (Prepared,""), (Processing,"build"), (Processing,"publish")}`：
   - `buildType=single` 不允许 `(Processing,"publish")`：记录并返回永久错误，不写 status、不创建 RpmRepo。
   - 非终态 phase 但不在 Pending / Prepared / Processing 内：记录未知 phase 错误，返回永久错误。
   - phase 合法但组合不在合法集合内（例如 `Processing` + 空 stage、`Pending` + `publish`）：记录错误，返回永久错误，**不写 status、不推进终态**。非法组合只能由外部或人工修正；每轮 resync 会重新入队，因此该错误会周期性重复记录。
5. 根据状态机确定当前 phase/stage 对应的处理阶段。
6. 执行对应阶段处理逻辑。
7. 阶段处理结果与错误统一按 7.6 返回；写入结果未知先按 9.4 确认。
8. 若状态没有变化，不调用状态更新 API。

状态写入原子性：

- 每次状态迁移先构造完整的目标 `Build.status`，再调用一次 `UpdateBuildStatus` 提交。
- `7.2` 至 `7.5` 中“写：”块列出的字段是本次写入需要合并的字段清单，不是多次独立 API 调用。
- `Build.status.conditions` 的变更也合并到同一次 `/status` 写入，不单独更新 condition。
- 写入结果未知（超时、连接中断、响应无法解析）时不重放原 PUT，按第九章「写入结果未知确认」处理。

后续各小节只描述阶段内部逻辑；子对象的读取、创建及恢复统一遵循第六章。

### 7.2 Pending

- 若 `Build.status.baseBuildRef` 为 nil：
  - 调用 `GetLastPublishedBuild(project, os, arch)`：
    - 若结果为空，写 `Build.status.baseBuildRef={}`；
    - 否则写：
      - `Build.status.baseBuildRef.name`（上一个发布成功 Build 的 `Build.metadata.name`）
      - `Build.status.baseBuildRef.repo`（上一个发布成功 Build 的 `Build.status.repo`）
  - 写入后立即返回零值 + `nil`，`ensure Snapshot` / `ensure RpmRepo` 与后续等待从下一轮继续；`{}` 与实际值同样视为已处理。
- 按第六章 Pending 行 ensure 依赖并等待 Snapshot Active。
- 满足后写 `Build.status.phase=Prepared`。

### 7.3 Prepared

- 按第六章 Prepared 行 ensure 依赖，就绪后执行以下写入。
- 写：
  - `Build.status.phase=Processing`
  - `Build.status.stage=build`
  - `Build.status.startTime`

### 7.4 Processing/build

- 按第六章 Processing/build 行 ensure 依赖，等待 BuildInfo Completed。
- 统一汇总规则（single 与非 single 共用）：`specStatus` 非空且全部 `value.build.status == Succeeded` 才判定成功；空 map 或任一状态非 Succeeded（含缺失、未完成、Failed、Aborted）均判定失败。结果不依赖 map 遍历顺序，目标 spec 的完整性由 4.4 的 Completed 契约保证。
- 若 `Build.spec.buildType=single`：
  - 构建失败：
    - 写：
      - `Build.status.phase=Failed`
      - `Build.status.stage=build`
      - `Build.status.endTime`
      - `BuildSucceed=False/BuildFailed`
  - 构建成功（跳过发布）：
    - 写：
      - `Build.status.phase=Skipped`
      - `Build.status.stage=publish`
      - `Build.status.endTime`
      - `BuildSucceed=True/BuildSucceeded`
- 否则：
  - 按上述汇总结果写 `BuildSucceed`：成功为 `True/BuildSucceeded`，失败为 `False/BuildFailed`。
  - `BuildSucceed` 仅记录所有包是否完全构建成功，不作为发布门禁。
  - 若 `Build.spec.buildTarget.publishFlag=false`（跳过发布），写：
    - `Build.status.phase=Skipped`
    - `Build.status.stage=publish`
    - `Build.status.endTime`
    - `BuildSucceed`（取上一步计算结果）
  - 否则写：
    - `Build.status.stage=publish`
    - `BuildSucceed`（取上一步计算结果）
    - `Build.status.phase` 保持 `Processing`，等待 RpmRepo 发布收口。

跳过发布的两个分支均保持 `Build.status.repo` 为空，不写 `PublishSucceed`，不等待发布收口；阶段入口的依赖 ensure 仍遵循第六章。

### 7.5 Processing/publish

- 按第六章 Processing/publish 行 ensure 依赖，就绪后使用返回的 RpmRepo 判定发布收口。
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


### 7.6 错误分类与队列结果映射

`Sync` 返回 `(ReconcileResult, error)`，BaseController 据此决定该 key 的重入方式；`Requeue` 与 `RequeueAfter` 不得同时非零。各分支的映射如下：

| 场景 | 返回 |
| --- | --- |
| Build 不存在、已终态、状态无变化 | 零值 + `nil` |
| 写入 `baseBuildRef` 后本轮返回、等待 `Snapshot.status.phase=Active`、`BuildInfo.status.phase=Completed`、RpmRepo 发布收口（`RpmRepo.status.release.phase` 为 `Ready` / `Failed`） | 零值 + `nil`（依赖 PollingSource 周期 resync 复查） |
| 子资源 POST 返回 409 AlreadyExists（优先匹配） | 按第六章 GET 后沿用；返回后续阶段结果，不直接套用通用 Conflict 重入；GET NotFound 时延迟 1s 重新 ensure |
| `/status` 或其他写入返回 409 Conflict（不含上述 AlreadyExists） | `ReconcileResult{RequeueAfter: 1s}` + `nil`（下轮重新 GET 并按第七章重算目标 status，不使用立即重入，避免持续冲突形成热循环） |
| API 临时错误（408、429、5xx、网络错误或超时；写入 Unknown 先执行确认流程） | 零值 + 原始错误（BaseController 走 `AddRateLimited`；超过 `--controller-max-retries` 后转慢速指数退避并持续重入） |
| 429 / 503 且响应带 `Retry-After` | 零值 + 携带 RetryAfter 的 `client.WriteError`（框架按该延时重入） |
| API/配置永久错误、非法 `(phase, stage)`、输入或客户端错误 | 不写业务终态或失败条件；记录错误并返回零值 + `controller.NewPermanentError(err)`（框架 Forget 仅清除本次队列退避；后续仍会被 PollingSource 周期 resync 重新入队） |
| 构建或发布确定性失败，且终态与条件已通过一次 `/status` 写入 | 零值 + `controller.NewPermanentError(err)`（终态即该 key 的终点） |
| 写入结果未知 | 先按 9.4「写入结果未知确认」确认，再按确认结果落到上述对应行返回 |

约定：

- NotFound 按资源与操作语义处理；AlreadyExists 优先于通用 Conflict。API 永久拒绝（400/401/403/422）、配置、输入及客户端错误不得写业务终态或失败条件；静态配置在 2.1 的 initializer 阶段校验失败即停止启动。
- 只有 7.4 / 7.5 已确认的构建或发布失败才写 Failed；终态及条件写入成功（包括 Unknown 确认成功）后返回业务 PermanentError，写入失败则返回写错误分类结果。
- `err != nil` 时不返回非零 `ReconcileResult`，避免框架记录 `invalid-result-with-error`；
- "等待"类分支统一用零值 + `nil`，不用 error 表达正常等待，避免污染错误指标并叠加额外退避；
- 只有需要比轮询周期更快复查时才使用 `RequeueAfter`。


## 八、条件与时间

### 8.1 Conditions 生命周期

`Build.status.conditions` 只记录构建与发布结果，不维护快照、构建信息、RPM 仓库等中间就绪状态；运行中的进度由 `Build.status.phase` / `Build.status.stage` 表达。只允许以下两个 type，结果确定前对应条件不出现，每轮 reconcile 内容变化时才通过 `/status` 写入：

| type             | 含义           | `status="True"` 的写入时机                                     | `status="False"` 的写入时机                                    |
| ---------------- | -------------- | ----------------------------------------------------------- | ------------------------------------------------------------ |
| `BuildSucceed` | 构建是否成功 | BuildInfo Completed 后，按 7.4 汇总成功（reason/message 为 BuildSucceeded） | BuildInfo Completed 后，按 7.4 汇总失败（reason/message 为 BuildFailed） |
| `PublishSucceed` | 发布是否成功   | `RpmRepo.status.release.phase=Ready`（本控制器写 `reason/message=PublishSucceeded`） | `RpmRepo.status.release.phase=Failed`（本控制器写 `reason/message=PublishFailed`）；`release.phase` 未到终态时不写 |

各阶段写入规则：

- Pending/Prepared、Processing/build 与 Processing/publish 的等待/运行阶段：不写任何条件。
- 构建和发布阶段按上表记录结果；PublishSucceed 直接依据 release.phase，不从 RpmRepo.status.conditions 复制。
- 跳过发布时不产生发布结果条件，具体写入清单见 7.4。
- 条件一旦写入即保留到终态，不因后续阶段变化删除或改写；`reason` 固定为上述四个值，不再细分中间原因。

condition merge helper：

```go
func MergeCondition(
    conditions []metav1.Condition,
    condType string,
    status metav1.ConditionStatus,
    reason, message string,
    observedGeneration int64,
    now metav1.Time,
) ([]metav1.Condition, bool)
```

- 按 `type` 定位并更新 `status`、`reason`、`message`、`observedGeneration`；
- `observedGeneration` 取 `Build.metadata.generation`（apiserver 仅在 spec 变化时递增该值）；
- `message` 写与 `reason` 同族的固定短语：`BuildSucceeded` / `BuildFailed` / `PublishSucceeded` / `PublishFailed`；动态错误详情只进日志，不写入 condition；
- `now` 由调用方传入，取本轮 Sync 唯一一次 `clock.Now()` 的 UTC 值（见 8.2）；helper 是纯函数，不持有 Clock，也不调用 `time.Now()`；
- 首次插入该 type 时设置 `lastTransitionTime=now`，返回 `changed=true`；
- 已有该 type 且 `status` 变化时设置 `lastTransitionTime=now`；`status` 不变时保留原时间，即使 reason、message 或 observedGeneration 变化也不更新时间；
- 结果按 `type` 排序；
- 所有目标字段无变化时保留原 `lastTransitionTime` 并返回 `changed=false`，即使传入的 now 不同也不产生变化；Controller 在其他 status 字段也无变化时跳过 `/status` 写入。

### 8.2 时间来源

本控制器所有主动时间来源都是构造时注入的 `clock.Clock`：

- `Build.status.startTime` 在进入 Processing 时写入，取本轮 `now`；
- `Build.status.endTime` 在进入终态时写入，取同一次调谐的 `now`；
- 新建 condition 或 condition.status 变化时，`lastTransitionTime` 使用传给 MergeCondition 的同一个 `now`；
- 已有的 `Build.status.startTime` / `endTime` 是 apiserver 中的业务数据，Controller 只读取，不使用本地时间覆盖或重写。

每次 `Sync` 开始时只调用一次 `clock.Now()` 并保存为 `now`，本周期内所有新时间戳复用该值；写入 API 消耗的时间不重新计入本周期，下一次调谐再取新的 `now`。写入 `metav1.Time` 时统一使用 `metav1.NewTime(now.UTC())`，持久化时间一律 UTC。

本控制器不设未就绪超时，见第十六章。

测试必须通过 FakeClock 推进时间，不使用真实 `sleep`，也不依赖真实时钟。

## 九、并发与一致性

### 9.1 单键串行与共享状态

BaseController 保证同一队列键 `{namespace}/{name}` 在任一时刻只由一个 worker 调和；不同 Build 可以并发。本控制器不维护跨 key 的共享业务状态，队列键之外没有需要加锁的内存结构。

### 9.2 乐观并发

Build、子控制器与外部中止流程都可能更新同一对象。本控制器必须：

- 写入前 GET 最新 Build，基于最新对象的 `resourceVersion` 构造目标 status；
- `/status` 冲突按 7.6 结束本轮并延迟重入，禁止用旧对象重放；
- 不直接修改 `Build.spec`，也不写 `Build.metadata`（标签由创建方或 apiserver 维护）。

### 9.3 与子控制器的竞态

- 本控制器只写 `Build.status`；`Snapshot.status` / `BuildInfo.status` / `RpmRepo.status` 由各自子控制器写入。
- `ensure` 按确定性名称 GET 后创建，`AlreadyExists` 视为沿用现有对象，不覆盖已存在 spec（见第六章）。
- Build 进入终态后本轮直接返回，不再产生新的子资源写入。

### 9.4 写入结果未知确认

写入失败必须区分三种结果，不能一律重试：

- **NotSent**（请求未发出）：直接返回可重试错误。
- **Rejected**（收到明确响应）：按 7.6 分类处理，不执行 Unknown 确认。
- **Unknown**（超时、连接中断、响应无法解析）：不得重放原 PUT，也不得用下一阶段重新计算的目标确认上一笔写入；按下述规则确认原写入意图。

**原写入意图与比较范围**

- 每次调用 `UpdateBuildStatus` 前，在本轮 reconcile 内保存独立、不可变的写入意图：Build 的 namespace/name、UID，以及本次需要达到的目标字段和值。
- 目标字段包括目标 `phase` / `stage`，以及本次写入的 `baseBuildRef`、`startTime`、`endTime`、`repo` 和按 type 定位的条件（以 7.2～7.5 的写入清单为准）。比较本次目标字段，不比较整个对象、整个 status 或其他未涉及的条件；不要求 `resourceVersion` 与请求相同。
- 条件比较其所有目标字段，按 type 匹配、不依赖列表顺序；时间按 API 序列化精度比较，不重新调用时钟。`baseBuildRef=nil`（未解析）与 `{}`（已解析但没有基础 Build）必须区分。

**确认与返回**

Unknown 后使用仍有效的 reconcile context GET 最新 Build，只执行一次确认读取，本轮不再发起状态写入或 ensure 子资源：

| GET 结果 | 处理与返回 |
| --- | --- |
| 同一 UID，全部目标字段已达到原写入意图 | 确认成功，结束本轮；返回原阶段成功写入后的结果。正常推进为零值 + `nil`；已确认写入构建/发布失败终态时，沿用 7.6 的业务失败返回，不返回原 Unknown 错误 |
| 同一 UID，但目标字段不一致，且对象仍非终态、未删除 | 不确认成功，也不重放原请求；返回 `ReconcileResult{RequeueAfter: 1s}` + `nil`，下一轮从入口 GET 最新对象，重新检查守卫并计算当前阶段动作 |
| NotFound、UID 不同，或目标不一致但对象已终态/正在删除 | 结束旧周期，返回零值 + `nil`；不得覆盖外部 Aborted 或更新替代对象 |
| GET 失败 | 按读取错误分类返回：临时错误退避、永久错误返回 `controller.NewPermanentError`；不发起补偿写入。context 已取消时不创建脱离 Manager 生命周期的确认任务，交由后续轮询或重启恢复 |

例如 `Pending → Prepared` 已提交但响应丢失，GET 得到 `Prepared` 且本次目标字段一致，即确认该迁移成功并结束本轮；不能再推导 `Processing/build` 作为本次确认目标。

写入意图仅是本轮内存态，不跨周期缓存、不持久化。进程重启后无法确认崩溃前的原意图，也不需要恢复原请求：由 PollingSource 重新入队，GET 持久化对象后按第七章正常调和，已终态则结束。

子资源创建沿用相同的写错误分类，但按第六章的 create-only ensure 规则确认存在性并沿用对象，不使用上述 Build status 字段比较。

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
- 重启后，PollingSource 首轮成功扫描对每个非终态 Build 发 Add，之后每轮对同 key 发 Update，恢复调和入队。完成收敛的时间取决于队列积压、API 可用性及子资源进度，不保证在一个 poll 周期内完成。
- 非终态扫描依赖 apiserver 的 `status.phase` 过滤能力。
- 同一 key 的重复 reconcile 由第九章的单键串行与注入幂等保证不会重复创建子资源。
- 重启会清空框架的快速重试计数与慢速指数退避状态（BaseController 的内存态）；业务状态与终态判定不受影响，但同一 key 的退避会从初始值重新开始，因此重启后可能出现短暂的更密集重试。
- 重启首轮成功扫描将所有非终态 Build 入队，由工作队列与 worker 数控制处理并发，不做分批或额外入队限速。

崩溃点与重启后的结论：

| 崩溃点 | 重启后行为 |
| --- | --- |
| 崩在 ensure 之间（子资源已创建但未记录） | 首轮 reconcile GET 到已存在对象并沿用，不重复创建（见第六章） |
| 崩在 `/status` 写入之前 | 状态未变化，按当前 `Build.status` 重算并写入同一目标状态 |
| 崩在 `/status` 结果未知之后 | 原写入意图随进程丢失；重启后 GET 持久化 Build，按当前 phase/stage 正常调和，已终态则结束；不恢复旧请求、不比较重新生成的目标来确认旧写入 |
| 崩在读 RpmRepo 发布收口结果前后 | 重启后按 7.5 ensure 子对象并恢复前序依赖，再检查 RpmRepo：`release.phase` 未到 `Ready` / `Failed` 则继续等待，已到终态则按 7.5 判定并写入与未崩溃时相同的 `Build.status`；观察到终态后不再更新 |
| 崩在终态写入之前 | 按当前 phase/stage 重算，写入与未崩溃时相同的终态；观察到终态后不再更新 |

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
- 纯函数：`MergeCondition`（首次插入与 status 变化使用传入 now；同值且 now 变化仍返回 `changed=false`；仅 reason/message/observedGeneration 变化返回 `changed=true` 但保留原转换时间；`observedGeneration` 取 `metadata.generation`）、buildType 分支判定；
- 时间来源：断言 `startTime` / `endTime` / 新写入的 `lastTransitionTime` 取自注入的 FakeClock 且为 UTC，同一轮所有新时间戳复用同一次 `now`；断言业务代码及 helper 不调用 `time.Now()`；
- 类型化客户端与 fake：NotFound、AlreadyExists、409 Conflict、resourceVersion 自增、`/status` 保留 spec、写错误三分类（NotSent / Rejected / Unknown）；
- 写入结果未知确认：模拟 `Pending → Prepared` 响应丢失后 GET 返回 Prepared，断言按原意图确认成功并结束本轮，不生成 Processing 目标、不 ensure 后续子资源；覆盖正常推进、baseBuildRef（含 nil/空对象区别）、时间序列化精度和失败终态的返回结果；
- 确认比较与异常：目标字段缺失或不一致时返回 `RequeueAfter: 1s`，下一轮重新 GET 调和；resourceVersion、无关 status 字段或条件顺序变化不影响确认；GET 返回 NotFound、不同 UID、外部 Aborted、删除中对象时不写入；GET 失败或 context 取消时不重放 PUT、不创建后台确认任务；
- 队列结果映射（见 7.6）：逐分支断言返回值形态——Build 不存在、已终态、状态无变化与全部等待类分支（等待 Snapshot Active / BuildInfo Completed / RpmRepo 发布收口）返回零值 + `nil`；409 Conflict 返回 `ReconcileResult{RequeueAfter: 1s}` + `nil`；临时错误返回零值 + 原始错误；永久错误返回零值 + `controller.NewPermanentError`；并断言任何 `err != nil` 的分支都不带非零 `ReconcileResult`；
- ensure 幂等：GET 命中直接沿用、创建返回 409 沿用现有对象、创建结果未知先 GET 确认；
- AlreadyExists 优先级：POST 返回 AlreadyExists 后 GET 命中正常对象，断言沿用并继续阶段，不覆盖 spec、不再次 POST、不直接返回通用 Conflict 重入；GET 命中删除中对象则等待，GET NotFound 则延迟 1s，其他读取错误按分类返回；
- 永久错误与业务失败隔离：覆盖 API 400/401/403/422、非法输入和客户端错误，断言不调用 UpdateBuildStatus、不写失败条件，只记录并返回 PermanentError；对照覆盖 single 构建失败和 release.phase=Failed，只有业务结论确认后才写 Failed，且状态写入失败不得被业务 PermanentError 覆盖；静态配置非法在 initializer 阶段启动失败；
- ensure 命中删除中子资源：子资源 GET 返回带 `deletionTimestamp` 的对象时，断言不调用 `Create`、不写 `Build.status`，且返回零值 + `nil`；对象消失后下一轮重新创建；
- 子对象缺失恢复：按第六章阶段表逐项覆盖 Snapshot / RpmRepo / BuildInfo 的 NotFound，断言按依赖顺序创建初始骨架，重建本身不回退 Build.phase/stage、不清空已确定的条件或时间；新子对象未就绪时等待，对应 Controller 推进后恢复收敛。Processing/publish 重建 BuildInfo 不重算已有 BuildSucceed；已有子对象不覆盖 spec/status；终态或删除中的 Build 不触发重建；
- single 依赖边界：只 ensure Snapshot / BuildInfo；本轮没有 RpmRepo 不影响正常推进，single + Processing/publish 返回永久错误且不创建 RpmRepo。与 BuildInfo Controller 联调时覆盖 bootstrap-only、bootstrap 加历史不可变过程仓、历史仓不可读时不下发 Job，以及 Job 完成后无需仓库物化即可完成 BuildInfo；
- `baseBuildRef` 写入即返回：`Build.status.baseBuildRef` 为 nil 时断言本轮只发生一次 `/status` 写入（写 `baseBuildRef`）、不调用 `CreateSnapshot` / `CreateRpmRepo`，且返回零值 + `nil`；下一轮以已写入的 `baseBuildRef` 继续 ensure 子资源；
- Snapshot 创建复制输入：覆盖 Project.defaultRef 为 Branch/Tag，断言只调用一次 `GetProject`，非 single 的 `CreateSnapshot` 收到的 `spec.defaultRef` 和 N 个 `packageRepos` 均与该返回对象一致；single 只包含 Build.spec.packages 命中的仓库，defaultRef 不变，修改构造出的 Snapshot 不影响 Project；已有 Snapshot（含缺少 defaultRef 的对象）存在时，不调用 `GetProject`、不调用 `CreateSnapshot`、不覆盖 spec；
- Project 读取失败：`GetProject` 返回 NotFound 时断言不调用 `CreateSnapshot`、不写 `Build.status`，且返回零值 + `controller.NewPermanentError`；返回临时错误时断言零值 + 原始错误；
- 删除中守卫：`deletionTimestamp` 非空时断言不调用 `UpdateBuildStatus`、不调用 `Create`，且返回零值 + `nil`；
- PollingSource：非终态 fieldSelector 生效、Add/Update/Delete 事件映射正确、List 失败时退避重试且不替换旧快照；
- conditions 生命周期：只写 `BuildSucceed` 与 `PublishSucceed`，未确定时不出现，等待/重试阶段不写条件，`message` 取固定短语。

### 14.2 集成测试

- 发布收口映射（成功）：`RpmRepo.status.release.phase=Ready` 时，断言同一次 `/status` 写入 `Build.status.phase=Success`、`stage=publish`、`repo=RpmRepo.status.release.contentURL`、`endTime` 非空与 `PublishSucceed=True/PublishSucceeded`；
- 发布收口映射（失败）：`release.phase=Failed` 时，断言同一次 `/status` 写入 `phase=Failed`、`stage=publish`、`endTime` 非空与 `PublishSucceed=False/PublishFailed`，不写 `repo`，且不覆盖已写入的 `BuildSucceed`；
- 等待语义：`RpmRepo.status.release` 缺失或 `release.phase` 为 `Pending` / `Creating` / `Prepared` 时断言不写 `Build.status`、返回零值 + `nil`；收口完成后下一轮收敛到同一终态；
- 跳过发布：`buildType=single` 单包成功、非 single 的 `buildTarget.publishFlag=false` 两种输入都断言直接写 `Skipped` / `publish` + `endTime` + `BuildSucceed`、不写 `PublishSucceed`、不消费或等待 RpmRepo 的发布收口结果（非 single 仍执行阶段入口的 RpmRepo 存在性 ensure；single 不访问本轮同名 RpmRepo）；
- `Build.spec.buildType=single` 分支：验证所有阶段不 GET/Create 本轮同名 RpmRepo，缺失时也不重建；覆盖目标仓库含多个 spec、全部成功、部分失败、空 map、缺失 build.status；只有非空且全部 Succeeded 才进入 Skipped/publish，其余进入 Failed/build。改变 map 插入顺序不改变结论；Completed 前即使已有部分成功结果也不得提前收口；与 BuildInfo Controller 联调验证结果范围仅限目标仓库，且 Completed 时目标 spec 结果齐全；
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

## 十六、假设与限制

- `Build.metadata.name` 为 uuid，不存在同名重建。
- `ebs.io/target-os`、`ebs.io/target-arch`、`ebs.io/build-type` 由创建方或 apiserver 补齐，且必须与 spec 一致；`GetLastPublishedBuild` 依赖 `ebs.io/target-os` / `ebs.io/target-arch` 两个 label 与 `status.phase` / `status.stage` 字段，`PollingSource` 只依赖 `status.phase` 字段，不依赖任何 label。
- 本轮不设未就绪超时。
- 启动入队、单副本和重启恢复的取舍见第十二章；部署身份与授权的范围见第十一章。
