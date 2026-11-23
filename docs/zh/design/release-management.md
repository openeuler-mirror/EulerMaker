# Release Management 软件包同步插件设计

## 1. 目标与范围

本文定义 `release-management` 独立插件。插件从发行版管理仓库提取软件包清单，经 Gateway 更新
`Project.spec.packageRepos`，不将发行版清单格式耦合到 EulerMaker 核心组件。
周期同步及 MachineAccount Service Token 接入已在 `backend/release-management` 实现。
GET Project 主动匿名读取，PATCH 使用机器凭据换取的短期 Token；Webhook 保持关闭。

插件内置 openEuler 清单解析逻辑，对接 [release-management 仓库](https://atomgit.com/openeuler/release-management)，
所有绑定使用相同解析规则，无需配置或选择解析器。
本文中的组件名指 EulerMaker 同步插件，“上游清单仓库”指该数据来源，两者独立部署和维护。
例如 `openEuler-24.03-LTS-Next:everything` 可以显式配置组合以下目录：

- `openEuler-22.03-LTS-Next/baseos`
- `openEuler-24.03-LTS-Next/everything-exclude-baseos`

组合关系由插件配置维护，不根据版本名称推测目录，也不要求目录所属版本相同。

首版范围：

- 单实例服务，先实现启动同步、周期校验和本地命令手动预览/触发。
- Webhook 默认关闭，真实投递契约确认后另行接入，不作为首版同步与恢复的前提。
- 一个绑定管理一个现有 Project 的完整软件包列表，不自动创建 Project。
- 同步仅影响工程配置，不自动创建 Build，不修改已创建的 Snapshot、BuildInfo、Job 或构建结果。
- 不新增 API 资源类型，不直接操作 ES/etcd，不在 Gateway 内拉取 Git、解析清单或运行同步队列。
- 不实现多副本选主、人工编辑与自动清单的三方合并、通用工作流引擎或源码镜像服务。

## 2. 组件边界与调用链

```text
启动同步 / 周期校验 / 手动触发（后续可接入 Webhook）
                         ↓
release-management：解析来源 ref → 固定 commit → 读取并合并清单 → 计算差异
                         ↓ HTTP(S)（GET 匿名；PATCH 携带 Service Token）
Gateway：机器账号身份核验、服务操作授权、PATCH 字段及候选对象检查
                         ↓ 现有内部连接
API Server：Project 校验、resourceVersion 并发控制、持久化
                         ↓
后续 Build 创建 Snapshot 时读取更新后的 Project
```

| 组件 | 职责 |
|------|------|
| release-management | 来源绑定、Git 同步、清单解析、列表归一、空列表保护、队列与重试、同步记录 |
| Gateway | 统一机器账号换取 Token，校验服务授权，复用资源入口，不包含 openEuler 特定逻辑 |
| API Server | 复用 Project 创建/更新校验及存储，不信任插件绕过校验 |
| Build/Snapshot Controller | 继续消费 Project/Snapshot，不感知插件或 Webhook |

后续“回调”分为两段：Git 平台通知插件清单可能变化；插件调用已有 Project API 写入结果。
Gateway 不新增专用回调路由，插件回调正文也不能直接指定 Project、仓库 URL、下载地址或软件包列表。

## 3. 配置与绑定

配置文件与凭据分开挂载。配置更新首版通过重启生效，启动前完整校验配置，不接受部分有效配置启动。

```yaml
gatewayURL: http://ebs-gateway:8080
machineCredentialFile: /etc/release-management/secrets/machine-credential.json
dataDir: /var/lib/release-management
listen: :8080
pollInterval: 10m
workers: 2
maxProjectBytes: 1048576
webhook:
  enabled: false

sources:
  - name: openeuler-release
    repository: https://atomgit.com/openeuler/release-management.git
    ref: refs/heads/master
    allowPrivateNetwork: false

bindings:
  - name: oe2403-next-everything
    project: openeuler-24-03-lts-next
    source: openeuler-release
    paths:
      - directory: openEuler-22.03-LTS-Next/baseos
      - directory: openEuler-24.03-LTS-Next/everything-exclude-baseos
    packageURLTemplate: https://atomgit.com/src-openeuler/{name}.git
```

- source、binding 名称唯一；同一 Project 名称只能有一个绑定。配置仅使用 project 名称，不固定 UID，
  同名工程重建后，允许下一轮继续同步；仍由 Gateway 校验当前调用方对该工程的权限。
- 一个绑定只引用一个 source；同一仓库的不同 ref 使用不同 source，首版不合并跨仓库版本。
- source ref 必须是完整的 `refs/...`，每轮解析为 commit 后读取，禁止直接使用浮动工作区文件。
- `paths` 为非空目录列表；目录必须是仓库内的规范相对路径，拒绝绝对路径、`..`、符号链接和子模块。
- 每个目录只读取直接子级 `.yaml`、`.yml` 普通文件；按路径排序，不隐式递归。目录不存在或没有清单文件时报错。
- `packageURLTemplate` 必须是经运维配置的 HTTP(S) 仓库地址模板，仅允许 `{name}` 占位；包名编码为单个路径段。
- 每个 path 可选设置 `packageRef: refs/heads/...`，用于显式覆盖该来源包的构建引用；未设置时 PackageRepo.ref 留空，
  使用 Project.defaultRef。缺少显式 ref 且 Project.defaultRef 为空时停止同步，不猜测默认分支。
- workers 为正整数，只允许不同绑定并发，同一绑定始终串行。
- `maxProjectBytes` 为正整数，默认 1 MiB；部署方须设置为不超过 Gateway 与 apiserver 实际请求上限的值，
  不是仅针对 packageRepos 的限制。插件不自动发现服务端上限，也不能通过调大该值绕过服务端限制。
- `webhook.enabled` 缺省为 false；首版不实现通知接入，显式设置 true 时启动校验失败，不静默忽略。
  Webhook 密钥、监听及认证配置在后续接入阶段确定，不要求周期同步部署预先提供。
- `listen` 默认 `:8080`，仅对内部网络提供探针和指标。source 的 `allowPrivateNetwork` 默认为 false，
  受信任内网清单源需显式开启；不影响插件访问内网 Gateway。来源地址、CA、机器凭据文件均由部署方配置。

清单仓库的 ref、清单所在目录、软件包构建 ref 是三种独立信息。source 的 commit 仅用于追溯清单版本，
不得写入软件包的 `commitID`。YAML 中的 `source_dir`、`destination_dir` 不自动映射为 PackageRepo.ref；
例如 baseos 清单的来源分支可能不同于本次工程构建分支，必须通过 Project.defaultRef 或 packageRef 明确决定。

## 4. 清单解析与列表所有权

### 4.1 内置 openEuler 清单解析

输入为单文档 YAML，顶层必须是对象且具有 `packages` 数组，每项必须是对象并包含非空字符串 `name`。
`source_dir`、`destination_dir`、`obs_from`、`obs_to`、`date` 等字段不是本插件的执行指令，允许存在但不参与生成。
拒绝重复 YAML key、多文档内容及不符合结构的条目，不静默跳过坏文件或坏条目。

转换顺序：

1. 从同一个固定 commit 读取绑定中的所有清单，完成全部解析。
2. 根据 name、URL 模板及 path.packageRef 生成 PackageRepo。
3. 按区分大小写的 name 合并；同名且完整字段相同则去重，URL/ref/commitID 不同则报冲突。
   不以目录顺序决定覆盖关系；所有源数据冲突均报错。
4. 按 name 排序，规范化可选字段，校验完整候选列表，计算 SHA-256 内容摘要。

候选列表完全来自配置目录中的清单，不提供包级排除、补充或覆盖入口。生成的 URL 由模板决定，ref 由
path.packageRef 决定，未设置时留空；不生成 commitID。调整包范围需修改上游清单或绑定选择的目录。

摘要基于规范化 PackageRepo 列表，不包含时间、同步次数或 Webhook delivery ID。列表一致但源 commit 改变时，
仅更新插件同步记录，不产生 Project 写入。

### 4.2 同步权威范围

绑定启用后，插件是 `spec.packageRepos` 的权威维护方，更新时替换整个数组，包括移除上游已删除的软件包。
不合并 Project 中临时手工增加的包，也不保留手工修改的 URL/ref/commitID；同步后统一使用清单生成结果。
首次启用前必须预览差异，确认接受人工配置被覆盖；需要保留逐包定制的工程不应启用该绑定。
这是部署操作前提：先执行 plan、人工确认，再启动服务；首版不持久化预览批准记录，也不设置运行时审批门禁。
plan 不是可直接应用的事务，源 ref 或 Project 在预览后变化时，启动同步会重新读取并计算差异、执行保护，
不保证实际写入与预览完全相同。新加绑定或调整来源、目录及 URL/ref 生成规则后重启服务，也应先执行相同预览流程。

Project 的 distro、buildArches、defaultRef、buildPayload、bootstrapRepos、所有 labels/annotations 和 status
均不由插件修改。新增与移除项按旧、新列表的包名集合计算；URL/ref/commitID 变化单独作为修改项展示。

保护规则：

- 候选列表为空始终拒绝自动应用；需要清空时先停用绑定，由 owner 手动处理。
- 不限制移除包的数量或比例；候选列表非空且通过其他校验后，可按源清单完整替换现有列表。
- 所有更新仍须满足输入规模、完整对象容量及 API 校验，不因移除包较多而跳过校验。

## 5. 触发与插件接口

### 5.1 触发机制

启动时为全部有效绑定入队；周期校验默认每 10 分钟触发，附加不超过周期 10% 的随机抖动。
首版不注册 Webhook 路由，对该路径的请求返回 404；周期校验独立负责来源变化、重启和配置漂移恢复。

| 插件入口 | 行为 |
|----------|------|
| `GET /healthz` | 进程存活 |
| `GET /readyz` | 配置有效、状态目录可用且后台 worker 已启动 |
| `GET /metrics` | 绑定级同步计数、最近成功时间、排队与执行累计耗时 |

后续启用 `POST /webhooks/atomgit/{source}` 时，Webhook 仅作为加速信号，周期校验仍负责漏通知补偿。
接入前须明确认证 Header、签名算法与签名正文范围或 Token 校验方式、仓库身份和 ref 的事件字段、
支持的事件类型，以及请求体与频率限制，并提供真实投递样例和对应测试。
Webhook 校验采用所部署 AtomGit 版本实际支持的签名或 Token 机制；
不假设与 GitHub 的 Header/签名算法相同。入口强制 HTTPS、限制请求体和频率，并匹配配置中的仓库身份及 ref。
无效凭据返回 401/403，格式错误返回 400，未知 source 返回 404；已验证但无关的事件返回 204。
暂不能接入平台认证时关闭 Webhook，保留周期同步，不能匿名接受更新通知。

通知处理不在 HTTP 请求内等待 Git 或 Project 更新。重复事件合并到相同 source/binding 的 dirty 标记，
202 只表示通知已接收，不表示工程已更新；重启丢失通知由启动扫描补偿。
回调 payload 中的 commit 只作为审计线索，不决定写入版本，每轮始终 fetch 并解析配置 ref 的当前值。
因此乱序通知不会直接重放旧 commit；Git SHA 不作为可排序的版本号比较。

手动操作通过插件本地命令执行：

- `release-management plan --binding <name>`：生成差异和保护判定，不更新 Project、不推进成功检查点。
- `release-management sync --binding <name>`：请求运行中的服务执行同一同步流程，通过本地受限控制通道入队。
- `release-management status --binding <name>`：读取绑定状态及最近一次结果，不触发同步。

手动触发不得启动第二个独立写入进程；本地控制通道使用仅部署用户可访问的 Unix socket，不增加公开管理接口。
socket 路径为 `dataDir/control.sock`，权限为 0600。服务运行时，三个命令均通过该通道执行，
plan 与后台同步复用绑定串行执行和 source 仓库锁；plan 可更新 Git 缓存，但不写 Project 或同步成功记录。
服务未运行时，plan 必须先取得状态目录进程锁才能独立执行；取得锁后也不能自动执行同步。
不能仅因 socket 不可连接就绕过进程锁。sync/status 在服务未运行时返回错误，不启动服务或读取过期记录冒充实时状态。

本地通道采用 HTTP over Unix socket：`POST /plan`、`POST /sync`、`GET /status`，binding 分别通过 JSON 请求字段
或查询参数传递；不接受客户端提供仓库地址、凭据或候选清单。响应使用 JSON，plan 返回来源 commit、Project UID、
resourceVersion、差异及保护判定，sync 返回入队确认，status 返回当前状态与最近结果。
命令退出码 0 表示操作完成或入队成功，1 表示执行失败，2 表示配置/参数无效或 plan 被保护规则阻止；
sync 返回成功不表示工程已经更新，须通过 status 确认最终结果。plan 输出只供本地核对，不写入普通服务日志。

### 5.2 同步状态

内部运行状态为 Idle、Queued、Syncing、Retrying、Blocked；它们不是新的 EBS API 对象。
运行中再次收到周期或手动触发时保留 dirty 标记，本轮结束后重新检查最新来源，不为每次触发累计独立任务。
后续接入的 Webhook 复用同一机制。
状态与最近一次结果通过插件本地 status 命令、结构化日志和指标查看，不公开凭据或完整软件包正文。

## 6. 更新 Project 的契约

### 6.1 认证与权限

插件使用 `type=service` 的 MachineAccount，不保存用户密码，不自动执行 OAuth 登录，也不冒充工程 owner。
交换及权限契约以 [Gateway 机器账号设计](ebs-gateway.md#machine-token) 为准，
机器账号由 Admin 创建，凭据由 apiserver 校验。
工程范围与操作权限控制由独立的 apiserver/Gateway 改动提供，尚未随本插件合入；当前已合入的 Gateway 使用不限制工程范围的固定允许列表。启用范围授权后，插件账号的 `projects` 应覆盖所有 bindings 的 project，只授予 `projects:update`，不需要 `builds:create`、`builds:abort` 或读取权限。目标授权配置如下：

```yaml
spec:
  type: service
  projects:
    - openeuler-24-03-lts-next
  permissions:
    - projects:update
```

插件自身只修改绑定工程的 packageRepos，不调用 Build 接口。`projects:update` 允许更新工程主资源，并非仅软件包列表；本地 bindings 和插件只提交 packageRepos 的行为不构成服务端字段级安全边界。机器凭据应交给可信内部服务，Gateway 保留现有 owner、服务器元数据、版本及门禁标签保护。不增加 packageRepos 专用权限。

全局 `machineCredentialFile` 指向只读 Secret 文件，所有绑定共用同一个机器账号，不支持绑定级覆盖。
需要不同身份时部署独立实例，并确保它们管理不同的工程。凭据内容为：

```json
{"clientID":"release-management","clientSecret":"base64url-encoded-random-secret"}
```

使用 HTTP Basic 携带以上凭据，向 `POST /auth/machine-token` 发送 `{}`，取得
`accessToken`、`tokenType=Bearer`、`expiresIn`。GET Project 不带 Authorization；PATCH 只携带 Bearer Token，不携带长期 secret。
匿名 GET 是主动选择的请求方式，不是认证失败后的降级。
凭据换取与资源请求都使用配置的 Gateway HTTP(S) 地址，禁止重定向。
可信内网允许 HTTP，机器凭据与 Token 明文传输；跨公网或不可信网络必须使用 HTTPS。
HTTPS 默认验证系统 CA，私有 CA 通过 gatewayCAFile 指定，不提供跳过证书校验的选项。
交换响应须为非空 Token、Bearer 类型及 300～86400 的整数有效秒数；格式异常按上游故障退避，
不解析未验证的 JWT 来扩大权限或有效期，也不将错误响应当作可用 Token 缓存。

Token 仅保存在内存，使用请求开始时刻及 expiresIn 保守计算单调时钟到期时间，
到达提前 `min(5m, TTL/5)` 的刷新点后，在下次写入前刷新，不启动独立刷新线程；
所有绑定共享一个并发安全 Token 缓存，同一时刻只执行一次交换，等待者可响应取消。
每次写请求及 401 恢复时重新读取凭据文件，以摘要识别变更；部署方可原子替换 Secret，账号重建后需下发新 secret。
刷新失败停止本次写入，按错误分类阻塞或退避，不继续发送旧 Token。
只有仍为当前版本的被拒绝 Token 才失效，避免并发请求清除刚刷新的 Token。

PATCH 收到明确 401 时，Gateway 尚未转发写入，最多强制刷新一次并重试相同请求，正文和 UID/版本前置条件保持不变。
第二次仍为 401 时清除当前被拒绝 Token，进入 Blocked；403 不触发刷新，PATCH 的并发核验沿用 6.2。
GET 不换取 Token，401/403 直接按访问失败处理；网络错误或 5xx 不直接重放 PATCH，沿用结果确认与下一轮重算。
429 遵循 Retry-After，网络错误与可重试 5xx 使用带抖动退避；定时触发不能绕过退避。
凭据不可读或格式非法时阻塞对应写入，周期或人工触发恢复检查，不将 PATCH 降级为匿名请求。
匿名 plan/GET 不读取凭据；无变更同步无需换取 Token。配置不接受 `ownerTokenFile`，凭据文件必须是严格 JSON，
仅含 clientID/clientSecret；拒绝重复/未知字段、超限内容、非法账号名及无效 secret，错误不回显文件内容。
Token 过期续期不等于有权更新工程，最终权限始终由 Gateway 实时核验。

### 6.2 资源调用

除 `POST /auth/machine-token` 外，插件只调用以下资源路由，不调用 Project 创建、删除、status 或任何 Build 写接口：

| 方法与路径 | 用途 |
|------------|------|
| `GET /apis/ebs/v1/projects/{project}` | 匿名读取完整 Project，用于对象身份核验、差异计算和完整候选对象大小预检 |
| `PATCH /apis/ebs/v1/projects/{project}` | 使用 JSON Merge Patch 替换 packageRepos |

示例：

```http
PATCH /apis/ebs/v1/projects/openeuler-24-03-lts-next
Authorization: Bearer <service-token>
Content-Type: application/merge-patch+json

{
  "metadata": {
    "uid": "<本轮 GET 读取的 Project UID>",
    "resourceVersion": "<本轮读取的版本>"
  },
  "spec": {
    "packageRepos": [
      {"name": "gcc_secure", "url": "https://atomgit.com/src-openeuler/gcc_secure.git"}
    ]
  }
}
```

Gateway 按现有 PATCH 流程读取对象、合并候选、执行权限与字段检查，并向 apiserver 提交带版本的更新。
客户端提供的版本不能被静默替换成 Gateway 新读到的版本；版本不一致时按拒绝/冲突处理，插件重新读取后再计算，
不假设所有陈旧版本都会返回相同 HTTP 状态。

apiserver 写入版本冲突返回 409；Gateway 候选对象版本校验也可能返回 403。插件对 PATCH 403 最多补充一次匿名 GET 进行核验：

- GET 成功且 UID 相同、未删除、resourceVersion 已变化：按并发变化处理，下一轮重新计算，不重放旧 PATCH。
- GET 成功但版本未变化，或 GET 返回 401/403：按权限拒绝进入 Blocked；后续周期或手动触发再核验。
- GET 返回 404 或对象删除中：按目标不可用进入 Blocked。
- GET 成功但 UID 与本轮初次 GET 不同：终止本轮，延迟重新入队，下一轮重新读取并计算，不能重放旧 PATCH。
- GET 超时、429 或可重试 5xx：延迟重入，不因确认失败直接判定权限不足。

版本变化只表示允许重新核验，不证明调用者具有更新权限；每次写入仍由 Gateway 授权。
409 同样重新读取计算，401 按 6.1 处理，不进入上述冲突确认流程。业务冲突重试不得绕过退避。

容量预检使用完整 GET 响应和本轮合并后的完整 Project，而非仅估算软件包列表。
当前 Gateway 默认请求上限为 1 MiB，apiserver 为 5 MiB；Gateway 还会检查旧对象响应和合并候选的大小。
插件检查 GET 原始 JSON、PATCH 正文及合并后候选的 JSON 字节数，任一超过 maxProjectBytes 就停止写入。
已有对象过大时，即使候选列表缩小也不能假定 Gateway 会接受；需先调整部署上限。
服务端 413，或 Gateway 明确返回 `current object exceeds body limit` 的 502，归为容量阻塞，
不作为临时网络故障连续重试。其他 502 按暂时性错误处理；本地预检通过不替代服务端最终校验。

### 6.3 每轮处理顺序

1. 获取绑定的单写执行权；fetch 来源 ref，解析 commit C，并记录本轮配置摘要。
2. 按配置名称 GET Project，核验返回名称一致、UID 和 resourceVersion 非空且未删除；
   将本轮 UID 固定为 U、版本固定为 V，并读取 defaultRef。NotFound 不自动创建工程。
3. 从 C 读取并生成完整列表 L，与当前 packageRepos 归一比较；任一步失败都不写入。
4. 候选列表为空时停止同步；若列表一致，记录已核对 C 与内容摘要；否则计算差异并执行完整对象容量预检。
5. PATCH 携带 U、V 和 L。成功响应须匹配本轮 U，且返回列表与 L 语义一致，才记录成功。
6. 写入超时或连接中断等结果未知时，GET 确认：名称及 UID 匹配本轮目标、未删除且列表等于 L 才视为成功；
   UID 已变化时，即使列表相同也不能确认旧请求成功，下一轮重新读取并计算。
7. 版本冲突后重新 GET、重新生成/比较并重新执行保护，不直接把新 resourceVersion 填入旧请求。
   下一轮重新解析来源 ref；不保留可无限重放的旧 PATCH 请求。

除明确 401 后的单次认证恢复外，每轮最多尝试一次写入；后续重试重新入队。进程恢复后同样先读取来源与 Project，再决定是否需要更新。
同一个绑定只允许一个写入者，不会同时提交两个不同的期望列表；来源在本轮执行中发生变化时，下一轮继续收敛。
UID 只用于本轮更新及结果确认，不与配置或上轮检查点绑定；同名工程重建后重新执行差异计算、空列表保护和容量预检。
若外部系统也维护 packageRepos，不保证双方最终一致，应停用其中一个写入者。

已创建 Snapshot 的内容保持不变；尚未完成 Snapshot 创建的 Build 可能读取更新后的 Project。
因此本插件不是“绑定 Build 创建时清单版本”的机制，需要该保证时另行扩展构建输入协议。

## 7. 错误与恢复

| 场景 | 处理 |
|------|------|
| Git 网络失败、Gateway 超时、429/可重试 5xx | 保留旧列表；指数退避 5 秒起、最大 5 分钟，附加抖动；429 尊重有效 Retry-After |
| Project 版本不一致 | 重新读取与计算，短暂延迟后入队，不计为源数据错误 |
| PATCH 403 | 按 6.2 补充一次 GET 分类，不能一律当成权限失效或无限按冲突重试 |
| Token 失效、机器凭据或服务授权不足 | 按 6.1 单次刷新规则处理；仍失败则 Blocked 并告警，周期或手动触发重新核验 |
| Project 不存在或删除中 | Blocked；不创建工程，后续周期或人工触发重新核验 |
| 本轮写入或确认期间 Project UID 变化 | 终止本轮并延迟重入；下一轮允许使用同名新工程的 UID，重新计算后再写入 |
| 来源 ref/目录不存在、YAML 非法、同名配置冲突 | Blocked；保留旧列表，后续来源或配置更新后重新核验 |
| 空结果 | Blocked；输出差异摘要，不写入空列表 |
| 本地容量预检失败、服务端 413、明确的旧对象超限 502 | Blocked；提示核对完整 Project 大小及部署上限，周期或人工触发重新核验 |
| apiserver 返回字段校验错误 | Blocked；记录字段路径及脱敏错误，不截断列表后重新提交 |
| 成功响应异常或请求结果未知 | 读取确认，不假定成功、不盲目重复写入 |
| 本地状态记录写入失败 | 不标记成功；恢复后根据 Project 实际内容确认，不回滚已生效更新 |

重试等待中收到新事件可以合并，但不得突破限流/退避造成请求风暴。某绑定失败不阻断其他绑定。

## 8. 持久化、部署与安全

```text
/var/lib/release-management/
├── repositories/{source}/       # 清单裸仓库缓存，不提供对外 clone
├── bindings/{binding}.json      # 最新确认/阻塞记录，不保存 Token
└── control.sock                 # 运行期间的本地命令通道，不是持久化记录
```

绑定记录包含配置摘要、最近确认成功的 Project UID、source commit、列表摘要、同步时间、变更计数及脱敏错误。
该 UID 仅供追溯成功结果，不是跨轮次身份约束；成功确认同名新工程后更新记录，未确认结果不推进成功记录。
记录采用临时文件、同步落盘、原子替换；只是恢复与审计检查点，不替代 Project 当前事实。
不保存可自动回滚的完整工程对象；回滚通过修正源清单/配置后重新同步，仍经过差异计算、空列表保护及其他校验。

首版使用单副本 Deployment、持久化卷和 Recreate 更新策略，并对状态目录加进程互斥锁，防止同目录双进程。
不允许多套独立卷的插件实例同时管理同一 Project；API 版本检查只能避免单次覆盖，不能替代单写者约束。
不同绑定可并发，共用 source 的 Git fetch 与对象读取须协调，避免 fetch/prune 干扰本轮固定 commit 的读取。

安全与容量边界：

- 仅访问运维配置的来源 URL；Webhook 不允许改变地址、凭据或目标工程。首版拒绝 Git 与 Gateway 的 HTTP(S) 重定向。
- Git 通过参数数组执行，不拼接 shell，不执行仓库脚本、hook、submodule 或 LFS 下载；YAML 使用安全解析。
- 来源 URL 限定可访问的主机与端口，DNS 解析后校验并为 Git 固定本轮地址；禁用环境代理与全局 Git 配置。
  默认拒绝私网、回环、链路本地、共享地址段及非全局单播地址，内网源通过 allowPrivateNetwork 显式放行。
- 单个 YAML 上限 8 MiB、每轮清单总量 32 MiB、最多 1000 个文件、10000 个不同软件包；
  原始包条目最多 100000 条，单文档节点最多 200000 个、深度最多 64 层，首版拒绝 YAML 别名。
- 完整对象容量预检与服务端超限处理遵循 6.2；不拆分数组分批写入以规避限制。
- Git 网络操作默认超时 2 分钟、单轮 5 分钟；子进程输出限长，退出时取消未开始任务并终止子进程。
- 部署 Secret 只读挂载，凭据文件更新通过原子替换；日志不打印 Authorization、Webhook 密钥、请求正文或带凭据 URL。
- 首版不需要来自 Git 平台的入站访问，仅对集群探针提供 healthz/readyz；本地命令使用 Unix socket。
  不因单个源故障使整个进程 readiness 失败，绑定异常通过状态和告警暴露。

结构化日志包含 binding、Project、source commit、结果、耗时和新增/删除/修改数量。
指标包含同步次数/失败次数、最近成功时间、队列等待和同步耗时；不将 commit、包名或错误正文用作指标标签。
重复失败限频记录，成功且无变化的周期校验使用 debug 日志。

## 9. 验收与实施顺序

### 9.1 测试覆盖

1. 两个目录从同一 commit 合并；合法未知 YAML 字段不影响输出；重复同值去重、重复冲突报错。
2. defaultRef 与显式 packageRef 的选择、所有上游包合并后的完整输出、URL 模板生成、输出顺序及摘要稳定。
3. 缺目录、无 YAML、坏文件、重复 YAML key、路径穿越、符号链接、过大输入时不更新工程。
4. 列表不变不写；空结果禁止写入；非空候选允许大量移除现有包，不受数量或比例限制。
   首次 plan 完整展示人工内容被删除或 URL/ref/commitID 被覆盖的差异，
   不写 Project、不推进成功检查点；启动仍重新读取，不把预览结果作为可重放写入请求。
5. 配置只需 Project 名称；PATCH 携带本轮 GET 的 UID/resourceVersion，其他 Project 字段不变。
   覆盖同名重建后正常同步、重启后不受旧检查点 UID 限制、本轮 UID 变化不误确认成功，以及新工程仍执行空列表保护。
6. Service Token 交换、提前刷新、并发刷新合并、401 单次恢复、403 不刷新、Secret 文件轮换与账号删除重建；
   缺凭据和无权限时写入不降级，匿名读取独立执行。Webhook 缺省关闭、路由返回 404，首版显式启用时报配置错误。
7. 重复周期/手动触发、运行中收到新触发、来源新版本替代旧重试任务；Git SHA 不按字符串大小排序。
8. 写入冲突、PATCH 403 后版本变化/未变化/确认失败、超时后已成功/未成功、响应对象异常、本地检查点失败和重启恢复。
9. 单绑定失败不阻塞其他绑定；无 Webhook 时周期发现来源变化；同绑定不并发；限流与退避不被重复触发绕过。
10. 更新不触发 Build，已有 Snapshot 不变；凭据、请求正文及包清单不泄漏到普通日志。
11. PATCH 较小但完整候选超限、旧对象超限、服务端 413、明确的容量 502 与其他 502 的分类。
12. 服务运行时 plan/sync/status 走同一本地通道；独立 plan 获取进程锁；plan 与 fetch 不竞争；
    socket 不可连接但进程锁被占用时禁止独立访问缓存；服务未运行时 sync/status 明确失败。
13. GET 不携带 Authorization，也不读取机器凭据；PATCH 使用 Service Token，确认与重试保留 UID/版本保护。
    插件仅提交 packageRepos，Gateway 仍保留 owner、服务器元数据和门禁标签保护。

Webhook 接入阶段另补认证失败、仓库/ref 过滤、事件类型、重复/乱序投递、请求体与频率限制及漏通知补偿测试。

### 9.2 实施阶段

1. 实现配置、内置 openEuler 清单解析及 plan，使用固定 commit 样本核对来源目录和构建 ref 策略。
2. 保留 Project GET/PATCH、单实例队列、空列表保护和结果确认，接入机器账号换取与自动刷新。
3. 创建 service 账号并挂载凭据，确认 Gateway 允许 Project 更新；启用范围授权时配置目标工程和 `projects:update`。执行并人工核验 plan，确认接受清单对人工包配置的替换，
   配置告警后启动周期同步；不依赖 Webhook 上线。
4. 后续根据真实 AtomGit Webhook 投递契约补齐配置、通知与认证，完成相关测试后才允许显式启用，仍默认关闭。

首版命令、镜像构建与部署约束见 [组件 README](../../../backend/release-management/README.md)。

服务认证复用 MachineAccount 和 Gateway，不改变 Project 模型或 Build/Snapshot Controller，
不增加插件到 ES/etcd 的连接；Gateway 不增加清单解析和同步业务。

### 9.3 待完成事项

- 合入独立的 apiserver/Gateway 工程范围与操作权限控制，配置插件账号的最小授权。
- 在线修改机器账号授权。
- Webhook 的真实投递契约、认证及通知接入。

上述能力不是可信内部环境中周期同步的前置条件。启用范围授权后，缺少工程或操作授权时，插件保持 403 阻塞及后续核验流程，不自动创建或中止 Build。
