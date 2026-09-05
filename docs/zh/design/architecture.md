# EulerMaker 架构设计

## 一、设计目标

EulerMaker 采用 Kubernetes-like 架构组织核心组件：以 `ebs-apiserver` 作为统一资源 API 和数据访问入口，以 etcd 和 Elasticsearch 作为组合主存储，通过资源 API 的 list/watch 或周期性 list 驱动控制器、调度器和执行机协作。

当前架构目标：

- 统一资源 API：Project、Snapshot、Build、BuildInfo、RpmRepo、BuildResource、Job、Runner 统一通过 `ebs/v1` API 暴露。
- 统一数据访问：业务组件不直接访问 etcd 和 Elasticsearch，统一通过 `ebs-apiserver` 读写资源。
- 声明式对象模型：资源由 `metadata/spec/status` 组成；提供 status 的资源将普通更新和 `/status` 更新分离。
- 事件与查询驱动协作：etcd 中的 Job、Runner 支持 watch；Elasticsearch 中的资源使用 list/get，不模拟 watch。
- Project 业务作用域：Snapshot、Build、BuildInfo、RpmRepo、BuildResource、Job 归属于 Project；除 BuildResource 外均提供内部全局 list，只有 Job 的全局 API 支持 watch。
- 构建结果数据面：`artifact-manager` 独立承载构建产物与实时日志正文，避免大文件流量经过资源 API 和 Gateway 数据转发链路。
- 可容器化部署：测试环境通过 `hacks/docker-compose.yml` 启动 etcd、Elasticsearch、`ebs-apiserver` 等组件。

---

## 二、整体架构

```text
用户 / 外部系统
      |
      v
┌──────────────────────────────────────────────┐       ┌────────────────────────┐
│                 ebs-gateway                  │<──────│         runner         │
│        认证、鉴权、审计、资源请求入口          │       │ Job 执行、心跳、产物上传 │
└──────────────────────┬───────────────────────┘       └───────────┬────────────┘
                       |                                           |
                       v                                           | 整文件上传/
┌───────────────────────────────────────────────┐                   | 实时日志追加
│                ebs-apiserver                  │                   v
│ ebs/v1 REST API / status / scoped list-watch  │       ┌────────────────────────┐
└───────────────┬──────────────────┬────────────┘       │    artifact-manager    │
                |                  |                    │ 上传、Manifest、日志 SSE │
                |                  |                    └───────────┬────────────┘
                v                  v                                |
┌──────────────────────────┐  ┌──────────────────────────┐          v
│           etcd           │  │      Elasticsearch        │  ┌────────────────────┐
│ Job/Runner：对象与 watch  │  │ 其他资源、IAM 与查询索引  │  │   本地持久化存储    │
└──────────────────────────┘  └──────────────────────────┘  └────────────────────┘

controllers / scheduler ── REST / list（Job 可 watch）──> ebs-apiserver
controllers ──读取 Completed Manifest / Artifact──> artifact-manager
Web UI ──Artifact 查询下载 / 日志 Range + SSE──> artifact-manager
artifact-manager ──Runner Token 校验──> ebs-gateway
```

核心原则：
- 所有资源读写最终都经过 `ebs-apiserver`；
- runner 的资源 API 统一访问 `ebs-gateway`，便于外部执行机和内部执行机使用同一套访问逻辑；构建正文直接访问 `artifact-manager`。
- etcd 和 Elasticsearch 都是主存储。
- etcd 保存 Job 和 Runner，提供原生 resourceVersion 与 list/watch；Elasticsearch 保存其余 `ebs/v1` 资源和 IAM 对象，提供 CRUD、分页与查询，但不支持 watch。
- `artifact-manager` 是构建产物、Job 上传清单和实时日志的数据服务；正文与私有元数据保存在其持久化目录，不写入 etcd 或 Elasticsearch。
- Runner 直接向 `artifact-manager` 传输文件和日志，`artifact-manager` 通过 `ebs-gateway` 的公开 Token 校验接口校验 Runner Token 的签名、有效期和 scope。首版不校验 Job 与 Runner 的绑定关系。

### 2.1 内部访问与信任边界

普通用户和外部系统不得直连 `ebs-apiserver`。`ebs-gateway`、scheduler 和 controller 是允许直连 `ebs-apiserver` 的受信任内部组件，各组件使用内部 CA 签发的独立 mTLS 客户端证书：

