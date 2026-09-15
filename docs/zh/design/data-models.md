# EulerMaker 对象结构体字段说明

## 概述

本文档完整定义 EulerMaker 系统中所有 RESTful 资源的结构体，每个字段均标明 Go 类型、JSON tag 与业务含义。所有资源遵循 `apiVersion: ebs/v1`。

---

## 通用元数据

每个顶层资源内嵌 `metav1.TypeMeta` 和 `metav1.ObjectMeta`：

```go
metav1.TypeMeta   `json:",inline"`     // apiVersion + kind
metav1.ObjectMeta `json:"metadata,omitempty"`
```

| 字段 | Go 类型 | JSON | 说明 |
|------|---------|------|------|
| `apiVersion` | string | `apiVersion` | `ebs/v1` |
| `kind` | string | `kind` | Project / Snapshot / Build / BuildInfo / RpmRepo / BuildResource / Job / Runner |
| `name` | string | `name` | 资源名称。Project/Runner 为集群内唯一；Snapshot/Build/BuildInfo/RpmRepo/BuildResource/Job 在所属 Project 内唯一。Project 名需满足 DNS1123 label 约束，只能使用小写字母、数字和 `-`；`default` 是系统保留名称，不能用于 Project |
| `uid` | string | `uid` | 系统生成的唯一 ID |
| `resourceVersion` | string | `resourceVersion` | 乐观锁版本号 |
| `generation` | int64 | `generation` | spec 变更递增 |
| `creationTimestamp` | Time | `creationTimestamp` | 创建时间 |
| `labels` | map[string]string | `labels` | 查询/筛选标签；系统保留标签见 [EulerMaker 标签约定](./labels.md) |
| `annotations` | map[string]string | `annotations` | 非标识元数据 |
| `deletionTimestamp` | Time | `deletionTimestamp` | 删除标记时间 |
| `finalizers` | []string | `finalizers` | 删除前清理操作 |

List 资源内嵌 `metav1.TypeMeta` 和 `metav1.ListMeta`：

```go
metav1.TypeMeta `json:",inline"`
metav1.ListMeta `json:"metadata,omitempty"`
Items           []Xxx `json:"items"`
```

Project 下的子资源使用嵌套路由，路径中的 `{project}` 是 Snapshot、Build、BuildInfo、RpmRepo、BuildResource、Job 的唯一项目归属来源。。

调度器和控制器可使用全局系统 API 跨 Project list 大部分对象。在 Project 级资源中，只有 Job 的全局 API 支持 watch。集群级资源 Runner 的 API 同样支持 list/watch。用户侧和项目侧调用使用 Project API。

当前 apiserver 基于 `GenericAPIServer` 实现，Project API 会在服务端重写到 scoped storage 路径，因此 Project 名必须满足 DNS1123 label 约束。需要展示带点号、空格或大小写的项目名时，使用 `Project.spec.displayName`。

| 子资源 | Project API | 全局 API | 主存储 | 对象定位 |
|--------|-------------|----------|--------|----------|
| Snapshot | `/apis/ebs/v1/projects/{project}/snapshots` | `/apis/ebs/v1/snapshots` | Elasticsearch | `ebs-snapshots` / `{project}/{name}` |
| Build | `/apis/ebs/v1/projects/{project}/builds` | `/apis/ebs/v1/builds` | Elasticsearch | `ebs-builds` / `{project}/{name}` |
| Job | `/apis/ebs/v1/projects/{project}/jobs` | `/apis/ebs/v1/jobs` | etcd | `/registry/ebs/jobs/{project}/{name}` |
| BuildInfo | `/apis/ebs/v1/projects/{project}/buildinfos` | `/apis/ebs/v1/buildinfos` | Elasticsearch | `ebs-buildinfos` / `{project}/{name}` |
| RpmRepo | `/apis/ebs/v1/projects/{project}/rpmrepos` | `/apis/ebs/v1/rpmrepos` | Elasticsearch | `ebs-rpmrepos` / `{project}/{name}` |
| BuildResource | `/apis/ebs/v1/projects/{project}/buildresources` | 不提供 | Elasticsearch | `ebs-buildresources` / `{project}/{name}` |

表中 Elasticsearch 对象定位格式为“索引 / 文档 ID”。Project scoped 对象统一使用 `{project}/{name}` 作为文档 ID；Job 使用相同层级的 etcd key。只有 Job 和 Runner 存入 etcd 并提供 list/watch。

---

## 结构体总览（48 个）

```
主资源（8）: Project Snapshot Build BuildInfo RpmRepo BuildResource Job Runner
列表类型（8）: ProjectList SnapshotList BuildList BuildInfoList RpmRepoList BuildResourceList JobList RunnerList
辅助结构体（32）: ProjectSpec ProjectStatus SnapshotSpec SnapshotStatus
                  BuildSpec BuildStatus BootstrapRepo JobSpec JobStatus
                  BuildInfoSpec BuildInfoStatus SpecDepend SpecStatus SpecBuildStatus SpecInstallStatus MissingDep
                  RpmRepoSpec RpmRepoStatus RpmMeta
                  BuildResourceSpec PackageResourceConfig
                  RunnerSpec RunnerTaint RunnerStatus RunnerAddress RunnerInfo
                  ResourceRequirements Toleration BuildTarget
                  PackageRepo PackageRepoStatus VersionConst
```

---

## 一、Project（项目）

**API**: `/apis/ebs/v1/projects`  
**Elasticsearch**: 索引 `ebs-projects`，文档 ID `{name}`

### Project

