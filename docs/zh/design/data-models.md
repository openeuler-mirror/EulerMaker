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
| `kind` | string | `kind` | Project / Snapshot / Build / BuildInfo / RpmRepo / Job / Runner |
| `name` | string | `name` | 资源名称。Project/Runner 为集群内唯一；Snapshot/Build/BuildInfo/RpmRepo/Job 在所属 Project 内唯一。Project 名需满足 DNS1123 label 约束，只能使用小写字母、数字和 `-` |
| `uid` | string | `uid` | 系统生成的唯一 ID |
| `resourceVersion` | string | `resourceVersion` | 乐观锁版本号 |
| `generation` | int64 | `generation` | spec 变更递增 |
| `creationTimestamp` | Time | `creationTimestamp` | 创建时间 |
| `labels` | map[string]string | `labels` | 查询/筛选标签 |
| `annotations` | map[string]string | `annotations` | 非标识元数据 |
| `deletionTimestamp` | Time | `deletionTimestamp` | 删除标记时间 |
| `finalizers` | []string | `finalizers` | 删除前清理操作 |

List 资源内嵌 `metav1.TypeMeta` 和 `metav1.ListMeta`：

```go
metav1.TypeMeta `json:",inline"`
metav1.ListMeta `json:"metadata,omitempty"`
Items           []Xxx `json:"items"`
```

Project 下的子资源使用嵌套路由，路径中的 `{project}` 是 Snapshot、Build、BuildInfo、RpmRepo、Job 的唯一项目归属来源。子资源名称只需在所属 Project 内唯一。

调度器和控制器可使用全局系统 API 跨 Project list 对象；在 Project 级资源中，只有 Job 的全局 API 支持 watch。集群级资源 Runner 的 API 同样支持 list/watch。用户侧和项目侧调用使用 Project API。

当前 apiserver 基于 `GenericAPIServer` 实现，Project API 会在服务端重写到 scoped storage 路径，因此 Project 名必须满足 DNS1123 label 约束。需要展示带点号、空格或大小写的项目名时，使用 `Project.spec.displayName`。

| 子资源 | Project API | 全局 API | 主存储 | 对象定位 |
|--------|-------------|----------|--------|----------|
| Snapshot | `/apis/ebs/v1/projects/{project}/snapshots` | `/apis/ebs/v1/snapshots` | Elasticsearch | `ebs-snapshots` / `{project}/{name}` |
| Build | `/apis/ebs/v1/projects/{project}/builds` | `/apis/ebs/v1/builds` | Elasticsearch | `ebs-builds` / `{project}/{name}` |
| Job | `/apis/ebs/v1/projects/{project}/jobs` | `/apis/ebs/v1/jobs` | etcd | `/registry/ebs/jobs/{project}/{name}` |
| BuildInfo | `/apis/ebs/v1/projects/{project}/buildinfos` | `/apis/ebs/v1/buildinfos` | Elasticsearch | `ebs-buildinfos` / `{project}/{name}` |
| RpmRepo | `/apis/ebs/v1/projects/{project}/rpmrepos` | `/apis/ebs/v1/rpmrepos` | Elasticsearch | `ebs-rpmrepos` / `{project}/{name}` |

表中 Elasticsearch 对象定位格式为“索引 / 文档 ID”。Project scoped 对象统一使用 `{project}/{name}` 作为文档 ID；Job 使用相同层级的 etcd key。只有 Job 和 Runner 存入 etcd 并提供 list/watch。

---

## 结构体总览（44 个）

```
主资源（7）: Project Snapshot Build BuildInfo RpmRepo Job Runner
列表类型（7）: ProjectList SnapshotList BuildList BuildInfoList RpmRepoList JobList RunnerList 
辅助结构体（30）: ProjectSpec ProjectStatus SnapshotSpec SnapshotStatus
                  BuildSpec BuildStatus BootstrapRepo JobSpec JobStatus
                  BuildInfoSpec BuildInfoStatus SpecDepend SpecStatus SpecBuildStatus SpecInstallStatus MissingDep
                  RpmRepoSpec RpmRepoStatus RpmMeta
                  RunnerSpec RunnerTaint RunnerStatus RunnerAddress RunnerInfo
                  ResourceRequirements Toleration BuildTarget
                  PackageRepo SpecCommit VersionConst
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
    SpecBranch       string                     `json:"specBranch,omitempty"`
    BuildPayload     string                     `json:"buildPayload,omitempty"`
    BuildTargets     []BuildTarget              `json:"buildTargets,omitempty"`
    PackageRepos     []PackageRepo              `json:"packageRepos,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `displayName` | string | 否 | 页面展示名称，默认使用创建时的 Project 名称 |
| `description` | string | 否 | 项目描述 |
| `specBranch` | string | 否 | 默认 spec 分支，默认 `"master"` |
| `buildPayload` | string | 否 | 构建环境宏，YAML 格式 |
| `buildTargets` | []BuildTarget | 是 | 构建目标列表 |
| `packageRepos` | []PackageRepo | 否 | 包仓库列表 |