```text
用户 / 外部系统
       |
       | 匿名只读 GET/HEAD 或 JWT
       v
ebs-gateway ─── mTLS ───┐
                        |
scheduler ───── mTLS ───┼──> ebs-apiserver
                        |
controllers ─── mTLS ───┘

artifact-manager ── Runner JWT ──> ebs-gateway /auth/check（公开）
runner ── Runner JWT + 文件正文 ──> artifact-manager
```

`ebs-apiserver` 根据客户端证书的 URI SAN 识别内部调用方，并按组件职责对 API group、资源、verb 和 subresource 执行授权。Gateway、scheduler 和 controller 必须使用不同的证书身份，不得共享客户端证书；不同 controller 在需要进一步限制权限时使用独立身份。

建议的内部证书身份：

```text
spiffe://eulermaker/internal/ebs-gateway
spiffe://eulermaker/internal/ebs-scheduler
spiffe://eulermaker/internal/ebs-controller
```

内部授权遵循最小权限和默认拒绝原则：

- `ebs-gateway` 只访问代理请求、解析 User/MachineAccount 和调用 IAM 内部凭据接口所需的 API。
- scheduler 只访问调度所需的全局 Job、Runner 和相关 status API，不访问 User、密码或 Project 权限管理接口。
- controller 只访问其控制循环负责的资源和 status API；不同 controller 可以按职责继续拆分权限。
- `artifact-manager` 不直接访问 etcd、Elasticsearch 或 `ebs-apiserver`，只调用 `ebs-gateway` 的公开 Token 校验接口；首版只校验 Token，不查询 Job/Runner 对象或校验两者关系。
- 未被识别的客户端证书，以及已认证组件访问职责之外的资源或 verb，均由 `ebs-apiserver` 拒绝。
- `/internal/iam/*` 只允许 `ebs-gateway` 的 mTLS 身份调用，scheduler 和 controller 不得访问。

Gateway 允许匿名和已认证调用方通过 Project API get/list Project、Snapshot、Build、BuildInfo、RpmRepo 和 Job 的完整对象，并允许读取这些公开对象的单对象 `/status`；公开读取不按 Project owner/member 过滤。匿名请求不能 watch、写入或访问 Runner/IAM；携带 Token 的公开读取仍先完成 Token 和 User 状态校验。认证用户的写权限由 JWT、User 状态和 Project 用户权限确定。Gateway 必须删除客户端传入的所有 `X-EBS-*` 身份头；需要身份授权的认证请求只注入可信的 `X-EBS-User` 和 `X-EBS-Scopes`，公开读取使用 Gateway 内部身份访问 apiserver并原样转发对象响应。这些身份头只有在 mTLS 调用方确认为 `ebs-gateway` 时才可信。Scheduler 和 controller 直连 `ebs-apiserver` 时，权限来自各自的 mTLS 身份和 apiserver 内部授权，不使用外部 JWT scope，也不能通过伪造 `X-EBS-*` header 获得 gateway 权限。

用户、Runner和内部组件的认证链路分别为：

```text
用户注册：注册资料 -> gateway 校验与注册限流 -> gateway mTLS -> apiserver IAM 创建 User 与凭据
用户登录：账号密码 -> gateway 登录限流 -> gateway mTLS -> apiserver IAM 认证 -> gateway 签发 JWT
用户写入或非公开请求：JWT -> UserResolve -> gateway Project 用户权限校验 -> gateway mTLS -> apiserver
匿名读取：无 Token GET/HEAD（公开对象、collection 或单对象 `/status`）-> gateway 公开资源白名单与限流 -> gateway mTLS -> apiserver -> 完整对象响应
认证公开读取：JWT -> UserResolve -> gateway 公开资源白名单与认证限流 -> gateway mTLS -> apiserver -> 完整对象响应
机机账号创建：管理员 -> gateway -> apiserver IAM 原子创建 MachineAccount 与凭据
Runner换取token：MachineAccount client凭据和Runner名称 -> gateway交换限流 -> apiserver IAM认证 -> gateway签发短期Runner JWT
Runner请求：短期Runner JWT -> gateway Runner身份与字段授权 -> gateway mTLS -> apiserver
系统请求：scheduler/controller mTLS -> apiserver 内部资源与 verb 授权
```

自助注册不自动签发 JWT。Gateway 只接受注册所需的普通用户字段，并通过单一内部注册接口提交；apiserver 负责用户名唯一性以及 User 与密码凭据的一致性。Runner 使用 MachineAccount client secret 换取最长 24 小时的 `ebs:runner` JWT。所有 `/internal/iam/*` 接口只信任 gateway 的 mTLS 身份，不接受外部 JWT、scheduler 或 controller 调用。

