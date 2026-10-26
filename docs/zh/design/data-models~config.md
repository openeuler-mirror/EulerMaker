# Config 数据模型与使用约定

## 1. 总体职责

本文描述集群级 `Config`。它以 `spec.content` 保存 YAML 文本，由使用方按对象名称解析。BuildInfo Controller 在创建 Job 时选择配置，Scheduler 和 Runner 使用 Job 中固化的结果。构建脚本另见 [Script 数据模型与使用约定](data-models~script.md)。

| 对象 | 配置内容 | 作用域 | Job 中的结果 |
|------|----------|--------|-------------|
| `Config/build-target` | 支持的 OS、Arch 和构建镜像 | 集群级，`Public` | `spec.runtimeSpec.image` |
| `Config/build-resource` | spec 软件包及架构的 CPU、内存规则 | 集群级，`OpsOnly` | `spec.resources` |
| `Config/services` | Artifact Manager 实例地址和 Git Server 管理地址 | 集群级，`OpsOnly` | 仅固化 Artifact 实例标识和 Git 逻辑克隆地址，不复制配置全文 |

`Config` 对象验证通用元数据、可见性和内容格式；apiserver 还在创建、更新 `Config/build-target`、`Config/build-resource` 和 `Config/services` 时解析并校验业务内容。使用方必须处理读取和解析错误；具体业务校验职责及配置结构见第 2～4 章，公共资源字段见 [数据模型](data-models.md)。