```go
type Project struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ProjectSpec   `json:"spec,omitempty"`
    Status ProjectStatus `json:"status,omitempty"`
}
```

### ProjectSpec

```go
type ProjectSpec struct {
    DisplayName      string                     `json:"displayName,omitempty"`
    Description      string                     `json:"description,omitempty"`
    DefaultRef       GitRef                     `json:"defaultRef,omitempty"`
    BuildPayload     string                     `json:"buildPayload,omitempty"`
    BuildTargets     []BuildTarget              `json:"buildTargets,omitempty"`
    PackageRepos     []PackageRepo              `json:"packageRepos,omitempty"`
    BootstrapRepo    []BootstrapRepo            `json:"bootstrapRepo,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `displayName` | string | 否 | 页面展示名称，默认使用创建时的 Project 名称 |
| `description` | string | 否 | 项目描述 |
| `defaultRef` | GitRef | 否 | 默认 spec 引用，仅允许 `Branch` / `Tag`；整体为空时默认 `{type: Branch, value: master}`，不接受旧字符串形式 |
| `buildPayload` | string | 否 | 构建环境宏，YAML 格式 |
| `buildTargets` | []BuildTarget | 是 | 构建目标列表 |
| `packageRepos` | []PackageRepo | 否 | 包仓库列表 |
| `bootstrapRepo` | []BootstrapRepo | 否 | Project 默认使用的引导 RPM 仓库 |

### ProjectStatus

```go
type ProjectStatus struct {
    Phase ProjectPhase `json:"phase,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | `ProjectPhase` | 公共 `ebs/v1` API 定义的稳定取值：`"Active"` / `"Terminating"` |

Project 不保存最新构建状态。调用方按 Project 路径查询 Build，通过 `ebs.io/target-os`、`ebs.io/target-arch` label 过滤，并使用默认的创建时间倒序和 `limit=1` 获取目标下最新 Build；构建状态以该 Build 的 `status` 为准。

### ProjectList

```go
type ProjectList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []Project `json:"items"`
}
```

---

## 二、Snapshot（快照）

**API**: `/apis/ebs/v1/projects/{project}/snapshots`  
**全局 API**: `/apis/ebs/v1/snapshots`  
**Elasticsearch**: 索引 `ebs-snapshots`，文档 ID `{project}/{name}`

### Snapshot

```go
type Snapshot struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   SnapshotSpec   `json:"spec,omitempty"`
    Status SnapshotStatus `json:"status,omitempty"`
}
```

### SnapshotSpec

```go
type SnapshotSpec struct {
    PackageRepos []PackageRepo        `json:"packageRepos,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `packageRepos` | []PackageRepo | 否 | 创建 Snapshot 时使用的包仓库列表 |

### SnapshotStatus

```go
type SnapshotStatus struct {
    Phase               SnapshotPhase                `json:"phase,omitempty"`
    PackageRepoStatuses map[string]PackageRepoStatus `json:"packageRepoStatuses,omitempty"`
    Conditions          []metav1.Condition           `json:"conditions,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | `SnapshotPhase` | 公共 `ebs/v1` API 定义的稳定取值：`Pending` / `Processing` / `Active` |
| `packageRepoStatuses` | map[string]PackageRepoStatus | Snapshot Controller 根据 `spec.packageRepos` 写入的各包解析状态；成功记录 commit，失败记录包级 error |
| `conditions` | []metav1.Condition | 仅记录无法归属于具体包的 Snapshot 整体异常，不使用包名作为 condition type |

### SnapshotList

```go
type SnapshotList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []Snapshot `json:"items"`
}
```

---

## 三、Build（构建）

**API**: `/apis/ebs/v1/projects/{project}/builds`  
**全局 API**: `/apis/ebs/v1/builds`  
**Elasticsearch**: 索引 `ebs-builds`，文档 ID `{project}/{name}`

### Build

```go
type Build struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   BuildSpec   `json:"spec,omitempty"`
    Status BuildStatus `json:"status,omitempty"`
}
```

### BuildSpec

```go
type BuildSpec struct {
    BuildType          string                 `json:"buildType,omitempty"`
    BootstrapRepo      []BootstrapRepo        `json:"bootstrapRepo,omitempty"`
    Packages           []string               `json:"packages,omitempty"`
    BuildTarget        BuildTarget            `json:"buildTarget,omitempty"`
}
```

| 字段             | Go 类型 | 必填 | 说明 |
|----------------|---------|------|------|
| `buildType`    | string | 否 | 构建类型：`"full"` / `"incremental"` / `"specified"` / `"single"`，默认 `"full"` |
| `buildTarget`  | BuildTarget | 是 | 构建目标 |
| `bootstrapRepo` | []BootstrapRepo | 否 | 引导 RPM 仓库 |
| `packages`     | []string | 是 | 构建的软件包 |

Build 创建后整个 `spec` 不可修改；普通 Update 只能修改服务端允许的 metadata，运行状态通过 `/status` 子资源更新。

### BootstrapRepo

```go
type BootstrapRepo struct {
    Name      string           `json:"name,omitempty"`
    Repo      string           `json:"repo,omitempty"`
}
```

| 字段     | Go 类型 | 说明                    |
|--------|---------|-----------------------|
| `name` | string | repo名称，如 `"everything"` |
| `repo` | string | 软件源地址                 |

### BuildStatus