部署时，`ebs-apiserver` 只暴露在内部网络，网络策略仅允许 gateway、scheduler 和 controller 连接。`ebs-gateway` 的 `/auth/check` 是公开接口：调用方无需服务身份且不提交请求正文，Gateway 校验请求携带的 Bearer Token 并返回身份与 scopes，同时执行限流。`artifact-manager` 根据响应确认 `ebs:runner` scope。Runner 仍统一通过 `ebs-gateway` 访问资源 API，不属于允许直连 `ebs-apiserver` 的组件；文件正文和实时日志则直接上传到 `artifact-manager`。

---

## 三、核心组件

| 组件 | 职责 |
|------|------|
| `ebs-gateway` | 系统入口，负责匿名公开读取、认证、鉴权、审计和请求转发 |
| `ebs-apiserver` | 统一资源 API，负责对象校验、默认值、存储访问、list，以及 Job/Runner watch 和各资源子资源 |
| `etcd` | Job、Runner 的主存储，提供原生 resourceVersion 和 list/watch |
| `Elasticsearch` | Project、Snapshot、Build、BuildInfo、RpmRepo、BuildResource 和 IAM 对象的主存储，提供 CRUD、分页和查询 |
| `controllers` | 通过 API 查询或监听职责内对象，推进 Snapshot、Build、RpmRepo 等资源状态 |
| `scheduler` | 监听全局 Job，选择 Runner 并更新 Job 状态 |
| `runner` | 通过 ebs-gateway 注册 Runner、上报心跳，并通过自身范围 Job list-watch 接收已分配任务 |
| `artifact-manager` | 接收 Runner 的构建产物和实时日志，负责流式落盘、完整性校验、幂等、Job 上传清单、查询下载及日志 SSE |
| `ebsctl` | 面向用户和运维的命令行客户端，首版通过 Gateway 操作资源 |

`ebsctl` 的命令、context、资源操作和输出协议见 [ebsctl.md](./ebsctl.md)。

---

## 四、资源模型

当前 API group：

```text
apiVersion: ebs/v1
```

当前顶层资源：

| 资源 | 作用域 | 说明 |
|------|--------|------|
| Project | 集群级 | 项目，是所有 Project 级资源的业务归属 |
| Snapshot | Project 级 | 项目快照 |
| Build | Project 级 | 构建任务 |
| BuildInfo | Project 级 | 软件包构建依赖与结果信息 |
| RpmRepo | Project 级 | RPM 仓库解析和发布信息 |
| BuildResource | Project 级 | 构建资源策略 |
| Job | Project 级 | 可调度执行任务 |
| Runner | 集群级 | 执行机 |

所有资源遵循 Kubernetes 风格对象结构：

```yaml
apiVersion: ebs/v1
kind: Project
metadata:
  name: openeuler-22-03-lts
spec:
  displayName: openEuler 22.03 LTS
status:
  phase: Active
```

Project 名用于内部 scoped storage，需要满足 DNS1123 label 约束，只能使用小写字母、数字和 `-`，不能包含 `.`。`default` 是系统保留作用域，不允许创建同名 Project。页面展示名称使用 `spec.displayName`。

IAM 资源使用独立的 `iam.ebs/v1` API group，包括 User 和 MachineAccount。

详细字段定义见 [data-models.md](./data-models.md)。

---

## 五、API 设计

### 5.1 Project API

```text
GET    /apis/ebs/v1/projects
POST   /apis/ebs/v1/projects
GET    /apis/ebs/v1/projects/{name}
PUT    /apis/ebs/v1/projects/{name}
PATCH  /apis/ebs/v1/projects/{name}
DELETE /apis/ebs/v1/projects/{name}
PUT    /apis/ebs/v1/projects/{name}/status
```

### 5.2 Project 子资源 API

用户侧通过 Project API 访问 Project 级资源：

```text
/apis/ebs/v1/projects/{project}/snapshots
/apis/ebs/v1/projects/{project}/builds
/apis/ebs/v1/projects/{project}/buildinfos
/apis/ebs/v1/projects/{project}/rpmrepos
/apis/ebs/v1/projects/{project}/buildresources
/apis/ebs/v1/projects/{project}/jobs
```

`{project}` 是对象的唯一项目归属来源，`spec` 中不重复保存 `projectName`。BuildResource 不属于匿名公开读取资源，普通 Project owner/member 仅可读取，写操作仅允许 Ops 以上身份。BuildResource 只提供 list/create/get/update/delete，不提供 patch、watch、`/status` 或全局 API。

