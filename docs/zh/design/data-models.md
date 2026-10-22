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
| `kind` | string | `kind` | Project / Snapshot / Build / BuildInfo / BuildGraph / RpmRepo / Config / Script / Job / Runner |
| `name` | string | `name` | 资源名称。Project/Runner/Config/Script 为集群内唯一；Snapshot/Build/BuildInfo/BuildGraph/RpmRepo/Job 在所属 Project 内唯一。Project 名需满足 DNS1123 label 约束，只能使用小写字母、数字和 `-`；`default` 是系统保留名称，不能用于 Project |
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

Project 下的子资源使用嵌套路由，路径中的 `{project}` 是 Snapshot、Build、BuildInfo、BuildGraph、RpmRepo、Job 的唯一项目归属来源。Config 和 Script 为集群级资源。

调度器和控制器可使用全局系统 API 跨 Project list 大部分对象。在 Project 级资源中，只有 Job 的全局 API 支持 watch。集群级资源 Runner 的 API 同样支持 list/watch。用户侧和项目侧调用使用 Project API。

当前 apiserver 基于 `GenericAPIServer` 实现，Project API 会在服务端重写到 scoped storage 路径，因此 Project 名必须满足 DNS1123 label 约束。需要展示带点号、空格或大小写的项目名时，使用 `Project.spec.displayName`。

| 子资源 | Project API | 全局 API | 主存储 | 对象定位 |
|--------|-------------|----------|--------|----------|
| Snapshot | `/apis/ebs/v1/projects/{project}/snapshots` | `/apis/ebs/v1/snapshots` | Elasticsearch | `ebs-snapshots` / `{project}/{name}` |
| Build | `/apis/ebs/v1/projects/{project}/builds` | `/apis/ebs/v1/builds` | Elasticsearch | `ebs-builds` / `{project}/{name}` |
| Job | `/apis/ebs/v1/projects/{project}/jobs` | `/apis/ebs/v1/jobs` | etcd | `/registry/ebs/jobs/{project}/{name}` |
| BuildInfo | `/apis/ebs/v1/projects/{project}/buildinfos` | `/apis/ebs/v1/buildinfos` | Elasticsearch | `ebs-buildinfos` / `{project}/{name}` |
| BuildGraph | `/apis/ebs/v1/projects/{project}/buildgraphs` | `/apis/ebs/v1/buildgraphs` | Elasticsearch | `ebs-buildgraphs` / `{project}/{name}` |
| RpmRepo | `/apis/ebs/v1/projects/{project}/rpmrepos` | `/apis/ebs/v1/rpmrepos` | Elasticsearch | `ebs-rpmrepos` / `{project}/{name}` |
| Config | 不提供 | `/apis/ebs/v1/configs` | Elasticsearch | `ebs-configs` / `{name}` |

表中 Elasticsearch 对象定位格式为“索引 / 文档 ID”。Project scoped 对象统一使用 `{project}/{name}` 作为文档 ID；Job 使用相同层级的 etcd key。只有 Job 和 Runner 存入 etcd 并提供 list/watch。

---

## 结构体总览

```
主资源: Project Snapshot Build BuildInfo BuildGraph RpmRepo Config Script Job Runner
列表类型: ProjectList SnapshotList BuildList BuildInfoList BuildGraphList RpmRepoList ConfigList ScriptList JobList RunnerList
辅助结构体: ProjectSpec ProjectStatus SnapshotSpec SnapshotStatus
                  BuildSpec BuildStatus BootstrapRepo JobSpec JobStatus
                  BuildInfoSpec BuildInfoStatus SpecStatusGroup SpecBuildStatus SpecInstallStatus MissingDep
                  BuildGraphStatus DcgNodeState
                  RpmRepoSpec RpmRepoStatus
                  ConfigSpec PackageResourceConfig
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

`metadata.annotations["ebs.io/artifacts"]` 指定 Project 使用的 Artifact Manager 实例，值须是 `Config/service-config` 的 `artifactManagers` 中已有的键；未设置时按 `default` 解释。创建 Project 或修改该 annotation 时，apiserver 实时读取 Config 确认实例存在；其他 Project 更新不重新校验。`/status` 更新不修改该 annotation。RpmRepo Controller 按该字段选择物化与发布请求的实例。

### ProjectSpec

```go
type ProjectSpec struct {
    DisplayName      string                     `json:"displayName,omitempty"`
    Description      string                     `json:"description,omitempty"`
    DefaultRef       string                     `json:"defaultRef,omitempty"`
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
| `defaultRef` | string | 否 | 默认完整 Git 引用，例如 `refs/heads/master`、`refs/tags/v1`；为空时默认 `refs/heads/master` |
| `buildPayload` | string | 否 | 构建环境参数，YAML 格式；`disable_check_path`、`use_kmod_libs`、`use_git_lfs`、`use_root`、`use_xz` 和 `unuse_gcc_secure` 是包仓库名列表，命中时向 Job payload 写入相应布尔标记；`unparsable_spec` 是 spec 名列表，仅供 BuildInfo Controller 组装依赖时使用 |
| `buildTargets` | []BuildTarget | 是 | 非空构建目标列表；创建和更新时 `os + arch` 组合必须唯一，其余配置不同也不能重复 |
| `packageRepos` | []PackageRepo | 否 | 包仓库列表；创建和更新时各条目 name 必须非空且在本 Project 内唯一，URL 或 ref 不同也不能使用同名条目 |
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
    DefaultRef   string               `json:"defaultRef,omitempty"`
    PackageRepos []PackageRepo        `json:"packageRepos,omitempty"`
}
```

| 字段 | Go 类型 | 必填 | 说明 |
|------|---------|------|------|
| `defaultRef` | string | 否 | Build Controller 创建时复制 `Project.spec.defaultRef`，与 packageRepos 来自同一次 Project GET；非空时必须为完整 `refs/...`，Snapshot 侧不填充默认值 |
| `packageRepos` | []PackageRepo | 否 | 创建 Snapshot 时从同一 Project 复制：single 仅保留 Build.spec.packages 指定的仓库，其他类型复制全部；仓库 ref 允许为空，不自动补齐 |

已存在的 Snapshot 不因 Project 变化而更新 defaultRef 或 packageRepos。Snapshot Controller 按 [PackageRepo](#packagerepo) 的提交选择规则解析，不回查 Project、不回写 spec。

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
| `conditions` | []metav1.Condition | 记录 Snapshot 整体异常；包级错误记录于 `packageRepoStatuses` |

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

`metadata.name` 必须为标准小写、带连字符的 UUID（`8-4-4-4-12`，不限定 v4），由 apiserver 在创建和普通更新时校验；调用方保证名称不复用。

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
    Packages           []string               `json:"packages,omitempty"`
    BuildTarget        BuildTarget            `json:"buildTarget,omitempty"`
}
```

