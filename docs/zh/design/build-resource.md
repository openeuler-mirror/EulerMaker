# BuildResource 设计文档

## 一、背景与目标

软件包构建所需的 CPU 和内存差异较大。若所有构建 Job 使用同一套资源参数，资源较小的包会浪费 Runner 容量，资源较大的包则可能因资源不足而失败。

本设计新增项目级对象 `BuildResource`，使用一个对象集中记录 Project 下全部 spec 软件包的构建资源需求。Build Controller 在创建 Job 时查询该对象，将匹配到的资源配置写入 `Job.spec.resources`，后续 Scheduler 和 Runner 继续使用已有的 Job 资源模型。

本设计的目标是：

- 一个对象包含 Project 下所有软件包的资源需求；
- 支持同一软件包按多种 CPU 架构声明不同配置，并允许后续增加 `riscv64` 等新架构；
- 支持表级和软件包级默认值；
- 在 `default` 命名空间提供由 apiserver 自动初始化的系统默认表；
- Project 未创建自己的表时自动使用系统默认表；
- 复用现有 `ResourceRequirements`，不引入第二套资源单位和解析规则；
- Job 创建后资源需求保持稳定，不受总表后续修改影响；
- 支持通过 `resourceVersion` 对整张表执行乐观并发更新。

本设计暂不包含：

- 根据历史构建指标自动推导资源需求；
- 在 Scheduler 调度阶段动态查询资源表；
- GPU、临时存储、网络带宽等扩展资源；
- 单个软件包资源项的独立 REST API。

## 二、设计假设

当前设计基于以下假设：

1. 软件包标识使用 spec 包名，例如 `gcc`、`kernel`，不使用最终生成的二进制 RPM 子包名。
2. 每个 Project 只维护一张有效资源表，其名称固定为 Project 名。
3. 资源表与 OS 无关，同一份配置适用于该 Project 的全部 Build Target OS。
4. CPU 和内存使用现有 Job 资源数量字符串格式，例如 CPU 使用 `"8"`，内存使用 `"16Gi"`。
5. Build Controller 负责读取资源表并生成 Job；Scheduler 只读取 `Job.spec.resources.requests`。
6. `default` 是系统保留命名空间，其中保存所有 Project、所有 OS 共享的默认表。
7. apiserver 启动时保证默认表存在，但不覆盖已经存在的默认表。

## 三、API 对象

### 3.1 资源范围

`BuildResource` 是 Project scoped 资源。Project 通过 API 路径和对象的 `metadata.namespace` 表达，不在 `spec` 中重复记录。

资源命名约定：

```text
Kind:       BuildResource
Plural:     buildresources
Singular:   buildresource
ShortNames: br
```

建议 API：

```text
GET    /apis/ebs/v1/projects/{project}/buildresources
POST   /apis/ebs/v1/projects/{project}/buildresources
GET    /apis/ebs/v1/projects/{project}/buildresources/{project}
PUT    /apis/ebs/v1/projects/{project}/buildresources/{project}
DELETE /apis/ebs/v1/projects/{project}/buildresources/{project}
```

不提供 `/apis/ebs/v1/buildresources` 全局 API。apiserver 不注册该路由，Gateway、Build Controller 和 ebsctl 也不得依赖全局端点。运维角色需要操作多个 Project 时，应逐个使用 Project scoped API。

Project 自定义表的名称必须等于 Project 名。例如：

```text
namespace: openeuler-24-03-lts-sp4
name:      openeuler-24-03-lts-sp4
```

系统默认表同样遵循该规则，使用 `default/default`。确定性名称保证每个 Project 最多有一个有效资源表，调用方也不需要先执行 list 查找对象。

### 3.2 系统默认对象

系统默认表使用以下固定身份：

```text
namespace: default
name:      default
```

完整对象路径为：

```text
/apis/ebs/v1/projects/default/buildresources/default
```

`default` 命名空间中的 `default` 使用与 Project 自定义对象完全相同的名称规则、字段和存储结构。它由 apiserver 保证存在，并作为其他 Project 的回退来源。

默认对象采用以下结构：

```yaml
apiVersion: ebs/v1
kind: BuildResource
metadata:
  name: default
  namespace: default
spec:
  default:
    requests:
      cpu: "4"
      memory: 8Gi
  packages: {}
```

首次部署时允许默认对象使用空 `packages`，此时表级 `spec.default` 对所有软件包生效。省略的 limits 分别取同级 requests，因此有效 limits 同样为 4 CPU、8Gi 内存。运维可在默认对象创建后通过 API 逐步补充软件包专属配置。

### 3.3 数据模型

