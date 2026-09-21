# Build Controller 设计

## 一、定位与范围

Build Controller 根据 Snapshot、BuildInfo 和 RpmRepo 的状态推进 Build 生命周期。

职责：

- 周期轮询非终态 Build，根据当前 phase/stage 和子资源状态推进构建与发布流程，记录结果与条件；观察到终态后停止调谐；
- 按阶段幂等创建所需的 Snapshot、RpmRepo、BuildInfo，后续阶段确认直接依赖缺失时记录异常并推进到 Failed；
- 支持重复调谐与重启恢复，提供结构化日志和指标。

职责边界：

- 仅编排 Build 生命周期；依赖解析和 Job 编排由 BuildInfo Controller 负责，仓库物化与发布由 RpmRepo Controller 驱动 Artifact Manager 完成；
- 通过包内 `Client` 访问 ebs-apiserver，只更新 `Build.status`，并按需创建 Snapshot、BuildInfo、RpmRepo；不修改已有子资源的 spec/status；
- 仅为 Build 注册 PollingSource，子资源在 reconcile 中按确定性名称按需读取。

## 二、依赖与组件边界

Build Controller 运行在现有 `controller-manager` 框架内，建议实现目录为：

```text
components/controller-manager/pkg/controllers/build/
  controller.go         # 配置、initializer、PollingSource 事件入队与 BaseController 组装
  reconciler.go         # Sync 入口守卫、单轮时间获取与 phase/stage 分派
  phases.go             # Pending、Prepared、Processing/build、Processing/publish 的业务决策
  ensure.go             # 子资源 GET/POST、AlreadyExists 沿用、创建 Unknown 确认（仅用于创建阶段）
  status.go             # Build 状态写入前并发检查、原写入意图保存、写入与 Unknown 确认
  client.go             # 类型化 Client、共享客户端适配、写错误分类及 GetLastPublishedBuild 查询
  conditions.go         # condition 常量与 MergeCondition 纯函数
  metrics.go            # build_controller_* 指标注册
```

依赖与注入：

- `Controller` 持有 `BaseController`（队列/Worker）、`Client`、`clock.Clock` 与配置；
- 构造时注入 `clock.Clock`（`k8s.io/utils/clock`）：生产用 `clock.RealClock{}`，测试用 `k8s.io/utils/clock/testing.FakeClock`；业务代码不得直接调用 `time.Now()` / `time.Since()`；
- initializer 负责构造 `Client`、注册 `builds` PollingSource handler 并创建 `BaseController`；

### 2.1 装配与配置

- 队列 key 为 `{Build.metadata.namespace}/{Build.metadata.name}`，`Build.metadata.namespace` 即 Project 名。
- Controller 配置项：轮询周期沿用框架全局 `--poll-period`（默认 30s），worker 数沿用框架配置。
- 配置校验时机：所有静态配置（apiserver 地址与 CA、请求超时）必须在 initializer 阶段完成校验；配置非法时 controller-manager 直接启动失败，不得延迟到单个 Build 的 reconcile 中处理，也不通过 HealthChecker 表达。

### 2.2 组件依赖与框架接口

**组件依赖与数据流**

组件数据流如下：

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

**框架接口与注册**

- 分页 List 的 fieldSelector 通过 `PollingSourceFactory.ForResource(gvr, period, options)` 注入；
- 项目级 List 与子资源创建使用共享 client 的 `ListProjectPage` 与 `Create`；
- 在 `components/controller-manager/cmd/controller-manager/main.go` 的 `initializers` map 中加入 `build.Initializer(...)`。

### 2.3 PollingSource 装配语义

- 仅注册 builds PollingSource，在 Source 启动前完成 handler 注册；通过 ListOptions 注入 2.4 的非终态过滤条件。
- Add/Update 均以 `{namespace}/{name}` 入队，包括 resourceVersion 未变化的周期 Update，以驱动等待中的 Build。
- Delete 仅记录日志、不入队；过滤结果中消失不等于对象被物理删除。
- reconcile 不读取 Source 内部快照，始终 GET 最新 Build；删除中的对象由 7.1 守卫短路。
- 分页扫描、失败退避及快照管理复用公共框架，不在本控制器重复实现。

### 2.4 PollingSource API 交互样例

每轮 scan 只对 `builds` 发起分页 List（`status.phase` 只支持 `=`/`==`/`!=`，故用四个 `!=` AND）：

```text
GET /apis/ebs/v1/builds?fieldSelector=status.phase!=Success,status.phase!=Failed,status.phase!=Aborted,status.phase!=Skipped&limit=100
```

首轮不发送 continue，后续携带响应中的 `metadata.continue`，直到为空。List 响应结构为 BuildList，items 为完整 Build 对象；对象样例见 4.1，项目级 GET 路径见 3.2。

## 三、类型化客户端与 Fake（在 build controller 内实现）

框架不新增完整类型化 CRUD；由 build controller 包自行实现。

### 3.1 接口定义

```go
package build

type Client interface {
    // Project 级资源读取
    // GetProject 读取 Project；用于构造 Snapshot 输入与 BuildInfo 的 bootstrapRepo、buildPayload。
    GetProject(ctx context.Context, project string) (*v1.Project, error)
    GetBuild(ctx context.Context, project, name string) (*v1.Build, error)
    // GetLastPublishedBuild 返回最后一个发布成功的 Build。
    // 项目作用域与过滤契约见 3.2。
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

`fake_test.go` 中的包内 fake 实现同一 `Client` 接口，不作为生产代码或独立子 package；内部用内存 map 保存对象，并模拟：

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
  buildTarget:
    os: openEuler-22.03-LTS
    arch: aarch64
    buildFlag: true
    publishFlag: true
status:
  phase: Pending
```

各阶段与终态的 status 写入清单以第七章为准。

字段来源：

| 字段                                                                                                     | 来源/写入方           | 说明                                             |
| ------------------------------------------------------------------------------------------------------ | ---------------- | ---------------------------------------------- |
| `Build.metadata.name` / `Build.metadata.namespace`                                                     | 用户提交 / API 路径    | uuid，不复用同名                                     |
| `Build.metadata.labels`                                                                                | 用户或调用方           | 创建 Build 时写入                               |
| `Build.spec.buildType`                                                                                 | 用户提交             | Controller 只读；允许值 `full` / `incremental` / `specified` / `single`，控制器只对 `single` 特判，其余取值（含未枚举值）一律按非 single 处理 |
| `Build.spec.packages` / `Build.spec.buildTarget` | 用户提交 | Controller 只读；full/incremental 的 packages 在创建时清空，single/specified 指定目标包 |
| `Build.status.phase` / `Build.status.stage` | Build Controller | Pending → Prepared → Processing 由本控制器状态机推进；publish 终态由本控制器依据 `RpmRepo.status.release.phase` 判定，`stage` 由本控制器写 `publish` |
| `Build.status.startTime` / `Build.status.endTime` / `Build.status.baseBuildRef` | Build Controller | 进入 Processing 时写 `startTime`，进入终态时写 `endTime`；`baseBuildRef` 在 Pending 阶段写入，`nil` 表示未解析，`{}` 表示无上一个发布成功的 Build |
| `BuildSucceed` / `PublishSucceed`（`Build.status.conditions`） | Build Controller | `BuildSucceed` 由 `BuildInfo.status.specStatus` 计算；`PublishSucceed` 由 `RpmRepo.status.release.phase` 推导（`Ready`→`True`、`Failed`→`False`），不再从 `RpmRepo.status.conditions` 复制；直接依赖 NotFound 的失败条件见第六章 |

