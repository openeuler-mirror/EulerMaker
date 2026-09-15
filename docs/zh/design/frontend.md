# EulerMaker 前端设计

## 一、文档状态

本文定义 EulerMaker Web 控制台的首版产品范围、技术架构、页面结构、权限行为、API 对接、实时日志、安全边界、部署方式和验收标准。

本文状态为 **Proposed**。实现应以当前 `ebs/v1`、`iam.ebs/v1`、Gateway 和 Artifact Manager 契约为准；资源字段的权威定义仍位于 [`api/ebs/v1`](../../../api/ebs/v1) 和 [`components/ebs-apiserver/pkg/apis/iam/v1`](../../../components/ebs-apiserver/pkg/apis/iam/v1)。

---

## 二、背景与目标

EulerMaker 已具备 Project、Snapshot、Build、BuildInfo、RpmRepo、BuildResource、Job 和 Runner 等 Kubernetes 风格资源，以及用户认证、Project 授权、产物下载和实时日志能力。当前需要一个面向社区用户、项目维护者和系统运维人员的 Web 控制台，把资源 API 组织成可理解、可操作的构建工作流。

首版目标：

- 匿名用户可以浏览 Project 及其公开构建资源。
- 注册用户可以创建 Project，管理自己拥有或参与的 Project，并发起构建。
- Ops 可以管理 BuildResource，并查看 Runner 运行状态。
- Admin 可以管理普通 User 和 MachineAccount，并具备系统级资源能力。
- 前端能力模型与 System 身份保持兼容，但 System 是受信任自动化身份，不提供账号密码登录入口。
- 用户可以查看 Build、Job、RPM 仓库和产物状态，并实时查看 Job 日志。
- 前端严格遵守 Gateway 的认证和授权边界，不直连 `ebs-apiserver`。
- API 类型、查询和错误处理集中管理，避免页面直接拼装后端请求。

首版不包括：

- 在浏览器中编辑任意资源 YAML 或 JSON。
- 修改 Job、Runner 或控制器维护的 `status`。
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

列表使用服务端分页。Project 详情按页签加载资源，不在进入页面时并发读取所有子资源。日志和大对象使用 Range、SSE 或下载接口，不一次性保存在 Pinia。

### 3.6 失败必须可见

实时请求失败时显示错误和重试入口，不能自动回退到 mock 数据。演示数据只能由开发或显式演示模式启用，并持续显示“演示数据”标识。

---

## 四、用户与权限

### 4.1 身份类型

| 身份 | Scope | 前端职责 |
|------|-------|----------|
| 匿名用户 | 无 | 浏览公开 Project、Snapshot、Build、BuildInfo、RpmRepo 和 Job |
| 普通用户 | `ebs:user` | 创建 Project；操作自己拥有或参与的 Project |
| 运维用户 | `ebs:ops` | 管理 BuildResource；只读查看 Runner |
| 管理员 | `ebs:admin` | 管理非管理员 User 和 MachineAccount；具备系统级业务资源能力 |
| 系统身份 | `ebs:system` | 受信任自动化调用方；前端不提供登录入口 |

这些 scope 是互斥身份，不应组合推导新角色。正常交互式登录只会获得 `ebs:user`、`ebs:ops` 或 `ebs:admin`。System 和 Runner 都是机机身份，不能通过 Web 控制台登录；能力矩阵保留 System 列是为了使界面判断与 Gateway 契约完整对应。

### 4.2 Project 关系

前端从 Project labels 解释普通用户与 Project 的关系：

```text
ebs.io/owner-user: <username>
ebs.io/member-user.<username>: "true"
```

页面可计算 `owner`、`member` 和 `none` 三种关系。创建 Project 时不允许普通用户指定 owner；Gateway 会使用 JWT `sub` 写入 `ebs.io/owner-user`。这些 labels 管理写权限，不表示 Project 的读取可见性；Gateway 白名单内的 Project 及其业务资源均允许匿名读取。

### 4.3 页面能力矩阵

