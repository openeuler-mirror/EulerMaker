# EulerMaker 架构设计

## 一、设计目标

EulerMaker 采用 Kubernetes-like 架构组织核心组件：以 `ebs-apiserver` 作为统一资源 API 和数据访问入口，以 etcd 和 Elasticsearch 作为组合主存储，通过 list/watch 驱动控制器、调度器和执行机协作。

当前架构目标：

- 统一资源 API：Project、Snapshot、Build、Job、Runner 统一通过 `ebs/v1` API 暴露。
- 统一数据访问：业务组件不直接访问 etcd 和 Elasticsearch，统一通过 `ebs-apiserver` 读写资源。
- 声明式对象模型：资源由 `metadata/spec/status` 组成，普通更新和 `/status` 更新分离。
- Watch 驱动协作：controller、scheduler、runner 通过 watch 获取资源变化。
- Project 业务作用域：Snapshot、Build、Job 归属于 Project，同时提供全局 list/watch API 给系统组件。
- 可容器化部署：测试环境通过 `hacks/docker-compose.yml` 启动 etcd、Elasticsearch 和 `ebs-apiserver`等组件。

---

## 二、整体架构

```
用户 / 外部系统
      |
      v
┌──────────────────────────────────────────────┐       ┌──────────────────┐
│                 ebs-gateway                  │<──────│      runner      │
│        认证、鉴权、审计、请求入口              │       │  Job 执行/心跳上报 │
└──────────────────────┬───────────────────────┘       └──────────────────┘
                       |
                       v
┌───────────────────────────────────────────────┐       ┌───────────────────┐
│                ebs-apiserver                  │<──────│    controllers    │
│  ebs/v1 REST API / status subresource / watch │       │     scheduler     │
└───────────────┬──────────────────┬────────────┘       └───────────────────┘
                |                  |                    REST / list / watch
                |                  |
                v
┌──────────────────────────┐  ┌──────────────────────────┐
│           etcd           │  │      Elasticsearch        │
│   主存储：对象与 watch     │  │   主存储：索引与增强数据   │
└──────────────────────────┘  └──────────────────────────┘
```

核心原则：
- 所有资源读写最终都经过 `ebs-apiserver`；
- runner 统一访问 `ebs-gateway`，便于外部执行机和内部执行机使用同一套访问逻辑。
- etcd 和 Elasticsearch 都是主存储。
- etcd 负责对象持久化、resourceVersion 和 list/watch，Elasticsearch 负责对象索引、搜索和增强数据。

---

## 三、核心组件

| 组件 | 职责 |
|------|------|
| `ebs-gateway` | 系统入口，负责认证、鉴权、审计和请求转发 |
| `ebs-apiserver` | 统一资源 API，负责对象校验、默认值、存储访问、list/watch、`/status` 子资源 |
| `etcd` | 主存储，保存资源对象、resourceVersion，并提供 list/watch |
| `Elasticsearch` | 主存储，保存对象索引、搜索字段和增强数据 |
| `controllers` | 监听 Project/Snapshot/Build 等对象变化，推进资源状态 |
| `scheduler` | 监听全局 Job，选择 Runner 并更新 Job 状态 |
| `runner` | 通过 ebs-gateway 注册 Runner、心跳上报、监听绑定到自己的 Job 并执行 |

---

## 四、资源模型

当前 API group：

```text
apiVersion: ebs/v1
```

当前顶层资源：

| 资源 | 作用域 | 说明 |
|------|--------|------|
| Project | 集群级 | 项目，是 Snapshot、Build、Job 的业务归属 |
| Snapshot | Project 级 | 项目快照 |
| Build | Project 级 | 构建任务 |
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

Project 名用于内部 scoped storage，需要满足 DNS1123 label 约束，只能使用小写字母、数字和 `-`，不能包含 `.`。页面展示名称使用 `spec.displayName`。

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

用户侧通过 Project API 管理 Snapshot、Build、Job：

```text
/apis/ebs/v1/projects/{project}/snapshots
/apis/ebs/v1/projects/{project}/builds
/apis/ebs/v1/projects/{project}/jobs
```

