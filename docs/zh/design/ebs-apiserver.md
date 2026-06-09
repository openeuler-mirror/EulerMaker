# ebs-apiserver 实现

## 概述

`ebs-apiserver` 是 EulerMaker 的资源 API 服务，代码位于 `components/ebs-apiserver`。服务基于 `k8s.io/apiserver` 的 `GenericAPIServer`，提供 Kubernetes 风格的 REST API、`/status` 子资源、watch/list 能力，以及 etcd 持久化能力。

Snapshot、Build、Job 采用折中实现：用户侧使用 Project API 表达项目归属，调度器和控制器使用全局系统 API list/watch 全部对象。Project API 由轻量路由适配层重写到 generic apiserver 的 scoped storage 路径，核心 list/watch/status 能力仍复用 generic apiserver。

服务使用 etcd 和 Elasticsearch 作为组合主存储：etcd 负责对象持久化、resourceVersion 和 list/watch；Elasticsearch 负责对象索引、搜索字段和增强数据。创建/更新对象时会同步写入 ES，读取单对象时会尝试从 ES 取回增强数据并覆盖返回对象。

## 架构

```
client / ebs-gateway
        │
        ▼
components/ebs-apiserver
        │
        ├── k8s.io/apiserver GenericAPIServer
        │   ├── REST storage
        │   ├── status subresource
        │   ├── validation/defaulting
        │   └── watch/list/get/create/update/delete
        │
        ├── etcd
        │   └── 主存储：对象与 watch
        │
        └── Elasticsearch
            └── 主存储：索引与增强数据
```

## 项目结构

```
components/ebs-apiserver/
├── cmd/server/main.go                 # 进程入口
├── pkg/
│   ├── apis/ebs/
│   │   ├── register.go                # API group 注册
│   │   ├── v1/
│   │   │   ├── types.go               # 资源类型
│   │   │   ├── register.go            # 版本资源注册
│   │   │   ├── defaults.go            # 默认值
│   │   │   └── zz_generated.deepcopy.go
│   │   └── validation/validation.go   # admission 校验
│   ├── registry/
│   │   ├── scoped_store.go            # 命名空间作用域 store 包装
│   │   └── ebs/*/storage.go           # 各资源 REST storage
│   ├── server/
│   │   ├── project_alias.go           # Project API 路由适配
│   │   └── server.go                  # apiserver 配置与资源安装
│   └── storage/
│       ├── es/                        # Elasticsearch client
│       └── hybrid/                    # etcd + ES 组合 storage
├── Dockerfile                         # openEuler 镜像构建
├── hack/                              # 代码生成脚本
├── go.mod
└── go.sum
```

## API 版本与资源

API group 定义为：

```text
Group: ebs
Version: v1
apiVersion: ebs/v1
```

已安装到 apiserver 的资源如下：

| 资源 | Project API | 全局 API | 子资源 |
|------|-------------|----------|--------|
| Project | `/apis/ebs/v1/projects` | - | `/status` |
| Snapshot | `/apis/ebs/v1/projects/{project}/snapshots` | `/apis/ebs/v1/snapshots` | `/status` |
| Build | `/apis/ebs/v1/projects/{project}/builds` | `/apis/ebs/v1/builds` | `/status`, `/abort` |
| Job | `/apis/ebs/v1/projects/{project}/jobs` | `/apis/ebs/v1/jobs` | `/status` |
| Runner | `/apis/ebs/v1/runners` | - | `/status` |

其中 `Snapshot`、`Build`、`Job` 是 Project 下的子资源，路径中的 `{project}` 是项目归属来源；全局 API 只用于调度器、控制器等系统组件做跨 Project 的 list/watch；`Project` 和 `Runner` 为集群级资源。

Project API 内部会重写为 scoped storage 请求，因此 Project 名需要满足 DNS1123 label 约束，只能使用小写字母、数字和 `-`，不能包含 `.`。页面展示名称使用 `Project.spec.displayName`。

## 存储设计

### etcd 主存储

apiserver 使用 `k8s.io/apiserver/pkg/registry/generic/registry.Store` 将资源对象写入 etcd，默认前缀为：

```text
/registry/ebs
```

资源的 etcd key 使用如下路径：

```text
/registry/ebs/projects/{name}
/registry/ebs/snapshots/{project}/{name}
/registry/ebs/builds/{project}/{name}
/registry/ebs/jobs/{project}/{name}
/registry/ebs/runners/{name}
```