### 4.2 Snapshot

仓库选择规则：`Build.spec.buildType=single` 时，以 `Build.spec.packages` 的去重名称集匹配 `Project.spec.packageRepos[].name`，只复制匹配条目，保持 Project 列表中的相对顺序；不补入其他仓库，也不为未找到的名称伪造 PackageRepo。所选条目完整 DeepCopy，包括 name、URL 和 ref。其他 buildType 复制完整仓库列表。Snapshot.spec.defaultRef 始终从同一次 Project GET 复制，包 ref 为空时仍保持为空。创建阶段使用上述选择规则；已存在对象不重新筛选或覆盖。

正常 single 输入应满足第十三章前置校验；若创建 Build 后 Project 变化导致目标缺失，只复制实际命中的条目，缺失目标沿用 Snapshot Controller 的整体 condition 处理规则，不替换成全量仓库。

仓库 ref 为空时由 Snapshot Controller 回退到 Snapshot.spec.defaultRef；解析结果由其写入 status。Build Controller 只等待 Active。Project 读取与错误处理见第六章。

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

RpmRepo 的版本推进与发布收口语义以 RpmRepo Controller 为准，RpmRepo 没有对象终态；Build Controller 只消费下表字段，并仅在创建时按下表写初始 status 与目标标签。`RpmRepoStatus` 的结构（`repository` / `release` / `conditions`）与 `docs/zh/design/data-models.md` 一致。

骨架形态（所有构建类型均在 Pending 阶段等待 Snapshot Active 后 ensure）：

```yaml
apiVersion: ebs/v1
kind: RpmRepo
metadata:
  name: ${build.metadata.name}
  namespace: ${build.metadata.namespace}
  labels:
    ebs.io/target-os: ${build.spec.buildTarget.os}
    ebs.io/target-arch: ${build.spec.buildTarget.arch}
spec: {}
status:
  repository:
    repositoryUID: DB3A8CE2-00CD-4C89-9C20-ADA417C83155
    contentURL: https://artifact-manager/repositories/v1/DB3A8CE2-00CD-4C89-9C20-ADA417C83155
    # 上一个发布成功 Build 同名 RpmRepo 的 status.repository.repositoryUID / contentURL；无历史发布时都省略
  release: null             # 非 single 尚未进入发布阶段；single 改为 {phase: Skipped}
  conditions: []
```

**基线初始化**

- 生效范围：所有构建类型。single 同样创建 Snapshot、RpmRepo、BuildInfo；创建 RpmRepo 时额外设置 `status.release.phase=Skipped`，非 single 的初始 release 为 nil。
- 只在 ensure 按确定性名称 GET 未命中、准备创建时读取一次历史对象，因此在创建那一刻固化一次：`Build.status.baseBuildRef.Name` 为空（`baseBuildRef={}`，即没有上一个发布成功的 Build）时不读取、不写入；已存在的 RpmRepo（含 `metadata.deletionTimestamp` 非空的对象）一律沿用，不重读、不覆盖，后续轮次也不再改写该字段。
- 取值来源是历史 Build 同名 RpmRepo 的 `status.repository.repositoryUID` 与 `status.repository.contentURL`，即过程仓的不可变版本标识与地址（形如 `https://artifact-manager/repositories/v1/{repositoryUID}`）；它们不是 `status.release.*`（正式发布信息）。两个字段都非空即可继承：种入后本轮过程仓的当前版本初期即继承版本，尚未产生本轮自己的版本。
- 只有在「确定没有可继承基线」时降级：历史对象 NotFound，或历史对象存在但 `status.repository` 缺失 / `repositoryUID` 为空 / `contentURL` 为空。此时降级为不带基线的创建（两个字段都不写入，避免版本 UID 与地址不匹配），输出结构化日志 `controller=build key=<ns/name> kind=RpmRepo name=<name> reason=BaseRepositoryUnavailable base_build=<name> error=<error>`，本轮继续正常推进，不写 Build 终态、不 requeue。
- 历史对象读取失败（网络错误、超时、408/429/5xx）不降级：本轮不创建 RpmRepo、不写 `Build.status`，把 `classifyReadError` 的分类结果返回给框架——临时错误走退避重试（依赖 30s 轮询复查，下一次读取成功后正常创建与推进），并输出结构化日志 `controller=build key=<ns/name> kind=RpmRepo name=<name> reason=BaseRepositoryReadFailed base_build=<name> retryable=<bool> error=<error>`；`401/403/400/422` 与响应身份契约错误按 `controller.NewPermanentError` 返回（同一日志 `retryable=false`）；Manager context 取消返回 `ctx.Err()`，同样不创建、不写 status。
- 创建请求携带成对的 `repositoryUID` / `contentURL`；single 额外携带 `release.phase=Skipped`。不初始化 transition、sourceJobUIDs 或 conditions；无可继承基线时 repository 保持 nil，但 single 仍写 Skipped。已有对象不补写或覆盖初始状态。
- 创建时同时写入目标标签 `ebs.io/target-os` / `ebs.io/target-arch`（值取 `Build.spec.buildTarget.os` / `arch`），使 RpmRepo 可按构建目标查询与归组；标签只在创建请求中携带一次，已存在的同名 RpmRepo 不补写、不修改其 metadata。


发布后仅消费以下状态字段，完整结构见 data-models.md：

```yaml
status:
  release:
    phase: Ready
    contentURL: <稳定入口地址>
```

字段来源：

| 字段                                    | 来源/写入方             | 说明                                             |
| ------------------------------------- | ------------------ | ---------------------------------------------- |
| `RpmRepo.metadata.name` / `namespace` | Build Controller | 与 `Build.metadata.name` 同名，namespace 来自 Build |
| `RpmRepo.metadata.labels` | Build Controller（创建时写入） | 写入 `ebs.io/target-os` / `ebs.io/target-arch`，值取 `Build.spec.buildTarget.os` / `arch` ；仅创建时写入一次，已有对象不补写、不改写 |
| `RpmRepo.spec`                        | Build Controller   | 空 `{}`                                         |
| `RpmRepo.status.repository.repositoryUID` | Build Controller（创建时种入） | 创建时复制历史过程仓的不可变版本 UID 作为本轮过程仓的当前版本；已有对象不覆盖；非 single 随后由 RpmRepo Controller 推进版本，single 保持该初始基线 |
| `RpmRepo.status.repository.contentURL` | Build Controller（创建时种入） | 与 `repositoryUID` 同源同时种入，复制历史过程仓的不可变地址；已有对象不覆盖；非 single 随后由 RpmRepo Controller 推进版本，single 保持该初始基线 |
| `RpmRepo.status.release.phase` | Build Controller（single 创建时）/ RpmRepo Controller（非 single） | single 初始为 `Skipped`，仅保存构建输入仓地址，不参与物化与发布；非 single 按 `Ready` / `Failed` 消费发布结果 |