`BuildResource`、`BuildResourceList`、`BuildResourceSpec`、`PackageResourceConfig` 和复用的 `ResourceRequirements` 的 Go 类型、JSON tag、必填性及字段说明统一维护在 [data-models.md](./data-models.md)。本文不重复定义字段，只说明对象的初始化、回退、匹配和消费语义。

`packages` 和 `arches` 使用 Map，而不是数组，原因如下：

- 软件包和架构天然具有唯一键；
- 调用方可以直接按包名和架构查找；
- 避免数组中出现重复软件包或重复架构；
- YAML 中更适合维护大规模软件包清单。

## 四、对象示例

```yaml
apiVersion: ebs/v1
kind: BuildResource
metadata:
  name: openeuler-24-03-lts-sp4
  namespace: openeuler-24-03-lts-sp4
spec:
  default:
    requests:
      cpu: "4"
      memory: 8Gi

  packages:
    bash:
      default:
        requests:
          memory: 12Gi

    gcc:
      default:
        requests:
          cpu: "8"
          memory: 16Gi
        limits:
          cpu: "16"
          memory: 32Gi
      arches:
        aarch64:
          requests:
            cpu: "12"

    kernel:
      arches:
        x86_64:
          requests:
            cpu: "16"
            memory: 32Gi
          limits:
            cpu: "32"
            memory: 64Gi
        aarch64:
          requests:
            cpu: "24"
            memory: 48Gi
          limits:
            cpu: "48"
            memory: 96Gi
        riscv64:
          requests:
            cpu: "16"
            memory: 32Gi
          limits:
            cpu: "32"
            memory: 64Gi
```

## 五、匹配规则

Build Controller 根据以下输入查询资源需求：

```text
project + specName + arch
```

解析分为“选择对象”和“选择对象内配置”两个阶段。

### 5.1 选择 BuildResource 对象

Build Controller 按以下顺序读取对象：

1. GET `{project}/buildresources/{project}`；
2. Project 对象返回 `404 NotFound` 时，GET `default/buildresources/default`；
3. 默认对象仍返回 `404 NotFound` 时，停止创建 Job，并记录 `DefaultBuildResourceNotFound`；
4. 其他错误，例如超时、无权限、服务端错误或反序列化失败，直接返回原错误，不得当作对象不存在处理。

回退以“Project 下整个 `BuildResource` 对象不存在”为条件。如果 Project 对象已经存在，但其中没有目标软件包、架构配置或表级默认值，不得继续从 `default` 对象补齐。这样可以保证 Project 一旦声明覆盖表，其内容就是完整、可审计的配置边界，避免同一次解析隐式混用两张表。

`project == "default"` 时只读取一次 `default/default`，不重复执行回退。

### 5.2 选择对象内资源配置

选定一个 `BuildResource` 后，按以下顺序逐字段覆盖：

1. 使用 `spec.default` 初始化完整配置；
2. 使用 `spec.packages[specName].default` 覆盖已声明字段；
3. 使用 `spec.packages[specName].arches[arch]` 覆盖已声明字段；
4. 未被覆盖的字段保留 `spec.default` 中的值。

合并分别作用于 `requests` 和 `limits` 中的 `cpu`、`memory` 键。同一级声明了 request 但未声明对应 limit 时，limit 默认等于该级 request；该级连 request 也未声明时，request 和 limit 一起继承上一级。例如软件包只配置 `requests.memory: 12Gi` 时，其有效 memory limit 也是 12Gi，而 CPU request/limit 从表级默认值继承。

`spec.default.requests` 必须完整声明 CPU 和 memory；`spec.default.limits` 可以缺省并取对应 request。因此即使软件包完全没有专属配置，也总能生成完整的 `Job.spec.resources`。对象间仍不混合：Project 表存在时，不从 `default/default` 补字段。

伪代码如下：

```go
func ResolveForProject(ctx context.Context, project, specName, arch string) (ResourceRequirements, ObjectReference, error) {
    table, err := client.GetBuildResource(ctx, project, project)
    if apierrors.IsNotFound(err) && project != "default" {
        table, err = client.GetBuildResource(ctx, "default", "default")
    }
    if err != nil {
        return ResourceRequirements{}, ObjectReference{}, err
    }
    resources, err := Resolve(*table, specName, arch)
    return resources, ReferenceFor(table), err
}

func Resolve(table BuildResource, specName, arch string) (ResourceRequirements, error) {
    resources := DeepCopy(table.Spec.Default)
    if pkg, ok := table.Spec.Packages[specName]; ok {
        resources = MergeResourceFields(resources, pkg.Default)
        if archResources, ok := pkg.Arches[arch]; ok {
            resources = MergeResourceFields(resources, archResources)
        }
    }
    return resources, ValidateEffectiveResources(resources)
}
```

## 六、Build 与 Job 集成