- [构建目标内容](#2-构建目标内容)
- [构建资源内容](#3-构建资源内容)
- [服务配置内容](#4-服务配置内容)

### 1.1 Config 公共资源与可见性

`Config` 是集群级 `ebs/v1` 资源，复数为 `configs`。对象名称在集群内唯一，不设置 namespace、status 或 Project 覆盖。内置对象为 `build-target`、`build-resource` 和 `services`，各自的 `spec.content` 是非空 UTF-8 YAML 文本；内容可以使用 YAML 兼容的 JSON 语法。`spec.visibility` 只允许 `Public` 和 `OpsOnly`：前者允许匿名及所有身份具名读取，后者仅允许 Ops/Admin 读取。

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

`Config` 只负责持久化原文、并发版本和访问控制，不携带业务类型字段，也不将 YAML 内容展开为独立 API 字段。对象名称决定使用方采用哪种解析器。Gateway 对具名 `GET/HEAD` 只读取一次 apiserver 对象，并基于同一份响应的 `visibility` 决定是否返回。匿名、普通登录用户和 Runner 身份不允许 `list`，也不能读取 `OpsOnly` 对象；仅 Ops/Admin 可 `list`、创建和修改。`visibility` 只能由 Ops/Admin 修改，Gateway 不接受客户端自行声明“本次请求公开”；apiserver 校验枚举和资源版本。

```text
GET/HEAD /apis/ebs/v1/configs
POST     /apis/ebs/v1/configs
GET/HEAD /apis/ebs/v1/configs/{name}
PUT      /apis/ebs/v1/configs/{name}
PATCH    /apis/ebs/v1/configs/{name}
```

上述 HEAD 由 Gateway 内部读取 Config 后响应；apiserver 通用资源路由本身不注册 HEAD。非运维身份只允许具名 `GET/HEAD` 且对象为 `Public`；集合 `GET/HEAD` 仅允许 Ops/Admin。三个内置对象均不可经 Gateway 删除。无 Watch 或 `/status`。`PUT/PATCH` 必须进行 `resourceVersion` 冲突校验，内容或可见性改变时递增 generation。通过 apiserver 内部 API 直接读取的控制器仍须验证对象名称和业务内容格式，不依赖 Gateway 过滤。

apiserver 校验名称、`visibility`、`content` 非空、UTF-8、无 NUL 及请求大小上限；对 `Config/build-target`、`Config/build-resource` 和 `Config/services` 分别执行第 2.2.1、3.2、4 章的内容校验。默认对象在 Ready 前以 create-only 语义初始化，已存在的不覆盖；首次模板分别设置相应可见性。其他 Config 的业务内容错误仍可能成功保存：使用方必须在消费时按对象名称解析和校验；解析失败不能使用空表或旧缓存静默继续。运维界面可在提交前本地预检，但其结果不能替代消费时校验。

## 2. 构建目标内容

### 2.1 目标与范围

`Config/build-target` 的 `spec.content` 提供 OS、架构和镜像映射，为前端和任务创建方提供同一份数据。

- 前端从配置生成目标 OS、Arch 下拉选项。
- BuildInfo Controller 根据 Build 的 OS、Arch 选择镜像，并写入 Job。
- Ops、Admin 可以通过 Gateway 修改配置，无需重启服务。

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

使用 1.1 节的 `/apis/ebs/v1/configs/build-target`。匿名具名读取仅在当前对象 `spec.visibility=Public` 时允许；写入仅允许 Ops/Admin。apiserver 在创建、更新时按 2.2.1 严格解析并校验 `content.targets`，拒绝未知字段、重复键和非法目标配置，返回 422。`PUT/PATCH` 冲突时运维客户端重新读取并提示合并，不能仅替换 resourceVersion 后盲目重放旧内容。

### 2.4 存储与初始化

使用 Elasticsearch，逻辑 alias 为 `ebs-configs`，首个物理索引为 `ebs-configs-v1`，文档 ID 为 `build-target`，沿用现有 alias 初始化机制。

复用现有通用 mapping，`content` 原样保存在 data 中，不新增 OS、Arch、image 的索引字段；客户端按名称 GET 对象，无需对内部配置做 ES 查询。资源不开放 status.phase/status.stage 过滤。

初始化模板 `defaults/config/build-target.yaml` 作为 `Config/build-target` 的内容随 apiserver 二进制嵌入。

启动时在 Ready 前：

1. 确保索引与 alias 存在，读取默认对象。
2. 对象存在则沿用，不把随版本发布的 YAML 覆盖到线上。
3. 对象不存在时校验初始化 YAML，并通过 create-only 写入创建。
4. 多实例创建冲突后读取获胜对象并继续；不覆盖。其他存储错误导致初始化失败，不能在未知结果下覆盖配置。

启动配置不是持续同步源。发布新版本不修改运维数据；导入生产映射由 Ops 使用 API 或前端完成。初始化空表不阻止 apiserver Ready，但新建 Build 会因目标未配置而被拒绝。

### 2.5 构建流程

#### 2.5.1 Project 与 Build

Project.spec.buildTargets 仍记录工程自己的构建目标。Project 创建/更新只做现有字段校验，不强制所有目标当前都在 `Config/build-target` 中，避免全局配置变更后工程无法进行其他编辑。

创建任意类型 Build 时，apiserver 在申请非 single 目标占用、写入 Build 之前读取 `Config/build-target`，反序列化后只检查 `spec.buildTarget.os/arch` 是否有对应映射，不重复校验整张目标表：

- 目标不存在：`422 Invalid`，错误定位到 `spec.buildTarget`，不创建 Build、不申请占用。
- 配置对象不存在、读取失败、响应类型错误或反序列化失败：`503 ServiceUnavailable`，不得当成用户输入错误或使用硬编码回退。
- 成功：继续已有 Project 目标、packages、并发占用等校验流程。

该检查不是锁定配置：Build 创建成功后配置仍可能变化，因此 Job 创建方仍须确认所选 OS/Arch 的镜像映射存在。

#### 2.5.2 BuildInfo Controller 创建 Job

每次需要创建新 Job 的 reconcile，读取一次最新 `Config/build-target`，反序列化其 `content`，不重复校验 OS、架构和镜像格式；该轮批量创建使用同一个内存快照，按所属 Build 的 OS、Arch 查询镜像。

镜像写入 `Job.spec.runtimeSpec.image`。该字段由配置解析结果确定，Job 模板不得覆盖它；runtimeSpec 其他字段及 `Config/build-resource` 解析按原规则执行。首版面向现有容器 rpmbuild Job，不把容器镜像解释为 VM 镜像。

读取失败、响应类型错误、反序列化失败或映射缺失时，不创建本轮的新 Job，输出结构化错误，按 controller 框架的错误分类及慢速重入策略等待配置恢复；不把配置问题直接写成构建失败。无需注册 Config watch。已有 Job 的观察、结果回收不依赖本轮配置可用性。

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

ebsctl 注册集群级 Config 的 get/list/create/replace/patch；`-p/-n` 不改变其路径。内置对象不允许 delete，watch 不支持。不导入镜像仓库凭据。

### 2.7 实现与验收

| 模块 | 修改范围 |
|------|----------|
| 公共 api | Config 类型、List、scheme/deepcopy 注册；业务内容结构作为使用方解析类型 |
| apiserver | Config storage、通用与内置构建配置内容校验、初始化、ES alias、OpenAPI、Build create 时检查目标映射 |
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

apiserver 创建、更新 `Config/build-resource` 时完整校验：

- 顶层 `default.requests` 必须同时包含 `cpu`、`memory`；`limits` 可缺省。
- 仅允许 `cpu`、`memory`，未知字段或资源键（如 `memroy`）报错，不静默忽略。
- spec 包名非空、符合现有 spec 名称规则；允许 `kernel:kernel-rt` 形式，每一段均须合法。
- 架构 key 满足 `^[a-z0-9][a-z0-9._-]{0,62}$`，与 Build Target/Runner 使用的名称精确匹配，不做别名转换。
- 每层 quantity 可解析且大于 0；合并后的 limits 不小于 requests。

以上规则仅用于具名 `Config/build-resource`，创建、更新时严格解析 YAML/JSON，拒绝未知字段、重复键和非法资源规则，返回 422。未知的其他 Config 名称可存储不同格式的 `content`。运维 UI 和 ebsctl 可提供保存前预检。BuildInfo Controller 信任 apiserver 保存的资源规则，只读取、反序列化并按继承规则合并，不重复校验包名、架构、quantity 或最终 requests/limits。新规则仅影响后续创建的 Job，已有 Job 的资源不回写。

### 3.3 读取与失败处理

BuildInfo Controller 在需要创建 Job 的 reconcile 中，GET `/apis/ebs/v1/configs/build-resource`，解析一次当前对象内容，本轮新 Job 共用同一解析结果。返回 404 时按 E-27 写入 BuildInfo 的 `ResourceConfigUnavailable=True` Condition（reason=`ResourceConfigMissing`），暂停新 Job 派发并于 5 秒后重新入队，保留各 spec 原有状态，不提前结束 BuildInfo；读取失败、响应类型/对象身份不符合契约或反序列化失败时，不以空规则/旧缓存继续，也不创建本轮新 Job，记录配置错误并按控制器错误退避策略重新入队。派发前成功读取配置后清除该 Condition 并继续派发；重复等待相同缺失配置不重复写入状态。已存在 Job 的观察和结果回收不受影响。

资源配置来源不写入 Job annotation；调度只依据 Job 中固化的 `spec.resources`。

### 3.4 初始化、更新与容量

apiserver 在 Ready 前以 create-only 方式确保 `Config/build-resource` 存在，初始化模板 `defaults/config/build-resource.yaml` 包含 4 CPU、8Gi 的表级默认值及软件包覆盖规则。已存在对象不覆盖；多实例创建冲突后读取获胜对象。启动模板不是持续同步源。初始化必须通过 Config API 的默认化、通用校验与资源规则校验，不能直接写 Elasticsearch；运维更新业务内容后不会被新版本部署覆盖。

更新整张表必须携带 `metadata.resourceVersion`；发生 409 时重新读取并按包名合并用户的修改，不得只替换版本重放旧对象。批量生成内容按包名稳定排序。规模达到数千至数万包时，应对最终序列化大小、请求上限及解析耗时做压测；BuildInfo Controller 可按 UID/resourceVersion 缓存解析结果，但每轮先读取当前对象，缓存失效或解析失败时必须停止新 Job 派发。首版不支持按软件包的独立 REST API 或存储分片。

`Config/build-resource` 仅允许 Ops/Admin 经 Gateway 读取或写入；BuildInfo Controller 使用内部身份直连 apiserver 读取。Gateway 不允许删除该内置对象，也不提供 Watch 或 `/status`。

## 4. 服务配置内容

### 4.1 对象与公共类型

`Config/services` 统一保存服务访问地址，固定 `visibility: OpsOnly`。按业务分为 `artifactManagers` 和 `gitServer` 两部分：前者支持多个实例，后者只配置一个服务入口，不增加 Git Server 实例选择或 Project annotation。

公共 `ebs/v1` 定义以下内容类型，apiserver、Controller Manager 和 Gateway 共用；这些结构只用于解析 `Config.spec.content`，不注册为独立 API 资源：

```go
const ServiceConfigName = "services"

type ServiceConfigContent struct {
    ArtifactManagers map[string]ServiceEndpoints `json:"artifactManagers" yaml:"artifactManagers"`
    GitServer        ServiceEndpoints            `json:"gitServer" yaml:"gitServer"`
}

type ServiceEndpoints struct {
    InternalURL       string `json:"internalURL" yaml:"internalURL"`
    RunnerInternalURL string `json:"runnerInternalURL,omitempty" yaml:"runnerInternalURL,omitempty"`
    RunnerExternalURL string `json:"runnerExternalURL,omitempty" yaml:"runnerExternalURL,omitempty"`
}
```

`ServiceEndpoints` 统一表示服务的控制面与 Runner 地址；`artifactManagers` 通过 map 表达多实例，`gitServer` 使用单个对象，JSON/YAML 字段保持不变。Runner 地址投影保留 `artifactManagers`、`gitServer` 两个同级字段，仅包含 Runner 地址，不返回 `internalURL`。

### 4.2 地址语义

| 字段 | 使用方与用途 |
|------|--------------|
| `artifactManagers.{instance}.internalURL` | Controller Manager 访问 Artifact Manager 的物化、发布接口和仓库元数据 |
| `artifactManagers.{instance}.runnerInternalURL` | 内网 Runner 上传日志和产物、访问构建仓库 |
| `artifactManagers.{instance}.runnerExternalURL` | 外网 Runner 上传日志和产物、访问构建仓库 |
| `gitServer.internalURL` | Snapshot Controller 调用同步、状态和提交解析接口；BuildInfo Controller 调用只读 `/command` |
| `gitServer.runnerInternalURL` | 内网 Runner 的源码克隆服务入口 |
| `gitServer.runnerExternalURL` | 外网 Git Server 服务入口；当前 Runner external 克隆使用 `origin_url`，不使用此字段 |

Git Server 的 Runner 地址填写 HTTP(S) 服务基础入口，不附加 `/git` 或仓库路径；例如 `http://git.internal:8084`，与 `clone_path: /git/atomgit.com/src-openeuler/gcc.git` 拼接后才是实际克隆地址。代理存在额外路径前缀时，将前缀保留在配置中。两个地址均可省略或为空，不回退到管理面的 `internalURL`。

`ebs.io/artifacts` annotation 及 `artifact://{instance}/...` 保持原语义。Project 创建或修改该 annotation 时，apiserver 检查 `artifactManagers` 中是否存在目标键；未设置时检查 `default`。其他 Project 更新不重新检查，不在 Project 中存放物理地址。

Git Server 返回与部署地址无关的 `clone_path`（`/git/...`）。Controller Manager 客户端加 `git-server://` 前缀，形成 `git-server:///git/...` 逻辑地址，固化到 Snapshot `cloneURL`，再进入 Job payload 的 `clone_url`；不使用 `gitServer.internalURL` 重写该逻辑地址。空 authority 表示当前唯一的 Git Server。Runner internal 模式使用 Git Server 内网地址解析逻辑地址；external 模式直接以 `origin_url` 替换本地 `clone_url`。两者均保留 `commit_id`，不修改 Snapshot 或 Job。

本对象只保存服务地址，不保存认证凭据、CA、私钥或请求超时。服务的监听参数和调用方的 TLS、超时、重试参数仍由部署配置管理。

### 4.3 准入校验与权限

apiserver 在创建和更新时通过 `ValidateConfig` 完整校验：

- `visibility` 必须为 `OpsOnly`，不能改为 `Public`。
- `artifactManagers` 必须非空，实例名符合 DNS1123 label 规则；所有实例的 `internalURL` 必填。不强制包含 `default`，使用该实例的 Project 仍需通过存在性检查。
- `gitServer` 和 `gitServer.internalURL` 必填。
- 所有非空地址必须是带主机名的绝对 HTTP(S) URL，显式端口为 1～65535，不包含凭据、query、fragment 或空白；允许路径前缀，拼接接口路径时必须保留前缀。Git Server 和各 Artifact Manager 的两个 Runner 地址均可省略或为空，内外网均允许 HTTP 或 HTTPS。
- YAML/JSON 严格解析，未知字段、重复键和任一服务非法均返回 `422 Invalid`，不部分保存。

Gateway 仅允许 Ops/Admin 读取、创建和修改原 Config，不允许删除；普通用户、匿名及 Runner 无直接读取权限。Gateway 的内置配置名称识别同步使用 `services`。完整业务校验归 apiserver，消费者只处理资源身份、类型、反序列化及实际使用项缺失等接口错误，不重复校验整张配置。

更新 `content` 仍需携带 `resourceVersion`。遇到冲突重新读取并合并运维修改，不得仅替换版本后覆盖其他服务配置。

### 4.4 默认对象

```yaml
apiVersion: ebs/v1
kind: Config
metadata:
  name: services
spec:
  visibility: OpsOnly
  content: |
    artifactManagers:
      default:
        internalURL: http://artifact-manager:8081
        runnerInternalURL: http://127.0.0.1:8081
        runnerExternalURL: ""
      art-01:
        internalURL: http://artifact-manager.art-01:8081
        runnerInternalURL: ""
        runnerExternalURL: ""
    gitServer:
      internalURL: http://git-server:8080
      runnerInternalURL: http://127.0.0.1:8084
      runnerExternalURL: ""
```

apiserver 在 Ready 前从内嵌 `defaults/config/services.yaml` 按 create-only 语义初始化，经同一 Config 校验后保存；已有对象不覆盖，多实例创建冲突后读取获胜对象。初始化不探测服务连通性，Git Server 或 Artifact Manager 尚未部署不阻止 apiserver Ready。

默认 `gitServer.internalURL` 适用于 Docker Compose 及与 Git Server 同命名空间的 Kubernetes 控制器；跨命名空间填写带命名空间的服务域名。默认 `127.0.0.1` 的 Runner 地址仅适用于与对应服务同机且采用宿主机网络的 Runner：Artifact Manager 使用宿主机端口 `8081`，Git Server 使用映射端口 `8084`。其他部署必须填写实际可达地址。修改服务地址通过 Ops API 或运维配置页面完成，不修改初始化模板来覆盖线上配置。

### 4.5 Controller Manager 消费与更新边界

Controller Manager 由公共 `pkg/serviceconfig` 负责读取 `Config/services` 和反序列化；`pkg/artifacturl` 只处理 Artifact 逻辑 URL，不再独立读取或解析专用 Config。

- 需要访问 Git Server 或 Artifact Manager 的 reconcile，按需读取一次最新 Config，本轮共享同一份不可变快照；不需要这些服务的状态回收、终态检查等路径不读配置。
- Snapshot 和 BuildInfo 使用 `gitServer.internalURL` 构造本轮 Git Server 客户端；RpmRepo 和 BuildInfo 从 `artifactManagers` 按实例取管理地址。同一 BuildInfo reconcile 同时访问两类服务时不重复 GET Config。
- 下一轮读取新版本后生效，无需重启 Controller Manager；不为 Config 注册 Watch。`--git-server-addr` 删除，超时、重试和同步状态缓存 TTL 参数保留；地址不再有命令行或硬编码回退。
- 共享的是配置提供者与客户端工厂，不能在并发 reconcile 之间修改同一个客户端的地址。本轮所有 Git 操作固定使用同一客户端。同步状态缓存必须按 Git Server 地址隔离，换地址后不能沿用旧服务的 `CheckSynced` 结果；只保留当前地址客户端及仍被在途调用持有的旧客户端，避免历次地址变更永久累积缓存。
- 可按 Config UID/resourceVersion 复用解析结果，但每轮仍读取当前版本。配置读取失败不使用旧缓存；请求结束后释放本轮快照引用，不缓存无限版本。
- 配置缺失、网络、超时、5xx 属于可重试错误；权限、对象身份/类型或反序列化契约错误返回 PermanentError，按框架规则等待后续轮询。均不作为包级同步失败计数，不直接写 Build、Snapshot 或 BuildInfo 的业务失败终态。具体远端业务请求错误仍遵循各控制器契约。

修改地址只改变后续请求的访问入口，不迁移服务数据。Git Server 地址切换要求新入口保留相同仓库路径并能访问已固化提交；Artifact Manager 同一实例改址必须仍能访问原仓库、Manifest 和上传数据。资源中已固化的逻辑克隆地址、commit 和逻辑实例标识不随配置更新重写。新增独立 Artifact 存储使用新实例键，不把原键直接指向空存储。

### 4.6 Runner 地址投影

Gateway 的 `GET /apis/ebs/v1/service-endpoints` 每次读取同一服务 Config，返回 `artifactManagers` 和 `gitServer`，各项仅包含 `runnerInternalURL`、`runnerExternalURL`，不返回任何管理地址。读取失败返回服务不可用，不返回旧缓存。该接口仍仅允许 Runner Token，不接受 Job、Project 或 Runner 名参数。

Runner 启动时读取投影，之后每 600 秒加抖动刷新。按 Job 的 `ebs.io/artifacts` annotation 和显式 `internal`/`external` 模式选择地址，运行中的 Job 保持已选地址。Artifact Manager 所选实例或地址缺失时等待配置恢复；internal 模式解析 `git-server:///` 时 Git 内网地址为空则明确失败；external 模式改用 payload 的 `origin_url`，该值缺失或无效时明确失败，不启动构建容器。均不回退到其他模式或 internalURL，详细源码处理规则见 [Runner 设计](runner.md)。

### 4.7 实现范围与验收

| 模块 | 调整范围 |
|------|----------|
| 公共 API | ServiceConfigContent、ServiceEndpoints、ServiceConfigName 及对应生成代码 |
| apiserver | 内置 Config 校验与初始化模板；Project annotation 改查 artifactManagers |
| Gateway | 内置配置名称识别；service-endpoints 投影两类服务的 Runner 地址 |
| Runner | 定时刷新服务地址，按模式解析 artifact://；internal 解析 git-server:///，external 使用 origin_url，写入本地 payload.json |
| Controller Manager | 公共配置提供者；两个 Git 消费者按轮取客户端；Artifact 地址解析读取新结构；删除 Git 地址参数 |
| 部署及运维文档 | 初始化 services、填写各访问地址；移除 Controller Manager 的 Git 地址参数 |

验收覆盖默认对象初始化与并发创建、更新不覆盖、两类服务字段校验、OpsOnly 保护、Project 实例检查、Runner 投影不包含管理地址、一轮多服务访问只读一次 Config、地址更新生效、在途调用地址固定、不同 Git Server 同步缓存隔离及配置异常不触发业务失败。部署模板不作为动态配置的覆盖源。
