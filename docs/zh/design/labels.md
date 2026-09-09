# EulerMaker 标签约定

## 1. 目的与范围

本文档集中定义 EulerMaker 使用的标签（label）契约，包括标签适用的对象、值语义、写入方、修改权限以及使用场景。其他设计文档只描述具体业务流程；新增系统保留标签时，应同步更新本文档。

本文档中的 API 对象标签均指 `metadata.labels`。标签的键和值必须满足 Kubernetes label 语法。未列入本文档的标签属于用户自定义元数据，不得被组件解释为权限、调度或生命周期控制信号。

## 2. API 对象标签总表

| 对象 | 标签 | 值 | 写入方 | 用途 |
|------|------|----|--------|------|
| Project | `ebs.io/owner-user` | User `metadata.name` | Gateway 或 system | 标识 Project owner，参与 Gateway 写权限判定 |
| Project | `ebs.io/member-user.<username>` | 固定为 `"true"` | Project owner 或 system | 授予指定用户 Project 成员权限 |
| Build | `ebs.io/target-os` | Build Target 的操作系统名称 | Build 创建方 | 按构建目标查询 Build |
| Build | `ebs.io/target-arch` | Build Target 的架构名称 | Build 创建方 | 按构建目标查询 Build |
| Build | `ebs.io/build-type` | Build `spec.buildType` | Build 创建方 | 按构建类型查询 Build |
| Runner | `ebs.io/runner-type` | 与 `spec.type` 相同 | Runner | 表达 Runner 类型 |
| Runner | `ebs.io/runner-arch` | 与 `spec.arch` 相同 | Runner | 表达 Runner 架构，供 `nodeSelector` 精确匹配 |
| Runner | `ebs.io/runner-capability.<name>` | 由具体能力定义 | Runner | 表达 Runner 自声明能力，供 `nodeSelector` 精确匹配 |
| Runner | `ebs.io/zone` | 部署环境定义的区域名称 | system | system 管理的调度区域 |

`ebs.io/` 前缀由 EulerMaker 保留。使用该前缀但未在本文档登记的标签，不具有稳定的公共语义。

## 3. Project 访问标签

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

## 7. 标签与注解的边界

以下字段是 Job annotations，不是 labels：

| Annotation | 用途 |
|------------|------|
| `ebs.io/build-resource-namespace` | 记录实际命中的 BuildResource Project/命名空间 |
| `ebs.io/build-resource` | 记录 BuildResource 名称 |
| `ebs.io/build-resource-generation` | 记录解析时的 BuildResource generation |

这些注解只用于审计。实际调度和运行时资源限制以 `Job.spec.resources` 为准。

## 8. 查询与扩展规则

- ES-backed 资源支持 Kubernetes 风格的 label selector；Job 和 Runner 由 etcd generic store 提供原生 label selector。
- 权限标签只能由 Gateway 授权逻辑解释，不能依赖普通 label selector 形成安全边界。
- 调度标签必须通过 Job `spec.nodeSelector` 与 Runner labels 匹配；组件不能从对象名称推导隐含标签。
- 新增系统标签时，应优先使用 `ebs.io/` 前缀，并在本文档明确适用对象、写入方、修改权限、值域和消费者。
- 删除或改变已有标签语义属于 API 行为变更，需要同步修改生产者、消费者、校验、权限规则、查询示例和测试。
