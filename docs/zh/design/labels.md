# EulerMaker 标签约定

## 1. 目的与范围

本文档集中定义 EulerMaker 使用的标签（label）契约，包括标签适用的对象、值语义、写入方、修改权限以及使用场景。其他设计文档只描述具体业务流程；新增系统保留标签时，应同步更新本文档。

本文档中的 API 对象标签均指 `metadata.labels`。标签的键和值必须满足 Kubernetes label 语法。未列入本文档的标签属于用户自定义元数据，不得被组件解释为权限、调度或生命周期控制信号。

## 2. API 对象标签总表

| 对象 | 标签 | 值 | 写入方 | 用途 |
|------|------|----|--------|------|
| Project | `project.ebs.io/type` | `community` / `personal` | 创建方；apiserver 默认 `personal` | 工程分类，详见 3.3 |
| Project | `ebs.io/owner-user` | User `metadata.name` | Gateway、Admin | 标识 Project owner，参与 Gateway 写权限判定 |
| Project | `ebs.io/member-user.<username>` | 固定为 `"true"` | Project owner | 授予指定用户 Project 成员权限 |
| Build | `ebs.io/target-os` | Build Target 的操作系统名称 | Build 创建方 | 按构建目标查询 Build |
| Build | `ebs.io/target-arch` | Build Target 的架构名称 | Build 创建方 | 按构建目标查询 Build |
| Build | `ebs.io/build-type` | Build `spec.buildType` | Build 创建方 | 按构建类型查询 Build |
| RpmRepo | `ebs.io/target-os` | 同名 Build 的 `spec.buildTarget.os` | Build Controller | 按构建目标查询/归组 RpmRepo |
| RpmRepo | `ebs.io/target-arch` | 同名 Build 的 `spec.buildTarget.arch` | Build Controller | 按构建目标查询/归组 RpmRepo |
| Job | `ebs.io/build-name` | 所属 Build 的 `metadata.name` | BuildInfo Controller | 按 Build 查询仓库输入 Job |
| Job | `ebs.io/spec-name` | Job 构建的 SPEC 名的可逆编码 | BuildInfo Controller | BuildInfo 回填与 RpmRepo 批次按解码后的 SPEC 名分组 |
| Job | `ebs.io/package-name` | 所属 `PackageRepo.name` 的可查询 label 值，编码规则见第 7 节 | BuildInfo Controller | 按软件包仓库筛选 Job 构建历史 |
| Job | `ebs.io/target-os` | 所属 Build 的目标操作系统 | BuildInfo Controller | 仓库元数据与 Job/Build 一致性校验 |
| Job | `ebs.io/target-arch` | 所属 Build 的目标架构 | BuildInfo Controller | 仓库元数据与 Job/Build 一致性校验 |
| Runner | `ebs.io/runner-type` | 与 `spec.type` 相同 | Runner | 表达 Runner 类型 |
| Runner | `ebs.io/runner-arch` | 与 `spec.arch` 相同 | Runner | 表达 Runner 架构，供 `nodeSelector` 精确匹配 |
| Runner | `ebs.io/runner-capability.<name>` | 由具体能力定义 | Runner | 表达 Runner 自声明能力，供 `nodeSelector` 精确匹配 |
| Runner | `ebs.io/zone` | 部署环境定义的区域名称 | system | system 管理的调度区域 |

`ebs.io/` 前缀由 EulerMaker 保留。使用该前缀但未在本文档登记的标签，不具有稳定的公共语义。

## 3. Project 标签

### 3.1 所有者

```yaml
metadata:
  labels:
    ebs.io/owner-user: alice
```

普通用户创建 Project 时，Gateway 强制将 `ebs.io/owner-user` 设置为 JWT `sub`。客户端提交的值不可信。Admin 创建 Project 时必须显式指定一个已存在且启用的 User。

Project 创建后，Gateway 不允许任何身份修改、删除或清空 owner 标签；所有权转移不通过 Gateway 提供。

### 3.2 成员

```yaml
metadata:
  labels:
    ebs.io/member-user.bob: "true"
```

成员标签的用户名位于标签 key 中，值只能是字符串 `"true"`。仅 Project owner 可以增删成员；新增成员必须对应已存在且启用的 User。普通成员不能修改 owner 或成员标签。