| 能力 | 匿名 | User Owner | User Member | Ops | System | Admin |
|------|------|------------|-------------|-----|--------|-------|
| 浏览公开业务资源 | 是 | 是 | 是 | 是 | 是 | 是 |
| 创建 Project | 否 | 是 | 是 | 否 | 是 | 是 |
| 修改/删除 Project | 否 | 修改、删除 | 否 | 否 | 是 | 是 |
| 管理 Project 成员 | 否 | 是 | 否 | 否 | 是 | 是 |
| 创建/修改 Project 子资源 | 否 | 是 | 是 | 否 | 是 | 是 |
| 删除 Project 子资源 | 否 | 是 | 否 | 否 | 是 | 是 |
| 读取 BuildResource | 否 | 所属 Project | 所属 Project | 全部 | 全部 | 全部 |
| 修改 BuildResource | 否 | 否 | 否 | 是 | 是 | 是 |
| 查看 Runner | 否 | 否 | 否 | 是 | 是 | 是 |
| 管理 Runner | 否 | 否 | 否 | 否 | 是 | 是 |
| 管理 User/MachineAccount | 否 | 否 | 否 | 否 | 否 | 是 |

按钮是否显示由前端能力函数统一判断，例如 `canEditProject(identity, project)`；页面不得散落 scope 字符串比较。服务端返回 403 时，以服务端结果为准并刷新当前资源。

---

## 五、信息架构

### 5.1 路由

```text
/
/login
/register
/projects
/projects/:project
/projects/:project/packages/:package
/projects/:project/snapshots
/projects/:project/builds
/projects/:project/builds/:build
/projects/:project/buildinfos
/projects/:project/rpmrepos
/projects/:project/jobs
/projects/:project/jobs/:job
/projects/:project/buildresources
/runners
/runners/:runner
/admin/users
/admin/machineaccounts
/settings
```

Project 详情的资源路由在桌面端表现为页签，在窄屏表现为二级菜单。单个 Build 和 Job 使用独立 URL；抽屉只能作为列表中的快捷预览，不能成为唯一详情入口。

### 5.2 全局导航

全局导航包括：

- 首页
- 工程
- Runner，仅 Ops、System 和 Admin 显示
- 用户管理，仅 Admin 显示
- 账号菜单：个人信息、语言、主题、退出

导航在小屏设备折叠为菜单。不得在固定宽度头部同时排列全部导航、法律链接和设置按钮。

### 5.3 首页

匿名和普通用户首页展示：

- EulerMaker 简介和主要操作入口。
- Project 列表摘要。
- 用户已登录时展示“我拥有的工程”和“我参与的工程”。

System 和 Admin 可以额外展示全局资源指标、Build 趋势、Job 队列深度和 Runner 状态。普通浏览页面不能为了全局统计对每个 Project 发起子资源请求，避免 N+1 请求和不完整统计。

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

- “创建工程”使用结构化表单填写工程名、显示名称、说明、SPEC 分支和首个 Build target。工程名在客户端按 DNS label 规则预检；普通用户创建的工程由 Gateway 注入 owner，Admin/System 创建时需指定已启用的普通用户作为 owner。
- “导入 YAML”读取或粘贴单个 `ebs/v1 Project` 清单。客户端解析 YAML，校验 `apiVersion`、`kind`、工程名和至少一个包含 `os`、`arch` 的 Build target；提交前移除 `status` 和服务器维护的 metadata 字段。该入口只用于创建 Project，不构成通用资源 YAML 编辑器。

两个入口共用 `POST /apis/ebs/v1/projects`。匿名用户点击后进入登录页并返回工程列表；Ops 不具备创建权限。409 冲突、校验错误和权限错误都在弹窗中保留输入并以当前界面语言展示。

### 5.5 工程详情

工程页面使用三个一级 Tab 组织信息：“工程详情”展示资源摘要、基础信息和最近构建；“构建历史”展示 Build 类型、目标、状态与起止时间；“工程配置”展示 Project spec、Build targets、Package repositories 和 Bootstrap repositories。页签状态写入 `tab` 查询参数，可刷新和分享。

构建历史采用主从布局：左侧可滚动列表展示构建名称、状态、类型、目标与开始时间，默认选中第一条；右侧展示所选 Build 的阶段、起止时间、结果仓库、基础构建、软件包和 Bootstrap repositories。窄屏设备按列表、详情的顺序改为上下布局。

工程配置中的 Build targets 使用列表展示 `os`、`arch`、`buildFlag` 和 `publishFlag`。工程 owner、Admin 和 System 可通过编辑弹窗新增、删除或修改目标，并以包含当前 `resourceVersion` 的完整 Project 执行 PUT；至少保留一个同时包含 `os` 与 `arch` 的目标。发生 409 时保留弹窗内容并提示重新加载后处理冲突。

Bootstrap repositories 同样向工程 owner、Admin 和 System 提供列表编辑能力。每项包含名称与仓库地址，允许新增、删除、修改或保存空列表以清除配置，并复用 Project PUT、`resourceVersion` 和冲突处理流程。

