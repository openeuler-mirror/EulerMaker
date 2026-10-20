# EulerMaker 前端设计

## 一、文档状态

构建配置支持运维编辑 OS/Arch/镜像映射，工程目标选项解析 `spec.content`，并保留旧工程已失效的目标值；规则见 [构建配置设计](data-models~config.md#26-前端与-ebsctl)。运维页面同时提供全局 Script 管理，脚本执行接入范围见 [Script 设计](data-models~script.md)。

本文定义 EulerMaker Web 控制台的首版产品范围、技术架构、页面结构、权限行为、API 对接、实时日志、安全边界、部署方式和验收标准。

本文记录当前页面与接口行为，性能、测试目标和待完善能力单独说明。资源访问以当前 `ebs/v1`、`iam.ebs/v1`、Gateway 和 Artifact Manager 契约为准；资源字段的权威定义位于 [`api/ebs/v1`](../../../api/ebs/v1) 和 [`api/iam/v1`](../../../api/iam/v1)。

---

## 二、背景与目标

目标资源模型包括 Project、Snapshot、Build、BuildInfo、RpmRepo、Config、Job 和 Runner 等 Kubernetes 风格资源，以及用户认证、Project 授权、产物下载和实时日志能力。当前需要一个面向社区用户、项目维护者和系统运维人员的 Web 控制台，把资源 API 组织成可理解、可操作的构建工作流。

首版目标：

- 匿名用户可以浏览 Project 及其公开构建资源。
- 注册用户可以创建 Project，管理自己拥有或参与的 Project，并发起构建。
- Ops 可以按 owner/member 关系管理 Project 并发起构建，同时管理 Config 和 Script，查看 Runner 运行状态并执行驱逐或取消驱逐。
- Admin 可以管理普通 User 和 MachineAccount，并具备系统级资源能力。
- 前端能力模型只根据用户权限和 Runner 身份展示功能。
- 用户可以查看 Build、Job、RPM 仓库和产物状态，并实时查看 Job 日志。
- 前端严格遵守 Gateway 的认证和授权边界，不直连 `ebs-apiserver`。
- API 类型、查询和错误处理集中管理，避免页面直接拼装后端请求。

首版不包括：

- 在浏览器中编辑任意资源 YAML 或 JSON。
- 提供任意 Job、Runner 或控制器维护的 `status` 编辑器；Runner 驱逐和取消驱逐使用下文定义的受控状态操作。
- 编排流水线、审批流和通知中心。
- 多集群切换。
- 在前端保存 Git、仓库或 Runner 凭据。
- AI 助手。

---

## 三、设计原则

### 3.1 Gateway 是资源访问边界

浏览器对 EBS 和 IAM 资源的请求统一发送到 `ebs-gateway`。前端、静态文件服务器和浏览器都不能持有 apiserver 的内部证书，也不能通过单独的匿名代理绕过 Gateway 的路由白名单、限流、审计和身份校验。

### 3.2 后端负责授权，前端负责解释能力

前端根据身份和 Project labels 控制导航、按钮和提示，让用户只看到合理操作；Gateway 仍是最终授权方。前端解析 JWT 只用于即时展示，不能把 JWT payload 视为授权结果。

### 3.3 资源模型优先

页面状态以服务端资源为准。创建、修改或中止请求成功后重新读取相关资源，不在浏览器中模拟控制器状态迁移。

### 3.4 URL 可恢复

Project、资源名称、页签、搜索条件、筛选条件和当前页应进入路由或 query。刷新页面、复制链接和浏览器前进后退后，用户仍能回到相同视图。

### 3.5 大数据按需加载

列表使用服务端分页。Project 详情按页签加载资源，不在进入页面时并发读取所有子资源。日志和大对象使用字段投影、Range 或下载接口，不一次性保存在 Pinia。

### 3.6 失败必须可见

实时请求失败时显示错误和重试入口，不能自动回退到 mock 数据。演示数据只能由开发或显式演示模式启用，并持续显示“演示数据”标识。

---

## 四、用户与权限

### 4.1 身份类型

| 身份 | Type | Scope | 前端职责 |
|------|------|-------|----------|
| 匿名用户 | 无 | 无 | 浏览公开 Project、Snapshot、Build、BuildInfo、RpmRepo 和 Job |
| 普通用户 | `user` | `ebs:user` | 创建 Project；操作自己拥有或参与的 Project |
| 运维用户 | `user` | `ebs:ops` | 继承普通用户的工程权限；管理 Config、Script 和 Runner，支持驱逐与取消驱逐 |
| 管理员 | `user` | `ebs:admin` | 管理非管理员 User 和 MachineAccount；具备系统级业务资源能力 |

用户 scope 互斥，不在令牌中组合。`ebs:ops` 通过 Gateway 授权策略继承普通用户的 owner/member 工程权限，同时保留额外的运维能力。正常交互式登录只会获得 `ebs:user`、`ebs:ops` 或 `ebs:admin`。Runner 是机机身份，不携带用户 scope，也不能通过 Web 控制台登录。

### 4.2 Project 关系

前端从 Project labels 解释普通用户与 Project 的关系：

```text
ebs.io/owner-user: <username>
ebs.io/member-user.<username>: "true"
```

页面可计算 `owner`、`member` 和 `none` 三种关系。创建 Project 时不允许普通用户指定 owner；Gateway 会使用 JWT `sub` 写入 `ebs.io/owner-user`。这些 labels 管理写权限，不表示 Project 的读取可见性；Gateway 白名单内的 Project 及其业务资源均允许匿名读取。

### 4.3 页面能力矩阵

| 能力 | 匿名 | User Owner | User Member | Ops | Admin |
|------|------|------------|-------------|-----|-------|
| 浏览公开业务资源 | 是 | 是 | 是 | 是 | 是 |
| 创建 Project | 否 | 是 | 是 | 是 | 是 |
| 修改/删除 Project | 否 | 修改、删除 | 否 | 仅自己拥有的 Project | 否 |
| 管理 Project 成员 | 否 | 是 | 否 | 仅自己拥有的 Project | 否 |
| 创建/修改 Project 子资源 | 否 | 是 | 是 | 按 owner/member 关系同普通用户 | 是 |
| 删除 Project 子资源 | 否 | 是 | 否 | 仅自己拥有的 Project | 是 |
| 读取 Config | 仅具名 Public | 仅具名 Public | 仅具名 Public | 全部（含 list） | 全部（含 list） |
| 修改 Config | 否 | 否 | 否 | 是 | 是 |
| 查看 Runner | 否 | 否 | 否 | 是 | 是 |
| 管理 Runner | 否 | 否 | 否 | 是 | 是 |
| 管理 User/MachineAccount | 否 | 否 | 否 | 否 | 是 |

按钮是否显示由前端能力函数统一判断，例如 `canEditProject(identity, project)`；页面不得散落 scope 字符串比较。服务端返回 403 时，以服务端结果为准并刷新当前资源。

---

## 五、信息架构

### 5.1 路由

```text
/                              -> /projects
/login
/register
/projects
/projects/:name                 # tab=overview / builds / config
/operations                    # /runners 为别名
/admin                         -> /admin/users
/admin/users
/admin/machineaccounts
/settings
```

工程详情通过页签组织软件包、构建历史、依赖图谱和工程配置。选中 Build 后在右侧展示构建详情，展开 spec 后展示 Job，再在 Job 条目内展示日志和产物；没有独立的 Build/Job 详情路由。全局 Config 和 Script 在运维页面管理。

### 5.2 全局导航

全局导航包括：

- 工程
- 运维管理（含机器管理），仅 Ops 和 Admin 显示
- 账号管理，仅 Admin 显示
- 账号菜单：个人信息、语言、主题、退出

导航在小屏设备折叠为菜单。不得在固定宽度头部同时排列全部导航、法律链接和设置按钮。

### 5.3 默认入口

根路径重定向到工程列表，不另设首页或全局监控面板。

### 5.4 工程列表

工程列表展示：

- `metadata.name`
- `spec.displayName`
- `spec.description`
- `status.phase`
- owner、当前用户关系
- Build target 摘要
- `metadata.creationTimestamp`

支持名称搜索、关系筛选和服务端分页。文本搜索只有在 API 提供对应 selector 时才下推；否则只筛选当前页，并明确标注“筛选当前页”。

工程列表提供两种创建入口：

- “创建工程”使用结构化表单填写工程名、显示名称、说明、SPEC 分支和首个 Build target。工程名在客户端按 DNS label 规则预检；普通用户和 Ops 创建的工程由 Gateway 注入 owner，Admin 创建时需指定已启用的普通用户或 Ops 用户作为 owner。
- “导入 YAML”读取或粘贴单个 `ebs/v1 Project` 清单。客户端解析 YAML，校验 `apiVersion`、`kind`、工程名和至少一个包含 `os`、`arch` 的 Build target；提交前移除 `status` 和服务器维护的 metadata 字段。该入口只用于创建 Project，不构成通用资源 YAML 编辑器。

两个入口共用 `POST /apis/ebs/v1/projects`。匿名用户点击后进入登录页并返回工程列表。409 冲突、校验错误和权限错误都在弹窗中保留输入并以当前界面语言展示。

### 5.5 工程详情

工程页面依次使用四个一级 Tab：“工程详情”展示软件包仓库及其 Job 历史；“构建历史”展示 Build、运行中任务统计和构建详情；“依赖图谱”展示构建的 BuildGraph；“工程配置”维护基础配置、Build targets、Bootstrap repositories、成员及 buildPayload。默认 ref 和创建时间显示在顶部工程容器。页签状态写入 `tab` 查询参数，可刷新和分享。

依赖图谱按需加载，使用 `tab=graph`。构建选择仅展示非 single 构建，通过 `labelSelector=ebs.io/build-type!=single` 在服务端过滤后按 50 条分页，只读取名称、构建类型和目标摘要。构建历史选中非 single 构建时沿用该选择，否则默认选择列表第一项；图通过同 Project、同名 `buildgraphs/{buildName}` 获取。

图谱展示区域内提供统一查找栏，忽略大小写并按子串匹配全部直接上游、中心节点和直接下游，包括滚动区域外的节点。查找不筛除节点或连线，不改变当前中心节点或重新请求 API；高亮全部匹配项，显示当前位置及匹配总数，支持上一项/下一项循环跳转并自动滚动定位。Enter/Shift+Enter 切换匹配项，Esc 清空；焦点在图谱区域内时 Ctrl+F/Command+F 聚焦查找栏。切换中心节点时清空查找。

左侧节点列表支持搜索和分页，以选中 spec 为中心显示全部一跳上游/下游，不对邻居分页；上游、下游各为单列，支持列内独立上下滚动，中间节点保持固定，较短列表垂直居中。弧线跟随列内滚动，仅绘制可见邻居的连线；窄屏可横向滚动画布。箭头从依赖提供者指向使用者，实线表示构建依赖、虚线表示安装依赖，保留两类依赖的独立版本约束；标记破环节点并单独显示自依赖。点击节点继续探索并重置两侧滚动位置，刷新时重新获取数据；切换构建、Project 或离开 Tab 会取消过时请求。尚未建图、空图和读取错误分别显示对应提示。

构建历史采用主从布局：左侧分页展示构建名称、状态、类型、目标与开始时间，支持隐藏单包构建；右侧展示所选 Build 的状态与 BuildInfo 的 spec 构建状态，支持搜索、状态过滤和分页。点击 spec 展开对应 Job 列表，点击 Job 名展开日志和 RPM 下载列表。用户有权限且 Build 处于 `Pending`、`Prepared` 或 `Processing` 时，该条列表记录提供中止按钮；二次确认显示当前状态，操作后重新读取构建历史。窄屏设备按列表、详情的顺序改为上下布局。

Spec 状态列表展示包的 Commit ID：根据 `BuildInfo.status.specRepoNames[specName]` 获取仓库名，再从同工程、同构建名称的 `Snapshot.status.packageRepoStatuses[repoName].commitId` 读取。列表显示前 12 位，悬停显示完整值。Snapshot 与 BuildInfo 并行读取，使用 `includeFields=status.packageRepoStatuses` 避免获取 Snapshot spec；刷新按钮仅显示图标，并重新加载两者。Snapshot 读取失败时单独提示，构建状态仍正常显示；切换构建时清空旧数据并取消旧请求。

软件包仓库列表中的名称可选中，选中后列表收窄到左侧，右侧显示 RPM 下载地址与对应 Job 历史。历史查询仅使用 `ebs.io/package-name` label selector：客户端使用[标签约定](labels.md#7-job-构建归属标签)中的规则从 `PackageRepo.name` 计算查询值，直接请求所属工程的 Job 列表并处理分页；界面显示 Project 中的完整名称，不显示截断或编码后的 label 值。没有该标签的存量 Job 不出现在包级历史中。

工程配置中的 Build targets 使用列表展示 `os`、`arch`、`buildFlag` 和 `publishFlag`。仅工程 owner 可通过编辑弹窗新增、删除或修改目标，并以包含当前 `resourceVersion` 的完整 Project 执行 PUT；至少保留一个同时包含 `os` 与 `arch` 的目标。发生 409 时保留弹窗内容并提示重新加载后处理冲突。

Bootstrap repositories 同样仅向工程 owner 提供列表编辑能力。每项包含名称与仓库地址，允许新增、删除、修改或保存空列表以清除配置，并复用 Project PUT、`resourceVersion` 和冲突处理流程。

工程配置采用左右分区：左侧依次展示基础配置、Build targets、Bootstrap repositories 和用户管理，右侧使用等高独立面板完整展示 `buildPayload`；Package repositories 位于工程详情 Tab。窄屏设备按左侧四个容器、Build payload 的顺序折叠为单列。

具有工程编辑权限的用户可在 Build payload 面板内切换编辑状态。编辑时只读代码块替换为保留换行的多行文本框，并提供取消和保存操作；保存复用 Project PUT 与冲突处理，失败时保留草稿。

详情页提供“导出 YAML”操作，读取完整 Project 后下载为 `<metadata.name>.yaml`。导出包含 metadata、spec 和 status；再次导入创建时移除 status 和服务端维护的 metadata 字段。

软件包列表维护 `spec.packageRepos` 的 name、URL 和 ref；包级历史按上述标签查询。构建详情读取 BuildInfo 的 `status.specStatus.build`，不展示安装状态。Snapshot、BuildInfo 和 RpmRepo 为控制器维护的只读资源，前端不提供创建入口。

Project 普通配置使用结构化表单。复杂且可能丢失未知字段的对象不能通过“读取—局部表单—整体 PUT”更新，应发送 Merge Patch，并携带当前 `resourceVersion`。发生 409 时保留用户草稿，重新加载服务端对象并提示用户比较后重试。

### 5.6 Build 创建流程

工程详情提供全量构建、增量构建、单包构建和指定增量四个入口。单包构建与指定增量弹窗从 `Project.spec.packageRepos` 展示可搜索的软件包多选列表，至少选择一个软件包，并将所选名称写入 `spec.packages`；全量和普通增量不提交该字段。四种类型均在弹窗中选择工程已配置的 Build target，默认全选；每个目标创建一条 Build 记录。

请求体严格使用当前 `BuildSpec`：

```yaml
apiVersion: ebs/v1
kind: Build
metadata:
  name: 0aa1fe2e-7d5d-46d1-9d2d-c31a331c1732
spec:
  buildType: specified
  packages:
    - kernel
  buildTarget:
    os: openEuler-24.03-LTS
    arch: x86_64
```

引导仓在 Project 中配置，由 Build Controller 创建 BuildInfo 时复制到其 spec；基础构建关系从服务端 `status.baseBuildRef` 展示。

### 5.7 Runner 页面

Runner 页面展示：

- 名称、`spec.instanceId`、类型、架构和是否不可调度。
- phase、heartbeat、capacity、allocatable。
- 地址、操作系统、内核、运行时和 Agent 版本。
- labels、taints 和 conditions。

Ops 和 Admin 均可进入运维页面查看 Runner，并执行驱逐和取消驱逐；普通用户不显示该入口，也不能通过前端路由直接进入。Gateway 支持的 Runner 管理权限比当前页面功能更广，前端暂不提供创建、删除、调度开关或 taint 编辑入口。

- 非 `Evicted` Runner 显示“驱逐”，`Evicted` Runner 显示“取消驱逐”，操作均需二次确认。
- 提交前 GET 最新 Runner，确认 UID 与选中对象一致，并携带最新 resourceVersion PATCH `/runners/{name}/status`；驱逐写 `Evicted`，取消驱逐写 `Offline`，等待新心跳恢复 `Online`，不修改 `spec.unschedulable`。
- 最新对象已驱逐或已解除驱逐时，对应操作不重复写入；UID 不一致时提示冲突。404、403、409 等错误显示在确认弹窗中，不自动重放写请求。
- 不提供任意 status 编辑入口；按钮权限只用于交互，最终授权由 Gateway 执行。

### 5.8 用户与 MachineAccount 管理

Admin 用户管理页支持：

- 列出和读取非 Admin User。
- 修改 `enabled`、单一 scope、`displayName` 和 `email`。
- 删除非 Admin User。
- 修改自己的密码。

User 只能通过 `/auth/register` 创建。前端不提供“管理员创建 User 并代设初始密码”的流程，因为 Gateway 的 User API 不接受 POST，Admin 也不能重置其他用户的密码。

MachineAccount 页面支持通过专用接口创建账号、列出和删除账号。创建时由浏览器 Web Crypto API 生成至少 32 字节随机值并编码为无填充 Base64URL client secret，再与账号配置一并提交。Gateway 成功响应不会回显 secret，因此页面只使用提交前的内存副本展示一次；离开结果页面后不再保留，不写入日志、Pinia、localStorage 或 sessionStorage。用户关闭一次性凭据页面前，界面必须明确提示其通过受保护渠道完成下发。

### 5.9 脚本管理

运维页面提供集群级 `Config/build-resource` 的资源配置页签，解析和编辑其 `spec.content` 中的软件包、架构规则；另有 `Config/build-target` 的构建目标编辑和全局 Script 列表、名称搜索、刷新、创建、正文编辑及删除。Ops 和 Admin 可管理脚本，但默认 `rpmbuild` 不显示删除入口；其他脚本删除前需要确认，并携带 UID/resourceVersion 前置条件。脚本正文按原文保存，不在浏览器中执行，不应包含凭据。

编辑前读取最新对象并确认 UID，保存时携带读取到的 resourceVersion；冲突时保留草稿并提示重新打开，不自动重放更新。前端对两份内置 Config 的 YAML 内容做业务预检，但保存成功不代表服务端已验证业务内容，消费方仍须重新校验。Gateway 负责权限，apiserver 只校验 Config 通用字段。

当前页面管理 Script 对象；BuildInfo Controller 会在新建 Job 时记录脚本引用，Runner 按引用拉取并执行。修改或删除脚本不会改写已创建 Job 中记录的引用，脚本正文也不固定为历史版本。

---

## 六、技术方案

### 6.1 技术栈

| 技术 | 用途 |
|------|------|
| Vue 3 | 页面和组件框架，使用 Composition API |
| TypeScript | API、表单和视图模型类型约束，启用 strict |
| Vite | 开发服务器和生产构建 |
| Vue Router | 路由、权限守卫和 URL 状态 |
| Pinia | 会话、用户偏好和轻量跨页面状态 |
| 自有 Vue 组件与 CSS | 表单、表格、弹窗、分页和日志展示 |
| @element-plus/icons-vue | 图标 |
| yaml | 工程清单导入导出与配置内容解析 |
| vue-i18n | `zh-CN` 和 `en-US` 国际化 |

首版代码放在仓库的 `frontend/` 目录，与 API 变更在同一 Pull Request 中完成契约更新和验证。

### 6.2 目录结构

```text
frontend/
├── Dockerfile
├── package.json
├── vite.config.ts
└── src/
    ├── main.ts
    ├── App.vue
    ├── api.ts
    ├── types.ts
    ├── router.ts
    ├── styles.css
    ├── assets/
    ├── components/
    ├── composables/
    ├── i18n/
    ├── stores/
    ├── utils/
    └── views/
```

请求封装集中在 `api.ts`，资源类型在 `types.ts`，路由在 `router.ts`；业务组件随页面职责拆分。

### 6.3 API 类型维护

当前前端手动维护 TypeScript 资源类型。API 变更时同步更新 Go 类型、生成的 DeepCopy/OpenAPI、前端类型及使用方，并执行 TypeScript 检查和生产构建。OpenAPI 自动生成 TypeScript 类型尚未接入。

### 6.4 状态管理

Pinia 只保存：

- 当前会话身份和 Token。
- 语言、主题、每页数量等用户偏好。
- 跨页面需要共享的少量资源摘要。

列表、详情、表单草稿和日志状态尽量由页面 composable 管理。资源缓存以完整 API 路径、query 和 `resourceVersion` 为键；写操作成功后失效相关列表和详情缓存。

不把全部 Project 子资源预加载进全局 store。

### 6.5 路由加载与构建产物

路由页面使用动态 import。当前没有接入 ECharts 或 xterm.js；日志以有限大小的纯文本窗口渲染。

首版性能目标：

- 初始入口 JS gzip 不超过 250 KiB。
- 100 行列表交互无明显阻塞。
- 10 MiB 日志不创建与总行数等量的永久 DOM 节点。

---

## 七、API 对接

### 7.1 同源路径

浏览器只访问当前站点：

| 路径 | 上游 | 用途 |
|------|------|------|
| `/auth/*` | ebs-gateway | 注册、登录、Token 检查、密码和 MachineAccount 操作 |
| `/apis/ebs/v1/*` | ebs-gateway | EBS 资源 |
| `/apis/iam.ebs/v1/*` | ebs-gateway | IAM 资源 |
| `/artifacts/v1/*` | artifact-manager | 产物、Manifest 和日志读取 |
| 其他路径 | frontend | 静态资源和 SPA history fallback |

生产入口通过 Ingress 或反向代理按路径转发。Docker Compose 环境由前端 Nginx 容器提供等价的同源反向代理；它只转发请求，不实现业务 API，也不连接 apiserver。开发环境由 Vite 使用相同规则代理到本地 Gateway 和 Artifact Manager。

### 7.2 认证会话

登录：

```http
POST /auth/login
Content-Type: application/json

{"username":"alice","password":"..."}
```

Token 只保存在 `sessionStorage`。应用启动时如果存在 Token，调用 `POST /auth/check` 获取权威身份和 scopes；校验失败立即清理会话。内存中保存的身份结构包括：

```ts
interface SessionIdentity {
  name: string;
  scopes: Array<"ebs:user" | "ebs:ops" | "ebs:admin">;
}
```

前端只读取身份名称和 `scopes`；界面权限和路由守卫根据 `scopes` 判断，不依赖 Gateway 响应中的 `identity.type`。

不把密码、MachineAccount secret、完整 JWT payload 或认证响应写入日志。退出登录清除 Token、身份和用户私有缓存。

### 7.3 资源请求

统一 API client 负责：

- 附加 `Authorization: Bearer`。
- 序列化 `labelSelector`、`fieldSelector`、`limit` 和 `continue`。
- 解析 Kubernetes `Status` 错误。
- 生成或接收 request ID，便于排障。
- 处理超时和主动取消。
- 对 GET 做有限重试，对写请求不自动重试。

公开 GET 不附加空 Authorization header。存在 Token 时正常附加 Token，让 Gateway 校验已登录用户状态；非法 Token 不能静默降级为匿名请求。

### 7.4 分页

ES-backed 资源使用 `metadata.continue`，不使用页码换算 offset。分页组件维护已访问页的 continue token 栈：

```text
第一页 token=""
  -> 响应 continue=A
第二页请求 continue=A
  -> 响应 continue=B
```

筛选条件、排序条件或每页数量变化时清空 token 栈并回到第一页。`remainingItemCount` 只作为提示，不依赖它计算绝对总页数。

### 7.5 刷新与 Watch

| 数据 | 首版策略 |
|------|----------|
| Project、Snapshot、Build、BuildInfo、RpmRepo、Config | 用户主动刷新；运行中详情可每 10 秒轮询 |
| Job | Project 页面每 5 秒轮询；后续可接入 Project Job watch |
| Runner | Ops/Admin 页面进入时加载，手动刷新及驱逐/取消驱逐成功后重新加载；当前不自动轮询或 watch |
| Job 日志 | 展开后通过 Range 读取末尾 1 MiB；未 Completed 时每 5 秒刷新 |

页面隐藏时降低普通资源轮询频率。相同资源的上一请求未完成时不发起下一轮。路由离开时通过 `AbortController` 取消请求。

### 7.6 错误处理

| 状态 | 前端行为 |
|------|----------|
| 400/422 | 在表单顶部展示通用错误；可可靠映射时标记具体字段 |
| 401 | 清理失效会话并跳转登录；匿名公开读取则显示服务不可用 |
| 403 | 显示权限不足，刷新身份和资源，不自动重试写操作 |
| 404 | 显示资源不存在，提供返回列表入口 |
| 409 | 保留草稿，重新读取资源，提示版本冲突或当前状态不允许操作 |
| 429 | 显示限流提示，读取 `Retry-After` 后允许重试 |
| 5xx/网络错误 | 保留当前已加载内容，显示最后更新时间和重试入口 |

错误提示不得直接渲染未经筛选的 HTML。后端 message 以纯文本显示，并对长度做上限限制。

---

## 八、实时日志与产物

### 8.1 日志读取

日志在 Job 条目内展示，不使用独立页面；组件挂载时请求 `/artifacts/v1/projects/{project}/jobs/{job}/logs/content?stream=combined`，携带 `Range: bytes=-1048576`。

- 读取末尾 1 MiB；通过 `X-Committed-Bytes` 判断截断并提示，通过 `X-Log-State` 展示状态。
- 未 Completed 时每 5 秒刷新；上一请求未结束时不重叠请求，组件卸载时取消请求和定时器。
- 日志框提供刷新和全屏按钮；失败时展示错误，404/416 展示空日志。
- 当前未接入 SSE、加载更早内容或日志内搜索。Artifact Manager 的 SSE 契约见其设计文档，不能将服务端支持视为前端已接入。

### 8.2 渲染约束

日志以 Vue 文本插值渲染到 `pre`，不解析 HTML；每次只保留末尾窗口，不累计全量日志。

### 8.3 产物

展开 Job 后，Succeeded Job 通过 `GET /artifacts/v1/projects/{project}/jobs/{job}/artifacts?category=artifact` 获取产物，筛选 Completed 的 RPM 并按文件名排序；其他状态展示空列表。

下载使用 `/artifacts/v1/projects/{project}/jobs/{job}/artifacts/{artifactID}/content`，不暴露本地文件路径。

---

## 九、安全与隐私

### 9.1 信任边界

```text
Browser
   |
   | HTTPS，同源
   v
Ingress / Reverse Proxy
   |-- /auth, /apis ----------> ebs-gateway ----mTLS----> ebs-apiserver
   |-- /artifacts ------------> artifact-manager
   `-- /* --------------------> frontend static files
```

Gateway 到 apiserver 的客户端证书认证已支持显式配置；默认开发 Compose 仍关闭该校验。apiserver 尚未按组件细分接口权限，部署时须限制 apiserver 的网络访问。

外部网络不能访问 apiserver。生产环境禁止配置 `insecureSkipVerify`，内部服务使用受信任 CA。

### 9.2 浏览器安全

- 设置 CSP：默认只允许同源资源和连接；禁止 object；限制 frame ancestor。
- 设置 `X-Content-Type-Options: nosniff`、`Referrer-Policy` 和合理的 `Permissions-Policy`。
- 后端文本使用 Vue 文本插值，不使用 `v-html`。
- 外链只允许 `http:` 和 `https:`，新窗口同时设置 `noopener,noreferrer`。
- 不把 Token 放入 query、URL fragment、localStorage、错误监控或埋点。
- 不在客户端保存密码、Git 凭据、MachineAccount secret 或内部服务地址。
- 公开 EBS 对象的全部 `metadata/spec/status` 均可能被匿名读取，前端新增字段不能承载秘密。

### 9.3 写操作保护

- 删除 Project、User 和 MachineAccount 要求输入资源名称确认。
- 中止 Build 使用二次确认，并显示当前 phase。
- 表单提交期间禁用重复提交。
- 写请求超时后不得自动重放；先读取目标对象确认结果。
- Patch 携带 `resourceVersion`，冲突后由用户决定是否重新提交。

---

## 十、国际化、可访问性与响应式设计

### 10.1 国际化

首版支持 `zh-CN` 和 `en-US`。资源名称、后端 message、label key 和枚举原值不翻译；页面标题、操作、状态解释和时间格式使用当前语言。

文案按领域拆分：

```text
i18n/messages/zh-CN/{common,auth,project,build,job,runner,admin}.ts
i18n/messages/en-US/{common,auth,project,build,job,runner,admin}.ts
```

避免单个千行语言文件。CI 检查两种语言 key 集合一致。

### 10.2 可访问性

- 所有表单字段具有关联 label、错误说明和必填状态。
- 仅图标按钮必须提供可翻译的 `aria-label`。
- 状态不能只依靠颜色表达，同时显示文字或图标。
- 键盘可到达导航、表格操作、弹窗和日志工具栏。
- 焦点在弹窗关闭后回到触发按钮。
- 图表提供文字摘要或等价数据表。
- 正文与交互控件满足 WCAG 2.1 AA 对比度。

### 10.3 响应式

| 宽度 | 行为 |
|------|------|
| `>= 1200px` | 完整导航、双栏详情、宽表格 |
| `768px - 1199px` | 折叠次要导航，详情单栏，表格横向滚动 |
| `< 768px` | 抽屉导航、卡片式关键字段、表单单列 |

固定列和固定头部必须在 200% 浏览器缩放下验证，不能遮挡标题、导航或操作按钮。

---

## 十一、部署与配置

### 11.1 构建

前端使用多阶段镜像：Node LTS 构建静态文件，最终镜像使用受维护的非 root 静态服务器镜像。基础镜像必须固定明确版本或 digest，不能使用 `latest`。

构建产物包含 commit、构建时间和版本号，可在设置页查看，但不暴露内部路径、环境变量或凭据。

### 11.2 运行时配置

生产环境优先使用同源相对路径，不把后端 IP 编译进 JavaScript。需要可变配置时，由静态服务器输出不含秘密的 `/config.json`：

```json
{
  "apiBase": "",
  "artifactBase": "",
  "defaultLocale": "zh-CN"
}
```

允许配置的只是公共路径和 UI 默认值。内部 upstream 地址由 Ingress 或反向代理配置，不发送给浏览器。

### 11.3 健康检查

前端静态服务提供：

```text
GET /healthz
GET /readyz
```

`healthz` 只表示进程存活；`readyz` 检查入口文件存在。后端可用性在页面内分别展示，不能让 Gateway 暂时不可用导致静态前端容器退出。

---

## 十二、测试与质量门禁

### 12.1 单元测试

重点覆盖：

- scope 和 Project labels 到 UI 能力的映射。
- Kubernetes List、Status、continue token 和 selector 序列化。
- API 对象到表单草稿及 Patch 的转换。
- phase、conditions、时间和资源数量格式化。
- JWT 会话过期处理，但不在测试中把 payload 解析结果当成授权。
- 日志 Range 尾部窗口、截断提示、刷新、取消请求和空日志处理。

### 12.2 组件测试

重点覆盖登录、Project 表单、Build 创建、冲突处理、危险操作确认、分页、空状态和错误状态。

### 12.3 端到端测试

使用真实 Gateway 协议或协议级测试服务覆盖：

1. 匿名浏览 Project 和构建记录。
2. 注册、登录、刷新和过期退出。
3. User 创建 Project、添加成员并创建 Build，查看控制器生成的 Snapshot。
4. Member 可以修改子资源但不能删除。
5. Ops/Admin 可查看 Runner、驱逐和取消驱逐；覆盖普通用户路由拒绝、确认弹窗、UID 变化、resourceVersion 冲突和操作后刷新。Ops 相对于普通用户还可以额外管理 Config。
6. Admin 管理普通 User 和 MachineAccount。
7. Build 中止、409 冲突和 429 限流。
8. Job 内联日志加载、周期刷新、全屏和 RPM 下载。

### 12.4 验证与目标门禁

当前可执行 `npm run typecheck` 和 `npm run build` 验证类型与生产构建。前述单元、组件和端到端测试为验收范围，不表示测试流水线已全部落地；后续补齐自动化测试、格式检查、依赖审计和 API 契约检查。

依赖升级同步维护锁文件。

---

## 十三、可观测性

前端错误记录包含页面、操作类型、HTTP 状态码和服务端 request ID，不记录请求/响应正文、Token、密码、secret、完整资源或用户输入。

页面提供最近一次成功刷新时间，并区分：

- Gateway 不可达。
- Artifact Manager 不可达。
- 当前身份无权限。
- 单个资源请求失败。

前端性能监测至少关注首屏加载时间、路由切换时间、大列表渲染时间、日志积压量和未处理异常数。

---

## 十四、待完善能力

- OpenAPI 到 TypeScript 的自动类型生成及契约差异检查。
- 按 12 章补齐单元、组件和端到端自动化测试。
- 按需求接入 SSE、历史日志分段加载和大日志虚拟渲染；当前读取方式见第八章。
- 全局监控和趋势图需要先明确后端聚合接口，不通过遍历全部工程拼装统计。

---

## 十五、验收标准

前端首版完成需同时满足：

- 所有浏览器资源请求符合 Gateway 和 Artifact Manager 的公开契约。
- 浏览器和前端容器均无法直连 apiserver。
- 匿名、User Owner、User Member、Ops 和 Admin 五类访问场景的页面和操作与权限矩阵一致。
- 所有列表支持空、加载、错误、分页和刷新状态。
- 写操作处理 401、403、409、429 和请求结果未知场景。
- Job 内联日志按第八章加载和刷新，截断、请求失败与空日志均有明确提示。
- 中英文文案完整，桌面端和移动端主要流程可用。
- TypeScript 与生产构建检查通过；自动化测试及依赖审计按第十二章逐步纳入门禁。
- 生产构建不包含 mock 数据、内部地址、测试账号或凭据。