Project 子资源不重复保存访问标签，而是通过所属 Project 继承权限。apiserver 只存储这些标签，不解释其授权语义；授权由 Gateway 完成。

### 3.3 工程分类

`project.ebs.io/type` 是系统保留的分类标签，只允许 `community`（社区工程）和 `personal`（个人工程），不增加对应 spec 字段。

- apiserver 创建及普通更新时补齐缺失标签为 `personal`；显式空值或其他值返回 `422 Invalid`。读取旧对象不写回，缺失标签按个人工程解释。
- 普通用户只能创建个人工程；不能修改或删除已有类型标签。旧对象可补写 `personal`。Ops 在自己拥有的 Project 中可修改类型；Ops、Admin 创建 Project 时可以指定类型，但 Admin 不能通过 Gateway 修改已有 Project。
- Gateway 负责上述权限；普通用户的 JSON/Merge PATCH 按最新对象计算完整候选对象，检查类型和访问标签后转为携带原 resourceVersion 的 PUT，避免通过替换 metadata/labels、move/copy 或并发修改绕过保护。
- Project `/status` 更新保留服务端原 labels，不能借状态更新修改分类。
- 分类不影响公开读取或 owner/member 权限；“个人工程”表示类型，不等于“我的工程”。

前端工程列表提供“社区工程 / 个人工程”Tab，默认社区工程。切换时重置分页，查询使用现有 `labelSelector`：

| Tab | 选择器 |
|-----|--------|
| 社区工程 | `project.ebs.io/type=community` |
| 个人工程 | `project.ebs.io/type!=community` |

不等于筛选包含无标签对象，保证旧工程在个人 Tab 中可见；过滤在服务端分页前完成。Tab 切换后忽略前一次未完成请求的结果。普通用户表单固定创建个人工程；Ops 及以上可以选择类型，YAML 导入保留显式标签并由后端校验。

## 4. Build 查询标签

Build 使用以下标签表达构建目标：

```yaml
metadata:
  labels:
    ebs.io/target-os: openEuler-24.03-LTS-SP4
    ebs.io/target-arch: x86_64
    ebs.io/build-type: full
```

`ebs.io/target-os`、`ebs.io/target-arch` 和 `ebs.io/build-type` 必须分别与 Build `spec.buildTarget.os`、`spec.buildTarget.arch` 和 `spec.buildType` 完全一致，不进行大小写折叠或别名转换。创建或更新 Build 时，apiserver 根据 spec 补齐缺失标签；客户端显式提供的标签与 spec 不一致时返回 `422 Invalid`，不能静默覆盖。

调用方可以组合标签选择器、状态字段选择器和默认创建时间倒序查询某个目标的最新 Build，例如：

```text
labelSelector=ebs.io/target-os=openEuler-24.03-LTS-SP4,ebs.io/target-arch=x86_64,ebs.io/build-type=full
fieldSelector=status.phase=Processing,status.stage=build
limit=1
```

`limit=1` 表示整个过滤结果中的最新一条记录，不表示对多个目标分别聚合。

## 5. Runner 调度标签

Runner 注册示例：

```yaml
metadata:
  labels:
    ebs.io/runner-type: ct
    ebs.io/runner-arch: x86_64
    ebs.io/runner-capability.rpmbuild: "true"
    ebs.io/zone: local
spec:
  type: ct
  arch: x86_64
```

约束如下：

- `ebs.io/runner-type` 和 `ebs.io/runner-arch` 创建时必须存在，并分别与 `spec.type`、`spec.arch` 相同；apiserver 负责权威校验。
- Runner 只能声明或更新 type、arch 和 `ebs.io/runner-capability.*` 标签。
- `ebs.io/zone` 以及其他管理标签只能由 system 维护；Runner PUT/PATCH 必须保留已有值。
- Scheduler 的 `NodeSelectorFilter` 对 Job `spec.nodeSelector` 和 Runner labels 执行逐项精确匹配。
- RuntimeFilter 使用 Job `spec.runtime` 和 Runner `spec.type`，不依赖 `ebs.io/runner-type`。

`ebs.io/runner-capability.<name>` 中 `<name>` 的含义和值域必须由引入该能力的设计另行登记。未登记的能力标签只参与通用 `nodeSelector` 匹配，不触发额外行为。

