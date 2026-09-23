# 构建配置设计

## 1. 总体职责

本文统一维护 BuildConf、BuildResource 和 Script 设计。BuildInfo Controller 在创建 Job 时选择配置，Scheduler 和 Runner 使用 Job 中固化的结果。Script 的 apiserver 资源接口和 Gateway 权限已实现，scriptRef 字段及 Controller/Runner 消费链路待接入。

| 对象 | 配置内容 | 作用域 | Job 中的结果 |
|------|----------|--------|-------------|
| BuildConf | 支持的 OS、Arch 和构建镜像 | 集群级 `default` | `spec.runtimeSpec.image` |
| BuildResource | spec 软件包及架构的 CPU、内存规则 | Project 级，缺失时回退全局默认表 | `spec.resources` |
| Script | 构建脚本内容 | 集群级，所有 Project 共用 | `spec.scriptRef`，Runner 按固定引用拉取 |

BuildConf 与 BuildResource 的 API、默认表已实现。当前 controller-manager 已提供 BuildConf 读取与镜像解析接口；BuildInfo Controller 尚未实现，创建 Job 时的消费规则由该控制器接入。字段类型统一维护在 [数据模型](data-models.md)。

- [BuildConf：构建环境](#2-buildconf构建环境)
- [BuildResource：资源规则](#3-buildresource资源规则)
- [Script：构建脚本](#4-script构建脚本)
- [创建 Job 时的组合](#5-创建-job-时的组合)

## 2. BuildConf：构建环境

### 2.1 目标与范围

BuildConf 是集群级构建配置，提供OS、架构和镜像映射，为前端和任务创建方提供同一份数据。

- 前端从配置生成目标 OS、Arch 下拉选项。
- BuildInfo Controller 根据 Build 的 OS、Arch 选择镜像，并写入 Job。
- Ops、Admin、System 可以通过 Gateway 修改配置，无需重启服务。

### 2.2 对象

资源使用 `ebs/v1`，Kind 为 `BuildConf`，复数为 `buildconfs`。首版只接受名称 `default`，不允许 namespace 或 generateName，不依赖同名 Project。

```yaml
apiVersion: ebs/v1
kind: BuildConf
metadata:
  name: default
spec:
  targets:
    openEuler-24.03-LTS-SP4:
      arches:
        x86_64:
          image: registry.example.com/build/openeuler:24.03-lts-sp4-amd64
        aarch64:
          image: registry.example.com/build/openeuler:24.03-lts-sp4-arm64
    openEuler-mainline:
      arches:
        x86_64:
          image: registry.example.com/build/openeuler:mainline-amd64
```

镜像地址为示例，不作为内置可用镜像。

字段定义见 [数据模型](data-models.md#buildconf构建配置待实现)。不设置 status，不额外维护 supportedOS、supportedArch、镜像摘要或配置版本字段；版本复用 metadata.resourceVersion / generation。

#### 2.2.1 校验与解释

| 字段 | 规则 |
|------|------|
| `spec.targets` | OS 到目标配置的映射；允许空表，表示暂不接受新的构建目标 |
| OS key | 非空、无首尾空白，满足 Build 目标 OS 对应 label value 的语法，与 `BuildTarget.os` 精确匹配 |
| `arches` | 每个 OS 至少包含一个架构；选项仅来自所选 OS，不对全部 OS 的架构取并集 |
| Arch key | 使用现有 Build 目标架构校验，不写死 x86_64/aarch64，与 `BuildTarget.arch` 精确匹配 |
| `image` | 必填，合法的容器镜像引用，允许 tag 或 digest；禁止 URL scheme、内嵌凭据和空白；使用镜像引用解析库校验，不联网检查存在性 |

不自动转换 OS 大小写、冒号、连字符，也不从 OS 名称拼接镜像。运维显式维护映射。缺失条目没有默认镜像回退。

### 2.3 API 与权限

```text
GET/HEAD /apis/ebs/v1/buildconfs
POST     /apis/ebs/v1/buildconfs
GET/HEAD /apis/ebs/v1/buildconfs/default
PUT      /apis/ebs/v1/buildconfs/default
PATCH    /apis/ebs/v1/buildconfs/default
```

提供标准对象与 BuildConfList 响应。只支持 JSON Merge Patch 和 JSON Patch，不支持 Strategic Merge Patch。无 DELETE、watch、/status 或 Project scoped 路由；移除 OS、Arch 通过修改 spec 完成，不删除全局对象。

- GET/HEAD 公开只读；带凭据请求遵循 Gateway 现有认证规则。配置中禁止保存密码、token 等秘密。
- POST/PUT/PATCH 仅允许 Ops、Admin、System，与 Project owner/member 无关；普通用户与 Runner 身份不得写入。
- apiserver 负责对象结构、字段值和版本校验，Gateway 负责身份权限，不复制字段校验逻辑。
- POST 已存在返回 `409 AlreadyExists`；更新必须携带匹配的 resourceVersion，PATCH 后的对象也必须满足版本检查。并发修改返回 `409 Conflict`。
- 非法配置返回 `422 Invalid`，对象缺失返回 `404`，未授权写入返回 `403`。无修改权限时禁止通过其他路径或子资源绕过。
- spec 实际改变时 generation 递增，元数据更新不递增 generation；不允许用户覆盖服务端 UID、时间及版本信息。

运维客户端更新冲突时重新读取并提示合并，不能仅替换 resourceVersion 后盲目重放旧配置。

### 2.4 存储与初始化

使用 Elasticsearch，逻辑 alias 为 `ebs-buildconfs`，首个物理索引为 `ebs-buildconfs-v1`，文档 ID 为 `default`，沿用现有 alias 初始化机制。

复用现有通用 mapping，配置保存在 data 中，不新增 OS、Arch、image 的索引字段；客户端直接 GET 全局对象，无需对内部配置做 ES 查询。资源不开放 status.phase/status.stage 过滤。

初始化文件为 `components/ebs-apiserver/pkg/server/default-build-conf.yaml`，随二进制嵌入。

启动时在 Ready 前：

1. 确保索引与 alias 存在，读取默认对象。
2. 对象存在则沿用，不把随版本发布的 YAML 覆盖到线上。
3. 对象不存在时校验初始化 YAML，并通过 create-only 写入创建。
4. 多实例创建冲突后读取获胜对象并继续；不覆盖。其他存储错误导致初始化失败，不能在未知结果下覆盖配置。

启动配置不是持续同步源。发布新版本不修改运维数据；导入生产映射由 Ops 使用 API 或前端完成。初始化空表不阻止 apiserver Ready，但新建 Build 会因目标未配置而被拒绝。

### 2.5 构建流程

#### 2.5.1 Project 与 Build

Project.spec.buildTargets 仍记录工程自己的构建目标。Project 创建/更新只做现有字段校验，不强制所有目标当前都在 BuildConf 中，避免全局配置变更后工程无法进行其他编辑。

创建任意类型 Build 时，apiserver 在申请非 single 目标占用、写入 Build 之前读取 BuildConf，校验 `spec.buildTarget.os/arch` 是否有对应映射：

- 目标不存在：`422 Invalid`，错误定位到 `spec.buildTarget`，不创建 Build、不申请占用。
- 配置对象不存在或读取失败：`503 ServiceUnavailable`，不得当成用户输入错误或使用硬编码回退。
- 成功：继续已有 Project 目标、packages、并发占用等校验流程。

该检查不是锁定配置：Build 创建成功后配置仍可能变化，因此 Job 创建方必须再次检查。

#### 2.5.2 BuildInfo Controller 创建 Job

每次需要创建新 Job 的 reconcile，读取一次最新 BuildConf；该轮批量创建使用同一个内存快照，按所属 Build 的 OS、Arch 查询镜像。

镜像写入 `Job.spec.runtimeSpec.image`。该字段由配置解析结果确定，Job 模板不得覆盖它；runtimeSpec 其他字段及 BuildResource 解析按原规则执行。首版面向现有容器 rpmbuild Job，不把容器镜像解释为 VM 镜像。

读取失败或映射缺失时，不创建本轮的新 Job，输出结构化错误，按 controller 框架的错误分类及慢速重入策略等待配置恢复；不把配置问题直接写成构建失败。无需注册 BuildConf watch。已有 Job 的观察、结果回收不依赖本轮配置可用性。

Job 创建沿用确定性名称及幂等流程：已存在的同一任务 Job 直接沿用，不能因配置改变更新其镜像；Create 返回结果未知时先确认原请求的写入意图，不能读取新配置后用不同镜像盲目重试同一次创建。

#### 2.5.3 生效边界

首版采用**创建 Job 时取配置**，不在 Build 或 BuildInfo 中额外保存镜像快照：

- 已创建的 Pending/Running Job 均继续使用自身固化的镜像。
- 修改或移除配置会影响之后创建的 Job，包括正在进行的 Build 中尚未创建的 Job。
- 同一 Build 可以包含使用不同镜像的 Job；不承诺 Build 级环境快照一致性。
- Runner 只执行 Job.spec.runtimeSpec，不读取 BuildConf。
- tag 可被镜像仓库重新指向，Job 固化 tag 不等于固化镜像内容；需要内容可复现时由运维填写 digest 引用。

这也是删除某个 OS/Arch 的语义：禁止后续接收该目标 Build，并暂停该目标尚未创建 Job 的派发，不删除或中止已有任务。恢复映射后重新调和即可继续。

### 2.6 前端与 ebsctl

“运维管理”新增“构建配置”入口，Ops 及以上支持按 OS/Arch 增删行、填写镜像，并支持完整 spec 的 YAML/JSON 编辑。两种模式共用同一份草稿，切换不得丢字段；保存携带原 resourceVersion。删除目标前提示对进行中 Build 的影响。

工程创建及工程目标编辑页面：

- 从 BuildConf 获取 OS 选项，选中 OS 后展示其 arches；选项按名称排序，不维护第二份前端列表。
- 新行默认 OS 未选择；切换 OS 后，原 Arch 不受支持则清空，要求重新选择。
- 空配置显示“暂无可用构建目标”；配置请求失败显示错误并支持重试，不回退到硬编码选项。
- 旧工程不受支持的目标仍显示原值并标注“不再支持”；不静默删除或替换。未修改的旧目标不阻止保存其他工程配置。
- 发起 Build 时，对不支持的目标禁用提交并给出提示；最终仍以 apiserver 校验为准。

ebsctl 注册集群级 BuildConf 的 get/list/create/replace/patch；`-p/-n` 不改变其路径。delete/watch 返回不支持。不导入镜像仓库凭据。

### 2.7 实现与验收

| 模块 | 修改范围 |
|------|----------|
| 公共 api | BuildConf 类型、List、scheme/deepcopy 注册 |
| apiserver | storage、校验、初始化、ES alias、OpenAPI、Build create 校验 |
| gateway | 公开读取白名单、Ops 及以上写权限、集群级路由识别 |
| controller-manager | 类型化读取接口，BuildInfo Controller 创建 Job 时解析镜像 |
| frontend | 运维配置编辑、OS/Arch 选项、不可用目标提示 |
| ebsctl | 集群级资源与允许操作注册 |

实现时使用现有可重复执行的代码生成脚本，不手工维护独立 OpenAPI 文件。

验收至少覆盖：合法/非法镜像和目标、空配置、两架构不同镜像、无权限写入、更新冲突、多实例初始化不覆盖、未配置目标创建 Build 不留下占用、配置读取失败、批量 Job 使用同一配置快照、Job 创建未知结果确认、配置更新不修改已有 Job、删除目标后新任务停发及恢复、前端旧目标保留和请求失败提示。

## 3. BuildResource：资源规则

### 3.1 背景与目标

软件包构建所需的 CPU 和内存差异较大。若所有构建 Job 使用同一套资源参数，资源较小的包会浪费 Runner 容量，资源较大的包则可能因资源不足而失败。

本设计新增项目级对象 `BuildResource`，使用一个对象集中记录 Project 下全部 spec 软件包的构建资源需求。BuildInfo Controller 在创建 Job 时查询该对象，将匹配到的资源配置写入 `Job.spec.resources`，后续 Scheduler 和 Runner 继续使用已有的 Job 资源模型。

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

### 3.2 设计假设

当前设计基于以下假设：

1. 软件包标识使用 spec 包名，例如 `gcc`、`kernel`，不使用最终生成的二进制 RPM 子包名。
2. 每个 Project 只维护一张有效资源表，约定以 Project 名查找有效资源表。
3. 资源表与 OS 无关，同一份配置适用于该 Project 的全部 Build Target OS。
4. CPU 和内存使用现有 Job 资源数量字符串格式，例如 CPU 使用 `"8"`，内存使用 `"16Gi"`。
5. BuildInfo Controller 负责读取资源表并创建 Job；Scheduler 只读取 `Job.spec.resources.requests`。
6. `default` 是系统保留命名空间，其中保存所有 Project、所有 OS 共享的默认表。
7. apiserver 启动时保证默认表存在，但不覆盖已经存在的默认表。

### 3.3 API 对象

#### 3.3.1 资源范围

`BuildResource` 是 Project scoped 资源。Project 通过 API 路径和对象的 `metadata.namespace` 表达，不在 `spec` 中重复记录。

约定使用 Project 名作为 `BuildResource` 的名称，但该约定不作为对象校验依据。

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

不提供 `/apis/ebs/v1/buildresources` 全局 API。apiserver 不注册该路由，Gateway、BuildInfo Controller 和 ebsctl 也不得依赖全局端点。运维角色需要操作多个 Project 时，应逐个使用 Project scoped API。

Project 自定义表按同名约定维护。例如：

```text
namespace: openeuler-24-03-lts-sp4
name:      openeuler-24-03-lts-sp4
```

系统默认表使用 `default/default`。调用方按约定名称读取，不需要先执行 list 查找；该约定不作为名称与命名空间必须相等的校验依据。

#### 3.3.2 系统默认对象

系统默认表使用以下固定身份：

```text
namespace: default
name:      default
```

完整对象路径为：

```text
/apis/ebs/v1/projects/default/buildresources/default
```

`default` 作用域中的 `default` 使用与 Project 自定义对象完全相同的名称规则、字段和存储结构。它由 apiserver 保证存在，并作为其他 Project 的回退来源。`default` 是系统保留作用域，不要求也不允许创建同名 Project。

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

#### 3.3.3 数据模型

`BuildResource`、`BuildResourceList`、`BuildResourceSpec`、`PackageResourceConfig` 和复用的 `ResourceRequirements` 的 Go 类型、JSON tag、必填性及字段说明统一维护在 [data-models.md](./data-models.md)。本文不重复定义字段，只说明对象的初始化、回退、匹配和消费语义。

`packages` 和 `arches` 使用 Map，而不是数组，原因如下：

- 软件包和架构天然具有唯一键；
- 调用方可以直接按包名和架构查找；
- 避免数组中出现重复软件包或重复架构；
- YAML 中更适合维护大规模软件包清单。

### 3.4 对象示例

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

### 3.5 匹配规则

BuildInfo Controller 根据以下输入查询资源需求：

```text
project + specName + arch
```

解析分为“选择对象”和“选择对象内配置”两个阶段。

#### 3.5.1 选择 BuildResource 对象

BuildInfo Controller 按以下顺序读取对象：

1. GET `{project}/buildresources/{project}`；
2. Project 对象返回 `404 NotFound` 时，GET `default/buildresources/default`；
3. 默认对象仍返回 `404 NotFound` 时，停止创建 Job，并记录 `DefaultBuildResourceNotFound`；
4. 其他错误，例如超时、无权限、服务端错误或反序列化失败，直接返回原错误，不得当作对象不存在处理。

回退以“Project 下整个 `BuildResource` 对象不存在”为条件。如果 Project 对象已经存在，但其中没有目标软件包、架构配置或表级默认值，不得继续从 `default` 对象补齐。这样可以保证 Project 一旦声明覆盖表，其内容就是完整、可审计的配置边界，避免同一次解析隐式混用两张表。

`project == "default"` 时只读取一次 `default/default`，不重复执行回退。

#### 3.5.2 选择对象内资源配置

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

### 3.6 BuildInfo 与 Job 集成

资源表仅作为创建 Job 时的配置来源，不作为 Scheduler 的直接输入：

```text
Project/{project} ──不存在──> default/default
       │                                  │
       └────────────────┬─────────────────┘
                        │ 按 specName、arch 解析
                        ▼
BuildInfo Controller
        │ 深拷贝解析结果
        ▼
Job.spec.resources
        │
        ├── requests → Scheduler 选择 Runner
        └── limits   → Runner 限制构建容器
```

### 3.7 校验规则

apiserver 创建或更新对象时执行以下校验：

#### 3.7.1 对象级校验

- `metadata.name` 必须符合 DNS1123 label；
- `metadata.namespace` 必须存在，并与 API 路径中的 Project 一致；
- 所有命名空间中的对象都不得声明 `spec.os`；该字段不属于 API 模型；
- `spec.default.requests` 必须完整声明 CPU 和 memory；limits 可以缺省，缺省值取同级 requests；
- Project 自定义对象的 `spec.packages` 不得为空；
- `default/default` 允许 `spec.packages` 为空，但必须声明有效的 `spec.default`；
- 软件包键允许使用 `kernel:kernel-rt` 形式表示 multibuild 子包；冒号分隔的每一段都必须是有效的 spec 包名；

#### 3.7.2 软件包与架构校验

- 软件包 Map key 必须为非空合法 spec 名称；
- 软件包必须至少声明 `default` 或一个 `arches` 条目；
- 架构不使用固定枚举，允许 `x86_64`、`aarch64`、`riscv64` 及后续新增架构；
- 架构 key 必须使用系统约定的规范名称，并满足 `^[a-z0-9][a-z0-9._-]{0,62}$`；
- Build Target、Runner label 和资源表必须使用完全一致的架构名称，apiserver 不自动转换 `risc-v`、`riscv64` 等别名；
- Map 结构天然禁止同一个软件包出现重复架构。

#### 3.7.3 资源数量校验

- 当前只允许 `cpu` 和 `memory` 两种资源键；
- 软件包 default 和架构配置可以只声明 CPU 或 memory，缺失字段按架构、软件包、表级 default 的顺序继承；
- 任一级声明 request 但省略对应 limit 时，limit 使用同级 request；
- CPU 和内存必须能被 Kubernetes `resource.ParseQuantity` 解析；
- CPU 和内存必须大于 0；
- 合并后的每种资源必须满足 `limits` 大于或等于 `requests`；
- 建议 CPU request 使用整数核，避免当前 Runner 以逻辑 CPU 整数上报时产生精度和超卖语义差异；
- 内存建议使用二进制单位 `Mi` 或 `Gi`。

未知资源键应直接拒绝，而不是忽略，以防 `memroy` 等拼写错误导致 Job 缺少有效资源约束。

### 3.8 apiserver 启动初始化

apiserver 通过 `go:embed` 内置全局默认 `BuildResource` YAML 清单（`components/ebs-apiserver/pkg/server/default-build-resource.yaml`）。初始清单使用表级 `spec.default` 覆盖所有软件包，不把软件包明细硬编码成 Go 字面量；后续可通过普通 API 更新软件包专属配置。

apiserver 在完成存储初始化、注册 REST storage 之后，对外进入 Ready 之前执行 `EnsureDefaultBuildResource`：

1. 使用系统保留的 `default` 作用域；不创建 `default` Project；
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

初始化必须走与普通 API 创建相同的 defaulting 和 validation 逻辑，不能直接向 Elasticsearch 写文档。默认对象与 Project 自定义表使用相同的字段校验。

该机制是“启动时确保存在”，不是持续 reconcile：运行中的默认对象被删除后，不会立即自动恢复，直到 apiserver 重启。因此 Gateway 禁止任何身份删除 `default/default`，但仍允许授权身份更新它；`default` 命名空间中的其他 `BuildResource` 按常规权限管理。不应在请求处理路径内临时创建默认对象。

默认表内容升级遵循 create-only 语义。新版 apiserver 携带的新模板不会覆盖集群中已经存在的对象；默认表的数据升级由运维通过正常 API 更新，以避免部署过程静默改变后续 Job 的资源需求。

### 3.9 更新与并发

该对象是整张总表，更新任意软件包都会改变同一个对象。因此客户端更新时必须携带最新的 `metadata.resourceVersion`，发生冲突时重新获取、合并并重试，不允许无条件覆盖。

推荐维护方式：

1. 从 apiserver 获取当前对象；
2. 修改目标 `spec.packages[specName]`；
3. 保留获取到的 `resourceVersion` 执行更新；
4. 收到 `409 Conflict` 时重新获取并执行键级合并；
5. 更新成功后校验返回的 `generation`。

批量生成工具应按软件包 key 做稳定排序后输出 YAML，以降低代码评审时的无关 diff。JSON/对象语义不依赖 Map 顺序。

### 3.10 存储与容量约束

总表对象可能包含数千或数万个软件包，设计和实现时必须评估序列化后的对象大小。建议：

- 主存储使用 Elasticsearch，与 Project、BuildInfo 等配置和索引类对象保持一致；
- apiserver 对对象设置明确的最大序列化大小，避免单次请求耗尽内存；
- 第一版建议限制软件包数量和对象大小，例如最多 50,000 个软件包、JSON 不超过 16 MiB，最终限制应根据真实数据测量确定；
- BuildInfo Controller 按 `metadata.resourceVersion` 或 `generation` 缓存解析后的 Map，避免为每个包重复反序列化整张表；
- 更新频率应保持较低，资源数据批量计算完成后一次性提交。

若真实数据超过 apiserver、网关或 Elasticsearch 的安全请求限制，应重新评估“单对象总表”的约束。此时可保持对外的逻辑总表语义，但在存储层引入分片；首版不实现分片。

### 3.11 权限建议

BuildResource 不属于公开读取资源，Gateway 必须按路径中的 Project 校验 owner/member 关系。授权规则如下：

- Project owner：只读自己拥有的 Project 下的对象；
- Project member：只读自己作为 member 的 Project 下的对象；
- BuildInfo Controller 内部服务身份：只读；
- 运维角色：跨 Project 读写；
- Scheduler 和 Runner：无需读取该对象。

普通 Project owner/member 禁止创建、更新、Patch 或删除任何 `BuildResource`，也不能通过 `default` 保留作用域路径读取全局默认对象。只有运维角色和 apiserver 启动初始化身份具有写权限。BuildInfo Controller 具有读取权限，以便执行回退。

### 3.12 实现范围

落地该设计需要完成：

1. 在 `ebs/v1` 增加对象、List 和辅助结构体；
2. 更新 scheme 注册、deepcopy 和 OpenAPI；
3. 增加 Elasticsearch 索引和仅 Project scoped 的 REST storage，确保未注册 `/apis/ebs/v1/buildresources`；
4. 增加对象及资源 quantity 校验；
5. 在 Gateway 中加入该 Project scoped 资源的鉴权映射；
6. 在 ebsctl 中增加 get/list/create/update/delete 支持；
7. 提供内置默认表清单，并在 apiserver Ready 前于保留的 `default` 作用域幂等创建默认对象；
8. 在 BuildInfo Controller 中实现 Project 优先、`default` 回退的对象查询、缓存与配置匹配；
9. 创建 Job 时写入 `Job.spec.resources`；
10. 增加 API、初始化、多副本并发创建、回退边界、匹配优先级、并发更新和大对象边界测试；
11. 更新统一数据模型文档。

## 4. Script：构建脚本

### 4.1 职责与更新模型

Script 管理可执行脚本；BuildConf 管理镜像；BuildResource 管理资源需求。`Project.spec.buildPayload`、`BuildInfo.spec.buildPayload` 和 `Job.spec.payload` 继续传递任务参数，不改成脚本引用，也不删除。对于 CT 构建，Runner 仍将 payload 写成 `/workspace/payload.yaml`，由脚本读取。

Script 的 spec 只包含 `content`，支持原地修改，通过 `resourceVersion` 防止并发覆盖，不另设 revision 对象或历史内容存储。Job 引用脚本对象，不固定其内容版本；Runner 每次执行尝试使用 GET 时获取的当前内容。

尚未拉取脚本的 Job 可以使用更新后的内容；已经成功拉取的执行尝试使用本地副本，不热更新。接受同一 Build 的不同 Job，以及同一 Job 的不同执行尝试，可能使用不同内容，不承诺历史脚本可重放。

### 4.2 资源与 API

新增集群级 `ebs/v1 Script`、`ScriptList`，复数 `scripts`，通过 Elasticsearch 持久化。所有 Project 共用同一组脚本，不设置 namespace，也不设 Project 覆盖或 `default` 保留作用域。

```yaml
apiVersion: ebs/v1
kind: Script
metadata:
  name: rpmbuild
spec:
  content: |
    #!/bin/bash
    set -euo pipefail
    # 示例入口；具体构建工具由镜像提供。
    exec /usr/local/bin/build-rpm --config /workspace/payload.yaml
```

接口路径：

```text
GET/POST     /apis/ebs/v1/scripts
GET/PUT/PATCH /apis/ebs/v1/scripts/{name}
```

提供集群级列表，不注册`/status`。首版不开放 DELETE 和自动回收，避免尚未执行或需要重试的 Job 丢失引用对象；后续引入回收能力时另行定义引用与保留策略。

apiserver 校验：

- `content` 为非空 UTF-8 文本，不允许 NUL，不设独立正文大小上限；不规范化换行或尾部空白。apiserver 保留 2 MiB 请求体上限，经 Gateway 访问还受其请求体限制。
- `content` 第一行必须以 `#!` 开头并指定非空的绝对解释器路径，例如 `#!/bin/bash`；解释器及其依赖由构建镜像提供。
- PUT/PATCH 允许更新脚本正文和允许修改的 metadata，更新时遵循现有 `resourceVersion` 冲突检查。
- 对象名称必填，使用 DNS subdomain 格式，集群内唯一；不允许设置 `metadata.namespace` 或 `generateName`。

脚本正文不存放密码、Token、私钥等凭据；凭据沿用独立的任务凭据机制，不通过脚本分发。

### 4.3 选择与 Job 引用

新增可选 `Project.spec.scriptRef.name`，仅用于选择全局脚本，不定义 Project 私有脚本。Build Controller 创建 BuildInfo 时将该选择复制到 `BuildInfo.spec.scriptRef.name`。它是名称选择，不是已解析的 UID 引用；与 buildPayload 一样不跟随后续 Project 修改。

BuildInfo Controller 创建新 Job 时：

1. 使用 BuildInfo 中的脚本名称；未配置时使用启动参数 `--default-script-name`，默认 `rpmbuild`。
2. 通过 `/apis/ebs/v1/scripts/{name}` GET 全局 Script。指定名称不存在或读取失败时不创建 Job，不回退到其他名称；默认名称仅在未配置名称时使用。
3. 校验所选对象，将 name、UID 固化到 `Job.spec.scriptRef`，不固定 resourceVersion 或内容摘要。UID 只标识对象身份，不限制同一对象的内容更新。
4. 与镜像、资源需求和 payload 一起提交 Job。已存在 Job 沿用原引用；创建结果未知时确认原创建意图，不重新选择对象。

```yaml
spec:
  runtime: ct
  scriptRef:
    name: rpmbuild
    uid: 61304b92-72cf-4a41-8bf7-8e0a9d14f6a5
```

Job 引用只包含 name、UID，两个字段必须同时存在，不包含 namespace。创建后引用不可变，但被引用脚本的内容可以修改。apiserver 校验结构和格式，不在 Job 写入过程中跨存储读取脚本；创建方负责选择，Runner 负责执行前确认对象身份和内容合法性。

默认名称的配置切换只影响后续新 Job，已有 Job 仍引用原对象；全局脚本内容更新可影响所有引用它的 Project，按 4.1 的拉取边界生效。

### 4.4 Runner 拉取与执行

首版支持 CT executor。Scheduler 不读取脚本，调度规则不变；其他 executor 收到带引用的 Job 时明确返回不支持，不将正文当作宿主机命令执行。

Runner 接受已绑定给自己的 Job 后、启动业务容器前：

1. 经 Gateway GET `/apis/ebs/v1/scripts/{name}`，获取引用指定的 Script 当前内容。
2. 校验响应 GVK、name、UID、namespace 为空和内容限制（包括 shebang）。任一不符均禁止执行；resourceVersion 与创建 Job 时不同不构成错误。
3. 在该 Job 的工作目录安全写入 `build-script.sh`，使用临时文件后原子替换，拒绝符号链接目标；与 payload 分开保存，并设置可读、可执行权限（`0555`）。脚本按只读文件挂载为 `/workspace/build-script.sh`，不得被其他挂载覆盖。
4. 容器入口明确设置为 `/workspace/build-script.sh`，清空镜像默认 CMD 参数，执行时按 shebang 启动容器内的解释器；不通过宿主机解释或执行脚本。需要覆盖镜像 ENTRYPOINT，而不只是向镜像追加 CMD。引用与 `runtimeSpec.command/args` 同时设置时视为配置错误，不静默选择其中一个。
5. 脚本读取 `/workspace/payload.yaml`，按现有约定输出产物、日志和退出码；沿用 Job 超时、中止、产物上传及清理流程。

拉取和重试消耗 Job 既有执行期限，不重新计算超时；等待期间响应中止。启动容器前仍检查取消及 Job 绑定状态。拉取本身不新增 Job phase/stage。

暂时不引入跨 Job 磁盘缓存，每个执行尝试重新 GET 并校验；成功拉取后本次尝试不再次刷新内容。不能仅凭 UID 或名称复用旧内容，否则会掩盖同一对象的更新。

| 场景 | Runner 行为 |
|------|-------------|
| 网络失败、超时、429、5xx | 在 Job 剩余期限内指数退避重试，初始 1 秒、上限 30 秒，加抖动；超时后按现有 Job 超时规则结束 |
| 404、401/403、响应格式错误 | 明确失败并记录错误，不回退到镜像入口或其他脚本 |
| UID 不匹配 | 明确失败，不执行同名重建对象，不刷新引用 |
| shebang 指定的解释器缺失、脚本无法执行或退出非零 | 按现有执行失败规则处理，不自动回退到其他解释器 |
| 中止或执行 context 取消 | 停止拉取/执行，沿用现有中止流程，不继续重试 |

日志记录 Job 标识、实际拉取的脚本 name/UID/resourceVersion、正文 SHA-256 和失败原因，不打印正文。摘要仅用于排查实际执行内容，不作为 Job 的版本约束，也不提供历史内容恢复能力。无 `scriptRef` 的旧 Job 保持原执行方式；新构建 Job 接入该能力后由创建方必填，不隐式降级。

### 4.5 权限

Gateway 负责身份与操作权限校验，Script 访问不按 Project 成员关系过滤；apiserver 负责对象和引用校验。

| 身份 | 权限 |
|------|------|
| Ops、Admin、System | 创建、读取、列表、更新全局脚本正文和允许的 metadata |
| 普通登录用户 | 读取、列表全局脚本，不能写脚本；修改 Project 的脚本选择仍遵循 Project 更新权限 |
| BuildInfo Controller 内部服务身份 | 读取全局脚本，以便创建 Job |
| Runner 机器身份 | 允许按名称 GET 全局脚本；不允许列表或写入 |
| Scheduler | 无新增权限 |

全局脚本可供登录用户和构建执行节点读取，不存放 Project 私有数据或凭据。读取脚本不授权执行任意 Job，Job 执行仍受已有绑定检查约束；匿名访问不开放。

### 4.6 初始化与落地范围

apiserver 通过可选参数 `--default-script-file=/path/to/script.yaml` 加载一个全局 Script 清单；未指定时不自动创建脚本，不内置示例脚本。清单名称应与 Controller 的 `--default-script-name` 对应。初始化在服务就绪前完成，仅在对象不存在时创建，已存在时不覆盖运维修改；升级脚本通过 PUT/PATCH 更新原对象完成。真实入口需与构建镜像的工具及 payload 契约匹配，不能直接把 4.2 的示例当作可用默认脚本。

后续实现涉及：

1. 公共 API、deepcopy、OpenAPI、ES 索引、REST storage、内容校验和更新冲突检查；同步数据模型。
2. Gateway 权限和 ebsctl 资源映射；运维可先通过 YAML 管理，不要求首版提供前端脚本编辑器。
3. Build Controller 复制名称选择，BuildInfo Controller 解析并固定 Job 引用；配置读取错误沿用控制器写错误/重试分类，不误判为已执行构建失败。
4. Runner 客户端拉取、内容验证、安全落盘、CT 显式 ENTRYPOINT 和取消处理；同步 Runner 设计。
5. 测试集群级路由和 namespace 拒绝、默认名称选择及指定名称缺失、脚本原地更新及冲突、Job 引用不可变、更新前后拉取与本地副本的生效边界、权限、UID 不匹配、超时中止和旧 Job 执行兼容。

当前已实现 Script 公共类型、apiserver 接口、ES 存储、校验、可选文件初始化和 Gateway 权限。scriptRef 字段、Controller 选择及 Runner 拉取执行仍为设计约定，未接入。

## 5. 创建 Job 时的组合

接入 Script 后，BuildInfo Controller 创建新 Job 时：

1. 依据 Build 的 OS、Arch 从 BuildConf 当前快照解析镜像，见 2.5。
2. 选择 Project BuildResource，只有对象不存在才回退 `default/default`；在选定表内按默认值、spec 软件包、架构逐字段解析，见 3.5。
3. 选择 Script 对象并生成引用，见 4.3。
4. 三部分解析都成功后，将镜像、资源需求、脚本引用和 payload 写入同一个 Job 创建请求；任一配置不可用时不派发该新 Job。
5. 已存在 Job 沿用，不因配置更新重写；创建结果未知时先确认原写入意图。

三类配置独立更新，不提供跨对象原子快照，也不承诺同一 Build 的全部 Job 使用相同配置版本。镜像、资源需求和脚本对象引用在创建 Job 时固定；脚本正文在 Runner 执行前拉取，更新生效边界见 4.1。具体权限、初始化和错误规则以各资源章节为准。
