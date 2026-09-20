# EulerMaker 标签约定

## 1. 目的与范围

本文档集中定义 EulerMaker 使用的标签（label）契约，包括标签适用的对象、值语义、写入方、修改权限以及使用场景。其他设计文档只描述具体业务流程；新增系统保留标签时，应同步更新本文档。

本文档中的 API 对象标签均指 `metadata.labels`。标签的键和值必须满足 Kubernetes label 语法。未列入本文档的标签属于用户自定义元数据，不得被组件解释为权限、调度或生命周期控制信号。

## 2. API 对象标签总表

| 对象 | 标签 | 值 | 写入方 | 用途 |
|------|------|----|--------|------|
| Project | `project.ebs.io/type` | `community` / `personal` | 创建方；apiserver 默认 `personal` | 工程分类，详见 3.3 |
| Project | `ebs.io/owner-user` | User `metadata.name` | Gateway 或 system | 标识 Project owner，参与 Gateway 写权限判定 |
| Project | `ebs.io/member-user.<username>` | 固定为 `"true"` | Project owner 或 system | 授予指定用户 Project 成员权限 |
| Build | `ebs.io/target-os` | Build Target 的操作系统名称 | Build 创建方 | 按构建目标查询 Build |
| Build | `ebs.io/target-arch` | Build Target 的架构名称 | Build 创建方 | 按构建目标查询 Build |
| Build | `ebs.io/build-type` | Build `spec.buildType` | Build 创建方 | 按构建类型查询 Build |
| Job | `ebs.io/build-name` | 所属 Build 的 `metadata.name` | BuildInfo Controller | 按 Build 查询仓库输入 Job |
| Job | `ebs.io/spec-name` | Job 构建的 spec 名 | BuildInfo Controller | 仓库物化时按 spec 替换旧 RPM |
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

普通用户创建 Project 时，Gateway 强制将 `ebs.io/owner-user` 设置为 JWT `sub`。客户端提交的值不可信。system 创建 Project 时必须显式指定一个已存在且启用的 User。

普通 Project owner 不能转移所有权；只有 system 可以修改该标签。删除或设置为空同样视为修改。

### 3.2 成员

```yaml
metadata:
  labels:
    ebs.io/member-user.bob: "true"
```

成员标签的用户名位于标签 key 中，值只能是字符串 `"true"`。Project owner 和 system 可以增删成员；新增成员必须对应已存在且启用的 User。成员不能修改 owner 或成员标签。

Project 子资源不重复保存访问标签，而是通过所属 Project 继承权限。apiserver 只存储这些标签，不解释其授权语义；授权由 Gateway 完成。

### 3.3 工程分类

`project.ebs.io/type` 是系统保留的分类标签，只允许 `community`（社区工程）和 `personal`（个人工程），不增加对应 spec 字段。

- apiserver 创建及普通更新时补齐缺失标签为 `personal`；显式空值或其他值返回 `422 Invalid`。读取旧对象不写回，缺失标签按个人工程解释。
- 普通用户只能创建个人工程；不能修改或删除已有类型标签。旧对象可补写 `personal`。Ops、Admin、System 可以指定、修改类型，但仍遵循原有 Project 写权限；Ops 不因此获得跨工程编辑权限。
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

`ebs.io/target-os`、`ebs.io/target-arch` 和 `ebs.io/build-type` 必须分别与 Build `spec.buildTarget.os`、`spec.buildTarget.arch` 和 `spec.buildType` 完全一致，不进行大小写折叠或别名转换。创建或更新 Build 时，apiserver 根据默认化后的 spec 补齐缺失标签；客户端显式提供的标签与 spec 不一致时返回 `422 Invalid`，不能静默覆盖。

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
| `ebs.io/spec-name` | 记录 Job 构建的 spec 名，供仓库按 spec 完整替换旧 RPM |
| `ebs.io/package-name` | 记录 Job 所属 `Project.spec.packageRepos[].name`，供工程详情按软件包查询 Job 历史；同一仓库有多个 spec 时，它们的 Job 使用相同包名标签 |

`ebs.io/package-name` 的来源是创建该 Job 时 `BuildInfo.spec.specDepends[specName].repoName`，而不是实时读取可能已修改的 Project，也不得从 spec 名或 Job 名推断。映射不存在时不创建 Job，应等待 BuildInfo 补齐或报告确定性错误。不写同名 annotation。

`PackageRepo.name` 目前仅要求非空，可能不符合 Kubernetes label 值的长度或字符规则。计算 `ebs.io/package-name` 的值时：如果原名只含 Kubernetes label 值允许的字符，且首尾为字母或数字，先截取前 63 个字符，再去掉截断位置末尾的 `-`、`_`、`.`；若结果不匹配保留的摘要形态 `^sha256-[a-z2-7]{52}$`，直接用作 label 值。含非法字符的原名，或截断后恰好匹配保留形态的原名，使用 `sha256-` 加原名 UTF-8 字节的 SHA-256 摘要经 RFC 4648 标准 Base32 无填充编码后转小写的 52 个字符。消费者按相同规则计算 label selector 的值；Job 不保存截断前的原名，界面可从当前 Project 的仓库列表展示名称。不同原名若截断后相同，将共享一个 label 值，因此按标签查询的历史会合并；需要区分这类包名时必须调整命名规则。

BuildResource 只在创建 Job 时解析为 `Job.spec.resources`，Job 不额外记录其配置来源。Build、spec 和包归属标签由 BuildInfo Controller 创建 Job 时写入，创建后不可修改；RpmRepo Controller 使用 `ebs.io/build-name` 的 label selector 查询候选 Job，并从 `ebs.io/spec-name` 读取 spec 归属，不得从对象名称推导这些关系。新建的构建 Job 必须同时具有上述三个归属标签；apiserver 校验包名标签值的语法，普通更新和 `/status` 更新不得改变这些字段。存量 Job 不回填，没有包名标签的 Job 不出现在前端包级历史中。

## 8. 查询与扩展规则

- ES-backed 资源支持 Kubernetes 风格的 label selector；Job 和 Runner 由 etcd generic store 提供原生 label selector。
- 权限标签只能由 Gateway 授权逻辑解释，不能依赖普通 label selector 形成安全边界。
- 调度标签必须通过 Job `spec.nodeSelector` 与 Runner labels 匹配；组件不能从对象名称推导隐含标签。
- 新增系统标签时，应优先使用 `ebs.io/` 前缀，并在本文档明确适用对象、写入方、修改权限、值域和消费者。
- 删除或改变已有标签语义属于 API 行为变更，需要同步修改生产者、消费者、校验、权限规则、查询示例和测试。