```go
type BuildStatus struct {
    Phase        BuildPhase         `json:"phase,omitempty"`
    Stage        string             `json:"stage,omitempty"`
    StartTime    metav1.Time        `json:"startTime,omitempty"`
    EndTime      metav1.Time        `json:"endTime,omitempty"`
    Repo         string             `json:"repo,omitempty"`
    BaseBuildRef *BaseBuildRef      `json:"baseBuildRef,omitempty"`
    Conditions   []metav1.Condition `json:"conditions,omitempty"`
}

type BaseBuildRef struct {
    Name string `json:"name,omitempty"`
    Repo string `json:"repo,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | `BuildPhase` | 公共 `ebs/v1` API 定义的稳定取值：`"Pending"` / `"Prepared"` / `"Processing"` / `"Success"` / `"Failed"` / `"Aborted"` / `"Skipped"`；后四项为终态 |
| `stage` | string | `"build"` / `"publish"`，标识构建阶段还是发布阶段 |
| `startTime` | metav1.Time | 开始时间 |
| `endTime` | metav1.Time | 结束时间 |
| `repo` | string | 生成的仓库 url |
| `baseBuildRef` | BaseBuildRef | 增量构建使用的基础 Build 名称及其仓库地址；没有基础 Build 时省略 |
| `conditions` | []metav1.Condition | 状态条件 |

### BuildList

```go
type BuildList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []Build `json:"items"`
}
```

---

## 四、BuildInfo（构建信息）

### BuildInfo

**API**: `/apis/ebs/v1/projects/{project}/buildinfos`
**全局 API**: `/apis/ebs/v1/buildinfos`
**Elasticsearch**: 索引 `ebs-buildinfos`，文档 ID `{project}/{name}`

```go
type BuildInfo struct {
    metav1.TypeMeta    `json:",inline"`
    metav1.ObjectMeta  `json:"metadata,omitempty"`
    Spec   BuildInfoSpec   `json:"spec,omitempty"`
    Status BuildInfoStatus `json:"status,omitempty"`
}
```

### BuildInfoSpec

```go
type BuildInfoSpec struct {
    SpecDepends  map[string]SpecDepend  `json:"specDepends,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `specDepends` | map[string]SpecDepend | key 为 `specName`，value 为该 spec 的依赖信息 |

---

### SpecDepend

```go
type SpecDepend struct {
    RepoName      string                  `json:"repoName"`
    SpecName      string                  `json:"specName"`
    SpecFileName  string                  `json:"specFileName,omitempty"`
    Version       string                  `json:"version"`
    Release       string                  `json:"release,omitempty"`
    Epoch         string                  `json:"epoch,omitempty"`
    ExclusiveArch []string                `json:"exclusiveArch,omitempty"`
    Provides      []string                `json:"provides,omitempty"`
    Requires      map[string]VersionConst `json:"requires,omitempty"`
    BuildRequires map[string]VersionConst `json:"buildRequires,omitempty"`
    BuildRemoves  map[string]VersionConst `json:"buildRemoves,omitempty"`
}
```

| 字段              | Go 类型                    | 说明                                                                |
| --------------- | ---------------------------  | ----------------------------------------------------------------- |
| `repoName`      | string                  | spec 所属仓库名 |
| `specName`      | string                  | spec 名称 |
| `specFileName`  | string                  | 解析的 spec 文件名（如 `gcc.spec`） |
| `version`       | string                  | 完整版本号（`epoch:version-release` 拼接后的字符串） |
| `release`       | string                  | 原始 release 段值 |
| `epoch`         | string                  | 原始 epoch 段值  |
| `exclusiveArch` | []string                | `ExclusiveArch` 减去 `ExcludeArch` 之后的最终架构列表 `EXCLUSIVE_ARCH` |
| `provides`      | []string                | spec 声明的 Provides（宏已展开） |
| `requires`      | map[string]VersionConst | 安装期依赖 Requires |
| `buildRequires` | map[string]VersionConst | 构建期依赖  |
| `buildRemoves`  | map[string]VersionConst | 以 `-` 开头的 BuildRequires 列表 |

---

### BuildInfoStatus

```go
type BuildInfoStatus struct {
    Phase       BuildInfoPhase        `json:"phase,omitempty"`
    Conditions  []metav1.Condition    `json:"conditions,omitempty"`
    SpecStatus  map[string]SpecStatus `json:"specStatus,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | `BuildInfoPhase` | 公共 `ebs/v1` API 定义的稳定取值：`"Pending"` / `"Processing"` / `"Completed"` |
| `conditions` | []metav1.Condition | 状态条件 |
| `specStatus` | map[string]SpecStatus | 各 spec 运行时状态 |

### SpecStatus

```go
type SpecStatus struct {
    Build   SpecBuildStatus   `json:"build,omitempty"`
    Install SpecInstallStatus `json:"install,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `build` | SpecBuildStatus | 构建状态 |
| `install` | SpecInstallStatus | 安装状态 |

---

### SpecBuildStatus

```go
type SpecBuildStatus struct {
    Status     string             `json:"status"`
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    JobName    string             `json:"jobName,omitempty"`
}
```

| 字段           | Go 类型 | 说明 |
|--------------|------|------|
| `status`     | string | `"Running"` /`"Succeeded"` / `"Failed"` / `"Aborted"`|
| `conditions` | []metav1.Condition | 构建状态条件 |
| `jobName`    | string | 最近一次关联的远端 jobName |

---

### SpecInstallStatus

```go
type SpecInstallStatus struct {
    Status      string                 `json:"status"`
    MissingDeps map[string]MissingDep  `json:"missingDeps,omitempty"`
    Conditions  []metav1.Condition     `json:"conditions,omitempty"`
}
```

| 字段 | Go 类型 | 说明                  |
|------|---------|---------------------|
| `status` | string | `Succeeded` / `Failed` |
| `missingDeps` | map[string]MissingDep | install 失败时记录       |
| `conditions` | []metav1.Condition | install 状态条件 |

