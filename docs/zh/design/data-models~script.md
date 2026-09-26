# Script 数据模型与使用约定

## 1. 职责与更新模型

Script 管理可执行脚本；`Config/build-target` 管理镜像，`Config/build-resource` 管理资源需求。`Project.spec.buildPayload` 和 `BuildInfo.spec.buildPayload` 中的 `rpmbuild_script` 选择全局脚本；Job 通过 `spec.scriptRefs` 记录创建时观察到的 Script 元数据。对于 CT 构建，Runner 仍将 Job payload 写成 `/workspace/payload.yaml`，由脚本读取。

Script 的 spec 只包含 `content`，支持原地修改，通过 `resourceVersion` 防止并发覆盖，不另设 revision 对象或历史内容存储。Job 记录创建时观察到的名称、UID 和 resourceVersion，但不固定内容版本。Runner 缓存匹配该观测值时复用内容；不匹配时 GET 当前 Script。

缓存未命中时，尚未拉取脚本的 Job 可以使用更新后的内容；缓存命中时复用先前拉取的内容。已经启动的执行尝试使用本地副本，不热更新。同一 Build 的不同 Job，以及同一 Job 的不同执行尝试，可能使用不同内容，不承诺历史脚本可重放。

## 2. 资源与 API

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

## 3. 选择与 Job payload

Project 可在 `spec.buildPayload` 中配置 `rpmbuild_script: custom-script`，只表示全局脚本名称，不定义 Project 私有脚本。Build Controller 创建 BuildInfo 时原样复制 `buildPayload`；Project 后续修改不影响已创建 BuildInfo。

BuildInfo Controller 创建新 Job 时解析 `BuildInfo.spec.buildPayload`：未配置或配置空字符串时固定使用全局 `rpmbuild` 脚本；配置为非字符串、非法 DNS subdomain 名称或 YAML 无法解析时返回配置错误，不静默回退。同一轮 reconcile 首次需要创建 Job 时 GET 对应 Script，并将响应的 name、UID、resourceVersion 复用于本轮后续新 Job 的 `spec.scriptRefs`（当前写入一个元素）；下一轮重新读取。读取失败不创建 Job，确认已存在 Job 时不需要读取 Script。`rpmbuild_script` 是控制器配置，不再写入 Job payload，避免出现两处名称。已创建 Job 的名称不因 Project 配置变化而改变；脚本内容乃至同名对象本身可以在执行前变化。

```yaml
spec:
  runtime: ct
  scriptRefs:
    - name: rpmbuild
      uid: 61304b92-72cf-4a41-8bf7-8e0a9d14f6a5
      resourceVersion: "v1:12:3"
```

`scriptRefs` 是创建时的观测记录，不是内容锁定。数组为空时使用镜像入口；非空时按顺序拉取所有脚本，第一项为主脚本和容器入口，其余脚本供主脚本调用。脚本名称不能重复。Runner 缓存未命中后 GET Script，不要求响应的 UID 或 resourceVersion 与观测值相同。同名 Script 删除后重建也可被尚未拉取脚本的 Job 使用。

## 4. Runner 拉取与执行

首版支持 CT executor。Scheduler 不读取脚本，调度规则不变；其他 executor 收到要求执行脚本的 Job 时明确返回不支持，不将正文当作宿主机命令执行。

Runner 接受已绑定给自己的 Job 后、启动业务容器前：

1. `scriptRefs` 为空时不拉取脚本，沿用镜像入口；非空时依次以每个引用的 name、UID、resourceVersion 检查 Runner 进程内缓存。同名缓存条目的 UID 和 resourceVersion 均匹配时复用，未命中时经 Gateway GET `/apis/ebs/v1/scripts/{name}` 获取当前内容。
2. 校验响应 GVK、name、namespace 为空、UID、resourceVersion 和内容限制（包括 shebang）。任一不符均禁止执行；响应的 UID/resourceVersion 可以与 Job 中的观测值不同。缓存使用实际响应元数据，下次遇到旧观测值会再次 GET。
3. 在该 Job 的工作目录下，将每个脚本写入 `scripts/{name}`，使用临时文件后原子替换；与 payload 分开保存，并设置 `0555` 权限。工作目录挂载于 `/workspace`，脚本路径为 `/workspace/scripts/{name}`，拒绝覆盖该目录的额外挂载。所有脚本成功拉取后才启动容器。
4. 容器入口明确设置为 `/workspace/scripts/{scriptRefs[0].name}`，清空镜像默认 CMD 参数，执行时按 shebang 启动容器内的解释器；不通过宿主机解释或执行脚本。需要覆盖镜像 ENTRYPOINT，而不只是向镜像追加 CMD。非空脚本数组与 `runtimeSpec.command/args` 同时设置时视为配置错误，不静默选择其中一个。
5. 脚本读取 `/workspace/payload.yaml`，按现有约定输出产物、日志和退出码；沿用 Job 超时、中止、产物上传及清理流程。