资源表仅作为创建 Job 时的配置来源，不作为 Scheduler 的直接输入：

```text
Project/{project} ──不存在──> default/default
       │                                  │
       └────────────────┬─────────────────┘
                        │ 按 specName、arch 解析
                        ▼
Build Controller
        │ 深拷贝解析结果
        ▼
Job.spec.resources
        │
        ├── requests → Scheduler 选择 Runner
        └── limits   → Runner 限制构建容器
```

创建 Job 时建议记录资源配置来源：

```yaml
metadata:
  annotations:
    ebs.io/build-resource-namespace: default
    ebs.io/build-resource: default
    ebs.io/build-resource-generation: "7"
```

其中：

- `ebs.io/build-resource-namespace` 记录实际命中的 Project/命名空间；
- `ebs.io/build-resource` 记录 BuildResource 名称；
- `ebs.io/build-resource-generation` 记录解析时的对象 generation。

注解只用于审计，实际调度始终以 `Job.spec.resources` 为准。总表更新不会改变已经创建的 Job；需要应用新配置时，应重新创建 Job。

## 七、校验规则

apiserver 创建或更新对象时执行以下校验：

### 7.1 对象级校验

- `metadata.name` 必须符合 DNS1123 label；
- `metadata.namespace` 必须存在，并与 API 路径中的 Project 一致；
- 所有命名空间中的对象都不得声明 `spec.os`；该字段不属于 API 模型；
- Project 自定义对象的 `metadata.name` 必须等于 `metadata.namespace`，即 Project 名；
- `spec.default.requests` 必须完整声明 CPU 和 memory；limits 可以缺省，缺省值取同级 requests；
- Project 自定义对象的 `spec.packages` 不得为空；
- `default/default` 允许 `spec.packages` 为空，但必须声明有效的 `spec.default`；
- 软件包键允许使用 `kernel:kernel-rt` 形式表示 multibuild 子包；冒号分隔的每一段都必须是有效的 spec 包名；

上述确定性名称约束保证同一 Project 下只能存在一张有效资源表，不需要额外执行跨对象唯一性查询。

### 7.2 软件包与架构校验

- 软件包 Map key 必须为非空合法 spec 名称；
- 软件包必须至少声明 `default` 或一个 `arches` 条目；
- 架构不使用固定枚举，允许 `x86_64`、`aarch64`、`riscv64` 及后续新增架构；
- 架构 key 必须使用系统约定的规范名称，并满足 `^[a-z0-9][a-z0-9._-]{0,62}$`；
- Build Target、Runner label 和资源表必须使用完全一致的架构名称，apiserver 不自动转换 `risc-v`、`riscv64` 等别名；
- Map 结构天然禁止同一个软件包出现重复架构。

### 7.3 资源数量校验

- 当前只允许 `cpu` 和 `memory` 两种资源键；
- 软件包 default 和架构配置可以只声明 CPU 或 memory，缺失字段按架构、软件包、表级 default 的顺序继承；
- 任一级声明 request 但省略对应 limit 时，limit 使用同级 request；
- CPU 和内存必须能被 Kubernetes `resource.ParseQuantity` 解析；
- CPU 和内存必须大于 0；
- 合并后的每种资源必须满足 `limits` 大于或等于 `requests`；
- 建议 CPU request 使用整数核，避免当前 Runner 以逻辑 CPU 整数上报时产生精度和超卖语义差异；
- 内存建议使用二进制单位 `Mi` 或 `Gi`。

未知资源键应直接拒绝，而不是忽略，以防 `memroy` 等拼写错误导致 Job 缺少有效资源约束。

## 八、apiserver 启动初始化

apiserver 通过 `go:embed` 内置全局默认 `BuildResource` JSON 清单。初始清单使用表级 `spec.default` 覆盖所有软件包，不把软件包明细硬编码成 Go 字面量；后续可通过普通 API 更新软件包专属配置。

apiserver 在完成存储初始化、注册 REST storage 之后，对外进入 Ready 之前执行 `EnsureDefaultBuildResource`：

1. 确认系统保留的 `default` Project/命名空间存在；如果 Project 是对象访问的必要前提，则先以同样的幂等方式创建它；
2. GET `default/default`；
3. 对象存在时直接完成，不修改、不合并，也不覆盖用户或运维已经更新的内容；
4. 仅在返回 `404 NotFound` 时读取内置清单，执行完整解码和字段校验后创建对象；
5. 创建返回 `AlreadyExists` 时视为成功，以兼容多个 apiserver 副本同时启动；
6. GET、校验或创建发生其他错误时，apiserver 保持 NotReady 并退出启动流程，避免系统在缺少默认表的情况下接受构建请求。

伪代码如下：