---

### MissingDep

```go
type MissingDep struct {
    NeededBy        string          `json:"neededBy,omitempty"`
    VersionRequests VersionConst    `json:"versionRequests,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `neededBy` | string | 依赖此 rpm 的上游 spec |
| `versionRequests` | VersionConst | 版本约束 |

---

### BuildInfoList

```go
type BuildInfoList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []BuildInfo `json:"items"`
}
```

---

## 五、RpmRepo（过程仓与正式发布状态）

### RpmRepo

**API**: `/apis/ebs/v1/projects/{project}/rpmrepos`
**全局 API**: `/apis/ebs/v1/rpmrepos`
**Elasticsearch**: 索引 `ebs-rpmrepos`，文档 ID `{project}/{name}`

```go
type RpmRepo struct {
    metav1.TypeMeta    `json:",inline"`
    metav1.ObjectMeta  `json:"metadata,omitempty"`
    Spec   RpmRepoSpec   `json:"spec,omitempty"`
    Status RpmRepoStatus `json:"status,omitempty"`
}
```

### RpmRepoSpec

```go
type RpmRepoSpec struct {
}
```

RpmRepo 与 Build 一对一，`metadata.name` 与 Build name 相同。RpmRepo 是可持续推进的逻辑仓库，每批完成 Job 在 Artifact Manager 中产生一个新的不可变物理版本。

```go
type RepositoryInput struct {
    JobName            string `json:"jobName"`
    JobUID             string `json:"jobUID"`
    SpecName           string `json:"specName"`
}

type RepositoryTransition struct {
    Inputs             []RepositoryInput `json:"inputs"`
    BaseRepositoryUID  string `json:"baseRepositoryUID,omitempty"`
    RepositoryUID      string `json:"repositoryUID"`
}

type ReleaseTransition struct {
    SourceRepositoryUID string   `json:"sourceRepositoryUID"`
    ExcludeSpecs        []string `json:"excludeSpecs,omitempty"`
}
```

`RepositoryTransition` 是提交物化前先写入的恢复检查点。存在 transition 时不得选择新输入或重新计算基础仓。
`ReleaseTransition` 在提交正式发布前固化源过程仓和排除集合；存在该字段时必须恢复原请求，不得根据最新 Project 或 BuildInfo 重新计算。

| 类型与字段 | 说明 |
|------------|------|
| `RepositoryInput.jobName` | 输入 Job 名称，用于 API 定位和诊断 |
| `RepositoryInput.jobUID` | 输入 Job 的稳定身份，用于 Manifest 查询和幂等计算 |
| `RepositoryInput.specName` | Job 产出的 spec，用于批次去重和错误归属 |
| `RepositoryTransition.inputs` | 已冻结并按稳定顺序保存的本批输入 |
| `RepositoryTransition.baseRepositoryUID` | 本次物化继承的不可变基础仓；首次构建可为空 |
| `RepositoryTransition.repositoryUID` | 根据固定输入计算出的目标物理版本 UID |
| `ReleaseTransition.sourceRepositoryUID` | 正式发布使用的 Ready 过程仓 UID |
| `ReleaseTransition.excludeSpecs` | 已规范化、去重并按字典序保存的排除集合 |

### RpmRepoStatus

```go
type RpmRepoRepositoryStatus struct {
    Phase             RpmRepoPhase          `json:"phase,omitempty"`
    RepositoryUID     string                `json:"repositoryUID,omitempty"`
    ContentURL        string                `json:"contentURL,omitempty"`
    RepositoryDigest string                `json:"repositoryDigest,omitempty"`
    PackageCount      int                   `json:"packageCount,omitempty"`
    RpmDepends        map[string]RpmMeta    `json:"rpmDepends,omitempty"`
    SourceJobUIDs     []string              `json:"sourceJobUIDs,omitempty"`
    Transition        *RepositoryTransition `json:"transition,omitempty"`
    UpdatedAt         *metav1.Time          `json:"updatedAt,omitempty"`
}

type RpmRepoReleaseStatus struct {
    Phase               RpmRepoReleasePhase `json:"phase,omitempty"`
    SourceRepositoryUID string              `json:"sourceRepositoryUID,omitempty"`
    ContentURL          string              `json:"contentURL,omitempty"`
    ReleaseDigest       string              `json:"releaseDigest,omitempty"`
    PackageCount        int                 `json:"packageCount,omitempty"`
    Transition          *ReleaseTransition  `json:"transition,omitempty"`
    UpdatedAt           *metav1.Time        `json:"updatedAt,omitempty"`
}

type RpmRepoStatus struct {
    Repository       *RpmRepoRepositoryStatus `json:"repository,omitempty"`
    Release          *RpmRepoReleaseStatus    `json:"release,omitempty"`
    Conditions        []metav1.Condition    `json:"conditions,omitempty"`
}
```

| 字段             | Go 类型            | 说明                                           |
|----------------|------------------|----------------------------------------------|
| `repository` | *RpmRepoRepositoryStatus | 构建过程仓的当前版本和推进状态 |
| `release` | *RpmRepoReleaseStatus | 正式发布的独立状态；不与过程仓 phase 混用 |
| `conditions` | []metav1.Condition | 状态条件（记录失败原因等） |