拉取和重试消耗 Job 既有执行期限，不重新计算超时；等待期间响应中止。启动容器前仍检查取消及 Job 绑定状态。拉取本身不新增 Job phase/stage。

缓存仅存在于 Runner 进程内，不持久化到磁盘；重启后重新 GET。相同 Script 名称但 Job 指定的 UID 或 resourceVersion 改变时重新 GET。成功拉取后本次执行不再刷新内容。

| 场景 | Runner 行为 |
|------|-------------|
| 网络失败、超时、429、5xx | 在 Job 剩余期限内指数退避重试，初始 1 秒、上限 30 秒，加抖动；超时后按现有 Job 超时规则结束 |
| 404、401/403、响应格式错误 | 明确失败并记录错误，不回退到镜像入口或其他脚本 |
| shebang 指定的解释器缺失、脚本无法执行或退出非零 | 按现有执行失败规则处理，不自动回退到其他解释器 |
| 中止或执行 context 取消 | 停止拉取/执行，沿用现有中止流程，不继续重试 |

拉取或校验失败时通过现有 Job 执行错误记录原因，不打印脚本正文。缓存不提供历史内容恢复能力。BuildInfo Controller 创建的构建 Job 写入一个脚本引用；其他 Job 可以使用空数组并沿用镜像入口。

## 5. 权限

Gateway 负责身份与操作权限校验，Script 访问不按 Project 成员关系过滤；apiserver 负责 Script 对象校验。

| 身份 | 权限 |
|------|------|
| Ops、Admin、System | 创建、读取、列表、更新全局脚本正文和允许的 metadata |
| 普通登录用户 | 读取、列表全局脚本，不能写脚本；修改 Project 的脚本选择仍遵循 Project 更新权限 |
| BuildInfo Controller 内部服务身份 | 按名称读取全局脚本，并将其元数据写入 Job |
| Runner 机器身份 | 允许按名称 GET 全局脚本；不允许列表或写入 |
| Scheduler | 无新增权限 |

全局脚本可供登录用户和构建执行节点读取，不存放 Project 私有数据或凭据。读取脚本不授权执行任意 Job，Job 执行仍受已有绑定检查约束；匿名访问不开放。

## 6. 初始化与落地范围

apiserver 通过可选参数 `--default-script-file=/path/to/script.yaml` 加载一个全局 Script 清单；未指定时不自动创建脚本，不内置示例脚本。未配置 `rpmbuild_script` 的构建需要存在名为 `rpmbuild` 的全局 Script。初始化在服务就绪前完成，仅在对象不存在时创建，已存在时不覆盖运维修改；升级脚本通过 PUT/PATCH 更新原对象完成。真实入口需与构建镜像的工具及 payload 契约匹配，不能直接把第 2 节的示例当作可用默认脚本。

当前已实现 Script 公共类型、deepcopy/OpenAPI、apiserver 接口、ES 存储、内容校验、更新冲突检查、可选文件初始化、Gateway 权限及前端运维脚本管理；BuildInfo Controller 已将创建时观察到的 Script 元数据写入新 Job 的 `scriptRefs`。Runner 已支持按该引用缓存或拉取脚本、校验内容并作为 CT 容器入口执行。

后续接入范围：

1. ebsctl 增加 Script 资源映射。
2. 补齐 Gateway HTTP 集成和异常中止场景测试。

## 7. 创建 Job 时的组合

接入 Script 后，BuildInfo Controller 创建新 Job 时：

1. 依据 Build 的 OS、Arch 从 `Config/build-target` 当前内容解析镜像，见 [Config 构建流程](data-models~config.md#25-构建流程)。
2. 读取 `Config/build-resource`，按表级默认值、spec 软件包、架构逐字段解析，见 [Config 构建资源内容](data-models~config.md#3-构建资源内容)。
3. 解析 `buildPayload.rpmbuild_script`，未配置时选用 `rpmbuild`，读取 Script 并构造单元素 `scriptRefs`，见第 3 节。
4. 配置解析成功后，将镜像、资源需求、脚本观测信息和 payload 写入同一个 Job 创建请求；解析或读取错误时不派发该新 Job。
5. 已存在 Job 沿用，不因配置更新重写；创建结果未知时先确认原写入意图。

镜像和资源配置独立更新，不提供跨对象原子快照，也不承诺同一 Build 的全部 Job 使用相同配置版本。镜像、资源需求和脚本观测信息在创建 Job 时记录；脚本正文在 Runner 执行前拉取，更新生效边界见第 1 节。具体权限、初始化和错误规则以各资源文档为准。