`{project}` 是对象的唯一项目归属来源，`spec` 中不重复保存 `projectName`。

### 5.3 全局系统 API

调度器和控制器通过全局 API 跨 Project list/watch：

```text
/apis/ebs/v1/snapshots
/apis/ebs/v1/builds
/apis/ebs/v1/jobs
```

典型 watch：

```bash
curl -k -N 'https://localhost:8443/apis/ebs/v1/builds?watch=true'
curl -k -N 'https://localhost:8443/apis/ebs/v1/jobs?watch=true'
```

### 5.4 Runner API

```text
GET    /apis/ebs/v1/runners
POST   /apis/ebs/v1/runners
GET    /apis/ebs/v1/runners/{name}
PUT    /apis/ebs/v1/runners/{name}
PATCH  /apis/ebs/v1/runners/{name}
DELETE /apis/ebs/v1/runners/{name}
PUT    /apis/ebs/v1/runners/{name}/status
```

---

## 六、ebs-apiserver

`ebs-apiserver` 基于 `k8s.io/apiserver` 的 `GenericAPIServer` 实现，复用 Kubernetes apiserver 的资源注册、REST storage、watch、resourceVersion、`/status` 子资源和对象元数据机制。

Project 子资源 API 由 `project_alias.go` 提供轻量适配：

```text
/apis/ebs/v1/projects/{project}/builds
```

会在服务端重写到内部 scoped storage 路径：

```text
/apis/ebs/v1/namespaces/{project}/builds
```

该内部路径只作为实现细节，外部文档和业务调用统一使用 Project API 和全局系统 API。

详细实现见 [ebs-apiserver.md](./ebs-apiserver.md)。

---

## 七、存储设计

etcd 主数据路径：

```text
/registry/ebs/projects/{name}
/registry/ebs/snapshots/{project}/{name}
/registry/ebs/builds/{project}/{name}
/registry/ebs/jobs/{project}/{name}
/registry/ebs/runners/{name}
```

这种布局同时满足：

- Project 内对象名称唯一：`{project}/{name}`。
- 全局 list/watch：监听 `/registry/ebs/builds`、`/registry/ebs/jobs` 等资源前缀。
- Project 内 list/watch：监听 `/registry/ebs/builds/{project}` 等 Project 子前缀。

Elasticsearch 索引：

```text
ebs-projects
ebs-snapshots
ebs-builds
ebs-jobs
ebs-runners
```

namespaced 对象写入 ES 时使用 `{project}/{name}` 作为文档 ID，请求时对 `/` 做 URL escape。

---

## 八、组件协作流程

### 8.1 用户请求

```text
用户 -> ebs-gateway -> ebs-apiserver -> etcd
```

`ebs-gateway` 负责入口能力，`ebs-apiserver` 负责资源语义和数据访问。

### 8.2 Controller 流程

```text
controller -> watch Project/Snapshot/Build
controller -> create/update Snapshot、Build、Job
controller -> update status
```

controller 只通过 API 操作资源，不直接访问 etcd。

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
runner -> ebs-gateway -> ebs-apiserver: register Runner
runner -> ebs-gateway -> ebs-apiserver: update Runner.status heartbeat
runner -> ebs-gateway -> ebs-apiserver: watch assigned Job
runner -> execute Job
runner -> ebs-gateway -> ebs-apiserver: update Job.status
```

Runner 作为集群级资源存在，调度标签使用 `metadata.labels`，资源容量和运行状态写入 `status`。runner 不直接访问 `ebs-apiserver`，统一访问 `ebs-gateway`，外部执行机和内部执行机使用同一套客户端逻辑。

---

## 九、部署

测试环境使用 `hacks/docker-compose.yml` 启动：

```text
etcd
Elasticsearch
ebs-apiserver
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

---

## 十、后续完善

当前架构后续主要完善方向：

- 生成真实 OpenAPI schema。
- 完善认证、鉴权、审计与多租户策略。
- 补齐 controller、scheduler、runner 的实现。
- 接入正式的镜像构建和发布流程。