single 的 `Skipped` 不表示构建已经完成，也不表示没有可用过程仓；Build 仍等待 BuildInfo Completed 后汇总结果。其继承地址保存在 `status.repository.contentURL`，不写 `status.release.contentURL`。RpmRepo Controller 应将 Skipped 视为终态并排除，不推进其过程仓或发布；非 single 仍按现有状态机推进。

### 4.4 BuildInfo

Build Controller 创建 BuildInfo 时，读取所属 Project 并深拷贝 `Project.spec.bootstrapRepo` 到 `BuildInfo.spec.bootstrapRepo`，同时将 `Project.spec.buildPayload` 原样复制到 `BuildInfo.spec.buildPayload`；`specDepends` 为空，不写 `status.specStatus`，由独立 BuildInfo Controller 填充并推进。仅在 Prepared 阶段 BuildInfo NotFound、即将创建时读取 Project，已有 BuildInfo 不重新读取 Project，也不覆盖 bootstrapRepo、buildPayload（包括已有空值）；Project 后续变更不影响已创建 BuildInfo。Project 未配置相应字段时保留为空。Project NotFound 返回 PermanentError，其他读取错误按 7.6 分类，不发送创建请求或写 Build.status。

single 的 BuildInfo 只包含目标仓库解析出的 spec，不包含其他仓库或依赖仓库的 spec；`Build.spec.packages` 可以指定多个目标仓库，每个仓库也可以包含多个 spec，不能假设 specStatus 只有一个条目。BuildInfo Controller 保证进入 Completed 时所有目标仓库的 spec 结果齐全，不得只记录已成功的部分 spec 就宣布完成。Build Controller 在 Completed 后按 7.4 的统一规则汇总结果。

**single 的执行契约**：Build Controller 在 Snapshot Active 后创建本轮同名 RpmRepo（`status.release.phase=Skipped`），再创建 BuildInfo。BuildInfo Controller 使用 `BuildInfo.spec.bootstrapRepo` 和本轮同名 RpmRepo 的 `status.repository.contentURL` 下发 Job，然后直接汇总结果，不再自行读取历史 RpmRepo。single 的 BuildInfo Completed 不以新过程仓生成、Job 被 RpmRepo Controller 消费或正式发布完成为前提；产物仍通过 Runner / Artifact Manager 上传与保存。

历史基线由 Build Controller 按 4.3 在创建 RpmRepo 时一次固化。BuildInfo Controller 下发 Job 时固化该对象中的不可变 URL，不使用正式仓稳定入口；repository 为空时仅使用 bootstrapRepo。同名 RpmRepo 缺失或读取失败时，不等同于无基线，不下发 Job，按依赖读取错误处理。

状态推进见第五章；非法阶段组合由 7.1 的入口守卫处理。

```yaml
apiVersion: ebs/v1
kind: BuildInfo
metadata:
  name: ${build.metadata.name}
  namespace: ${build.metadata.namespace}
spec:
  specDepends: {}
  buildPayload: ${project.spec.buildPayload}
  bootstrapRepo:
    - name: everything
      repo: https://example.com/repo/everything
status:
  phase: Pending
```

字段来源：

| 字段                                                                                       | 来源/写入方               | 说明                                   |
| ---------------------------------------------------------------------------------------- | -------------------- | ------------------------------------ |
| `BuildInfo.metadata.name` / `namespace` | Build Controller | 与 `Build.metadata.name` 同名，namespace 来自 Build |
| `BuildInfo.spec.specDepends`                                                             | BuildInfo Controller | 由 BuildInfo Controller 解析生成 |
| `BuildInfo.spec.buildPayload` | Build Controller | 创建时从 Project.spec.buildPayload 原样复制；已有对象不覆盖，不跟随 Project 变更 |
| `BuildInfo.spec.bootstrapRepo` | Build Controller | 创建时从 Project.spec.bootstrapRepo 深拷贝，已有对象不覆盖 |
| `BuildInfo.status.phase` / `BuildInfo.status.specStatus` / `BuildInfo.status.conditions` | BuildInfo Controller | Pending → Processing → Completed，并回写各包状态          |

## 五、状态机

非终态 Build 指 `Build.status.phase` 不在终态集合 `{Success, Failed, Aborted, Skipped}` 中。

| 当前 phase / stage | 动作与分支 | 下一 phase / stage |
| --- | --- | --- |
| `Pending` / 空 | 若 `Build.status.baseBuildRef` 为 nil：定位上一个发布成功的 Build 并写 `baseBuildRef`（结果为空写 `{}`），本轮即返回 | 仍 `Pending` / 空（下一轮继续） |
| `Pending` / 空 | ensure Snapshot，等待 `Snapshot.status.phase=Active`；就绪后所有类型均 ensure RpmRepo | Snapshot Active 且所需 RpmRepo ensure 成功后：`Prepared` / 空 |
| `Prepared` / 空 | ensure BuildInfo | `Processing` / `build` |
| `Processing` / `build` | BuildInfo GET 返回 NotFound | `Failed` / `build` |
| `Processing` / `build` | 等待 `BuildInfo.status.phase=Completed`；完成后按以下分支处理 | — |
|  | `Build.spec.buildType=single` 且结果汇总失败（build 或 install 非 Succeeded，汇总规则见 7.4） | `Failed` / `build` |
|  | `buildType=single` 且结果汇总成功，或非 single 且 `Build.spec.buildTarget.publishFlag=false`（由本控制器判定跳过发布） | `Skipped` / `publish` |
|  | 其余情况（非 single 且 `Build.spec.buildTarget.publishFlag=true`） | `Processing` / `publish` |
| `Processing` / `publish` | RpmRepo GET 返回 NotFound | `Failed` / `publish` |
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
   - POST 返回 `WriteError.Outcome=Rejected` 且为 409（AlreadyExists）：优先按创建冲突处理，GET 后沿用现有对象，不修改 spec；不直接返回通用 Conflict 的 `RequeueAfter: 1s`。GET 成功后仍检查 deletionTimestamp：未删除则沿用并继续本阶段，删除中则按第 1 步等待；GET NotFound 表示并发删除，返回 `RequeueAfter: 1s`，下轮重新 ensure，本轮不再次 POST；其他 GET 错误按读取错误分类返回。
   - POST 返回 404 表示目标路由或父对象暂不可用（apiserver 版本落后、Project 尚未创建等），按临时错误退避重试：既不按 AlreadyExists 沿用，也不按永久错误收口；每次创建拒绝输出 `controller` / `key` / `kind` / `name` / `operation` / `status` / `retryable` 日志。