## 6. 构建容器标签

Runner 创建构建容器时至少写入：

| 标签 | 值 |
|------|----|
| `ebs.io/project` | Job `metadata.namespace` |
| `ebs.io/job` | Job `metadata.name` |
| `ebs.io/runner` | Runner `metadata.name` |

这些是容器运行时标签，不是 EBS API 对象的 `metadata.labels`。Runner 使用它们在重启后定位容器，并执行幂等恢复、停止和清理。它们不能作为 API 权限或调度依据。

## 7. Job 构建归属标签

BuildInfo Controller 创建 Job 时必须写入以下 labels：

| Label | 用途 |
|-------|------|
| `ebs.io/build-name` | 记录所属 Build name（由唯一 UUID 生成），供 RpmRepo Controller 建立仓库输入关系和队列隔离 |
| `ebs.io/spec-name` | 记录 SPEC 名的可逆编码，供 BuildInfo 回填和 RpmRepo 批次分组；实际 RPM 替换按产物元数据执行 |
| `ebs.io/package-name` | 记录 Job 所属 `Project.spec.packageRepos[].name`，供工程详情按软件包查询 Job 历史；同一仓库有多个 spec 时，它们的 Job 使用相同包名标签 |

`ebs.io/spec-name` 不添加固定前缀，通常保留 ASCII 字母数字和非末尾的 `-`、`.`，其他 UTF-8 字节转义为 `_HH`（大写十六进制，包括 `_` 和末尾的 `-`、`.`）。以非字母数字或字面 `X` 开头时，首字节转义为 `X_HH`，保证标签首字符合法且编码可逆。读取时先解码；前端按原始 SPEC 名查询 Job 时先用同一规则计算 label selector。当前暂不处理编码后超过标签长度上限的名称。

`ebs.io/package-name` 的来源是创建该 Job 时 BuildInfo Controller 本轮解析结果 `specDepends[specName].repoName`，而不是实时读取可能已修改的 Project，也不得从 spec 名或 Job 名推断。映射不存在时不创建 Job，应等待解析结果补齐或报告确定性错误。不写同名 annotation。

`ebs.io/package-name` 使用与 `ebs.io/spec-name` 相同的逐字节可逆编码；编码后超过 63 字符时截取前 63 字符，再去掉末尾的 `-`、`_`、`.`，使其符合 Kubernetes label 值规则。未截断的值可解码回原名；截断后不保证可逆或唯一，不同包名可能共享标签值，按标签查询的历史也会合并。消费者从完整 `PackageRepo.name` 按同一规则计算 label selector；Job 不另存原名，界面显示 Project 中的完整名称。旧 Job 的既有标签不会自动改写；编码值发生变化的包名，其旧 Job 不会出现在按新标签查询的历史中。

BuildInfo Controller 创建 Job 时从集群级 `Config/build-resource` 的 `spec.content` 解析 `Job.spec.resources`；资源配置来源不写入 Job annotation，调度始终以 Job 中固化的资源需求为准。Build、spec 和包归属标签由 BuildInfo Controller 创建 Job 时写入，创建后不可修改；RpmRepo Controller 使用 `ebs.io/build-name` 的 label selector 查询候选 Job，并从 `ebs.io/spec-name` 解码读取原始 spec 归属，不得从对象名称推导这些关系。新建的构建 Job 必须同时具有上述三个归属标签；apiserver 校验包名标签值的语法，普通更新和 `/status` 更新不得改变这些字段。存量 Job 不回填，没有包名标签的 Job 不出现在前端包级历史中。

## 8. 查询与扩展规则

- ES-backed 资源支持 Kubernetes 风格的 label selector；Job 和 Runner 由 etcd generic store 提供原生 label selector。
- 权限标签只能由 Gateway 授权逻辑解释，不能依赖普通 label selector 形成安全边界。
- 调度标签必须通过 Job `spec.nodeSelector` 与 Runner labels 匹配；组件不能从对象名称推导隐含标签。
- 新增系统标签时，应优先使用 `ebs.io/` 前缀，并在本文档明确适用对象、写入方、修改权限、值域和消费者。
- 删除或改变已有标签语义属于 API 行为变更，需要同步修改生产者、消费者、校验、权限规则、查询示例和测试。