### 5.3 内部全局系统 API

调度器和控制器使用各自的 mTLS 身份直连 apiserver，通过内部全局 API 跨 Project list。这些路径不经 Gateway，也不对外部客户端开放：

```text
/apis/ebs/v1/snapshots
/apis/ebs/v1/builds
/apis/ebs/v1/buildinfos
/apis/ebs/v1/rpmrepos
/apis/ebs/v1/jobs
```

只有 Job 和 Runner 使用 etcd 并支持 watch。典型的全局 Job watch：

```bash
curl -k -N 'https://localhost:8443/apis/ebs/v1/jobs?watch=true'
```

BuildResource 不注册 `/apis/ebs/v1/buildresources`；系统组件也必须指定 Project 路径访问。

### 5.4 Runner API

```text
GET    /apis/ebs/v1/runners
POST   /apis/ebs/v1/runners
GET    /apis/ebs/v1/runners/{name}
PUT    /apis/ebs/v1/runners/{name}
PATCH  /apis/ebs/v1/runners/{name}
DELETE /apis/ebs/v1/runners/{name}
PUT    /apis/ebs/v1/runners/{name}/status
GET    /apis/ebs/v1/runners/{name}/jobs
GET    /apis/ebs/v1/runners/{name}/jobs?watch=true
```

上述是资源支持的完整 API 集合，不代表 Runner token 拥有全部 verb。Runner 使用受限自注册模型：只能创建、读取和受限更新名称与 JWT `sub`、`runner` claim 一致的自身对象，并更新自身 `/status`；`metadata.name` 本身不可修改。Runner 不能 list/watch Runner 集合、访问其他 Runner 或执行 DELETE。Runner 普通对象更新仅允许自身声明的 type、arch、hostname 和能力 labels，`unschedulable`、taints、zone 及其他管理字段由 system 调用方维护。详细字段规则见 [ebs-gateway.md](./ebs-gateway.md) 和 [runner.md](./runner.md)。

`/runners/{name}/jobs` 提供自身已分配 Job 的 list-watch，gateway强制路径名称与 Runner token身份一致，apiserver再按 `Job.status.runner={name}` 进行可信过滤。Runner先 list并记录 resourceVersion，再从该版本建立带超时和 BOOKMARK 的 watch；resourceVersion失效时重新 list。

---

## 六、ebs-apiserver

`ebs-apiserver` 基于 `k8s.io/apiserver` 的 `GenericAPIServer` 实现，复用 Kubernetes apiserver 的资源注册、REST storage、对象元数据机制，以及 etcd 资源的 watch、resourceVersion 和 `/status` 能力；Elasticsearch 资源由 ESStore 提供 CRUD、list 和对应子资源。

常规 Project 子资源 API 由 `project_alias.go` 提供轻量适配：

```text
/apis/ebs/v1/projects/{project}/builds
```

会在服务端重写到内部 scoped storage 路径：

```text
/apis/ebs/v1/namespaces/{project}/builds
```

该内部路径只作为实现细节，外部文档和业务调用统一使用 Project API 和全局系统 API。BuildResource 使用独立的 Project scoped 路由实现，并刻意不注册全局 API。apiserver 在进入 Ready 前幂等确保 `default/default` BuildResource 存在；已有对象不会被启动配置覆盖。

详细实现见 [ebs-apiserver.md](./ebs-apiserver.md)。

---

## 七、存储设计

etcd 只保存需要原生 watch 的 Job 和 Runner：

```text
/registry/ebs/jobs/{project}/{name}
/registry/ebs/runners/{name}
```

Job 的 `{project}/{name}` 布局同时支持全局和 Project 范围 list/watch；Runner 是集群级资源。

Elasticsearch 索引：

```text
ebs-projects
ebs-snapshots
ebs-builds
ebs-buildinfos
ebs-rpmrepos
ebs-buildresources
ebs-users
ebs-machineaccounts
```

Project scoped 的 ES 对象统一使用 `{project}/{name}` 作为文档 ID。User、MachineAccount 使用各自名称定位。

---

## 八、组件协作流程

### 8.1 用户请求

```text
注册：用户 -> ebs-gateway -> ebs-apiserver IAM -> User + 密码凭据
登录：用户 -> ebs-gateway -> ebs-apiserver IAM -> JWT
用户 -> ebs-gateway -> ebs-apiserver -> etcd / Elasticsearch
```