3. **结果未知处理**
   - 仅当 `WriteError.Outcome=Unknown` 时：
     - 先 GET 确认实际结果；
     - 存在则沿用，不存在则返回可重试错误。
   - `NotSent` 与其他 `Rejected` 按 7.6 / 9.3 返回，不执行 Unknown 确认；明确收到的 5xx 拒绝响应仍为 `Rejected`，不能仅因状态码为 5xx 就进入确认流程。

ensure 不推进子资源状态、不等待子资源就绪、不覆盖已存在 spec、不删除其他 Build 的对象。

**按阶段创建与读取**：仅在 Pending 创建 Snapshot/RpmRepo，在 Prepared 创建 BuildInfo；Processing 阶段只读直接依赖，不自动重建，也不恢复前序依赖链。

| 阶段 | 操作 | NotFound 处理 |
| --- | --- | --- |
| Pending（baseBuildRef 已固化） | ensure Snapshot → 等待 Active → ensure RpmRepo（所有类型，按 4.3 固化基线；single 写 Skipped）→ 进入 Prepared | 在本阶段按顺序创建 |
| Prepared | ensure BuildInfo，存在且未删除后进入 Processing/build；不重新查询 Snapshot/RpmRepo | 从 Project 复制 bootstrapRepo、buildPayload 并创建 BuildInfo |
| Processing/build | GET BuildInfo，等待 Completed；不查询 Snapshot/RpmRepo | 记录异常，不创建任何子资源，写入 Failed/build |
| Processing/publish（仅非 single） | GET RpmRepo，判断 release.phase；不查询 Snapshot/BuildInfo | 记录异常，不创建任何子资源，写入 Failed/publish |

Processing 阶段 GET 直接依赖返回 NotFound 时，输出结构化日志 `reason=ChildResourceMissing`，包含 controller、Build key/UID、phase/stage、子资源 kind/name；不重建，按第九章写入前校验与并发协议，通过一次 `/status` 写入 `phase=Failed`、保留当前 `stage`、填写 `endTime`。build 阶段写 `BuildSucceed=False/ChildResourceMissing`，publish 阶段写 `PublishSucceed=False/ChildResourceMissing`；保留其他状态字段与条件。终态写入成功（含 Unknown 确认成功）后返回业务 PermanentError，写入失败按 7.6 / 9.3 分类处理，不覆盖外部 Aborted。仅明确的 NotFound 触发此规则，读取超时、权限错误等不视为缺失。对象正在删除但仍存在时继续等待，不创建、不推进；终态或删除中的 Build 由入口守卫直接结束。

创建阶段的幂等边界：没有额外的创建记录时，仅凭 Pending/Prepared 无法区分“从未创建”与“创建成功但在阶段推进前被删除”。因此这些阶段的 ensure 仍允许 NotFound 后创建，不承诺识别所有删除场景；新对象使用创建时的 Project 配置。禁止的是已推进到后续阶段后的自动重建。AlreadyExists/Unknown 的确认 GET 若为 NotFound，按前述创建协议结束本轮，不在一个周期内循环 POST。

本版本不巡检前序子资源，不增加 annotation 或低频全链路检查；phase/stage 仅说明此前已完成相关步骤，不保证子资源现在仍存在。子 Controller 仍需检查自身运行时依赖。

Snapshot 的创建需要先读 Project：仅当 Snapshot NotFound 时才 `GetProject(Build.metadata.namespace)`，并以同一返回对象的 `spec.defaultRef` 和按 4.2 筛选后的 `spec.packageRepos` 构造 Snapshot spec 后创建；已有 Snapshot 时不读 Project、不覆盖 spec。`GetProject` 返回 NotFound 时按永久错误处理，不创建子资源、不写 `Build.status`。

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
7. 阶段处理结果与错误统一按 7.6 返回；写入结果未知先按 9.3 确认。
8. 若状态没有变化，不调用状态更新 API。

状态写入原子性：

- 每次状态迁移先构造完整的目标 `Build.status`，再调用一次 `UpdateBuildStatus` 提交。
- `7.2` 至 `7.5` 中“写：”块列出的字段是本次写入需要合并的字段清单，不是多次独立 API 调用。
- `Build.status.conditions` 的变更也合并到同一次 `/status` 写入，不单独更新 condition。
- 写入返回 `WriteError.Outcome=Unknown` 时不重放原 PUT，按第九章「写入结果未知确认」处理。

后续各小节只描述阶段内部逻辑；子对象的读取、创建及缺失处理统一遵循第六章。

### 7.2 Pending

- 若 `Build.status.baseBuildRef` 为 nil：
  - 调用 `GetLastPublishedBuild(project, os, arch)`：
    - 若结果为空，写 `Build.status.baseBuildRef={}`；
    - 否则写：
      - `Build.status.baseBuildRef.name`（上一个发布成功 Build 的 `Build.metadata.name`）
  - 写入后立即返回零值 + `nil`，`ensure Snapshot` / `ensure RpmRepo` 与后续等待从下一轮继续；`{}` 与实际值同样视为已处理。
- 按第六章 Pending 行先 ensure Snapshot 并等待 Active，再 ensure RpmRepo（所有类型，按 4.3 固化基线；single 写 Skipped）。
- 满足后写 `Build.status.phase=Prepared`。

### 7.3 Prepared

- 按第六章 Prepared 行 ensure BuildInfo；BuildInfo 存在且未删除后执行以下写入。
- 写：
  - `Build.status.phase=Processing`
  - `Build.status.stage=build`
  - `Build.status.startTime`

### 7.4 Processing/build

- 按第六章 Processing/build 行读取 BuildInfo，缺失时按第六章写 Failed/build 并结束；存在时等待 BuildInfo Completed。
- 统一汇总规则（single 与非 single 共用）：`specStatus` 非空且每个目标 spec 的 `value.build.status == Succeeded` 且 `value.install.status == Succeeded` 才判定成功；空 map、任一 spec 的 `build` 或 `install` 不是 `Succeeded`（含缺失/未评估、未完成、Failed、Aborted）均判定失败。结果不依赖 map 遍历顺序，目标 spec 的完整性由 4.4 的 Completed 契约保证。
- 若 `Build.spec.buildType=single`：
  - 结果汇总失败（build 或 install 非 Succeeded）：
    - 写：
      - `Build.status.phase=Failed`
      - `Build.status.stage=build`
      - `Build.status.endTime`
      - `BuildSucceed=False/BuildFailed`
  - 结果汇总成功（跳过发布）：
    - 写：
      - `Build.status.phase=Skipped`
      - `Build.status.stage=publish`
      - `Build.status.endTime`
      - `BuildSucceed=True/BuildSucceeded`