```go
func EnsureDefaultBuildResource(ctx context.Context, client BuildResourceInterface) error {
    const namespace = "default"
    const name = "default"

    _, err := client.BuildResources(namespace).Get(ctx, name, metav1.GetOptions{})
    switch {
    case err == nil:
        return nil
    case !apierrors.IsNotFound(err):
        return err
    }

    object, err := loadAndValidateEmbeddedDefault()
    if err != nil {
        return err
    }
    _, err = client.BuildResources(namespace).Create(ctx, object, metav1.CreateOptions{})
    if apierrors.IsAlreadyExists(err) {
        return nil
    }
    return err
}
```

初始化必须走与普通 API 创建相同的 defaulting 和 validation 逻辑，不能直接向 Elasticsearch 写文档。默认对象同样满足名称等于命名空间的校验，不需要额外的命名例外。

该机制是“启动时确保存在”，不是持续 reconcile：运行中的默认对象被删除后，不会立即自动恢复，直到 apiserver 重启。为降低误删风险，Gateway 应只允许运维角色修改或删除 `default` 命名空间中的 `BuildResource`。如果业务要求删除后立即恢复，应后续增加独立 Controller，不应在请求处理路径内临时创建。

默认表内容升级遵循 create-only 语义。新版 apiserver 携带的新模板不会覆盖集群中已经存在的对象；默认表的数据升级由运维通过正常 API 更新，以避免部署过程静默改变后续 Job 的资源需求。

## 九、更新与并发

该对象是整张总表，更新任意软件包都会改变同一个对象。因此客户端更新时必须携带最新的 `metadata.resourceVersion`，发生冲突时重新获取、合并并重试，不允许无条件覆盖。

推荐维护方式：

1. 从 apiserver 获取当前对象；
2. 修改目标 `spec.packages[specName]`；
3. 保留获取到的 `resourceVersion` 执行更新；
4. 收到 `409 Conflict` 时重新获取并执行键级合并；
5. 更新成功后校验返回的 `generation`。

批量生成工具应按软件包 key 做稳定排序后输出 YAML，以降低代码评审时的无关 diff。JSON/对象语义不依赖 Map 顺序。

## 十、存储与容量约束

总表对象可能包含数千或数万个软件包，设计和实现时必须评估序列化后的对象大小。建议：

- 主存储使用 Elasticsearch，与 Project、BuildInfo 等配置和索引类对象保持一致；
- apiserver 对对象设置明确的最大序列化大小，避免单次请求耗尽内存；
- 第一版建议限制软件包数量和对象大小，例如最多 50,000 个软件包、JSON 不超过 16 MiB，最终限制应根据真实数据测量确定；
- Build Controller 按 `metadata.resourceVersion` 或 `generation` 缓存解析后的 Map，避免为每个包重复反序列化整张表；
- 更新频率应保持较低，资源数据批量计算完成后一次性提交。

若真实数据超过 apiserver、网关或 Elasticsearch 的安全请求限制，应重新评估“单对象总表”的约束。此时可保持对外的逻辑总表语义，但在存储层引入分片；首版不实现分片。

## 十一、权限建议

BuildResource 不属于公开读取资源，Gateway 必须按路径中的 Project 校验 owner/member 关系。授权规则如下：

- Project owner：只读自己拥有的 Project 下的对象；
- Project member：只读自己作为 member 的 Project 下的对象；
- Build Controller MachineAccount：只读；
- 运维角色：跨 Project 读写；
- Scheduler 和 Runner：无需读取该对象。

普通 Project owner/member 禁止创建、更新、Patch 或删除任何 `BuildResource`，也不能通过 `default` Project 路径读取全局默认对象。只有运维角色和 apiserver 启动初始化身份具有写权限。Build Controller 具有读取权限，以便执行回退。

## 十二、实现范围

落地该设计需要完成：

1. 在 `ebs/v1` 增加对象、List 和辅助结构体；
2. 更新 scheme 注册、deepcopy 和 OpenAPI；
3. 增加 Elasticsearch 索引和仅 Project scoped 的 REST storage，确保未注册 `/apis/ebs/v1/buildresources`；
4. 增加对象及资源 quantity 校验；
5. 在 Gateway 中加入该 Project scoped 资源的鉴权映射；
6. 在 ebsctl 中增加 get/list/create/update/delete 支持；
7. 提供内置默认表清单，并在 apiserver Ready 前幂等创建 `default` 命名空间及默认对象；
8. 在 Build Controller 中实现 Project 优先、`default` 回退的对象查询、缓存与配置匹配；
9. 创建 Job 时写入 `Job.spec.resources` 和包含实际来源命名空间的审计注解；
10. 增加 API、初始化、多副本并发创建、回退边界、匹配优先级、并发更新和大对象边界测试；
11. 更新统一数据模型文档。