`ebs-gateway` 负责注册和登录入口、认证鉴权及请求代理，`ebs-apiserver` 负责 User/凭据一致性、资源语义和数据访问。注册成功不自动登录，用户需再通过登录接口获取 JWT。

### 8.2 Controller 流程

```text
controller -> list/get Project、Snapshot、Build、BuildInfo、RpmRepo
controller -> watch Job（需要事件流时）
build controller -> get Project BuildResource；不存在时 get default/default
controller -> create/update Snapshot、Build、BuildInfo、RpmRepo、Job
controller -> update status
```

controller 只通过 API 操作资源，不直接访问 etcd 或 Elasticsearch。ES-only 资源不支持 watch，控制器需使用分页 list、显式触发或周期性协调。

### 8.3 Scheduler 流程

```text
scheduler -> watch /apis/ebs/v1/jobs
scheduler -> 过滤 Pending Job
scheduler -> 选择 Runner
scheduler -> update Job.status.runner / phase
```

Scheduler 使用全局 Job API，不需要逐个 Project 建立 watch。

### 8.4 Runner 流程

```text
runner -> ebs-gateway: exchange MachineAccount credential for short-lived Runner JWT
runner -> ebs-gateway -> ebs-apiserver: register Runner
runner -> ebs-gateway -> ebs-apiserver: update Runner.status heartbeat
runner -> ebs-gateway -> ebs-apiserver: list/watch /runners/{runner}/jobs
runner -> execute Job
runner -> artifact-manager: single-request streaming upload artifacts
runner -> artifact-manager: append and finalize real-time container logs
runner -> artifact-manager: complete immutable Job upload manifest
runner -> ebs-gateway -> ebs-apiserver: update Job.status
```

Runner 作为集群级资源存在，调度标签使用 `metadata.labels`，资源容量和运行状态写入 `status`。runner 不直接访问 `ebs-apiserver`，资源操作统一访问 `ebs-gateway`，外部执行机和内部执行机使用同一套客户端逻辑。构建产物和日志正文直接上传到 `artifact-manager`，避免大文件经过 Gateway；Artifact Manager 将 Runner Token 发送给 Gateway 校验签名、有效期和 scope。

### 8.5 Artifact 与 repo 流程

```text
runner -> artifact-manager: upload Completed Artifact
runner -> artifact-manager: finalize logs/container.log Artifact
runner -> artifact-manager: complete immutable JobUploadManifest
runner -> ebs-gateway -> ebs-apiserver: update Job artifact summary/status
repo controller -> watch Job status
repo controller -> artifact-manager: query fixed manifest generation and verify digest
repo controller -> artifact-manager: stream-download RPM Artifacts
repo controller -> generate and publish RPM repository
repo controller -> ebs-apiserver: update RpmRepo/Build/Job status
```

`artifact-manager` 的本地 Job 上传清单是一次 Job 完整文件集合的事实来源；Job status 只保存 `artifactState`、`artifactGeneration`、`artifactDigest` 和 `artifactCount` 等可 watch 摘要。Repo Controller 不能扫描 Artifact Manager 的内部目录，也不能根据当前 Artifact 列表推断上传是否结束。

Web UI 通过 Artifact API 查询和下载 Completed Artifact。实时日志先使用 Range 读取已提交历史内容，再通过 SSE 接收新增 chunk；日志封账后转为普通 `category=log` Artifact。Artifact Manager 的详细协议、数据结构和本地恢复规则见 [artifact-manager.md](./artifact-manager.md)。

---

## 九、部署

测试环境使用 `hacks/docker-compose.yml` 启动：

```text
etcd
Elasticsearch
ebs-apiserver
ebs-gateway
artifact-manager
ebs-runner
```

启动命令：

```bash
docker compose -f hacks/docker-compose.yml up -d
```

关键服务地址：

| 服务 | 地址 |
|------|------|
| etcd | `http://localhost:2379` |
| Elasticsearch | `http://localhost:9200` |
| ebs-apiserver | `https://localhost:8443` |
| ebs-gateway | `http://localhost:8080` |
| artifact-manager | `http://localhost:8081` |

---

## 十、后续完善

当前架构后续主要完善方向：

- 持续校验 OpenAPI schema 与实际资源模型的一致性。
- 细化各 controller、scheduler 的内部 mTLS 最小权限策略。
- 补齐 controller，并完善 scheduler、runner 的生产级故障恢复与可观测性。
- 接入正式的镜像构建和发布流程。