- 否则：
  - 按上述汇总结果写 `BuildSucceed`：成功为 `True/BuildSucceeded`，失败为 `False/BuildFailed`。
  - `BuildSucceed` 记录所有目标 spec 的构建与安装依赖是否全部成功，不作为发布门禁。
  - 若 `Build.spec.buildTarget.publishFlag=false`（跳过发布），写：
    - `Build.status.phase=Skipped`
    - `Build.status.stage=publish`
    - `Build.status.endTime`
    - `BuildSucceed`（取上一步计算结果）
  - 否则写：
    - `Build.status.stage=publish`
    - `BuildSucceed`（取上一步计算结果）
    - `Build.status.phase` 保持 `Processing`，等待 RpmRepo 发布收口。

跳过发布的两个分支均不写 `PublishSucceed`，不等待发布收口；阶段入口的依赖读取与缺失处理仍遵循第六章。

### 7.5 Processing/publish

- 按第六章 Processing/publish 行读取 RpmRepo，缺失时按第六章写 Failed/publish 并结束；存在时判定发布收口；正常轮次不再读取 BuildInfo 或等待其 Completed。
- 等待 RpmRepo 发布收口：`RpmRepo.status.release` 缺失，或 `release.phase` 不是 `Ready` / `Failed` 时，返回零值 + `nil`，下一轮由周期 resync 复查。
- 收口后（`release.phase` ∈ `{Ready, Failed}`）一次 `/status` 写：
  - `release.phase=Ready`：
    - `Build.status.phase=Success`
    - `Build.status.stage=publish`
    - `Build.status.endTime`
    - `PublishSucceed=True/PublishSucceeded`（reason 与 message 由本控制器固定，不从 RpmRepo 复制）
  - `release.phase=Failed`：
    - `Build.status.phase=Failed`
    - `Build.status.stage=publish`
    - `Build.status.endTime`
    - `PublishSucceed=False/PublishFailed`
- 本轮返回值：`release.phase=Ready` 返回零值 + `nil`；`release.phase=Failed` 返回零值 + `controller.NewPermanentError`。


### 7.6 错误分类与队列结果映射

`Sync` 返回 `(ReconcileResult, error)`，BaseController 据此决定该 key 的重入方式；`Requeue` 与 `RequeueAfter` 不得同时非零。各分支的映射如下：

写错误先按 `WriteError.Outcome` 分流（见 9.3），只有 `Rejected` 再按 HTTP 状态码匹配拒绝响应规则；`Unknown` 先走确认流程。下表中的 API 临时错误分类也适用于读取请求，但读取失败不进入写入确认流程。

| 场景 | 返回 |
| --- | --- |
| Build 不存在、已终态、状态无变化 | 零值 + `nil` |
| 写入 `baseBuildRef` 后本轮返回、等待 `Snapshot.status.phase=Active`、`BuildInfo.status.phase=Completed`、RpmRepo 发布收口（`RpmRepo.status.release.phase` 为 `Ready` / `Failed`） | 零值 + `nil`（依赖 PollingSource 周期 resync 复查） |
| 子资源 POST 返回 409 AlreadyExists（优先匹配） | 按第六章 GET 后沿用；返回后续阶段结果，不直接套用通用 Conflict 重入；GET NotFound 时延迟 1s 重新 ensure |
| `/status` 或其他写入返回 409 Conflict（不含上述 AlreadyExists） | `ReconcileResult{RequeueAfter: 1s}` + `nil`（下轮重新 GET 并按第七章重算目标 status，不使用立即重入，避免持续冲突形成热循环） |
| API 临时错误（404、408、429、5xx、网络错误或超时；写入 Unknown 先执行确认流程） | 零值 + 原始错误（BaseController 走 `AddRateLimited`；超过 `--controller-max-retries` 后转慢速指数退避并持续重入） |
| 429 / 503 且响应带 `Retry-After` | 零值 + 携带 RetryAfter 的 `client.WriteError`（框架按该延时重入） |
| API/配置永久错误、非法 `(phase, stage)`、输入或客户端错误 | 不写业务终态或失败条件；记录错误并返回零值 + `controller.NewPermanentError(err)`（框架 Forget 仅清除本次队列退避；后续仍会被 PollingSource 周期 resync 重新入队） |
| 构建或发布确定性失败，或 Processing 直接依赖确认缺失，且终态与条件已通过一次 `/status` 写入 | 零值 + `controller.NewPermanentError(err)`（终态即该 key 的终点） |
| 写入结果未知 | 先按 9.3「写入结果未知确认」确认，再按确认结果落到上述对应行返回 |

约定：

- NotFound 按资源与操作语义处理；AlreadyExists 优先于通用 Conflict。API 永久拒绝（400/401/403/422）、配置、输入及客户端错误不得写业务终态或失败条件；静态配置在 2.1 的 initializer 阶段校验失败即停止启动。
- 7.4 / 7.5 已确认的构建或发布失败，以及第六章 Processing 直接依赖确认缺失时写 Failed；终态及条件写入成功（包括 Unknown 确认成功）后返回业务 PermanentError，写入失败则返回写错误分类结果。
- `err != nil` 时不返回非零 `ReconcileResult`，避免框架记录 `invalid-result-with-error`；
- "等待"类分支统一用零值 + `nil`，不用 error 表达正常等待，避免污染错误指标并叠加额外退避；
- 只有需要比轮询周期更快复查时才使用 `RequeueAfter`。


## 八、条件与时间

### 8.1 Conditions 生命周期

`Build.status.conditions` 只记录构建与发布结果，不维护快照、构建信息、RPM 仓库等中间就绪状态；运行中的进度由 `Build.status.phase` / `Build.status.stage` 表达。只允许以下两个 type，结果确定前对应条件不出现，每轮 reconcile 内容变化时才通过 `/status` 写入：

| type             | 含义           | `status="True"` 的写入时机                                     | `status="False"` 的写入时机                                    |
| ---------------- | -------------- | ----------------------------------------------------------- | ------------------------------------------------------------ |
| `BuildSucceed` | 构建与安装依赖是否全部成功 | BuildInfo Completed 后，按 7.4 汇总成功（reason/message 为 BuildSucceeded） | BuildInfo Completed 后，按 7.4 汇总失败（reason/message 为 BuildFailed）；Processing/build 的 BuildInfo NotFound 时为 ChildResourceMissing |
| `PublishSucceed` | 发布是否成功   | `RpmRepo.status.release.phase=Ready`（本控制器写 `reason/message=PublishSucceeded`） | `RpmRepo.status.release.phase=Failed`（本控制器写 `reason/message=PublishFailed`）；Processing/publish 的 RpmRepo NotFound 时为 ChildResourceMissing；对象存在且 `release.phase` 未到终态时不写 |

各阶段写入规则：