`Snapshot`、`Build`、`Job` 按 `{project}/{name}` 存在资源全局前缀下。全局 list/watch 监听对应资源前缀，例如 `/registry/ebs/builds`；Project API 的单 Project list/watch 监听对应 Project 子前缀，例如 `/registry/ebs/builds/{project}`。

### Elasticsearch 主存储

`pkg/storage/hybrid/EnricherStore` 包装 generic etcd store：

- `Create`：先写 ES，再写 etcd。ES 写入失败会导致创建失败。
- `Update`：先写 etcd，再尝试写 ES。ES 写入失败不会阻断 etcd 更新。
- `Get`：并发读取 etcd 和 ES。etcd 失败则请求失败；ES 成功时尝试用 ES 中的 `data` 覆盖返回对象。
- `List/Watch`：直接走 etcd。
- `Delete`：先删 etcd，再删除 ES 文档。

namespaced 对象写入 ES 时使用 `{project}/{name}` 作为文档 ID，HTTP 请求中会对 `/` 做 URL escape。

ES client 启动时会 ping ES 并确保以下索引存在：

```text
ebs-projects
ebs-snapshots
ebs-builds
ebs-jobs
ebs-runners
```

## 默认值与校验

各资源 storage strategy 负责在创建和更新时保护 `spec/status` 边界：

- 普通资源更新会保留旧 `status`。
- `/status` 更新会保留旧 `spec`。
- `Project` 创建默认 `status.phase = Active`。
- `Snapshot` 创建默认 `status.phase = Created`。
- `Build` 创建默认 `status.phase = Pending`。
- `Job` 创建默认 `status.phase = Pending`。
- `Runner` 创建默认 `status.phase = Registering`。

当前校验逻辑位于 `pkg/apis/ebs/validation/validation.go`，主要校验必填字段、Project 名称格式、Runner 类型枚举，以及 Runner 的 `type`/`arch` 更新不可变。

## 启动参数

入口为：

```text
components/ebs-apiserver/cmd/server/main.go
```

关键默认参数：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--etcd-servers` | `http://etcd:2379` | etcd 地址 |
| `--secure-port` | `8443` | HTTPS 监听端口 |
| `--es-servers` | `http://elasticsearch:9200` | Elasticsearch 地址 |

示例：

```bash
cd components/ebs-apiserver
go run ./cmd/server \
  --etcd-servers=http://localhost:2379 \
  --es-servers=http://localhost:9200 \
  --secure-port=8443
```

## 镜像构建

组件使用顶层 `Dockerfile` 构建镜像，它基于 openEuler 构建并运行：

```bash
cd components/ebs-apiserver
docker build -t ebs-apiserver:latest .
```

## API 示例

创建 Project：

```bash
curl -k -X POST https://localhost:8443/apis/ebs/v1/projects \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "ebs/v1",
    "kind": "Project",
    "metadata": {"name": "openeuler-22-03-lts"},
    "spec": {
      "displayName": "openEuler 22.03 LTS",
      "description": "openEuler 22.03 LTS",
      "specBranch": "master",
      "buildTargets": [{
        "osVariant": "openEuler-22.03-LTS",
        "architecture": "aarch64"
      }]
    }
  }'
```

创建 Build：

```bash
curl -k -X POST https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/builds \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "ebs/v1",
    "kind": "Build",
    "metadata": {"name": "build-001"},
    "spec": {
      "snapshotName": "snapshot-001",
      "buildType": "full",
      "buildTarget": {
        "osVariant": "openEuler-22.03-LTS",
        "architecture": "aarch64"
      }
    }
  }'
```

更新 Job 状态：

```bash
curl -k -X PUT https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/jobs/job-001/status \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "ebs/v1",
    "kind": "Job",
    "metadata": {"name": "job-001"},
    "status": {
      "phase": "Running",
      "runner": "runner-001"
    }
  }'
```

Watch Job：

```bash
curl -k -N 'https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/jobs?watch=true'
```

Watch 全局 Build：

```bash
curl -k -N 'https://localhost:8443/apis/ebs/v1/builds?watch=true'
```

## 待完善项

已知需要继续完善：

- OpenAPI schema 当前是空对象占位，需要生成真实 schema。
- 认证、鉴权、Admission 当前在 RecommendedOptions 中被置空，实际生产部署应由 gateway 或 apiserver 自身补齐。