| `repository` 字段 | Go 类型 | 说明 |
|--------------------|---------|------|
| `phase` | RpmRepoPhase | `"Pending"` / `"Processing"` / `"Ready"` / `"Failed"`；逻辑仓库可持续推进，均不是对象终态 |
| `repositoryUID` | string | 当前已发布不可变物理版本的 UID |
| `contentURL` | string | 当前物理版本的不可变仓库地址 |
| `repositoryDigest` | string | 仓库内容的确定性 SHA-256 摘要 |
| `packageCount` | int | 仓库 RPM 文件数量 |
| `rpmDepends` | map[string]RpmMeta | 仓库中每个 RPM 的元信息 |
| `sourceJobUIDs` | []string | 当前物理版本对应的输入 Job UID 集合，按字典序保存 |
| `transition` | *RepositoryTransition | 正在物化或等待确认的下一版本 |
| `updatedAt` | *metav1.Time | 当前物理版本的发布时间 |

`release.phase` 的稳定取值为 `Pending`、`Creating`、`Prepared`、`Ready`、`Failed`。`release.transition` 只在正式发布尚未完成时存在；发布准备和激活成功后，将固定输入提升到 `sourceRepositoryUID`，写入 `contentURL`、`releaseDigest`、`packageCount`，再清除 transition。失败原因写入 RpmRepo 顶层 `conditions`，condition type 必须区分过程仓和正式发布错误。

| `release` 字段 | Go 类型 | 说明 |
|----------------|---------|------|
| `phase` | RpmRepoReleasePhase | 正式发布状态 |
| `sourceRepositoryUID` | string | 已发布版本使用的过程仓 UID |
| `contentURL` | string | Project/架构稳定仓库入口 |
| `releaseDigest` | string | 正式版本内容摘要 |
| `packageCount` | int | 正式版本包含的 RPM 数量 |
| `transition` | *ReleaseTransition | 正在准备或激活的固定发布输入 |
| `updatedAt` | *metav1.Time | 正式发布状态最近更新时间 |

### RpmMeta

```go
type RpmMeta struct {
    Version  string                  `json:"version"`
    SpecName string                  `json:"specName"`
    Provides map[string]string       `json:"provides,omitempty"`
    Requires map[string]VersionConst `json:"requires,omitempty"`
}
```

| 字段         | Go 类型             | 说明                                |
|------------|-------------------|-----------------------------------|
| `version`  | string            | 版本号（`epoch:ver-rel` 格式）           |
| `specName` | string            | 由哪个 spec 产出                       |
| `provides` | map[string]string | rpm 元数据里的 Provides 声明            |
| `requires` | map[string]VersionConst | rpm 元数据里的 Requires 声明（运行时依赖）     |

---

### RpmRepoList

```go
type RpmRepoList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []RpmRepo `json:"items"`
}
```

---

## 六、BuildResource（构建资源表）

**API**: `/apis/ebs/v1/projects/{project}/buildresources`

**Elasticsearch**: 索引 `ebs-buildresources`，文档 ID `{project}/{name}`

Project 对象不存在时如何回退到默认对象，以及 apiserver 如何初始化默认对象，见 [BuildResource 设计文档](./build-resource.md)。

`BuildResource` 不注册 `/apis/ebs/v1/buildresources` 全局 API。系统组件、运维工具和普通用户都必须通过明确的 Project 路径访问，避免跨 Project 枚举或误更新资源表。

`BuildResource` 不属于公开读取资源。Project owner 和 member 只能读取与自己具有 owner/member 关系的 Project 下的对象，禁止全部写操作；跨 Project 读写仅允许运维或受信任系统身份。

### BuildResource

```go
type BuildResource struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec              BuildResourceSpec `json:"spec,omitempty"`
}
```

### BuildResourceSpec

```go
type BuildResourceSpec struct {
    Default  ResourceRequirements             `json:"default,omitempty"`
    Packages map[string]PackageResourceConfig `json:"packages"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `default` | ResourceRequirements | 是 | 表级默认资源需求，requests 必须完整声明 CPU 和 memory；limits 可缺省并取同级 requests |
| `packages` | map[string]PackageResourceConfig | 是 | Project 下全部软件包的资源配置，Map key 为 spec 包名。Project 自定义表不得为空；`default/default` 可为空，但必须声明有效的表级 `default` |

`BuildResourceSpec` 不包含 OS 字段。同一张表适用于所属 Project 的全部 Build Target OS。

### PackageResourceConfig

```go
type PackageResourceConfig struct {
    Default ResourceRequirements            `json:"default,omitempty"`
    Arches  map[string]ResourceRequirements `json:"arches,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `default` | ResourceRequirements | 否 | 该软件包未匹配架构专属配置时使用的默认资源需求 |
| `arches` | map[string]ResourceRequirements | 否 | 按 CPU 架构记录的资源需求；key 使用规范架构名 |

每个软件包必须至少声明 `default` 或一个 `arches` 条目。架构采用开放集合，不固定为 `x86_64` 和 `aarch64`；可增加 `riscv64` 等新架构。架构名必须满足 `^[a-z0-9][a-z0-9._-]{0,62}$`，并与 Build Target 和 Runner label 使用的名称完全一致。

BuildResource 只允许 `cpu` 和 `memory` 两种资源键。表级 `spec.default.requests` 必须完整声明二者；limits 可以缺省。软件包 default 和架构配置可以只覆盖其中一个或多个键。任一级声明某项 request 但省略对应 limit 时，limit 取同级 request；该级未声明 request 时，request 和 limit 按“架构配置 → 软件包 default → 表级 default”逐字段继承。合并结果必须满足每项 limit 大于或等于 request。所有资源值必须是大于 0、可由 Kubernetes `resource.ParseQuantity` 解析的字符串。

配置按以下顺序逐字段覆盖：

