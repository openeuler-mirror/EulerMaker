# Script 数据模型与使用约定

## 1. 职责与更新模型

Script 管理可执行脚本；`Config/build-target` 管理镜像，`Config/build-resource` 管理资源需求。`Project.spec.buildPayload`、`BuildInfo.spec.buildPayload` 和 `Job.spec.payload` 继续传递任务参数，不改成脚本引用，也不删除。对于 CT 构建，Runner 仍将 payload 写成 `/workspace/payload.yaml`，由脚本读取。

Script 的 spec 只包含 `content`，支持原地修改，通过 `resourceVersion` 防止并发覆盖，不另设 revision 对象或历史内容存储。Job 引用脚本对象，不固定其内容版本；Runner 每次执行尝试使用 GET 时获取的当前内容。

尚未拉取脚本的 Job 可以使用更新后的内容；已经成功拉取的执行尝试使用本地副本，不热更新。接受同一 Build 的不同 Job，以及同一 Job 的不同执行尝试，可能使用不同内容，不承诺历史脚本可重放。

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

## 3. 选择与 Job 引用

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

默认名称的配置切换只影响后续新 Job，已有 Job 仍引用原对象；全局脚本内容更新可影响所有引用它的 Project，按第 1 节的拉取边界生效。

## 4. Runner 拉取与执行

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

## 5. 权限

Gateway 负责身份与操作权限校验，Script 访问不按 Project 成员关系过滤；apiserver 负责对象和引用校验。

| 身份 | 权限 |
|------|------|
| Ops、Admin、System | 创建、读取、列表、更新全局脚本正文和允许的 metadata |
| 普通登录用户 | 读取、列表全局脚本，不能写脚本；修改 Project 的脚本选择仍遵循 Project 更新权限 |
| BuildInfo Controller 内部服务身份 | 读取全局脚本，以便创建 Job |
| Runner 机器身份 | 允许按名称 GET 全局脚本；不允许列表或写入 |
| Scheduler | 无新增权限 |

全局脚本可供登录用户和构建执行节点读取，不存放 Project 私有数据或凭据。读取脚本不授权执行任意 Job，Job 执行仍受已有绑定检查约束；匿名访问不开放。

## 6. 初始化与落地范围

apiserver 通过可选参数 `--default-script-file=/path/to/script.yaml` 加载一个全局 Script 清单；未指定时不自动创建脚本，不内置示例脚本。清单名称应与 Controller 的 `--default-script-name` 对应。初始化在服务就绪前完成，仅在对象不存在时创建，已存在时不覆盖运维修改；升级脚本通过 PUT/PATCH 更新原对象完成。真实入口需与构建镜像的工具及 payload 契约匹配，不能直接把第 2 节的示例当作可用默认脚本。

当前已实现 Script 公共类型、deepcopy/OpenAPI、apiserver 接口、ES 存储、内容校验、更新冲突检查、可选文件初始化、Gateway 权限及前端运维脚本管理。

后续接入范围：

1. ebsctl 增加 Script 资源映射。
2. 增加 scriptRef 字段，由 Build Controller 复制名称选择，BuildInfo Controller 解析并固定 Job 引用；配置读取错误沿用控制器写错误/重试分类，不误判为已执行构建失败。
3. Runner 客户端拉取、内容验证、安全落盘、CT 显式 ENTRYPOINT 和取消处理；同步 Runner 设计。
4. 补齐默认名称选择及指定名称缺失、Job 引用不可变、更新前后拉取与本地副本的生效边界、UID 不匹配、超时中止和旧 Job 执行兼容测试。

## 7. 创建 Job 时的组合

接入 Script 后，BuildInfo Controller 创建新 Job 时：

1. 依据 Build 的 OS、Arch 从 `Config/build-target` 当前内容解析镜像，见 [Config 构建流程](data-models~config.md#25-构建流程)。
2. 读取 `Config/build-resource`，按表级默认值、spec 软件包、架构逐字段解析，见 [Config 构建资源内容](data-models~config.md#3-构建资源内容)。
3. 选择 Script 对象并生成引用，见第 3 节。
4. 三部分解析都成功后，将镜像、资源需求、脚本引用和 payload 写入同一个 Job 创建请求；任一配置不可用时不派发该新 Job。
5. 已存在 Job 沿用，不因配置更新重写；创建结果未知时先确认原写入意图。

三类配置独立更新，不提供跨对象原子快照，也不承诺同一 Build 的全部 Job 使用相同配置版本。镜像、资源需求和脚本对象引用在创建 Job 时固定；脚本正文在 Runner 执行前拉取，更新生效边界见第 1 节。具体权限、初始化和错误规则以各资源文档为准。