工程配置采用左右分区：左侧纵向展示基础配置、Build targets 与 Bootstrap repositories，右侧使用等高独立面板完整展示 `buildPayload`；Package repositories 位于底部整行。窄屏设备按基础配置、Build targets、Bootstrap repositories、Build payload、Package repositories 的顺序折叠为单列。

具有工程编辑权限的用户可在 Build payload 面板内切换编辑状态。编辑时只读代码块替换为保留换行的多行文本框，并提供取消和保存操作；保存复用 Project PUT 与冲突处理，失败时保留草稿。

工程概览展示 Project 配置、成员、目标环境以及各资源的状态摘要。后续子页面职责如下：

详情页提供“导出 YAML”操作，将 Gateway 返回的完整 Project 对象下载为 `<metadata.name>.yaml`。导出包含 metadata、spec 和 status，适合存档与诊断；通过工程导入入口再次创建时，客户端会移除 status 和服务器维护的 metadata 字段。

| 页面 | 内容与操作 |
|------|------------|
| Packages | 展示和维护 `spec.packageRepos`，显示 Git ref 和 Build targets |
| Snapshots | 查看准备状态、各仓库解析结果和 conditions；允许创建 Snapshot |
| Builds | 查看类型、目标、包列表、阶段、结果仓库和 conditions；允许创建和中止 Build |
| BuildInfo | 查看 SPEC 元数据、构建依赖、安装依赖和对应 Job |
| RPM Repositories | 分别展示过程仓 `status.repository` 与发布 `status.release` |
| Jobs | 查看调度、Runner、阶段、资源请求、结果和日志入口 |
| Build Resources | 查看包和架构级资源规则；只有 Ops/System/Admin 可编辑 |

Project 普通配置使用结构化表单。复杂且可能丢失未知字段的对象不能通过“读取—局部表单—整体 PUT”更新，应发送 Merge Patch，并携带当前 `resourceVersion`。发生 409 时保留用户草稿，重新加载服务端对象并提示用户比较后重试。

### 5.6 Build 创建流程

Build 创建采用三步表单：

1. 选择 Build 类型和软件包。
2. 选择 Project 已配置的 Build target，并设置允许用户覆盖的 Bootstrap repository。
3. 展示即将提交的资源摘要并确认。

请求体严格使用当前 `BuildSpec`：

```yaml
apiVersion: ebs/v1
kind: Build
metadata:
  name: build-20260915-001
spec:
  buildType: incremental
  packages:
    - kernel
  buildTarget:
    os: openEuler-24.03-LTS
    arch: x86_64
  bootstrapRepo:
    - name: bootstrap
      repo: https://example.invalid/repo
```

前端不得提交已经移除的 `snapshotName` 或 `prevBuildRepo`。基础构建关系从服务端 `status.baseBuildRef` 展示。

### 5.7 Runner 页面

Runner 页面展示：

- 名称、`spec.instanceId`、类型、架构和是否不可调度。
- phase、heartbeat、capacity、allocatable。
- 地址、操作系统、内核、运行时和 Agent 版本。
- labels、taints 和 conditions。

Ops 只有只读能力。System/Admin 的调度开关或 taint 修改必须按照 Gateway 允许字段生成 Patch，不提供直接 status 编辑入口。

### 5.8 用户与 MachineAccount 管理

Admin 用户管理页支持：

- 列出和读取非 Admin User。
- 修改 `enabled`、单一 scope、`displayName` 和 `email`。
- 删除非 Admin User。
- 修改自己的密码。

User 只能通过 `/auth/register` 创建。前端不提供“管理员创建 User 并代设初始密码”的流程，因为 Gateway 的 User API 不接受 POST，Admin 也不能重置其他用户的密码。

MachineAccount 页面支持通过专用接口创建账号、列出和删除账号。创建时由浏览器 Web Crypto API 生成至少 32 字节随机值并编码为无填充 Base64URL client secret，再与账号配置一并提交。Gateway 成功响应不会回显 secret，因此页面只使用提交前的内存副本展示一次；离开结果页面后不再保留，不写入日志、Pinia、localStorage 或 sessionStorage。用户关闭一次性凭据页面前，界面必须明确提示其通过受保护渠道完成下发。

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
| Element Plus | 表单、表格、弹窗、分页、提示和无障碍基础组件 |
| ECharts | 趋势和资源分布图；按需加载 |
| xterm.js 或虚拟列表 | 大体量实时日志渲染 |
| vue-i18n | `zh-CN` 和 `en-US` 国际化 |