1. 使用 `spec.default` 初始化完整配置；
2. 使用 `packages[specName].default` 覆盖已声明字段；
3. 使用 `packages[specName].arches[arch]` 覆盖已声明字段；
4. 未覆盖字段保留 `spec.default` 值。

`requests` 和 `limits` 分别按 `cpu`、`memory` 键合并，不在 Project 对象和 `default/default` 对象之间跨对象补齐。

### BuildResourceList

```go
type BuildResourceList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []BuildResource `json:"items"`
}
```

---

## 七、Job（任务）

**API**: `/apis/ebs/v1/projects/{project}/jobs`  
**全局 API**: `/apis/ebs/v1/jobs`  
**etcd**: `/registry/ebs/jobs/{project}/{name}`

### Job

```go
type Job struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   JobSpec   `json:"spec,omitempty"`
    Status JobStatus `json:"status,omitempty"`
}
```

### JobSpec

```go
type JobSpec struct {
    Priority     int64                `json:"priority,omitempty"`
    Runtime      string               `json:"runtime,omitempty"`
    RuntimeSpec  runtime.RawExtension `json:"runtimeSpec,omitempty"`
    TimeoutSeconds int64              `json:"timeoutSeconds,omitempty"`
    Resources    ResourceRequirements `json:"resources,omitempty"`
    NodeSelector map[string]string    `json:"nodeSelector,omitempty"`
    Tolerations  []Toleration         `json:"tolerations,omitempty"`
    Payload      string               `json:"payload,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `priority` | int64 | 否 | Job 调度优先级，值越大越优先，默认：0 |
| `runtime` | string | 否 | 执行运行时类型，如 `ct`/`vm`/`hw`，默认 `ct` |
| `runtimeSpec` | runtime.RawExtension | 否 | 运行时专属配置，由对应 runtime 解释 |
| `timeoutSeconds` | int64 | 否 | 最大运行秒数，默认 10800 |
| `resources` | ResourceRequirements | 否 | Job 资源请求与限制 |
| `nodeSelector` | map[string]string | 否 | Runner label 精确匹配条件，如通过 `ebs.io/runner-arch` 选择架构 |
| `tolerations` | []Toleration | 否 | 可容忍的 Runner 污点 |
| `payload` | string | 否 | YAML 格式的 Job 参数内容，用于记录任务执行所需的业务输入 |

### ResourceRequirements

```go
type ResourceRequirements struct {
    Requests map[string]string `json:"requests,omitempty"`
    Limits   map[string]string `json:"limits,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `requests` | map[string]string | 资源需求，如 `{"cpu": "4", "memory": "8Gi"}`。调度器用于匹配 Runner 的可分配容量 |
| `limits` | map[string]string | 资源上限，如 `{"cpu": "8", "memory": "16Gi"}`。用于限制 Job 最大资源使用量 |

### Toleration

```go
type Toleration struct {
    Key      string `json:"key,omitempty"`
    Operator string `json:"operator,omitempty"`
    Value    string `json:"value,omitempty"`
    Effect   string `json:"effect,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `key` | string | 匹配 Runner taint 的键 |
| `operator` | string | 匹配操作符：`Equal`（相等）、`Exists`（存在）、`Gt`（大于）、`Lt`（小于） |
| `value` | string | 匹配值，与 `key` 配合使用 |
| `effect` | string | 容忍效果：`NoSchedule`（不调度）、`PreferNoSchedule`（尽量不调度）、`NoExecute`（不执行并驱逐） |

### JobStatus

```go
type JobRepositoryState string

const (
    JobRepositoryPublished JobRepositoryState = "Published"
    JobRepositoryFailed    JobRepositoryState = "Failed"
)

type JobStatus struct {
    Phase      JobPhase    `json:"phase,omitempty"`
    Stage      JobStage    `json:"stage,omitempty"`
    Runner     string      `json:"runner,omitempty"`
    StartTime  metav1.Time `json:"startTime,omitempty"`
    EndTime    metav1.Time `json:"endTime,omitempty"`
    ResultRoot string      `json:"resultRoot,omitempty"`
    Message    string      `json:"message,omitempty"`
    RestartCount int64     `json:"restartCount,omitempty"`
    RepositoryState JobRepositoryState `json:"repositoryState,omitempty"`
    RepositoryUID   string             `json:"repositoryUID,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | `JobPhase` | 公共 `ebs/v1` API 定义的稳定取值：`"Pending"` / `"Running"` / `"Completed"` / `"Failed"` / `"Aborted"`；后三项为终态 |
| `stage` | `JobStage` | 公共 `ebs/v1` API 定义的稳定取值：`"Pending"` / `"Running"` / `"PostRun"`。失败时保留最后到达的执行阶段，不使用 `Failed` stage |
| `runner` | string | 实际执行的 runner 名称 |
| `startTime` | metav1.Time | 开始时间 |
| `endTime` | metav1.Time | 结束时间 |
| `resultRoot` | string | 结果存储路径 |
| `message` | string | 状态消息 |
| `restartCount` | int64 | 重试次数，默认 0。调度器可使用该字段计算重试退避时间 |
| `repositoryState` | JobRepositoryState | RpmRepo Controller 的处理结果：`Published` / `Failed`；空值表示尚未处理 |
| `repositoryUID` | string | `repositoryState=Published` 时记录包含该 Job 所在批次的不可变物理仓库 UID；同批 Job 共享该值 |

### JobList

```go
type JobList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []Job `json:"items"`
}
```

---

## 八、Runner（执行机）