| 字段             | Go 类型 | 必填 | 说明 |
|----------------|---------|------|------|
| `buildType`    | string | 是 | 构建类型：`"full"` / `"incremental"` / `"specified"` / `"single"` |
| `buildTarget`  | BuildTarget | 是 | 构建目标 |
| `packages`     | []string | 条件必填 | `single`、`specified` 为用户指定的目标仓库名；`full` 为空；`incremental` 创建时为空，由 Build Controller 在当前 Snapshot Active 后写入变更仓库与上次发布成功轮次中失败 spec 所属仓库的去重集合。该集合是依赖扩散的种子，不是最终 spec 构建集 |

Build 创建后，除 `incremental` 类型在 Pending 阶段由 Build Controller 幂等更新 `spec.packages` 外，其余 `spec` 字段不可修改。`packages` 允许为空数组表示没有构建种子；仅当 Build 进入 Prepared 后，才表示本轮种子已确认。普通用户不能修改 Controller 计算结果。运行状态通过 `/status` 子资源更新。

### 单包构建的 Git ref 覆盖

本节定义单包构建的引用覆盖与固化边界。Build Controller 将引用覆盖值固化到 Snapshot 的包级 ref，由 Snapshot Controller 解析并固化提交。调谐见 [Build Controller 4.2](controller-manager~build-controller.md)，脚本执行见 [Script 源码获取契约](data-models~script.md)。

`Build.metadata.annotations["ebs.io/git-ref"]` 可覆盖本次构建的完整 Git ref：

```yaml
metadata:
  annotations:
    ebs.io/git-ref: refs/merge-requests/501/head
spec:
  buildType: single
  packages:
    - gcc
```

- 仅在 `buildType=single` 且 `len(spec.packages)==1` 时允许设置；这里是数组长度，不是去重后的数量。未设置 annotation 时普通 single 仍允许指定多个包仓库。
- Build 创建时包名必须存在于所属 Project。Build Controller 创建 Snapshot 时，从同一次 Project GET 复制目标仓库和 defaultRef，再将 Build 的 `ebs.io/git-ref` 写入唯一目标仓库的 `packageRepos[].ref`，并清空该包的 commitID；Build annotation 保留，Snapshot 无需复制该 annotation。不修改 Project、不指定 fork URL。目标仓库缺失时报告输入错误，不回退默认分支。
- annotation 必须非空、以 `refs/` 开头并符合完整 Git ref 名称规则；不接受 `HEAD`、`FETCH_HEAD`、refspec、通配符或 `^{commit}` 等 revision 表达式，不自动 trim 或补前缀。创建后该键的存在性和值不可变，`/status` 保留原 metadata。
- Snapshot 创建后不重新复制 Project 或 Build 输入；Snapshot Controller 只消费 Snapshot 中固化的 packageRepos 和 defaultRef，成功写入的 commitId/cloneUrl 后续不重新解析。所有批次 Job 均使用该 Snapshot 的固定提交。
- Job 不传递动态 ref，统一携带 origin_url、clone_url 和 commit_id。
- `refs/merge-requests/501/head` 表示 PR 源码，不自动合并目标分支、不改写为 merge ref。引用名称由托管平台决定。

**源码一致性边界**：Snapshot 固定的是成功解析并持久化的提交，不是 Build 创建瞬间的 PR 提交；固化前远端 ref 仍可能变化。固化后，管理面解析和所有 Job 均使用该 commit，引用后续更新不影响本次构建。引用不存在时记录明确的包级解析失败，不回退其他引用。

授权仍沿用 Project 内创建 Build 的权限。`rpmspec` 可执行 shell/Lua 宏，BI-02 隔离完成前仅接收受信任、经审核的源码，不自动接入任意外部 PR。