### ProjectStatus

```go
type ProjectStatus struct {
    Phase             string                 `json:"phase,omitempty"`
    LastBuildStatus   map[string]string      `json:"lastBuildStatus,omitempty"`
}
```

| 字段 | Go 类型             | 说明                           |
|------|-------------------|------------------------------|
| `phase` | string            | `"Active"` / `"Terminating"` |
| `lastBuildStatus` | map[string]string | key是构建os/arch,value是最新构建 ID  |

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
    PrevSnapshot     string                 `json:"prevSnapshot,omitempty"`
    SpecCommits      map[string]SpecCommit  `json:"specCommits,omitempty"`
    BuildTargets     []BuildTarget          `json:"buildTargets,omitempty"`
    PackageRepos     []PackageRepo          `json:"packageRepos,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `prevSnapshot` | string | 否 | 同一 Project 下的前一快照名称（增量构建用） |
| `specCommits` | map[string]SpecCommit | 是 | 各包 spec 提交信息 |
| `buildTargets` | []BuildTarget | 是 | 构建目标 |
| `packageRepos` | []PackageRepo | 是 | 快照包含的软件包仓库列表 |

### SnapshotStatus

```go
type SnapshotStatus struct {
    Phase        string             `json:"phase,omitempty"`
    Conditions   []metav1.Condition `json:"conditions,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | string | `Pending` / `Processing` / `Active` |
| `conditions` | []metav1.Condition | 状态条件 |

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
    SnapshotName       string                 `json:"snapshotName,omitempty"`
    BuildType          string                 `json:"buildType,omitempty"`
    BootstrapRepo      []BootstrapRepo        `json:"bootstrapRepo,omitempty"`
    Packages           []string               `json:"packages,omitempty"`
    BuildTarget        BuildTarget            `json:"buildTarget,omitempty"`
    PrevBuildRepo      string                 `json:"prevBuildRepo,omitempty"`
}
```

| 字段             | Go 类型 | 必填 | 说明 |
|----------------|---------|------|------|
| `snapshotName` | string | 是 | 使用同一 Project 下的快照 |
| `buildType`    | string | 否 | 构建类型：`"full"` / `"incremental"` / `"specified"` / `"single"`，默认 `"full"` |
| `buildTarget`  | BuildTarget | 是 | 构建目标 |
| `bootstrapRepo` | []BootstrapRepo | 否 | 引导 RPM 仓库 |
| `packages`     | []string | 是 | 构建的软件包 |
| `prevBuildRepo` | string | 否 | 上一次构建的最终 repo url |

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
    Phase        string             `json:"phase,omitempty"`
    Stage        string             `json:"stage,omitempty"`
    StartTime    metav1.Time        `json:"startTime,omitempty"`
    EndTime      metav1.Time        `json:"endTime,omitempty"`
    Repo         string             `json:"repo,omitempty"`
    Conditions   []metav1.Condition `json:"conditions,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | string | `"Pending"` / `"Prepared"` / `"Processing"` / `"Success"` / `"Failed"` / `"Aborting"` / `"Aborted"` |
| `stage` | string | `"build"` / `"publish"`，标识构建阶段还是发布阶段 |
| `startTime` | metav1.Time | 开始时间 |
| `endTime` | metav1.Time | 结束时间 |
| `repo` | string | 生成的仓库 url |
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
    Phase       string                `json:"phase,omitempty"`
    Conditions  []metav1.Condition    `json:"conditions,omitempty"`
    SpecStatus  map[string]SpecStatus `json:"specStatus,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | string | `"Pending"` / `"Processing"` / `"Completed"` |
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

## 五、RpmRepo（RPM 仓库解析信息）

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

### RpmRepoStatus

```go
type RpmRepoStatus struct {
    Phase        string                  `json:"phase,omitempty"`
    RpmDepends   map[string]RpmMeta      `json:"rpmDepends,omitempty"`
    Conditions   []metav1.Condition      `json:"conditions,omitempty"`
}
```

| 字段             | Go 类型            | 说明                                           |
|----------------|------------------|----------------------------------------------|
| `phase`        | string           | `"Pending"` / `"Processing"` / `"Completed"` |
| `rpmDepends`   | map[string]RpmMeta                 | 仓库里每个 rpm 的元信息,key 格式 `<rpm名>@<spec名>`       |
| `conditions`   | []metav1.Condition | 状态条件（记录失败原因等）                                |

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

## 六、Job（任务）

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
type JobStatus struct {
    Phase      string      `json:"phase,omitempty"`
    Stage      string      `json:"stage,omitempty"`
    Runner     string      `json:"runner,omitempty"`
    StartTime  metav1.Time `json:"startTime,omitempty"`
    EndTime    metav1.Time `json:"endTime,omitempty"`
    ResultRoot string      `json:"resultRoot,omitempty"`
    Message    string      `json:"message,omitempty"`
    RestartCount int64     `json:"restartCount,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | string | `"Pending"` / `"Running"` / `"Completed"` / `"Failed"` / `"Aborted"` |
| `stage` | string | `"Pending"` / `"Running"` / `"PostRun"` / `"Failed"` |
| `runner` | string | 实际执行的 runner 名称 |
| `startTime` | metav1.Time | 开始时间 |
| `endTime` | metav1.Time | 结束时间 |
| `resultRoot` | string | 结果存储路径 |
| `message` | string | 状态消息 |
| `restartCount` | int64 | 重试次数，默认 0。调度器可使用该字段计算重试退避时间 |

### JobList

```go
type JobList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []Job `json:"items"`
}
```

---

## 七、Runner（执行机）

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
    Type          string        `json:"type,omitempty"`
    Arch          string        `json:"arch,omitempty"`
    Hostname      string        `json:"hostname,omitempty"`
    Unschedulable bool          `json:"unschedulable,omitempty"`
    Taints        []RunnerTaint `json:"taints,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `type` | string | 否 | 执行机类型：`ct` / `vm` / `hw`，默认 `ct` |
| `arch` | string | 是 | CPU 架构：`aarch64`/`x86_64` |
| `hostname` | string | 否 | 执行机主机名。当前 runner agent 填写 runner 资源名 |
| `unschedulable` | bool | 否 | 是否禁止调度新 Job |
| `taints` | []RunnerTaint | 否 | 反亲和污点 |

> 调度标签统一使用 `metadata.labels`，不在 `spec` 中重复定义。`spec.type` 和 `spec.arch` 创建后不可变。

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
    Phase       string             `json:"phase,omitempty"`
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
| `phase` | string | 执行机状态：`Registering`/`Booting`/`Running`/`Idle`/`Offline` |
| `conditions` | []Condition | 详细状态条件，当前 runner agent 暂不主动填充 |
| `capacity` | map[string]string | Runner 上报的总资源容量。当前包含 `cpu`、`memory`、`ephemeral-storage`：`cpu` 为逻辑 CPU 数，`memory` 使用 `Mi`，`ephemeral-storage` 使用 `Gi` |
| `allocatable` | map[string]string | Runner 上报的可调度资源容量。当前 `cpu`、`memory` 与 `capacity` 一致，`ephemeral-storage` 为 runner 工作目录所在文件系统的可用空间，使用 `Gi` |
| `addresses` | []RunnerAddress | 执行机地址列表 |
| `info` | RunnerInfo | 执行机系统与 agent 信息 |
| `heartbeat` | Time | 最后心跳时间 |

Runner 创建时 apiserver 默认置为 `Registering`；当前 runner agent 启动后置为 `Booting`，心跳时根据是否存在运行中的 Job 置为 `Idle` 或 `Running`，退出时置为 `Offline`。

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

## 八、公共子结构体

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
    Url           string          `json:"url,omitempty"`
    Branch        string          `json:"branch,omitempty"`
    GitTag        string          `json:"gitTag,omitempty"`
    CommitId      string          `json:"commitId,omitempty"`
    BuildTargets  []BuildTarget   `json:"buildTargets,omitempty"`
}
```

| 字段             | Go 类型         | 说明                       |
|----------------|---------------|--------------------------|
| `name`         | string        | spec 包名称                 |
| `url`          | string        | spec 仓库 Git URL          |
| `branch`       | string        | spec 分支，与 `gitTag`、`commitId` 三选一 |
| `gitTag`       | string        | Git 标签，与 `branch`、`commitId` 三选一 |
| `commitId`     | string        | 指定提交 ID，与 `branch`、`gitTag` 三选一 |
| `buildTargets` | []BuildTarget | 构建目标 |


### SpecCommit

```go
type SpecCommit struct {
    SpecUrl    string `json:"specUrl,omitempty"`
    CommitId   string `json:"commitId,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `specUrl` | string | spec 仓库 URL |
| `commitId` | string | 提交 ID |

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
| Build | `Pending` / `Prepared` / `Processing` / `Success` / `Failed` / `Aborting` / `Aborted` |
| BuildInfo | `Pending` / `Processing` / `Completed`                                                |
| RpmRepo | `Pending` / `Processing` / `Completed`                                                |
| Job | `Pending` → `Running` → `Completed` / `Failed` / `Aborted`                            |
| Runner | `Registering` → `Booting` → `Idle` / `Running` → `Offline`                            |

## 附录 B：结构体引用关系图

```
ProjectSpec
├── BuildTarget
└── PackageRepo
    └── BuildTarget

SnapshotSpec
├── SpecCommit
├── BuildTarget
└── PackageRepo
    └── BuildTarget

BuildSpec
├── BuildTarget
└── BootstrapRepo

BuildInfoSpec ──▶ SpecDepend ──▶ VersionConst

BuildInfoStatus
└── SpecStatus
    ├── SpecBuildStatus
    └── SpecInstallStatus
        └── MissingDep ──▶ VersionConst

RpmRepoStatus ──▶ RpmMeta ──▶ VersionConst

JobSpec
├── ResourceRequirements
└── Toleration

RunnerSpec ──▶ RunnerTaint
RunnerStatus
├── RunnerAddress
└── RunnerInfo
```
