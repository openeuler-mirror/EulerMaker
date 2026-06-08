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
| `kind` | string | `kind` | Project / Snapshot / Build / Job / … |
| `name` | string | `name` | 资源名称。Project/Runner 为集群内唯一；Snapshot/Build/Job 在所属 Project 内唯一。Project 名需满足 DNS1123 label 约束，只能使用小写字母、数字和 `-` |
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

Project 下的子资源使用嵌套路由，路径中的 `{project}` 是 Snapshot、Build、Job 的唯一项目归属来源。子资源名称只需在所属 Project 内唯一。

调度器和控制器使用全局系统 API list/watch 全部对象；用户侧和项目侧调用使用 Project API。

当前 apiserver 基于 `GenericAPIServer` 实现，Project API 会在服务端重写到 scoped storage 路径，因此 Project 名必须满足 DNS1123 label 约束。需要展示带点号、空格或大小写的项目名时，使用 `Project.spec.displayName`。

| 子资源 | Project API | 全局 API | etcd |
|--------|-------------|----------|------|
| Snapshot | `/apis/ebs/v1/projects/{project}/snapshots` | `/apis/ebs/v1/snapshots` | `/registry/ebs/snapshots/{project}/{name}` |
| Build | `/apis/ebs/v1/projects/{project}/builds` | `/apis/ebs/v1/builds` | `/registry/ebs/builds/{project}/{name}` |
| Job | `/apis/ebs/v1/projects/{project}/jobs` | `/apis/ebs/v1/jobs` | `/registry/ebs/jobs/{project}/{name}` |

---

## 结构体总览（28 个）

```
主资源（5）: Project Snapshot Build Job Runner
列表类型（5）: ProjectList SnapshotList BuildList JobList RunnerList
辅助结构体（18）: ProjectSpec ProjectStatus SnapshotSpec SnapshotStatus
                  BuildSpec BuildStatus JobSpec JobStatus
                  RunnerSpec RunnerTaint RunnerStatus RunnerAddress RunnerInfo
                  BuildTarget BuildTargetFlags PackageRepo SpecCommit PackageStatus
```

---

## 一、Project（项目）