**API**: `/apis/ebs/v1/runners`  
**etcd**: `/registry/ebs/runners/{name}`

### Runner

```go
type Runner struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   RunnerSpec   `json:"spec,omitempty"`
    Status RunnerStatus `json:"status,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `apiVersion` | string | - | `ebs/v1` |
| `kind` | string | - | `Runner` |
| `metadata` | ObjectMeta | 是 | 标准元数据 |
| `spec` | RunnerSpec | 是 | 执行机规格 |
| `status` | RunnerStatus | - | 执行机状态 |

当前 runner agent 注册 Runner 时会写入 `metadata.labels["ebs.io/runner-type"]` 和 `metadata.labels["ebs.io/runner-arch"]`，分别对应 `spec.type` 和 `spec.arch`。

### RunnerSpec

```go
type RunnerSpec struct {
    InstanceID   string        `json:"instanceId,omitempty"`
    Type          string        `json:"type,omitempty"`
    Arch          string        `json:"arch,omitempty"`
    Unschedulable bool          `json:"unschedulable,omitempty"`
    Taints        []RunnerTaint `json:"taints,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `instanceId` | string | 是 | Runner 安装实例持久化 UUID；规范小写 UUID v4，同名对象恢复时必须匹配 |
| `type` | string | 否 | 执行机类型：`ct` / `vm` / `hw`，默认 `ct` |
| `arch` | string | 是 | CPU 架构标识，不限制枚举值 |
| `unschedulable` | bool | 否 | 是否禁止调度新 Job |
| `taints` | []RunnerTaint | 否 | 反亲和污点 |

> 调度标签统一使用 `metadata.labels`，不在 `spec` 中重复定义。`spec.instanceId`、`spec.type` 和 `spec.arch` 创建后不可变；`instanceId` 不能修改或清空。

### RunnerTaint

```go
type RunnerTaint struct {
    Key    string `json:"key"`
    Value  string `json:"value,omitempty"`
    Effect string `json:"effect"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `key` | string | 污点键 |
| `value` | string | 污点值 |
| `effect` | string | 效果：`NoSchedule`/`PreferNoSchedule`/`NoExecute` |

### RunnerStatus

```go
type RunnerStatus struct {
    Phase       RunnerPhase        `json:"phase,omitempty"`
    Conditions  []metav1.Condition `json:"conditions,omitempty"`
    Capacity    map[string]string  `json:"capacity,omitempty"`
    Allocatable map[string]string  `json:"allocatable,omitempty"`
    Addresses   []RunnerAddress    `json:"addresses,omitempty"`
    Info        RunnerInfo         `json:"info,omitempty"`
    Heartbeat   metav1.Time        `json:"heartbeat,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | `RunnerPhase` | 公共 `ebs/v1` API 定义的稳定取值：`Online` / `Offline` |
| `conditions` | []Condition | 详细状态条件，当前 runner agent 暂不主动填充 |
| `capacity` | map[string]string | Runner 上报的总资源容量。当前包含 `cpu`、`memory`、`ephemeral-storage`：`cpu` 为逻辑 CPU 数，`memory` 使用 `Mi`，`ephemeral-storage` 使用 `Gi` |
| `allocatable` | map[string]string | Runner 上报的可调度资源容量。当前 `cpu`、`memory` 与 `capacity` 一致，`ephemeral-storage` 为 runner 工作目录所在文件系统的可用空间，使用 `Gi` |
| `addresses` | []RunnerAddress | 执行机地址列表 |
| `info` | RunnerInfo | 执行机系统与 agent 信息 |
| `heartbeat` | Time | 最后心跳时间 |

Runner 创建时 apiserver 默认置为 `Offline`。Runner agent 完成本地初始化并具备接收任务能力后，通过首次心跳置为 `Online`；主动下线或心跳超时后置为 `Offline`。Runner 的忙闲状态由绑定 Job 计算，不通过 Runner phase 表达。

### RunnerAddress

```go
type RunnerAddress struct {
    Type    string `json:"type,omitempty"`
    Address string `json:"address,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `type` | string | 地址类型。当前 runner agent 上报 `Hostname`，并在发现非 loopback 地址时上报 `InternalIP` |
| `address` | string | 地址值 |

### RunnerInfo

```go
type RunnerInfo struct {
    OS             string `json:"os,omitempty"`
    KernelVersion  string `json:"kernelVersion,omitempty"`
    Arch           string `json:"arch,omitempty"`
    RuntimeVersion string `json:"runtimeVersion,omitempty"`
    AgentVersion   string `json:"agentVersion,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `os` | string | 操作系统 |
| `kernelVersion` | string | 内核版本 |
| `arch` | string | CPU 架构 |
| `runtimeVersion` | string | 执行运行时版本，当前 runner agent 暂不主动填充 |
| `agentVersion` | string | Runner agent 版本 |

### RunnerList

```go
type RunnerList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []Runner `json:"items"`
}
```

---

## 九、公共子结构体

### BuildTarget

```go
type BuildTarget struct {
    Os             string      `json:"os,omitempty"`
    Arch           string      `json:"arch,omitempty"`
    BuildFlag      bool        `json:"buildFlag,omitempty"`
    PublishFlag    bool        `json:"publishFlag,omitempty"`
}
```

| 字段         | Go 类型 | 说明                         |
|------------|---------|----------------------------|
| `os` | string | 构建os |
| `arch`     | string | `"aarch64"` / `"x86_64"`   |
| `buildFlag`    | bool | 构建标志                       |
| `publishFlag`    | bool | 发布标志                       |

---

### PackageRepo