- Pending/Prepared、Processing/build 与 Processing/publish 的等待/运行阶段：不写任何条件。
- 构建和发布阶段按上表记录结果；PublishSucceed 直接依据 release.phase，不从 RpmRepo.status.conditions 复制。
- 跳过发布时不产生发布结果条件，具体写入清单见 7.4。
- 条件一旦写入即保留到终态，不因后续阶段变化删除或改写；`reason` 固定为 `BuildSucceeded`、`BuildFailed`、`PublishSucceeded`、`PublishFailed` 或 `ChildResourceMissing`，不再细分中间原因。

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
- `message` 写与 `reason` 同族的固定短语：`BuildSucceeded` / `BuildFailed` / `PublishSucceeded` / `PublishFailed` / `ChildResourceMissing`；动态错误详情只进日志，不写入 condition；
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

- 每次 Build status 写入前，再 GET 最新 Build，与本轮入口 GET 后用于计算决策的对象比较；依次执行以下检查：
  - NotFound、UID 不同、已处于终态或 `metadata.deletionTimestamp` 非空：放弃本轮写入并结束旧周期，返回零值 + `nil`。
  - 同 UID、仍非终态且未删除，但 `resourceVersion` 改变：放弃旧决策，返回 `ReconcileResult{RequeueAfter: 1s}` + `nil`；下一轮从入口 GET 重新调和，本轮不继续写入或 ensure 子资源。
  - UID 与 resourceVersion 均未变化且守卫通过：基于该对象合并本轮目标 status，保留未涉及的字段，并提交一次 `/status` 写入。
  - GET 失败（除上述 NotFound）：按 7.6 的读取错误分类返回，不发起写入。
- 不得仅将旧目标对象的 resourceVersion 替换成最新值后提交；否则会绕过旧决策应触发的冲突，可能覆盖外部 Aborted。写入前 GET 与实际 PUT 之间仍可能发生并发更新，由 PUT 的 resourceVersion 乐观锁阻止覆盖；
- `/status` 冲突按 7.6 结束本轮并延迟重入，禁止用旧对象重放；
- 不直接修改 `Build.spec`，也不写 `Build.metadata`（标签由创建方或 apiserver 维护）。

### 9.3 写入结果未知确认

所有创建与状态写入失败统一以类型化客户端返回的 `WriteError.Outcome` 为分流依据，不根据 HTTP 状态码、错误文本或是否表现为超时再次推断 Outcome。客户端适配层负责返回写错误三分类，控制器分别处理：

- **NotSent**（请求未发出）：按错误原因决定是否重试，不执行 Unknown 确认。本地输入、对象类型、目标路径或 metadata 等客户端校验错误返回零值 + `controller.NewPermanentError(err)`；临时网络错误或请求超时返回零值 + 原始错误，由框架退避重入。Manager context 已取消时直接返回零值 + `ctx.Err()`，不继续请求或启动后台确认任务。Outcome 只决定写入结果的处理路径，不代表错误一定可重试。
- **Rejected**（收到明确响应）：按 7.6 的 HTTP 状态码规则分类处理，包括明确的 408、429 与 5xx 响应；不执行 Unknown 确认。创建时的 AlreadyExists 沿用规则仍优先于通用 Conflict 重入。
- **Unknown**（客户端无法确认写入结果，例如请求发出后连接中断或响应无法解析）：不得重放原 PUT，也不得用下一阶段重新计算的目标确认上一笔写入；按下述规则确认原写入意图。子资源创建则按第六章确认存在性。

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
- 重启首轮扫描将非终态 Build 入队，由 worker 数控制并发，不额外分批或限速。后续入队遵循 2.3；收敛时间取决于队列积压、API 可用性及子资源进度，不保证在一个 poll 周期内完成。
- 重启会清空框架的快速重试计数与慢速指数退避状态（BaseController 的内存态）；业务状态与终态判定不受影响，但同一 key 的退避会从初始值重新开始，因此重启后可能出现短暂的更密集重试。

崩溃点与重启后的结论：

| 崩溃点 | 重启后行为 |
| --- | --- |
| 崩在 ensure 之间（子资源已创建但未记录） | 首轮 reconcile GET 到已存在对象并沿用，不重复创建（见第六章） |
| 崩在 `/status` 写入之前 | 状态未变化，按当前 `Build.status` 重算并写入同一目标状态 |
| 崩在 `/status` 结果未知之后 | 原写入意图随进程丢失；重启后 GET 持久化 Build，按当前 phase/stage 正常调和，已终态则结束；不恢复旧请求、不比较重新生成的目标来确认旧写入 |
| 崩在读 RpmRepo 发布收口结果前后 | 重启后按 7.5 直接读取 RpmRepo，缺失时按第六章写 Failed/publish；`release.phase` 未到 `Ready` / `Failed` 则继续等待，已收口则按 7.5 写入 Build 终态；观察到 Build 终态后不再更新 |
| 崩在终态写入之前 | 按当前 phase/stage 重算，写入与未崩溃时相同的终态；观察到终态后不再更新 |

可用性：本轮仅运行单个 active controller-manager 实例，未实现 leader election。多副本不会破坏 Build 状态（依赖 `resourceVersion` 乐观并发），但会产生重复 GET、Conflict 与指标噪声，不等价于完整高可用方案。

本控制器不实现自定义 `HealthChecker`：对象是否就绪属于业务状态，不应令进程 `/healthz` 失败；Source 同步与陈旧状态由 manager 的 `/readyz` 负责。

## 十三、前置条件

- Build 校验：apiserver 创建 `single` 或 `specified` Build 时，要求 `Build.spec.packages` 非空且每个包名均存在于所属 `Project.spec.packageRepos`，允许多个包。创建钩子在基础校验后读取一次 Project；不存在的包返回 422 字段错误，Project 读取错误原样返回。该校验不应用于 full/incremental，也不在普通更新或 status 更新时重新检查；创建后 Project 变化仍由后续调和处理。single 的仓库选择仍按 4.2 的去重名称集执行，specified 仍复制全部仓库。
- RpmRepo Controller：负责 repo 解析、发布与 `RpmRepo.status` 推进。Build 依赖其提供 `RpmRepo.status.release.phase` 与 `RpmRepo.status.release.contentURL`；`RpmRepoStatus` 的 `repository` / `release` / `conditions` 结构已在公共 API 落地（见 `api/ebs/v1/types.go` 与 `docs/zh/design/data-models.md`）。在该 Controller 就绪前 Build 会停在 `Processing/publish` 等待。

- 配套消费契约：RpmRepo Controller 的轮询与发布候选过滤、入口终态判断均需排除 `Skipped`（与 Ready、Failed 一并处理）；single 不生成新版本、不正式发布。公共数据模型和 RpmRepo Controller 文档需同步此契约；本次仅修改 Build Controller 设计，不代表代码已实现。

## 十四、测试计划

### 14.1 单元测试

