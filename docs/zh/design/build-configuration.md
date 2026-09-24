# 构建配置设计

## 1. 总体职责

本文统一维护集群级 `Config` 和 `Script` 的设计。`Config` 以 `spec.content` 保存 YAML 文本，由使用方按对象名称解析；它不取代 `Script`。BuildInfo Controller 在创建 Job 时选择配置，Scheduler 和 Runner 使用 Job 中固化的结果。以下为目标设计，现有 `BuildConf`、`BuildResourceConfig` 代码尚待迁移。

| 对象 | 配置内容 | 作用域 | Job 中的结果 |
|------|----------|--------|-------------|
| `Config/build-target` | 支持的 OS、Arch 和构建镜像 | 集群级，`Public` | `spec.runtimeSpec.image` |
| `Config/build-resource` | spec 软件包及架构的 CPU、内存规则 | 集群级，`OpsOnly` | `spec.resources` |
| Script | 构建脚本内容 | 集群级，所有 Project 共用 | `spec.scriptRef`，Runner 按固定引用拉取 |

`Config` 对象只验证通用元数据、可见性和内容大小，不在 apiserver 解码业务内容。使用方必须解析、校验并明确处理非法内容；当前两份配置的业务结构见第 2、3 章，公共资源字段见 [数据模型](data-models.md)。

- [构建目标内容](#2-构建目标内容)
- [构建资源内容](#3-构建资源内容)
- [Script：构建脚本](#4-script构建脚本)
- [创建 Job 时的组合](#5-创建-job-时的组合)

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

## 4. Script：构建脚本

### 4.1 职责与更新模型

Script 管理可执行脚本；`Config/build-target` 管理镜像，`Config/build-resource` 管理资源需求。`Project.spec.buildPayload`、`BuildInfo.spec.buildPayload` 和 `Job.spec.payload` 继续传递任务参数，不改成脚本引用，也不删除。对于 CT 构建，Runner 仍将 payload 写成 `/workspace/payload.yaml`，由脚本读取。

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

当前已实现 Script 公共类型、deepcopy/OpenAPI、apiserver 接口、ES 存储、内容校验、更新冲突检查、可选文件初始化、Gateway 权限及前端运维脚本管理。

后续接入范围：

1. ebsctl 增加 Script 资源映射。
2. 增加 scriptRef 字段，由 Build Controller 复制名称选择，BuildInfo Controller 解析并固定 Job 引用；配置读取错误沿用控制器写错误/重试分类，不误判为已执行构建失败。
3. Runner 客户端拉取、内容验证、安全落盘、CT 显式 ENTRYPOINT 和取消处理；同步 Runner 设计。
4. 补齐默认名称选择及指定名称缺失、Job 引用不可变、更新前后拉取与本地副本的生效边界、UID 不匹配、超时中止和旧 Job 执行兼容测试。

## 5. 创建 Job 时的组合

接入 Script 后，BuildInfo Controller 创建新 Job 时：

1. 依据 Build 的 OS、Arch 从 `Config/build-target` 当前内容解析镜像，见 2.5。
2. 读取 `Config/build-resource`，按表级默认值、spec 软件包、架构逐字段解析，见第 3 章。
3. 选择 Script 对象并生成引用，见 4.3。
4. 三部分解析都成功后，将镜像、资源需求、脚本引用和 payload 写入同一个 Job 创建请求；任一配置不可用时不派发该新 Job。
5. 已存在 Job 沿用，不因配置更新重写；创建结果未知时先确认原写入意图。

三类配置独立更新，不提供跨对象原子快照，也不承诺同一 Build 的全部 Job 使用相同配置版本。镜像、资源需求和脚本对象引用在创建 Job 时固定；脚本正文在 Runner 执行前拉取，更新生效边界见 4.1。具体权限、初始化和错误规则以各资源章节为准。