```go
type PackageRepo struct {
    Name          string          `json:"name,omitempty"`
    URL           string          `json:"url,omitempty"`
    Ref           GitRef          `json:"ref,omitempty"`
    BuildTargets  []BuildTarget   `json:"buildTargets,omitempty"`
}

type GitRefType string

const (
    GitRefBranch GitRefType = "Branch"
    GitRefTag    GitRefType = "Tag"
    GitRefCommit GitRefType = "Commit"
)

type GitRef struct {
    Type  GitRefType `json:"type,omitempty"`
    Value string     `json:"value,omitempty"`
}
```

| 字段             | Go 类型         | 说明                       |
|----------------|---------------|--------------------------|
| `name`         | string        | spec 包名称                 |
| `url`          | string        | spec 仓库 Git URL          |
| `ref`          | GitRef        | Git 引用；`type` 为 `Branch`、`Tag` 或 `Commit`，`value` 为对应分支名、标签名或完整 commit ID |
| `buildTargets` | []BuildTarget | 构建目标 |

`ref.type=Branch` 解析 `refs/heads/<value>`，`ref.type=Tag` 解析 `refs/tags/<value>^{commit}`，`ref.type=Commit` 直接使用 `value`。Project 创建和普通更新时，允许省略仓库的 `ref`，或传入 `null` / `{}`（type、value 均为空）；apiserver 在校验前复制 `Project.spec.defaultRef` 的 type 和 value 并持久化。工程级 `defaultRef` 仅支持 Branch/Tag，整体为空时默认成 `{type: Branch, value: master}`；仅填写 type 或 value 均校验失败。仓库显式填写的 ref 保持不变，修改工程默认引用不会改写已有 ref；仓库仍可显式使用 Commit。Snapshot 不提供该默认逻辑，仍要求每个 ref 的 type 和 value 完整；从 Project 复制时使用已经补齐并固化的值。

旧清单及存量 Project 的 `spec.specBranch` 必须迁移为 `spec.defaultRef`：原字符串转换为 `{type: Branch, value: <原值>}`，原 GitRef 对象则保留 type/value 并重命名字段。不会自动读取旧字段作为默认引用。


### PackageRepoStatus

```go
type PackageRepoStatus struct {
    CloneURL string           `json:"cloneUrl,omitempty"`
    CommitID string           `json:"commitId,omitempty"`
    Error    *SpecCommitError `json:"error,omitempty"`
}

type SpecCommitError struct {
    Code      SpecCommitErrorCode `json:"code,omitempty"`
    Message   string              `json:"message,omitempty"`
    Retryable bool                `json:"retryable,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `cloneUrl` | string | git-server 返回的只读 clone URL；仅完成本轮同步确认后填写 |
| `commitId` | string | 成功解析出的完整提交 ID；设置后 error 必须为空 |
| `error` | *SpecCommitError | 包级解析错误；设置后 commitId 必须为空。`retryable=true` 表示后续继续解析 |

`SpecCommitErrorCode` 的稳定取值为 `ValidationFailed`、`SyncFailed`、`SyncTimeout`、`ResolveFailed`、`CommitConflict` 和 `RetryExhausted`。map 中不存在对应包表示尚未处理；存在 commitId 表示成功；存在不可重试 error 表示已跳过。

---

### VersionConst

```go
type VersionConst struct {
    GT string `json:"gt,omitempty"`
    GE string `json:"ge,omitempty"`
    EQ string `json:"eq,omitempty"`
    LE string `json:"le,omitempty"`
    LT string `json:"lt,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `gt` | string | `>` |
| `ge` | string | `>=` |
| `eq` | string | `=` |
| `le` | string | `<=` |
| `lt` | string | `<` |

---

## 附录 A：状态枚举汇总

| 资源 | Phase 可选值                                                                             |
|------|---------------------------------------------------------------------------------------|
| Project | `Active` / `Terminating`                                                              |
| Snapshot | `Pending` / `Processing` / `Active`                                                   |
| Build | `Pending` / `Prepared` / `Processing` / `Success` / `Failed` / `Aborted` / `Skipped` |
| BuildInfo | `Pending` / `Processing` / `Completed`                                                |
| RpmRepo | 过程仓：`Pending` / `Processing` / `Ready` / `Failed`；正式发布：`Pending` / `Creating` / `Prepared` / `Ready` / `Failed` |
| Job | `Pending` → `Running` → `Completed` / `Failed` / `Aborted`                            |
| Runner | `Offline` ↔ `Online`                                                               |

## 附录 B：结构体引用关系图

```
ProjectSpec
├── BuildTarget
├── PackageRepo
│   └── BuildTarget
└── BootstrapRepo

SnapshotSpec
└── PackageRepoStatus

BuildSpec
├── BuildTarget
└── BootstrapRepo

BuildInfoSpec ──▶ SpecDepend ──▶ VersionConst

BuildInfoStatus
└── SpecStatus
    ├── SpecBuildStatus
    └── SpecInstallStatus
        └── MissingDep ──▶ VersionConst

RpmRepoStatus
├── RpmRepoRepositoryStatus
│   ├── RpmMeta ──▶ VersionConst
│   └── RepositoryTransition ──▶ RepositoryInput
└── RpmRepoReleaseStatus ──▶ ReleaseTransition

BuildResourceSpec
├── ResourceRequirements (default)
└── PackageResourceConfig
    ├── ResourceRequirements (default)
    └── ResourceRequirements (arches)

JobSpec
├── ResourceRequirements
└── Toleration

RunnerSpec ──▶ RunnerTaint
RunnerStatus
├── RunnerAddress
└── RunnerInfo
```