- 状态机：Pending → Prepared → Processing/build → Processing/publish → 终态，覆盖各终态分支与等待态；
- 状态机守卫：非法 `(phase, stage)` 组合（如 `Processing` + 空 stage、`Pending` + `publish`）断言不调用 `UpdateBuildStatus`、对象保持原 phase/stage，并返回零值 + `controller.NewPermanentError`；
- 纯函数：`MergeCondition`（首次插入与 status 变化使用传入 now；同值且 now 变化仍返回 `changed=false`；仅 reason/message/observedGeneration 变化返回 `changed=true` 但保留原转换时间；`observedGeneration` 取 `metadata.generation`）、buildType 分支判定；
- 时间来源：断言 `startTime` / `endTime` / 新写入的 `lastTransitionTime` 取自注入的 FakeClock 且为 UTC，同一轮所有新时间戳复用同一次 `now`；断言业务代码及 helper 不调用 `time.Now()`；
- 类型化客户端与 fake：NotFound、AlreadyExists、409 Conflict、resourceVersion 自增、`/status` 保留 spec、写错误三分类（NotSent / Rejected / Unknown）；
- Outcome 分流：对 Create 与 UpdateBuildStatus 分别覆盖 `Rejected` 的 408/429/5xx，断言不触发 Unknown 确认 GET；覆盖 `Unknown` 的连接中断、超时与响应解析失败，断言执行对应确认流程。`NotSent` 不执行确认读取；控制器不得通过错误文本或状态码覆盖客户端给出的 Outcome；
- NotSent 原因分类：覆盖本地输入、对象类型、目标路径和 metadata 校验错误，断言返回 PermanentError；临时网络错误或请求超时返回原始错误供框架退避；Manager context 取消返回 ctx.Err()。所有分支均不执行 Unknown 确认、不写业务终态或失败条件，且返回的 ReconcileResult 为零值；
- 写入结果未知确认：模拟 `Pending → Prepared` 响应丢失后 GET 返回 Prepared，断言按原意图确认成功并结束本轮，不生成 Processing 目标、不 ensure 后续子资源；覆盖正常推进、baseBuildRef（含 nil/空对象区别）、时间序列化精度和失败终态的返回结果；
- 写入前并发校验：入口 GET 后、写入前 GET 时分别模拟 NotFound、UID 改变、外部 Aborted 和删除中对象，断言不调用 UpdateBuildStatus 且返回零值 + `nil`；同 UID 非终态对象仅 resourceVersion 改变时，断言不提交旧目标、返回 `RequeueAfter: 1s`，下一轮重新计算。UID/resourceVersion 均未变化时允许写入并保留无关字段；写入前 GET 后再发生并发更新时，由 PUT Conflict 结束本轮并延迟重入；
- 确认比较与异常：目标字段缺失或不一致时返回 `RequeueAfter: 1s`，下一轮重新 GET 调和；resourceVersion、无关 status 字段或条件顺序变化不影响确认；GET 返回 NotFound、不同 UID、外部 Aborted、删除中对象时不写入；GET 失败或 context 取消时不重放 PUT、不创建后台确认任务；
- 队列结果映射（见 7.6）：逐分支断言返回值形态——Build 不存在、已终态、状态无变化与全部等待类分支（等待 Snapshot Active / BuildInfo Completed / RpmRepo 发布收口）返回零值 + `nil`；409 Conflict 返回 `ReconcileResult{RequeueAfter: 1s}` + `nil`；临时错误（含子资源 POST 返回 404）返回零值 + 原始错误；永久错误返回零值 + `controller.NewPermanentError`；并断言任何 `err != nil` 的分支都不带非零 `ReconcileResult`；
- ensure 幂等：GET 命中直接沿用、创建返回 409 沿用现有对象、创建结果未知先 GET 确认；创建返回 404 断言按临时错误退避（零值 + 原始 `WriteError`）、不写 `Build.status`、不触发确认读取，且输出含 `kind`、`status=404`、`retryable=true` 的 `reason=CreateRejected` 日志；创建返回 400/401/403/422 断言返回 `PermanentError` 且日志 `retryable=false`；
- 阶段读取范围：Prepared 与 Processing/build 的 BuildInfo 存在时，断言不 GET Snapshot/RpmRepo；Processing/publish 的 RpmRepo 存在时，断言不 GET Snapshot/BuildInfo，直接使用发布结果。前序对象单独被删除不触发全链路巡检或重建；
- Snapshot 就绪门禁：Pending 中 Snapshot 未 Active 时不 GET/Create RpmRepo；Active 后才 ensure RpmRepo。Prepared 只 ensure BuildInfo，不再检查前序对象；RpmRepo 没有可用过程仓版本不阻止正常阶段推进；
- RpmRepo 基线种入：所有构建类型在 `baseBuildRef.Name` 非空时断言只读取一次同名历史 RpmRepo、`CreateRpmRepo` 的 `spec` 为 `{}` 且 `status.repository.repositoryUID` 与 `status.repository.contentURL` 都等于历史值、非 single 的 release 为空、single 的 release.phase 为 Skipped，conditions 为空；所有类型的 `baseBuildRef={}` 断言零次历史读取、repository 为 nil；single 仍创建 release.phase=Skipped；历史对象 NotFound 或存在但 `status.repository` 缺失 / `repositoryUID` 为空 / `contentURL` 为空时断言仍创建 RpmRepo、两个字段都为空，并输出 `reason=BaseRepositoryUnavailable` 日志，且不写 Build 终态、不延迟重入；历史对象读取失败（网络错误、超时、408/429/5xx）断言返回可重试错误、`CreateRpmRepo` 与 `UpdateBuildStatus` 均为 0、Build 保持 Pending 并输出 `reason=BaseRepositoryReadFailed retryable=true`；`401/403/400/422` 与响应身份契约错误断言返回 `PermanentError`、同样不创建且日志 `retryable=false`；已有 RpmRepo（含 `metadata.deletionTimestamp` 非空）断言不读取历史、不覆盖基线字段；创建后重入与进程重启断言不重复查询、不改写这些字段；
- RpmRepo 目标标签：断言 `CreateRpmRepo` 请求的 `metadata.labels` **恰好**为 `{ebs.io/target-os: <Build.spec.buildTarget.os>, ebs.io/target-arch: <Build.spec.buildTarget.arch>}`（无多余标签），有/无继承基线两条创建路径都一致；single 与非 single 均覆盖相同标签断言；已有 RpmRepo 命中时断言沿用对象且**不补写、不改写**其 labels；
- AlreadyExists 优先级：POST 返回 AlreadyExists 后 GET 命中正常对象，断言沿用并继续阶段，不覆盖 spec、不再次 POST、不直接返回通用 Conflict 重入；GET 命中删除中对象则等待，GET NotFound 则延迟 1s，其他读取错误按分类返回；
- 永久错误与业务失败隔离：覆盖 API 400/401/403/422、非法输入和客户端错误，断言不调用 UpdateBuildStatus、不写失败条件，只记录并返回 PermanentError；对照覆盖 single 结果汇总失败（build 或 install 非 Succeeded）、release.phase=Failed 和 Processing 直接依赖 NotFound，仅确认对应失败原因后才写 Failed，且状态写入失败不得被业务 PermanentError 覆盖；静态配置非法在 initializer 阶段启动失败；
- 删除中子资源：本轮读到 deletionTimestamp 时不调用 Create、不写 Build.status，返回零值 + nil；对象消失后，仅 Pending/Prepared 的创建职责允许 ensure 创建，Processing 记录缺失异常并写 Failed；
- 子对象缺失：Processing/build 的 BuildInfo 或 Processing/publish 的 RpmRepo 返回 NotFound 时，断言输出 ChildResourceMissing 日志，不调用任何 Create/GetProject、不恢复前序依赖；同一次 `/status` 写入 Failed、保留当前 stage、填写 endTime，并写对应条件 False/ChildResourceMissing，保留其他条件。写入成功后返回业务 PermanentError；覆盖 Conflict、Unknown 确认及外部 Aborted 保护，写入失败不得被业务 PermanentError 覆盖。重启后仍按此规则处理，已 Failed 后不再调和；读取超时、权限错误不触发该失败分支；
- single 依赖边界：Pending 按 Snapshot Active → ensure RpmRepo（Skipped）→ Prepared 推进；Prepared 才 ensure BuildInfo。覆盖继承基线、无基线和历史读取失败；验证 BuildInfo 使用本轮 RpmRepo 的 URL、不再读取历史对象，当前 RpmRepo 缺失或读取失败不下发 Job，Skipped 不触发物化或发布；single + Processing/publish 仍返回永久错误。
- `baseBuildRef` 写入即返回：`Build.status.baseBuildRef` 为 nil 时断言本轮只发生一次 `/status` 写入（写 `baseBuildRef`）、不调用 `CreateSnapshot` / `CreateRpmRepo`，且返回零值 + `nil`；下一轮以已写入的 `baseBuildRef` 继续 ensure 子资源；
- Snapshot 创建复制输入：覆盖 Project.defaultRef 为 Branch/Tag，断言只调用一次 `GetProject`，非 single 的 `CreateSnapshot` 收到的 `spec.defaultRef` 和 N 个 `packageRepos` 均与该返回对象一致；single 只包含 Build.spec.packages 命中的仓库，defaultRef 不变，修改构造出的 Snapshot 不影响 Project；已有 Snapshot（含缺少 defaultRef 的对象）存在时，不调用 `GetProject`、不调用 `CreateSnapshot`、不覆盖 spec；
- single 多包输入：覆盖多个包名及重复包名，断言 Snapshot 仅包含去重后命中的仓库并保持 Project 列表顺序；BuildInfo Completed 后汇总所有目标仓库的 spec，每个 spec 的 build 与 install 都为 Succeeded 才进入 Skipped/publish，任一 build 或 install 非 Succeeded 则进入 Failed/build；
- Project 读取失败：`GetProject` 返回 NotFound 时断言不调用 `CreateSnapshot`、不写 `Build.status`，且返回零值 + `controller.NewPermanentError`；返回临时错误时断言零值 + 原始错误；
- BuildInfo 创建输入：覆盖所有构建类型、空引导仓及 Prepared 创建阶段重入，断言创建前读取 Project 并深拷贝 bootstrapRepo、原样复制 buildPayload；覆盖空 buildPayload，确认 Project 后续变更不影响已创建对象；修改创建对象不影响 Project。已有 BuildInfo 不因 Project 配置变化覆盖输入、不为其读取 Project；AlreadyExists 沿用服务端对象。读取 Project 失败不调用 CreateBuildInfo、不写 Build.status；创建使用当前 Project 配置；
- 删除中守卫：`deletionTimestamp` 非空时断言不调用 `UpdateBuildStatus`、不调用 `Create`，且返回零值 + `nil`；
- PollingSource：非终态 fieldSelector 生效、Add/Update/Delete 事件映射正确、List 失败时退避重试且不替换旧快照；
- conditions 生命周期：只写 `BuildSucceed` 与 `PublishSucceed`，未确定时不出现，等待/重试阶段不写条件，`message` 取固定短语。