**API**: `/apis/ebs/v1/projects`  
**etcd**: `/registry/ebs/projects/{name}`

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
    BuildEnvMacros   string                     `json:"buildEnvMacros,omitempty"`
    BuildTargets     []BuildTarget              `json:"buildTargets,omitempty"`
    PackageRepos     []PackageRepo              `json:"packageRepos,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `displayName` | string | 否 | 页面展示名称，默认使用创建时的 Project 名称 |
| `description` | string | 否 | 项目描述 |
| `specBranch` | string | 否 | 默认 spec 分支，默认 `"master"` |
| `buildEnvMacros` | string | 否 | 构建环境宏，YAML 格式 |
| `buildTargets` | []BuildTarget | 是 | 构建目标列表 |
| `packageRepos` | []PackageRepo | 否 | 包仓库列表 |

### ProjectStatus

```go
type ProjectStatus struct {
    Phase         string             `json:"phase,omitempty"`
    SnapshotCount int32              `json:"snapshotCount,omitempty"`
    BuildCount    int32              `json:"buildCount,omitempty"`
    LastBuildTime metav1.Time        `json:"lastBuildTime,omitempty"`
    Conditions    []metav1.Condition `json:"conditions,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | string | `"Pending"` / `"Active"` / `"Terminating"` |
| `snapshotCount` | int32 | 快照总数 |
| `buildCount` | int32 | 构建总数 |
| `lastBuildTime` | metav1.Time | 最后构建时间 |
| `conditions` | []metav1.Condition | 状态条件列表 |

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
**etcd**: `/registry/ebs/snapshots/{project}/{name}`

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
    IsTrunk        bool                 `json:"isTrunk,omitempty"`
    PrevSnapshot   string               `json:"prevSnapshot,omitempty"`
    SpecCommits    map[string]SpecCommit `json:"specCommits,omitempty"`
    BuildTargets   []BuildTarget        `json:"buildTargets,omitempty"`
    GroundProjects map[string]string    `json:"groundProjects,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `isTrunk` | bool | 否 | 是否主干快照，默认 false |
| `prevSnapshot` | string | 否 | 同一 Project 下的前一快照名称（增量构建用） |
| `specCommits` | map[string]SpecCommit | 是 | 各包 spec 提交信息 |
| `buildTargets` | []BuildTarget | 是 | 构建目标 |
| `groundProjects` | map[string]string | 否 | 基础项目快照映射，key=`项目_架构` |

### SnapshotStatus

```go
type SnapshotStatus struct {
    Phase     string      `json:"phase,omitempty"`
    BuildId   string      `json:"buildId,omitempty"`
    StartTime metav1.Time `json:"startTime,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | string | `"Created"` / `"Building"` / `"Completed"` / `"Failed"` |
| `buildId` | string | 关联构建 ID |
| `startTime` | metav1.Time | 开始时间 |

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
**etcd**: `/registry/ebs/builds/{project}/{name}`

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
    SnapshotName string      `json:"snapshotName,omitempty"`
    BuildType    string      `json:"buildType,omitempty"`
    BuildTarget  BuildTarget `json:"buildTarget,omitempty"`
    Packages     []string    `json:"packages,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `snapshotName` | string | 是 | 使用同一 Project 下的快照 |
| `buildType` | string | 是 | `"full"`/`"incremental"`/`"specified"`/`"single"` |
| `buildTarget` | BuildTarget | 是 | 构建目标 |
| `packages` | []string | 否 | 指定构建的包列表，空 = 全量 |

### BuildStatus

```go
type BuildStatus struct {
    Phase         string                   `json:"phase,omitempty"`
    StartTime     metav1.Time              `json:"startTime,omitempty"`
    EndTime       metav1.Time              `json:"endTime,omitempty"`
    RepoId        string                   `json:"repoId,omitempty"`
    PackageStatus map[string]PackageStatus `json:"packageStatus,omitempty"`
    Conditions    []metav1.Condition       `json:"conditions,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | string | `"Pending"` / `"Building"` / `"Completed"` / `"Failed"` / `"Aborted"` |
| `startTime` | metav1.Time | 开始时间 |
| `endTime` | metav1.Time | 结束时间 |
| `repoId` | string | 生成的仓库 ID |
| `packageStatus` | map[string]PackageStatus | 各包构建状态 |
| `conditions` | []metav1.Condition | 状态条件 |

### PackageStatus

```go
type PackageStatus struct {
    Phase     string      `json:"phase,omitempty"`
    JobId     string      `json:"jobId,omitempty"`
    StartTime metav1.Time `json:"startTime,omitempty"`
    EndTime   metav1.Time `json:"endTime,omitempty"`
    Attempts  int32       `json:"attempts,omitempty"`
    Message   string      `json:"message,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | string | `"Pending"`/`"Building"`/`"Completed"`/`"Failed"`/`"Aborted"` |
| `jobId` | string | 关联 Job ID |
| `startTime` | metav1.Time | 开始时间 |
| `endTime` | metav1.Time | 结束时间 |
| `attempts` | int32 | 重试次数 |
| `message` | string | 状态消息 |

### BuildList

```go
type BuildList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []Build `json:"items"`
}
```

---

## 四、Job（任务）

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
    Runner       string               `json:"runner,omitempty"`
    Arch         string               `json:"arch,omitempty"`
    Runtime      int64                `json:"runtime,omitempty"`
    DockerImage  string               `json:"dockerImage,omitempty"`
    RepoUrl      string               `json:"repoUrl,omitempty"`
    Package      string               `json:"package,omitempty"`
    ImageConfig  runtime.RawExtension `json:"imageConfig,omitempty"`
    Env          map[string]string    `json:"env,omitempty"`
    Commands     []string             `json:"commands,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `runner` | string | 否 | 期望的执行机规格，如 `"dc-64g"` |
| `arch` | string | 是 | `"aarch64"` / `"x86_64"` |
| `runtime` | int64 | 否 | 最大运行秒数，默认 10800 |
| `dockerImage` | string | 否 | Docker 环境镜像 |
| `repoUrl` | string | 否 | 依赖仓库 URL |
| `package` | string | 否 | 包名 |
| `imageConfig` | runtime.RawExtension | 否 | 预留镜像配置 |
| `env` | map[string]string | 否 | 环境变量 |
| `commands` | []string | 否 | 执行命令列表 |

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
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | string | `"Pending"` / `"Running"` / `"Completed"` / `"Failed"` / `"Aborted"` |
| `stage` | string | `"Pending"` / `"Running"` / `"PostRun"` |
| `runner` | string | 实际执行的 runner 名称 |
| `startTime` | metav1.Time | 开始时间 |
| `endTime` | metav1.Time | 结束时间 |
| `resultRoot` | string | 结果存储路径 |
| `message` | string | 状态消息 |

### JobList

```go
type JobList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []Job `json:"items"`
}
```

---

## 五、Runner（执行机）

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
| `type` | string | 是 | 执行机类型：`dc`/`vm`/`hw` |
| `arch` | string | 是 | CPU 架构：`aarch64`/`x86_64` |
| `hostname` | string | 否 | 宿主机名称 |
| `unschedulable` | bool | 否 | 是否禁止调度新 Job |
| `taints` | []RunnerTaint | 否 | 反亲和污点 |

> 调度标签统一使用 `metadata.labels`，不在 `spec` 中重复定义。

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
    RunningJobs []string           `json:"runningJobs,omitempty"`
    Addresses   []RunnerAddress    `json:"addresses,omitempty"`
    Info        RunnerInfo         `json:"info,omitempty"`
    Heartbeat   metav1.Time        `json:"heartbeat,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | string | 执行机状态：`Registering`/`Booting`/`Running`/`Idle`/`Offline` |
| `conditions` | []Condition | 详细状态条件 |
| `capacity` | map[string]string | 总资源容量，如 `cpu`/`memory`/`disk`/`jobs` |
| `allocatable` | map[string]string | 可调度资源容量，如 `cpu`/`memory`/`disk`/`jobs` |
| `runningJobs` | []string | 当前运行中的 Job 名称列表 |
| `addresses` | []RunnerAddress | 执行机地址列表 |
| `info` | RunnerInfo | 执行机系统与 agent 信息 |
| `heartbeat` | Time | 最后心跳时间 |

### RunnerAddress

```go
type RunnerAddress struct {
    Type    string `json:"type,omitempty"`
    Address string `json:"address,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `type` | string | 地址类型：`Hostname`/`InternalIP`/`ExternalIP` |
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
| `runtimeVersion` | string | 执行运行时版本 |
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

## 六、公共子结构体

### BuildTarget

```go
type BuildTarget struct {
    OsVariant      string           `json:"osVariant,omitempty"`
    Architecture   string           `json:"architecture,omitempty"`
    GroundProjects []string         `json:"groundProjects,omitempty"`
    Flags          BuildTargetFlags `json:"flags,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `osVariant` | string | OS 变体，如 `"openEuler-22.03-LTS"` |
| `architecture` | string | `"aarch64"` / `"x86_64"` |
| `groundProjects` | []string | 依赖的基础项目列表 |
| `flags` | BuildTargetFlags | 构建标志 |

---

### BuildTargetFlags

```go
type BuildTargetFlags struct {
    Build   bool `json:"build,omitempty"`
    Publish bool `json:"publish,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `build` | bool | 是否构建，默认 true |
| `publish` | bool | 是否发布，默认 false |

---

### PackageRepo

```go
type PackageRepo struct {
    SpecName   string `json:"specName,omitempty"`
    SpecUrl    string `json:"specUrl,omitempty"`
    SpecBranch string `json:"specBranch,omitempty"`
    GitTag     string `json:"gitTag,omitempty"`
    CommitId   string `json:"commitId,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `specName` | string | spec 包名称 |
| `specUrl` | string | spec 仓库 Git URL |
| `specBranch` | string | spec 分支（与 gitTag 二选一） |
| `gitTag` | string | Git 标签（与 specBranch 二选一） |
| `commitId` | string | 指定提交 ID |


### SpecCommit

```go
type SpecCommit struct {
    SpecUrl    string `json:"specUrl,omitempty"`
    SpecBranch string `json:"specBranch,omitempty"`
    CommitId   string `json:"commitId,omitempty"`
    CommitTime string `json:"commitTime,omitempty"`
    GitRepo    string `json:"gitRepo,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `specUrl` | string | spec 仓库 URL |
| `specBranch` | string | spec 分支 |
| `commitId` | string | 提交 ID |
| `commitTime` | string | 提交时间 |
| `gitRepo` | string | Git 仓库名 |

---

## 附录 A：状态枚举汇总

| 资源 | Phase 可选值 |
|------|-------------|
| Project | `Active` |
| Snapshot | `Created` → `Building` → `Completed` / `Failed` |
| Build | `Pending` → `Building` → `Completed` / `Failed` / `Aborted` |
| Job | `Pending` → `Running` → `Completed` / `Failed` / `Aborted` |

## 附录 B：结构体引用关系图

```
ProjectSpec
├── BuildTarget ──▶ BuildTargetFlags
├── PackageRepo

SnapshotSpec
├── SpecCommit
├── BuildTarget
└── map[string]string

BuildSpec ──▶ BuildTarget
BuildStatus ──▶ PackageStatus

RunnerSpec ──▶ RunnerTaint
RunnerStatus
├── RunnerAddress
└── RunnerInfo
```