输入契约验收：普通 single 多包、PR 单元素覆盖、annotation 准入及不可变、Snapshot 输入固化、完整引用不存在时不回退、成功解析后引用变化与重启均沿用固定 commit。

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
| `repo` | string | 外部软件源的基础地址，不包含架构目录；BuildInfo Controller 使用时追加 `/{buildTarget.arch}` |

### BuildStatus

```go
type BuildStatus struct {
    Phase        BuildPhase         `json:"phase,omitempty"`
    Stage        string             `json:"stage,omitempty"`
    StartTime    metav1.Time        `json:"startTime,omitempty"`
    EndTime      metav1.Time        `json:"endTime,omitempty"`
    BaseBuildRef *BaseBuildRef      `json:"baseBuildRef,omitempty"`
    Conditions   []metav1.Condition `json:"conditions,omitempty"`
}

type BaseBuildRef struct {
    Name string `json:"name,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | `BuildPhase` | 公共 `ebs/v1` API 定义的稳定取值：`"Pending"` / `"Prepared"` / `"Processing"` / `"Success"` / `"Failed"` / `"Aborted"` / `"Skipped"`；后四项为终态 |
| `stage` | string | `"build"` / `"publish"`，标识构建阶段还是发布阶段 |
| `startTime` | metav1.Time | 开始时间 |
| `endTime` | metav1.Time | 结束时间 |
| `baseBuildRef` | BaseBuildRef | 仅非 single 使用的基础 Build 名称，single 保持为空；未设置表示尚未查询，`{}` 表示已查询但没有基础 Build；过程仓 UID 和地址从同名 RpmRepo 获取 |
| `conditions` | []metav1.Condition | 状态条件；BuildInfo 因 buildPayload 配置错误进入 `Failed` 时，`BuildSucceed=False/reason=BuildPayloadParseFailed`，错误详情见同名 BuildInfo 的 condition message |

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
    BuildPayload string                `json:"buildPayload,omitempty"`
    BootstrapRepo []BootstrapRepo      `json:"bootstrapRepo,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `buildPayload` | string | Build Controller 创建时从所属 Project.spec.buildPayload 原样复制的构建环境宏（YAML）；未配置时为空，已有 BuildInfo 不覆盖，不跟随 Project 后续变更 |
| `bootstrapRepo` | []BootstrapRepo | Build Controller 创建时从所属 Project.spec.bootstrapRepo 深拷贝，已有 BuildInfo 不覆盖；供构建任务使用的引导 RPM 仓库 |

---

### BuildInfoStatus

```go
type BuildInfoStatus struct {
    Phase             BuildInfoPhase              `json:"phase,omitempty"`
    Conditions        []metav1.Condition          `json:"conditions,omitempty"`
    SpecStatus        SpecStatusGroup             `json:"specStatus"`
    SpecRepoNames     map[string]string           `json:"specRepoNames,omitempty"`
    FailedPackages    []string                    `json:"failedPackages,omitempty"`
    PendingJobCreates map[string]PendingJobCreate `json:"pendingJobCreates,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | string | `"Pending"` / `"Processing"` / `"Completed"` / `"Failed"` / `"Aborted"`；`Failed` 为 buildPayload 配置无效的终态，不代表已有 Job 均已终态 |
| `conditions` | []metav1.Condition | 状态条件；`BuildPayloadInvalid=True/reason=BuildPayloadParseFailed` 表示 buildPayload 配置无效，错误详情见 message；`JobAbortFailed=True` 表示已有 Job 的中止尝试失败 |
| `specStatus` | SpecStatusGroup | 按字段分组的各 spec 运行时状态；`build` 键集为构建集 |
| `specRepoNames` | map[string]string | spec 名到所属包仓库名的持久化映射；与构建集状态一起写入，供异常收敛及重启恢复时汇总 `failedPackages` |
| `failedPackages` | []string | Pending 组装时持久化确定性的 Snapshot 包解析、spec 下载/解析失败仓库；写入 Completed 时再合并最终构建/安装未成功的 spec 所属仓库，去重排序后供下一轮 Build Controller 重试；Failed 时保留当前值，不涵盖仍运行的 Job；已恢复成功的中途构建/安装失败不保留 |
| `pendingJobCreates` | map[string]PendingJobCreate | 已登记但尚未确认创建结果的 Job；非空时不得写入 Completed，Failed 时可保留条目供诊断 |

BuildInfo `/status` 校验：`Failed` 为不可回退终态，且必须包含 `BuildPayloadInvalid=True/reason=BuildPayloadParseFailed`；不要求 `pendingJobCreates` 为空。包级构建失败不映射为 BuildInfo Failed。具体调谐流程见 [BuildInfo Controller 设计](controller-manager~buildinfo-controller.md)。

### SpecStatusGroup

```go
type SpecStatusGroup struct {
    Build         map[string]SpecBuildStatus   `json:"build,omitempty"`
    Install       map[string]SpecInstallStatus `json:"install,omitempty"`
    DispatchCount map[string]int64             `json:"dispatchCount,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `build` | map[string]SpecBuildStatus | spec 名到构建状态；预建空状态也占一个键 |
| `install` | map[string]SpecInstallStatus | spec 名到安装状态；未评估时可无对应键 |
| `dispatchCount` | map[string]int64（缺失视为 0） | spec 名到下发计数；创建 Job 成功后 `+= 1`，回填时以同 spec 现存 Job 数兜底；达到有效 required 后不再下发 |

例如 `status.specStatus.build.gcc.status` 为构建结果，`status.specStatus.install.gcc.status` 为安装结果；只需构建详情时，GET/LIST 可用 `includeFields=status.phase,status.failedPackages,status.conditions,status.specStatus.build` 选择返回字段。
读取旧的按 spec 名组织的 `specStatus` 时，API 类型会转换为分组结构；后续写入只使用新结构。

---

### SpecBuildStatus

```go
type SpecBuildStatus struct {
    Status     string             `json:"status"`
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

| 字段           | Go 类型 | 说明 |
|--------------|------|------|
| `status`     | string | `"Running"` / `"Succeeded"` / `"Failed"` / `"ArchUnsupported"` / `"Aborted"`（历史残留值）|
| `conditions` | []metav1.Condition | 构建状态条件 |

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

## BuildGraph（构建依赖图）

**API**: `/apis/ebs/v1/projects/{project}/buildgraphs`

**全局 API**: `/apis/ebs/v1/buildgraphs`

**Elasticsearch**: 别名 `ebs-buildgraphs`，初始物理索引 `ebs-buildgraphs-v1`，文档 ID `{project}/{name}`

```go
type BuildGraph struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Status BuildGraphStatus `json:"status"`
}

type BuildGraphStatus struct {
    Nodes map[string]DcgNodeState `json:"nodes"`
}

type BuildGraphList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []BuildGraph `json:"items"`
}
```

- BuildGraph 是 BuildInfo Controller 生成的依赖图检查点，不是用户输入，因此不设 spec，也不维护独立 phase/stage。
- 与所属 BuildInfo 同 Project、同名；`metadata.ownerReferences` 指向该 BuildInfo 的 `ebs/v1`、kind、name、UID，标记 `controller=true`。所属关系创建后不可修改，不单独在 BuildInfo 中保存引用字段。
- 非 single 构建首次建图后一次 POST 创建完整对象；后续只有运行期 install 补边和追加破环点通过 `PUT /status` 更新。初始构建集为空时允许 `nodes: {}`，以对象存在性而非节点数量判断是否已持久化。single 不创建 BuildGraph。
- BuildInfo 的常规结果回填不携带图；BuildGraph 使用独立 UID 和 resourceVersion。构建依赖存在性校验仍消费本轮解析的 `buildRequires`，图负责派发顺序和恢复。
- BuildInfo 终态后保留图供诊断；历史构建或 Project 清理时，将 BuildGraph 纳入显式清理清单，核对 owner UID 并使用对象 UID/resourceVersion 删除前置条件。ownerReferences 只记录归属，不假定已有通用垃圾回收器；仅删除 BuildInfo 不自动删除图，不清理仍在构建的图。
- 获取、缓存版本核对、写入确认及丢失处理以 [BuildInfo Controller 15.9](controller-manager~buildinfo-controller.md#159-buildgraph-持久化与-dcg-缓存g-02) 为准。

### DcgNodeState

```go
type DcgNodeState struct {
    Version        string                  `json:"version,omitempty"`
    OutDep         []string                `json:"outDep,omitempty"`
    InDep          map[string]VersionConst `json:"inDep,omitempty"`
    InstallInDep   map[string]VersionConst `json:"installInDep,omitempty"`
    BootstrapBreak bool                    `json:"bootstrapBreak,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `version` | string | spec 完整版本号，初始建图后冻结 |
| `outDep` | []string | 依赖本 spec 的下游 spec 列表，与 build/install 入边的次数保持一致；同一下游同时具有两类入边时记录两次 |
| `inDep` | map[string]VersionConst | 本 spec 依赖的上游 spec → 版本约束，初始建图后冻结 |
| `installInDep` | map[string]VersionConst | 安装期依赖的上游 spec → 版本约束；运行期只追加缺失边 |
| `bootstrapBreak` | bool | 破环点标记；已有标记保留，新环可追加，重启不重选 |

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

`RepositoryTransition` 是提交物化前先写入的恢复检查点，也可保留已放弃的失败批次。存在 transition 时不得选择新输入或重新计算基础仓；失败收口保留原 inputs、baseRepositoryUID 与 repositoryUID。
`ReleaseTransition` 在提交正式发布前固化源过程仓和排除集合；存在该字段时必须恢复原请求，不得根据最新 Project 或 BuildInfo 重新计算。

| 类型与字段 | 说明 |
|------------|------|
| `RepositoryInput.jobName` | 输入 Job 名称，用于 API 定位和诊断 |
| `RepositoryInput.specName` | Job 产出的 spec，用于批次去重和错误归属 |
| `RepositoryTransition.inputs` | 已冻结并按稳定顺序保存的本批输入 |
| `RepositoryTransition.baseRepositoryUID` | 本次物化继承的不可变基础仓；首次构建可为空 |
| `RepositoryTransition.repositoryUID` | 根据固定输入计算出的目标物理版本 UID |
| `ReleaseTransition.sourceRepositoryUID` | 正式发布使用的 Ready 过程仓 UID |
| `ReleaseTransition.excludeSpecs` | 已规范化、去重并按字典序保存的排除集合 |

### RpmRepoStatus

```go
type RpmRepoRepositoryStatus struct {
    RepositoryUID     string                `json:"repositoryUID,omitempty"`
    ContentURL        string                `json:"contentURL,omitempty"`
    SourceJobNames    []string              `json:"sourceJobNames,omitempty"`
    SkippedJobNames   []string              `json:"skippedJobNames,omitempty"`
    Transition        *RepositoryTransition `json:"transition,omitempty"`
    UpdatedAt         *metav1.Time          `json:"updatedAt,omitempty"`
}

type RpmRepoReleaseStatus struct {
    Phase               RpmRepoReleasePhase `json:"phase,omitempty"`
    SourceRepositoryUID string              `json:"sourceRepositoryUID,omitempty"`
    ContentURL          string              `json:"contentURL,omitempty"`
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
| `release` | *RpmRepoReleaseStatus | 正式发布的独立状态；过程仓不定义 phase |
| `conditions` | []metav1.Condition | 状态条件（记录失败原因等） |

| `repository` 字段 | Go 类型 | 说明 |
|--------------------|---------|------|
| `repositoryUID` | string | 当前已发布不可变物理版本的 UID |
| `contentURL` | string | 当前物理版本的不可变仓库地址 |
| `sourceJobNames` | []string | 本对象已成功消费的累计 Job 名称集合，去重并按字典序保存；继承基线不计入，非空表示本对象已产出版本 |
| `skippedJobNames` | []string | Artifact Manager 确认为异常输入并已跳过的 Job 名称；去重排序，重启后不重新入选，不算作已物化产物 |
| `transition` | *RepositoryTransition | 在途版本的固定输入，或失败收口时原样保留的已放弃批次 |
| `updatedAt` | *metav1.Time | 过程仓 status 最近一次有效写入时间（提交批次、失败收口或提升版本） |

`repositoryUID` 与 `contentURL` 成对记录最近一次确认可用的不可变过程仓版本；没有可用版本时为空。生成下一版本期间保留这两个字段，以 `transition` 记录未完成的生成意图（包括重试和结果确认）；成功后以一次 CAS 替换当前版本并清空 `transition`。可定位到单个异常 Job 的清单、产物或 RPM 输入失败会把其名称加入 `skippedJobNames`、清空检查点并重新组批；其它批次失败收口保留 transition、写 RepositoryReady=False 与 release.phase=Failed、PublishSucceed=False，不清除已有可用版本。

RpmRepo 的 `repository.contentURL` 和 `release.contentURL` 保存 `artifact:///repositories/.../` 逻辑地址，不绑定 Artifact Manager 部署地址。

仅非 single 构建创建同名 RpmRepo；single 直接使用稳定发布入口，不创建占位对象。publishFlag=false 的 RpmRepo 先完成过程仓处理，再写 release.phase=Skipped，保留已生成的过程仓。Skipped 不要求 release.contentURL，不允许 release.transition；RpmRepo Controller 将其视为发布终态，不执行正式发布、不因 Build 中止改写为 Failed。Skipped 不是 Artifact Manager 的物理仓库或发布记录状态，也不表示 Build 已完成。

`release.phase` 的稳定取值为 `Pending`、`Creating`、`Prepared`、`Ready`、`Failed`、`Skipped`。`release.transition` 只在正式发布尚未完成时存在；发布准备和激活成功后，将固定输入提升到 `sourceRepositoryUID`，写入 `contentURL`，再清除 transition。失败原因写入 RpmRepo 顶层 `conditions`，condition type 必须区分过程仓和正式发布错误。因同名 `Build.status.phase=Aborted` 或 `BuildInfo.status.phase=Failed` 而终止发布时，`release.phase=Failed`、`release.transition=nil`，分别记录 `PublishSucceed=False/reason=BuildAborted` 或 `BuildInfoFailed`；`release.contentURL` 为空，`repository.*` 保留。已终态对象不改写。发布终态为 `Ready`、`Failed`、`Skipped`。

RpmRepo 的 `/status` 校验：release 非空时 phase 必须为上述枚举；release.transition 仅允许处于 Pending / Creating / Prepared，Ready 必须有 contentURL。repository 的 UID 与 URL 必须成对，sourceJobNames 非空要求版本指针非空；skippedJobNames 不允许空值、重复值或与 sourceJobNames 重叠；repository.transition 非空要求 repositoryUID 和 inputs 非空。允许空 status、继承基线以及失败时保留的批次。conditions 按下表校验 Type + Status + Reason 的合法组合，不能任意交叉搭配；空 reason 和旧 reason 不接受。`/status` 保留原 spec 与受保护 metadata。

| Type | Status | Reason | 场景 |
| --- | --- | --- | --- |
| `RepositoryReady` | `True` | `RepositoryCreated` | 最近一次过程仓批次成功提升 |
| `RepositoryReady` | `False` | `RepositoryCreationFailed` | 过程仓不可重试失败或重试耗尽 |
| `PublishSucceed` | `True` | `ReleaseActivated` | 正式发布已成功激活 |
| `PublishSucceed` | `False` | `ReleaseFailed` | 正式发布准备或激活失败 |
| `PublishSucceed` | `False` | `RepositoryCreationFailed` | 过程仓失败导致无法发布 |
| `PublishSucceed` | `False` | `NoPublishableArtifacts` | 已完成构建没有可发布产物 |
| `PublishSucceed` | `False` | `BuildAborted` | 同名 Build 被中止 |
| `PublishSucceed` | `False` | `BuildInfoFailed` | 同名 BuildInfo 因配置校验失败而进入 Failed |


| `release` 字段 | Go 类型 | 说明 |
|----------------|---------|------|
| `phase` | RpmRepoReleasePhase | 正式发布状态（终态：`Ready` / `Failed` / `Skipped`） |
| `sourceRepositoryUID` | string | 已发布版本使用的过程仓 UID |
| `contentURL` | string | Project/OS/架构稳定仓库入口 |
| `transition` | *ReleaseTransition | 正在准备或激活的固定发布输入 |
| `updatedAt` | *metav1.Time | 正式发布状态最近更新时间 |

### RpmRepoList

```go
type RpmRepoList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []RpmRepo `json:"items"`
}
```

---

## 六、Config（集群级配置）

**API**：`/apis/ebs/v1/configs`；**Elasticsearch**：alias `ebs-configs`，文档 ID `{name}`。不提供 Project-scoped API、Watch 或 `/status`。资源生命周期、可见性及消费规则见 [构建配置设计](./data-models~config.md#11-config-公共资源与可见性)。

```go
type Config struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec              ConfigSpec `json:"spec"`
}

type ConfigSpec struct {
    Visibility ConfigVisibility `json:"visibility"`
    Content    string           `json:"content"`
}

type ConfigVisibility string

const (
    ConfigVisibilityPublic  ConfigVisibility = "Public"
    ConfigVisibilityOpsOnly ConfigVisibility = "OpsOnly"
)

type ConfigList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []Config `json:"items"`
}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `metadata.name` | 是 | 集群唯一；内置对象为 `build-target`、`build-resource` 和 `service-config` |
| `spec.visibility` | 是 | `Public` 允许所有身份及匿名具名读取；`OpsOnly` 仅 Ops/Admin 读取 |
| `spec.content` | 是 | 非空 UTF-8 YAML 文本；apiserver 按内置对象名称校验业务结构，消费方解析使用 |

`Config` 不设置 namespace、status 或业务类型字段。Gateway 仅允许 Ops/Admin list；匿名、普通用户和 Runner 只能具名读取 `Public` 对象，`OpsOnly` 仅 Ops/Admin 可读。Ops/Admin 可创建更新，三个内置对象不可经 Gateway 删除。apiserver 校验公共字段、资源版本及内置对象的业务内容；`service-config` 固定为 `OpsOnly`。

### 内置内容格式

`build-target` 的 `content` 解码为以下内部结构（不是公开 API 类型）：

```go
type BuildTargetContent struct {
    Targets map[string]BuildTargetContentTarget `yaml:"targets"`
}
type BuildTargetContentTarget struct {
    Arches map[string]BuildTargetContentArch `yaml:"arches"`
}
type BuildTargetContentArch struct {
    Image string `yaml:"image"`
}
```

`targets` 可为空；每个 OS 下至少有一个架构，每个架构必须有合法镜像引用。完整校验见 [构建目标内容](./data-models~config.md#22-对象)。

`build-resource` 的 `content` 解码为以下内部结构（不是公开 API 类型）：

```go
type BuildResourceContent struct {
    Default  ResourceRequirements             `yaml:"default"`
    Packages map[string]PackageResourceConfig `yaml:"packages"`
}
type PackageResourceConfig struct {
    Default ResourceRequirements            `yaml:"default"`
    Arches  map[string]ResourceRequirements `yaml:"arches"`
}
```

`default.requests` 必须同时包含 CPU 和 memory；`packages` 可为空。仅允许 CPU、memory 资源键；包名和架构名按 [构建资源内容](./data-models~config.md#3-构建资源内容) 的规则校验。按表级默认值、包级默认值、架构专属值逐字段合并，同一级 request 未给 limit 时取该级 request；最终 limits 均不得低于 requests。内容解析器需拒绝未知字段和重复 YAML key，不能静默丢失规则。

BuildInfo Controller 读取 `Config/build-target` 与 `Config/build-resource`，分别固化镜像及资源到 Job；Job 创建后配置变化不回写。Script 是独立资源，不复用 Config 内容。

`service-config` 的内容由公共 `ServiceConfigContent` 解析，包含 Artifact Manager 实例表 `artifactManagers` 和单个 Git Server 配置 `gitServer`；两类服务共用 `ServiceEndpoints`，分别记录控制面 `internalURL`、内网 Runner 的 `runnerInternalURL` 和外网 Runner 的 `runnerExternalURL`。字段、默认对象和消费边界统一见 [服务配置内容](data-models~config.md#4-服务配置内容)。

## Script（全局脚本）

Script 为集群级资源，通过 `/apis/ebs/v1/scripts` 管理，不包含 namespace 或 status。脚本正文可以原地更新，使用 resourceVersion 控制写入冲突。

```go
type Script struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec ScriptSpec `json:"spec"`
}

type ScriptSpec struct {
    Content string `json:"content"`
}

type ScriptList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []Script `json:"items"`
}
```

`content` 为非空 UTF-8 文本，不允许 NUL，不设独立正文大小上限，仍受请求体限制；首行通过 shebang 指定容器内解释器。名称为集群内唯一的 DNS subdomain，不支持 generateName。完整约定见 [Script 设计](data-models~script.md)。资源 API、BuildInfo Controller 的 Job `scriptRefs` 观测信息生成及 Runner 拉取执行均已接入。

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

BuildInfo Controller 创建的 Job 在 `metadata.labels` 中记录所属 Build、spec 与软件包仓库：`ebs.io/build-name`、`ebs.io/spec-name`、`ebs.io/package-name`。包名 label 的值遵循[标签约定](labels.md#7-job-构建归属标签)中的截断及编码规则，从创建时的 BuildInfo Controller 本轮解析结果 `specDepends[specName].repoName` 取得。

Job 的 `metadata.annotations["ebs.io/artifacts"]` 固化创建时 Project 的 Artifact Manager 实例名，未配置时为 `default`。Runner 用该值从 Gateway 获取的实例地址列表中选择日志和产物上传目标；创建后该 annotation 不可修改。

### JobSpec

```go
type ScriptRef struct {
    Name            string `json:"name"`
    UID             string `json:"uid"`
    ResourceVersion string `json:"resourceVersion"`
}

type JobSpec struct {
    Priority     int64                `json:"priority,omitempty"`
    Runtime      string               `json:"runtime,omitempty"`
    RuntimeSpec  runtime.RawExtension `json:"runtimeSpec,omitempty"`
    ScriptRefs   []ScriptRef          `json:"scriptRefs,omitempty"`
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
| `scriptRefs` | []ScriptRef | 否 | 为空时使用镜像入口；非空时按顺序拉取，第一项为主脚本，名称不得重复；记录创建 Job 时观察到的 Script name、UID、resourceVersion，不锁定执行时的脚本内容，见 [Script 设计](data-models~script.md) |
| `timeoutSeconds` | int64 | 否 | 最大运行秒数，默认 10800 |
| `resources` | ResourceRequirements | 否 | Job 资源请求与限制 |
| `nodeSelector` | map[string]string | 否 | Runner label 精确匹配条件，如通过 `ebs.io/runner-arch` 选择架构 |
| `tolerations` | []Toleration | 否 | 可容忍的 Runner 污点 |
| `payload` | string | 否 | CT 构建 Job 使用 JSON 格式记录业务输入；Runner 将其写入 `/workspace/payload.json`。其他运行时按各自入口约定解释该字段 |

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
    Phase      JobPhase    `json:"phase,omitempty"`
    Stage      JobStage    `json:"stage,omitempty"`
    Build      *JobBuildResult   `json:"build,omitempty"`
    Install    *JobInstallResult `json:"install,omitempty"`
    Runner     string      `json:"runner,omitempty"`
    StartTime  metav1.Time `json:"startTime,omitempty"`
    EndTime    metav1.Time `json:"endTime,omitempty"`
    Message    string      `json:"message,omitempty"`
    RestartCount int64     `json:"restartCount,omitempty"`
}
```

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `phase` | `JobPhase` | 公共 `ebs/v1` API 定义的稳定取值：`"Pending"` / `"Running"` / `"Succeeded"` / `"Failed"` / `"Aborted"`；后三项为终态 |
| `stage` | `JobStage` | 公共 `ebs/v1` API 定义的稳定取值：`"Pending"` / `"Running"` / `"PostRun"`。失败时保留最后到达的执行阶段，不使用 `Failed` stage |
| `build` | `*JobBuildResult` | 脚本构建结果，`status` 为 `Succeeded` 或 `Failed`；与产物上传后的 Job 终态分开记录 |
| `install` | `*JobInstallResult` | 构建后安装检查结果，`status` 为 `Succeeded` 或 `Failed`，失败时可携带 `missingDeps`；缺失表示未得到可靠结果 |
| `runner` | string | 实际执行的 runner 名称 |
| `startTime` | metav1.Time | 开始时间 |
| `endTime` | metav1.Time | 结束时间 |
| `message` | string | 执行或上传错误说明，不承载安装检查的结构化结果 |
| `restartCount` | int64 | 重试次数，默认 0。调度器可使用该字段计算重试退避时间 |

### JobList

```go
type JobList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []Job `json:"items"`
}
```

### 历史 Job 快照（oldjobs）

Job Controller 可选择将到期的终态 Job 归档至 Elasticsearch，再从 etcd 删除；默认仍直接删除。归档复用 `Job` / `JobList`，不新增 `OldJob` 类型。

| 项目 | 约定 |
| --- | --- |
| 工程内查询 | `GET /apis/ebs/v1/projects/{project}/oldjobs[/{name}]` |
| 内部跨工程查询 | `GET /apis/ebs/v1/oldjobs` |
| ES alias / 初始索引 | `ebs-oldjobs` / `ebs-oldjobs-v1` |
| 文档 ID | `{project}/{jobName}` |
| 快照内容 | 完整 metadata、spec、status，保留源 UID 和 resourceVersion |
| 归档时间 | ES 存储元数据 `archivedAt`，不新增 Job API 字段 |

历史入口只读，不参与调度和 Watch；普通 `jobs` 查询不自动合并历史对象。归档仅保存对象，不延长日志和产物的保留期。接口、mapping、版本冲突及保留规则统一见 [历史 Job 归档](ebs-apiserver.md#历史-job-归档ebs-oldjobs)。

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
| `phase` | `RunnerPhase` | 公共 `ebs/v1` API 定义的稳定取值：`Online` / `Offline` / `Evicted` |
| `conditions` | []Condition | 详细状态条件，当前 runner agent 暂不主动填充 |
| `capacity` | map[string]string | Runner 上报的总资源容量。当前包含 `cpu`、`memory`、`ephemeral-storage`：`cpu` 为逻辑 CPU 数，`memory` 使用 `Mi`，`ephemeral-storage` 使用 `Gi` |
| `allocatable` | map[string]string | Runner 上报的可调度资源容量。当前 `cpu`、`memory` 与 `capacity` 一致，`ephemeral-storage` 为 runner 工作目录所在文件系统的可用空间，使用 `Gi` |
| `addresses` | []RunnerAddress | 执行机地址列表 |
| `info` | RunnerInfo | 执行机系统与 agent 信息 |
| `heartbeat` | Time | 最后心跳时间 |

Runner 创建时 apiserver 默认置为 `Offline`。Runner agent 完成本地初始化并具备接收任务能力后，通过首次心跳置为 `Online`；主动下线或心跳超时后置为 `Offline`。Runner 的忙闲状态由绑定 Job 计算，不通过 Runner phase 表达。

### Runner 驱逐状态

`status.phase=Evicted` 表示暂停向该 Runner 分配新 Job。Scheduler 的 PhaseFilter 仅接受 Online；已绑定 Job 继续执行，不因驱逐自动中止或迁移。Runner Controller 不把 Evicted 改为 Offline。
Runner agent 上报心跳或主动下线前读取最新 Runner，保留 Evicted，并携带该对象的 resourceVersion 写入；发生冲突时不重放旧状态，下一次上报重新读取。解除驱逐需显式更新 phase 为 Online 或 Offline，后者由后续心跳恢复在线。此状态不代替 spec.unschedulable，恢复后仍需通过其他调度过滤。


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
    Ref           string          `json:"ref,omitempty"`
    CommitID      string          `json:"commitID,omitempty"`
}
```

| 字段             | Go 类型         | 说明                       |
|----------------|---------------|--------------------------|
| `name`         | string        | spec 包名称                 |
| `url`          | string        | spec 仓库 Git URL          |
| `ref`          | string        | 完整 Git 引用，例如 `refs/heads/main`、`refs/tags/v1`、`refs/merge-requests/501/head`；为空时使用 Snapshot 的 defaultRef |
| `commitID`     | string        | 可选固定提交，完整 40 位小写十六进制 SHA-1；与 ref 可同时配置，优先选择该提交 |

`Project.spec.defaultRef`、`Snapshot.spec.defaultRef` 和 `PackageRepo.ref` 统一使用完整 `refs/...` 字符串。非空值按 Git ref-name 规则校验，不接受裸分支名、Commit ID 或 `{type, value}` 对象。固定提交使用独立的 `commitID`。Project 的 defaultRef 为空时默认为 `refs/heads/master`；Snapshot 不填默认值，仓库 ref 允许省略或为空字符串，创建和更新均不自动补齐。修改 Project 默认引用不会改写显式包 ref。Build Controller 从同一次 Project GET 复制 defaultRef 与所选 packageRepos（含 commitID），single 仅复制指定包，其他类型复制全部。

Snapshot 提交选择优先级为 PackageRepo.commitID > PackageRepo.ref > Snapshot.defaultRef。Build 的 Git ref 覆盖已在创建 Snapshot 时固化为包级 ref，解析阶段只消费 Snapshot.spec。Git Server 同步满足 baseline 后，普通引用解析 `<ref>^{commit}`；固定提交验证 `<commitID>^{commit}` 的结果必须等于输入 SHA，成功后写入 status 的 commitId 和 cloneUrl。未配置任何提交或引用时记录 ValidationFailed；引用或提交不存在时记录 ResolveFailed，不回退其他来源。固定提交必须已存在于同步后的镜像中；不会通过任意 SHA 额外向上游抓取对象。

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
| `cloneUrl` | string | Git Server 客户端由 `clone_path` 生成的 `git-server:///git/...` 逻辑克隆地址；仅完成本轮同步确认后填写 |
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
| BuildInfo | `Pending` / `Processing` / `Completed` / `Failed` / `Aborted`                         |
| RpmRepo | 过程仓无 phase；正式发布：`Pending` / `Creating` / `Prepared` / `Ready` / `Failed` / `Skipped` |
| Job | `Pending` → `Running` → `Succeeded` / `Failed` / `Aborted`                            |
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
└── BuildTarget

BuildInfoSpec
└── BootstrapRepo

BuildInfoStatus
└── SpecStatusGroup
    ├── SpecBuildStatus
    └── SpecInstallStatus
        └── MissingDep ──▶ VersionConst

BuildGraphStatus
└── DcgNodeState ──▶ VersionConst

RpmRepoStatus
├── RpmRepoRepositoryStatus
│   └── RepositoryTransition ──▶ RepositoryInput
└── RpmRepoReleaseStatus ──▶ ReleaseTransition

BuildResourceContent
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