首版代码放在仓库的 `frontend/` 目录，与 API 变更在同一 Pull Request 中完成契约更新和验证。

### 6.2 目录结构

```text
frontend/
├── index.html
├── package.json
├── vite.config.ts
├── tsconfig.json
├── Dockerfile
├── src/
│   ├── main.ts
│   ├── App.vue
│   ├── api/
│   │   ├── client.ts
│   │   ├── auth.ts
│   │   ├── artifacts.ts
│   │   ├── resources.ts
│   │   └── generated/
│   │       └── schema.ts
│   ├── auth/
│   │   ├── capabilities.ts
│   │   └── session.ts
│   ├── components/
│   │   ├── charts/
│   │   ├── forms/
│   │   ├── layout/
│   │   ├── logs/
│   │   └── resources/
│   ├── composables/
│   ├── i18n/
│   ├── router/
│   ├── stores/
│   ├── styles/
│   ├── views/
│   └── utils/
└── tests/
    ├── unit/
    ├── component/
    └── e2e/
```

页面组件建议控制在 500 行以内。Project 详情的表单、列表和状态摘要分别拆成领域组件，避免单个页面同时承担请求、权限、表单转换和渲染。

### 6.3 API 类型生成

`src/api/generated/schema.ts` 从 apiserver OpenAPI 定义生成，不手写复制 Go 结构体。生成文件不直接暴露给页面；`resources.ts` 将生成类型转换为前端稳定的领域类型和表单草稿。

API 类型变更流程：

```text
修改 Go API 类型
  -> 更新 DeepCopy 和 OpenAPI
  -> 生成 TypeScript schema
  -> TypeScript 编译检查
  -> API 契约测试
```

CI 检查生成结果无未提交差异，防止主干 API 与前端类型漂移。

### 6.4 状态管理

Pinia 只保存：

- 当前会话身份和 Token。
- 语言、主题、每页数量等用户偏好。
- 跨页面需要共享的少量资源摘要。

列表、详情、表单草稿和日志状态尽量由页面 composable 管理。资源缓存以完整 API 路径、query 和 `resourceVersion` 为键；写操作成功后失效相关列表和详情缓存。

不把全部 Project 子资源预加载进全局 store。

### 6.5 路由加载与构建产物

除首页和基础布局外，页面使用动态 import。ECharts、xterm.js、YAML 等大依赖只在对应页面加载。生产构建应形成基础框架、图表、日志和各路由页面等独立 chunk。

首版性能目标：

- 初始入口 JS gzip 不超过 250 KiB。
- 普通列表页面不加载 ECharts 和 xterm.js。
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
  scopes: Array<"ebs:user" | "ebs:ops" | "ebs:admin" | "ebs:system">;
  expiresAt: number;
}
```

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
| Project、Snapshot、Build、BuildInfo、RpmRepo、BuildResource | 用户主动刷新；运行中详情可每 10 秒轮询 |
| Job | Project 页面每 5 秒轮询；后续可接入 Project Job watch |
| Runner | Ops 每 10 秒轮询；System/Admin 后续可使用 watch |
| Job 日志 | Range 获取历史内容，SSE 接收增量 |

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

### 8.1 日志状态机

```text
LoadingHistory -> ConnectingSSE -> Streaming -> Completed
                         ^              |
                         |              v
                  CatchingUpByRange <- Disconnected
