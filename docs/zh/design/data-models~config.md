# Config 数据模型与使用约定

## 1. 总体职责

本文描述集群级 `Config`。它以 `spec.content` 保存 YAML 文本，由使用方按对象名称解析。BuildInfo Controller 在创建 Job 时选择配置，Scheduler 和 Runner 使用 Job 中固化的结果。构建脚本另见 [Script 数据模型与使用约定](data-models~script.md)。

| 对象 | 配置内容 | 作用域 | Job 中的结果 |
|------|----------|--------|-------------|
| `Config/build-target` | 支持的 OS、Arch 和构建镜像 | 集群级，`Public` | `spec.runtimeSpec.image` |
| `Config/build-resource` | spec 软件包及架构的 CPU、内存规则 | 集群级，`OpsOnly` | `spec.resources` |

`Config` 对象只验证通用元数据、可见性和内容大小，不在 apiserver 解码业务内容。使用方必须解析、校验并明确处理非法内容；当前两份配置的业务结构见第 2、3 章，公共资源字段见 [数据模型](data-models.md)。

- [构建目标内容](#2-构建目标内容)
- [构建资源内容](#3-构建资源内容)

### 1.1 Config 公共资源与可见性

`Config` 是集群级 `ebs/v1` 资源，复数为 `configs`。对象名称在集群内唯一，不设置 namespace、status 或 Project 覆盖。首批对象为 `build-target` 和 `build-resource`，各自的 `spec.content` 是非空 UTF-8 YAML 文本；内容可以使用 YAML 兼容的 JSON 语法。`spec.visibility` 只允许 `Public` 和 `OpsOnly`：前者允许匿名及所有身份具名读取，后者仅允许 Ops/Admin/System 读取。

```yaml
apiVersion: ebs/v1
kind: Config
metadata:
  name: build-target
spec:
  visibility: Public
  content: |
    targets: {}
```

`Config` 只负责持久化原文、并发版本和访问控制，不携带业务类型字段，也不将 YAML 内容展开为独立 API 字段。对象名称决定使用方采用哪种解析器。Gateway 对具名 `GET/HEAD` 只读取一次 apiserver 对象，并基于同一份响应的 `visibility` 决定是否返回。匿名、普通登录用户和 Runner 身份不允许 `list`，也不能读取 `OpsOnly` 对象；仅 Ops/Admin/System 可 `list`、创建和修改。`visibility` 只能由 Ops/Admin/System 修改，Gateway 不接受客户端自行声明“本次请求公开”；apiserver 校验枚举和资源版本。

```text
GET/HEAD /apis/ebs/v1/configs
POST     /apis/ebs/v1/configs
GET/HEAD /apis/ebs/v1/configs/{name}
PUT      /apis/ebs/v1/configs/{name}
PATCH    /apis/ebs/v1/configs/{name}
```

非运维身份只允许具名 `GET/HEAD` 且对象为 `Public`；集合 `GET/HEAD` 仅允许 Ops/Admin/System。两个内置对象均不可经 Gateway 删除。无 Watch 或 `/status`。`PUT/PATCH` 必须进行 `resourceVersion` 冲突校验，内容或可见性改变时递增 generation。通过 apiserver 内部 API 直接读取的控制器仍须验证对象名称和业务内容格式，不依赖 Gateway 过滤。

apiserver 仅校验名称、`visibility`、`content` 非空、UTF-8、无 NUL 及请求大小上限，不解析目标 OS、镜像或资源数量。默认对象在 Ready 前以 create-only 语义初始化，已存在的不覆盖；首次模板分别设 `Public` 与 `OpsOnly`。内容错误可能成功保存：Build 创建方、BuildInfo Controller 和前端必须在使用前解析并按第 2、3 章校验；解析失败不能使用空表或旧缓存静默继续。运维界面可在提交前本地预检，但其结果不能替代消费时校验。

## 2. 构建目标内容

### 2.1 目标与范围

`Config/build-target` 的 `spec.content` 提供 OS、架构和镜像映射，为前端和任务创建方提供同一份数据。

- 前端从配置生成目标 OS、Arch 下拉选项。
- BuildInfo Controller 根据 Build 的 OS、Arch 选择镜像，并写入 Job。
- Ops、Admin、System 可以通过 Gateway 修改配置，无需重启服务。

### 2.2 对象

对象的名称固定为 `build-target`，默认 `visibility: Public`。以下展示完整对象；业务结构从 `content` 中解码，不是 `Config.spec.targets`。

```yaml
apiVersion: ebs/v1
kind: Config
metadata:
  name: build-target
spec:
  visibility: Public
  content: |
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

解码后的内容结构见 [数据模型](data-models.md#内置内容格式)。不额外维护 supportedOS、supportedArch、镜像摘要或配置版本字段；版本复用 Config 的 metadata.resourceVersion / generation。

#### 2.2.1 校验与解释

| 字段 | 规则 |
|------|------|
| `content.targets` | OS 到目标配置的映射；允许空表，表示暂不接受新的构建目标 |
| OS key | 非空、无首尾空白，满足 Build 目标 OS 对应 label value 的语法，与 `BuildTarget.os` 精确匹配 |
| `arches` | 每个 OS 至少包含一个架构；选项仅来自所选 OS，不对全部 OS 的架构取并集 |
| Arch key | 使用现有 Build 目标架构校验，不写死 x86_64/aarch64，与 `BuildTarget.arch` 精确匹配 |
| `image` | 必填，合法的容器镜像引用，允许 tag 或 digest；禁止 URL scheme、内嵌凭据和空白；使用镜像引用解析库校验，不联网检查存在性 |

不自动转换 OS 大小写、冒号、连字符，也不从 OS 名称拼接镜像。运维显式维护映射。缺失条目没有默认镜像回退。

### 2.3 API 与权限

使用 1.1 节的 `/apis/ebs/v1/configs/build-target`。匿名具名读取仅在当前对象 `spec.visibility=Public` 时允许；写入仅允许 Ops/Admin/System。通用 `Config` 的校验不保证 `content.targets` 正确，使用方按 2.2.1 校验。`PUT/PATCH` 冲突时运维客户端重新读取并提示合并，不能仅替换 resourceVersion 后盲目重放旧内容。

### 2.4 存储与初始化

使用 Elasticsearch，逻辑 alias 为 `ebs-configs`，首个物理索引为 `ebs-configs-v1`，文档 ID 为 `build-target`，沿用现有 alias 初始化机制。

复用现有通用 mapping，`content` 原样保存在 data 中，不新增 OS、Arch、image 的索引字段；客户端按名称 GET 对象，无需对内部配置做 ES 查询。资源不开放 status.phase/status.stage 过滤。

初始化模板作为 `Config/build-target` 随 apiserver 二进制嵌入，内容可沿用现有默认目标映射；实施时替换现有 `default-build-conf.yaml`。

启动时在 Ready 前：

1. 确保索引与 alias 存在，读取默认对象。
2. 对象存在则沿用，不把随版本发布的 YAML 覆盖到线上。
3. 对象不存在时校验初始化 YAML，并通过 create-only 写入创建。
4. 多实例创建冲突后读取获胜对象并继续；不覆盖。其他存储错误导致初始化失败，不能在未知结果下覆盖配置。

启动配置不是持续同步源。发布新版本不修改运维数据；导入生产映射由 Ops 使用 API 或前端完成。初始化空表不阻止 apiserver Ready，但新建 Build 会因目标未配置而被拒绝。

### 2.5 构建流程

#### 2.5.1 Project 与 Build

Project.spec.buildTargets 仍记录工程自己的构建目标。Project 创建/更新只做现有字段校验，不强制所有目标当前都在 `Config/build-target` 中，避免全局配置变更后工程无法进行其他编辑。

创建任意类型 Build 时，apiserver 在申请非 single 目标占用、写入 Build 之前读取 `Config/build-target`，解析并校验 `spec.buildTarget.os/arch` 是否有对应映射：

- 目标不存在：`422 Invalid`，错误定位到 `spec.buildTarget`，不创建 Build、不申请占用。
- 配置对象不存在、读取失败或内容无效：`503 ServiceUnavailable`，不得当成用户输入错误或使用硬编码回退。
- 成功：继续已有 Project 目标、packages、并发占用等校验流程。

该检查不是锁定配置：Build 创建成功后配置仍可能变化，因此 Job 创建方必须再次检查。

#### 2.5.2 BuildInfo Controller 创建 Job

每次需要创建新 Job 的 reconcile，读取一次最新 `Config/build-target`，解析并校验其 `content`；该轮批量创建使用同一个内存快照，按所属 Build 的 OS、Arch 查询镜像。

镜像写入 `Job.spec.runtimeSpec.image`。该字段由配置解析结果确定，Job 模板不得覆盖它；runtimeSpec 其他字段及 `Config/build-resource` 解析按原规则执行。首版面向现有容器 rpmbuild Job，不把容器镜像解释为 VM 镜像。

读取失败、内容无效或映射缺失时，不创建本轮的新 Job，输出结构化错误，按 controller 框架的错误分类及慢速重入策略等待配置恢复；不把配置问题直接写成构建失败。无需注册 Config watch。已有 Job 的观察、结果回收不依赖本轮配置可用性。

Job 创建沿用确定性名称及幂等流程：已存在的同一任务 Job 直接沿用，不能因配置改变更新其镜像；Create 返回结果未知时先确认原请求的写入意图，不能读取新配置后用不同镜像盲目重试同一次创建。

#### 2.5.3 生效边界

首版采用**创建 Job 时取配置**，不在 Build 或 BuildInfo 中额外保存镜像快照：

- 已创建的 Pending/Running Job 均继续使用自身固化的镜像。
- 修改或移除配置会影响之后创建的 Job，包括正在进行的 Build 中尚未创建的 Job。
- 同一 Build 可以包含使用不同镜像的 Job；不承诺 Build 级环境快照一致性。
- Runner 只执行 Job.spec.runtimeSpec，不读取 Config。
- tag 可被镜像仓库重新指向，Job 固化 tag 不等于固化镜像内容；需要内容可复现时由运维填写 digest 引用。

这也是删除某个 OS/Arch 的语义：禁止后续接收该目标 Build，并暂停该目标尚未创建 Job 的派发，不删除或中止已有任务。恢复映射后重新调和即可继续。

### 2.6 前端与 ebsctl

“运维管理”新增“构建配置”入口，Ops 及以上支持按 OS/Arch 增删行、填写镜像，并支持完整 spec 的 YAML/JSON 编辑。两种模式共用同一份草稿，切换不得丢字段；保存携带原 resourceVersion。删除目标前提示对进行中 Build 的影响。

工程创建及工程目标编辑页面：

- 从 `Config/build-target.spec.content` 解析 OS 选项，选中 OS 后展示其 arches；选项按名称排序，不维护第二份前端列表。
- 新行默认 OS 未选择；切换 OS 后，原 Arch 不受支持则清空，要求重新选择。
- 空配置显示“暂无可用构建目标”；配置请求失败显示错误并支持重试，不回退到硬编码选项。
- 旧工程不受支持的目标仍显示原值并标注“不再支持”；不静默删除或替换。未修改的旧目标不阻止保存其他工程配置。
- 发起 Build 时，对不支持的目标禁用提交并给出提示；最终仍以 apiserver 校验为准。

ebsctl 注册集群级 Config 的 get/list/create/replace/patch；`-p/-n` 不改变其路径。两个内置对象不允许 delete，watch 不支持。不导入镜像仓库凭据。

### 2.7 实现与验收

| 模块 | 修改范围 |
|------|----------|
| 公共 api | Config 类型、List、scheme/deepcopy 注册；业务内容结构作为使用方解析类型 |
| apiserver | Config storage、通用校验、初始化、ES alias、OpenAPI、Build create 时解析目标内容 |
| gateway | 按 visibility 授权具名读取、禁止匿名 list、Ops 及以上写权限 |
| controller-manager | 读取 Config 并解析对应 content，创建 Job 时固化镜像 |
| frontend | 运维配置编辑、OS/Arch 选项、不可用目标提示 |
| ebsctl | 集群级资源与允许操作注册 |

实现时使用现有可重复执行的代码生成脚本，不手工维护独立 OpenAPI 文件。

验收至少覆盖：合法/非法镜像和目标、空配置、两架构不同镜像、无权限写入、更新冲突、多实例初始化不覆盖、未配置目标创建 Build 不留下占用、配置读取失败、批量 Job 使用同一配置快照、Job 创建未知结果确认、配置更新不修改已有 Job、删除目标后新任务停发及恢复、前端旧目标保留和请求失败提示。

## 3. 构建资源内容

### 3.1 对象与内容格式

`Config/build-resource` 是全部 Project 共用的资源规则表，默认 `visibility: OpsOnly`。BuildInfo Controller 在创建 Job 时读取该对象，以 spec 包名和架构解析资源，并固化到 `Job.spec.resources`；Scheduler、Runner 不读取 Config。包名指 spec 包名（如 `gcc`），不是二进制 RPM 子包名。规则与目标 OS 无关。

```yaml
apiVersion: ebs/v1
kind: Config
metadata:
  name: build-resource
spec:
  visibility: OpsOnly
  content: |
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
```

`default`、`packages` 和 `arches` 是 `content` 内部的 YAML 字段，不是 Config API 的独立字段。`packages` 可以为空，表示所有包使用表级默认值；包和架构使用 map，避免重复键。解析器必须检测 YAML 重复键，不能因后值覆盖前值而静默接受错误规则。软件包项可仅配置 `default` 或一个及以上 `arches`。

### 3.2 匹配与校验

按 `content.default` → `content.packages[specName].default` → `content.packages[specName].arches[arch]` 逐字段覆盖 `requests` 和 `limits` 的 `cpu`、`memory`。同一级声明 request 而未声明对应 limit 时，limit 取该级 request；该级没有声明该项 request 时，request 和 limit 均继承上一级。最终有效结果必须包含 CPU、内存的 requests 与 limits，且每项 limit ≥ request。资源单位使用现有 Kubernetes quantity 语义，数值须大于 0。

消费方解析内容时校验：

- 顶层 `default.requests` 必须同时包含 `cpu`、`memory`；`limits` 可缺省。
- 仅允许 `cpu`、`memory`，未知字段或资源键（如 `memroy`）报错，不静默忽略。
- spec 包名非空、符合现有 spec 名称规则；允许 `kernel:kernel-rt` 形式，每一段均须合法。
- 架构 key 满足 `^[a-z0-9][a-z0-9._-]{0,62}$`，与 Build Target/Runner 使用的名称精确匹配，不做别名转换。
- 每层 quantity 可解析且大于 0；合并后的 limits 不小于 requests。

以上是 **`build-resource` 内容解析器**的规则，不是 apiserver 对所有 Config 的创建/更新校验。未知的其他 Config 名称可存储不同格式的 `content`。为避免错误内容已保存却影响 Job 派发，运维 UI 和 ebsctl 可提供保存前预检，但消费方仍必须独立校验。新规则仅影响后续创建的 Job，已有 Job 的资源不回写。

### 3.3 读取与失败处理

BuildInfo Controller 在需要创建 Job 的 reconcile 中，GET `/apis/ebs/v1/configs/build-resource`，解析一次当前对象内容，本轮新 Job 共用同一解析结果。返回 404 时沿用现有 E-27 契约，将当前 spec 标为 `Failed`（`DefaultBuildResourceConfigNotFound`），不创建 Job；读取失败、内容非法或无法得到有效资源时，不以空规则/旧缓存继续，也不创建本轮新 Job，记录配置错误并按控制器错误退避策略重新入队。配置恢复后继续派发。已存在 Job 的观察和结果回收不受影响。

Job annotation `ebs.io/build-resource-config` 记录 `build-resource`，`ebs.io/build-resource-config-generation` 记录本次解析对象的 generation，供审计；调度仍只依据 Job 中固化的资源值。

### 3.4 初始化、更新与容量

apiserver 在 Ready 前以 create-only 方式确保 `Config/build-resource` 存在，初始化内容至少为 4 CPU、8Gi 的表级默认值和空 `packages`。已存在对象不覆盖；多实例创建冲突后读取获胜对象。启动模板不是持续同步源。初始化必须通过 Config API 的默认化与通用校验，不能直接写 Elasticsearch；运维更新业务内容后不会被新版本部署覆盖。

更新整张表必须携带 `metadata.resourceVersion`；发生 409 时重新读取并按包名合并用户的修改，不得只替换版本重放旧对象。批量生成内容按包名稳定排序。规模达到数千至数万包时，应对最终序列化大小、请求上限及解析耗时做压测；BuildInfo Controller 可按 UID/resourceVersion 缓存已验证的解析结果，但每轮先读取当前对象，缓存失效或解析失败时必须停止新 Job 派发。首版不支持按软件包的独立 REST API 或存储分片。

`Config/build-resource` 仅允许 Ops/Admin/System 经 Gateway 读取或写入；BuildInfo Controller 使用内部身份直连 apiserver 读取。Gateway 不允许删除该内置对象，也不提供 Watch 或 `/status`。