### 14.2 集成测试

- 发布收口映射（成功）：`RpmRepo.status.release.phase=Ready` 时，断言同一次 `/status` 写入 `Build.status.phase=Success`、`stage=publish`、`repo=RpmRepo.status.release.contentURL`、`endTime` 非空与 `PublishSucceed=True/PublishSucceeded`；
- 发布收口映射（失败）：`release.phase=Failed` 时，断言同一次 `/status` 写入 `phase=Failed`、`stage=publish`、`endTime` 非空与 `PublishSucceed=False/PublishFailed`，不写 `repo`，且不覆盖已写入的 `BuildSucceed`；
- 等待语义：`RpmRepo.status.release` 缺失或 `release.phase` 为 `Pending` / `Creating` / `Prepared` 时断言不写 `Build.status`、返回零值 + `nil`；收口完成后下一轮收敛到同一终态；
- 跳过发布：`buildType=single` 结果汇总成功、非 single 的 `buildTarget.publishFlag=false` 两种输入都断言直接写 `Skipped` / `publish` + `endTime` + `BuildSucceed`、不写 `PublishSucceed`、不消费或等待发布结果；BuildInfo 已存在时不 GET RpmRepo，single 仅在 Pending ensure 本轮同名 RpmRepo，后续不消费其发布结果；
- `Build.spec.buildType=single` 分支：验证 Pending 创建本轮同名 RpmRepo 且 release.phase=Skipped，后续阶段不巡检或重建该对象；覆盖目标仓库含多个 spec、build 与 install 全部成功、部分失败、空 map、缺失 build.status、缺失 install.status；只有非空且每个 spec 的 build 与 install 都为 Succeeded 才进入 Skipped/publish，其余进入 Failed/build。改变 map 插入顺序不改变结论；Completed 前即使已有部分成功结果也不得提前收口；与 BuildInfo Controller 联调验证结果范围仅限目标仓库，且 Completed 时目标 spec 结果齐全；
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
- 配套 API 契约：增加 `RpmRepoReleaseSkipped` 枚举；创建 RpmRepo 时保留成对的 repositoryUID / contentURL，以及显式传入的 `release.phase=Skipped`。Skipped 不要求 release.contentURL，不允许 release.transition；未提供 repository 时保持 nil。当前创建策略与状态校验需同步实现，不能清除 single 的 Skipped。
- `ebs.io/target-os`、`ebs.io/target-arch`、`ebs.io/build-type` 由创建方或 apiserver 补齐，且必须与 spec 一致；`GetLastPublishedBuild` 依赖 `ebs.io/target-os` / `ebs.io/target-arch` 两个 label 与 `status.phase` / `status.stage` 字段，`PollingSource` 只依赖 `status.phase` 字段，不依赖任何 label。
- 本轮不设未就绪超时。
- 启动入队、单副本和重启恢复的取舍见第十二章；部署身份与授权的范围见第十一章。