```

首次打开 Job 日志：

1. 请求 `/artifacts/v1/projects/{project}/jobs/{job}/logs/content`。
2. 大日志默认读取末尾窗口，并提供“加载更早内容”。
3. 保存 `X-Committed-Bytes` 和 `X-Log-Next-Sequence`。
4. 日志未完成时，以 `afterSequence` 建立 SSE。
5. 收到 `complete` 后关闭 SSE，刷新解码器并展示完整 Artifact 下载入口。

SSE `log` 事件中的 Base64 内容先还原为字节，再使用 `TextDecoder("utf-8", { stream: true })` 增量解码，避免多字节字符跨 chunk 时损坏。

连接断开或回放窗口失效后，使用：

```http
Range: bytes={committedOffset}-
```

补齐内容，再以新的 sequence 建立 SSE。不能依赖原生 `EventSource` 解析 409 响应正文。

### 8.2 渲染约束

- 使用虚拟列表或终端组件，仅保留有限可视 DOM。
- 日志事件批量刷新，频率不高于每动画帧一次。
- 用户滚离底部时暂停自动滚动，但继续接收并记录 offset/sequence。
- 页面隐藏时降低渲染频率，不能停止消费事件。
- 搜索只作用于已加载日志；完整日志搜索引导用户下载后处理。

### 8.3 产物

Job 详情读取 Manifest 后按 category 和 relative path 展示产物。下载链接使用 Artifact ID，不在 URL 中暴露本地存储路径。页面展示 size、SHA-256、媒体类型和创建时间，并为校验值提供复制按钮。

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
- 日志 Base64、UTF-8 增量解码和断线补齐状态机。

### 12.2 组件测试

重点覆盖登录、Project 表单、Build 创建、冲突处理、危险操作确认、分页、空状态和错误状态。

### 12.3 端到端测试

使用真实 Gateway 协议或协议级测试服务覆盖：

1. 匿名浏览 Project 和构建记录。
2. 注册、登录、刷新和过期退出。
3. User 创建 Project、添加成员、创建 Snapshot 和 Build。
4. Member 可以修改子资源但不能删除。
5. Ops 管理 BuildResource 并只读查看 Runner。
6. Admin 管理普通 User 和 MachineAccount。
7. Build 中止、409 冲突和 429 限流。
8. Job 日志历史加载、SSE 增量、断线补齐和完成下载。

### 12.4 CI 门禁

每个前端变更至少执行：

```text
格式检查
ESLint
TypeScript strict 检查
单元与组件测试
生产构建
依赖高危漏洞审计
OpenAPI 生成差异检查
关键端到端冒烟测试
```

依赖升级使用锁文件。CI 不运行来源不明的安装脚本；确需安装脚本的依赖进入显式允许列表。

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

## 十四、分阶段交付

### 阶段一：浏览和认证

- 建立 Vue/TypeScript/Vite 工程和 API 类型生成流程。
- 完成布局、路由、国际化和主题。
- 完成注册、登录、Token 校验和退出。
- 完成 Project 列表、Project 详情及各资源只读页面。

### 阶段二：Project 工作流

- 创建和修改 Project。
- 管理 Project 成员。
- 维护 package repositories 和 build targets。
- 创建 Snapshot、Build，中止 Build。

### 阶段三：运行与产物

- Job 和 Runner 页面。
- Artifact、Manifest 和下载入口。
- Range + SSE 实时日志。
- System/Admin 全局监控。

### 阶段四：运维与管理

- BuildResource 管理。
- User 和 MachineAccount 管理。
- 性能、可访问性和端到端测试加固。

每个阶段均应连接真实 Gateway 契约完成验收，不能以 mock 页面完成作为阶段终点。

---

## 十五、与 PR 165 的关系

PR 165 可作为视觉和工程原型，以下内容可以选择性迁移：

- Vue 3、Vite、Element Plus、Pinia、ECharts 和 vue-i18n 的基础配置。
- 品牌资源、状态标签、图表、主题和时间工具函数。
- 登录、注册、列表和详情页的信息组织方式。

以下内容必须按本文重新实现：

- 删除 `/public-apis` 及前端容器直连 apiserver 的代理。
- 使用当前 `ebs:user`、`ebs:ops`、`ebs:admin`、`ebs:system` 权限模型。
- 使用 `ebs.io/owner-user` 和 `ebs.io/member-user.*` Project labels。
- 从当前 OpenAPI 生成类型，移除旧的 Build、Snapshot、RpmRepo、PackageRepo 和 Runner 字段。
- 拆分大页面和语言文件，增加路由懒加载。
- 增加权限、表单、API 契约、日志和端到端测试。

---

## 十六、验收标准

前端首版完成需同时满足：

- 所有浏览器资源请求符合 Gateway 和 Artifact Manager 的公开契约。
- 浏览器和前端容器均无法直连 apiserver。
- 匿名、User Owner、User Member、Ops、System 和 Admin 六类访问场景的页面和操作与权限矩阵一致。
- 不再使用当前 API 已移除的字段。
- 所有列表支持空、加载、错误、分页和刷新状态。
- 写操作处理 401、403、409、429 和请求结果未知场景。
- Job 日志可以从历史内容无缝衔接 SSE，断线后不丢失、不重复展示内容。
- 中英文文案完整，桌面端和移动端主要流程可用。
- TypeScript、测试、生产构建、依赖审计和 OpenAPI 差异检查全部通过。
- 生产构建不包含 mock 数据、内部地址、测试账号或凭据。