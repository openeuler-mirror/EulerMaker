# ebs-apiserver

`ebs-apiserver` 是 EulerMaker 的资源 API 服务，基于 `k8s.io/apiserver` 的 `GenericAPIServer` 实现，提供 REST API、`/status` 子资源，以及按资源类型选择的 etcd 或 Elasticsearch 持久化。

Job、Runner 使用 etcd并支持list/watch；Project、Snapshot、Build、BuildInfo、RpmRepo使用Elasticsearch，支持CRUD、selector和分页但不支持watch。

使用 `--enable-iam` 启用内置 IAM 模块，提供 User 和 MachineAccount 管理及认证能力。

## 架构

```
client / ebs-gateway
        |
        v
ebs-apiserver
        |
        +-- etcd              Job、Runner：对象与 watch
        |
        +-- Elasticsearch     其余资源：对象、过滤与分页
```

## 资源列表

| 资源 | 主存储 | Project API | 全局 API | Watch | 子资源 |
|------|--------|-------------|----------|-------|--------|
| Project | Elasticsearch | `/apis/ebs/v1/projects` | - | 否 | `/status` |
| Snapshot | Elasticsearch | `/apis/ebs/v1/projects/{project}/snapshots` | `/apis/ebs/v1/snapshots` | 否 | `/status` |
| Build | Elasticsearch | `/apis/ebs/v1/projects/{project}/builds` | `/apis/ebs/v1/builds` | 否 | `/status`, `/abort` |
| BuildInfo | Elasticsearch | `/apis/ebs/v1/projects/{project}/buildinfos` | `/apis/ebs/v1/buildinfos` | 否 | `/status` |
| RpmRepo | Elasticsearch | `/apis/ebs/v1/projects/{project}/rpmrepos` | `/apis/ebs/v1/rpmrepos` | 否 | `/status` |
| Job | etcd | `/apis/ebs/v1/projects/{project}/jobs` | `/apis/ebs/v1/jobs` | 是 | `/status` |
| Runner | etcd | - | `/apis/ebs/v1/runners` | 是 | `/status` |
| User（可选） | Elasticsearch | - | `/apis/iam.ebs/v1/users` | 否 | - |
| MachineAccount（可选） | Elasticsearch | - | `/apis/iam.ebs/v1/machineaccounts` | 否 | - |

`Snapshot`、`Build`、`BuildInfo`、`RpmRepo`、`Job` 的 Project API 表达业务归属；全局 API 用于跨 Project list，只有 Job 支持全局 watch。Project API 会在 apiserver 内部重写到 scoped storage 路径，因此 Project 名需要满足 DNS1123 label 约束，只能使用小写字母、数字和 `-`，不能包含 `.`。

## 项目结构

```
ebs-apiserver/
├── cmd/server/main.go
├── pkg/
│   ├── apis/ebs/
│   │   ├── register.go
│   │   ├── v1/
│   │   │   ├── types.go
│   │   │   ├── register.go
│   │   │   ├── defaults.go
│   │   │   └── zz_generated.deepcopy.go
│   │   └── validation/validation.go
│   ├── registry/ebs/
│   │   ├── project/storage.go
│   │   ├── snapshot/storage.go
│   │   ├── build/storage.go
│   │   ├── buildinfo/storage.go
│   │   ├── rpmrepo/storage.go
│   │   ├── scopedresource/storage.go
│   │   ├── job/storage.go
│   │   └── runner/storage.go
│   ├── server/
│   │   ├── project_alias.go
│   │   └── server.go
│   └── storage/
│       ├── es/
│       └── esstore/
├── Dockerfile
├── hack/
├── go.mod
└── go.sum
```

## 快速开始

### Docker Compose

仓库根目录下可以使用 `hacks/docker-compose.yml` 启动测试环境：

```bash
docker compose -f hacks/docker-compose.yml up -d
```

当前 compose 文件默认使用已发布的 `ebs-apiserver` 镜像；如果需要验证本地代码，需要先构建并推送镜像，或临时在 compose 中增加 `build` 配置。

该 compose 文件包含：

| 服务 | 地址 |
|------|------|
| etcd | `http://localhost:2379` |
| Elasticsearch | `http://localhost:9200` |
| ebs-apiserver | `https://localhost:8443` |

### 本地编译

```bash
go mod tidy
CGO_ENABLED=0 go build -o ebs-apiserver ./cmd/server
```

### 本地运行

```bash
./ebs-apiserver \
  --etcd-servers=http://localhost:2379 \
  --es-servers=http://localhost:9200 \
  --enable-iam \
  --secure-port=8443
```

`--enable-iam` 是 IAM 模块唯一新增的启动配置。模块使用固定的 Argon2id 参数、密码策略和锁定策略，并复用 `--es-servers` 连接。

### Docker 构建

```bash
docker build -t eulermaker/ebs-apiserver:dev .
```

## API 示例

### 创建 User

```bash
curl -k -X POST https://localhost:8443/apis/iam.ebs/v1/users \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion":"iam.ebs/v1",
    "kind":"User",
    "metadata":{"name":"alice"},
    "spec":{"enabled":true,"displayName":"Alice","email":"alice@example.com"}
  }'
```

内部密码设置和认证接口分别为：

```text
PUT  /internal/iam/v1/users/{name}/password
POST /internal/iam/v1/authenticate
```

这些接口供 ebs-gateway 在受信任网络中调用，密码和密码哈希不会出现在 User API 响应中。

### 创建 Project

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
      "buildPayload": "debug_package: false",
      "buildTargets": [{
        "os": "openEuler-22.03-LTS",
        "arch": "aarch64",
        "buildFlag": true,
        "publishFlag": true
      }],
      "packageRepos": [{
        "name": "gcc",
        "url": "https://example.com/src-openeuler/gcc.git",
        "branch": "master"
      }]
    }
  }'
```

### 查询 Project

```bash
curl -k https://localhost:8443/apis/ebs/v1/projects
curl -k https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts
```

### 创建 Snapshot

`buildTargets` 为必填字段；无法获取 commit 时，`specCommits` 可以省略或为空对象：

```bash
curl -k -X POST https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/snapshots \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "ebs/v1",
    "kind": "Snapshot",
    "metadata": {"name": "snap-001"},
    "spec": {
      "specCommits": {
        "gcc": {
          "specUrl": "https://example.com/src-openeuler/gcc.git",
          "commitId": "0123456789abcdef"
        }
      },
      "buildTargets": [{
        "os": "openEuler-22.03-LTS",
        "arch": "aarch64",
        "buildFlag": true
      }]
    }
  }'
```

### 创建 Build

```bash
curl -k -X POST https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/builds \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "ebs/v1",
    "kind": "Build",
    "metadata": {"name": "build-001"},
    "spec": {
      "snapshotName": "snap-001",
      "buildType": "full",
      "packages": ["gcc"],
      "buildTarget": {
        "os": "openEuler-22.03-LTS",
        "arch": "aarch64",
        "buildFlag": true,
        "publishFlag": true
      }
    }
  }'
```

`buildType` 支持 `full`、`incremental`、`specified` 和 `single`；省略时默认为 `full`。Build 创建时还必须提供 `snapshotName`、`packages` 以及包含 `os`、`arch` 的 `buildTarget`。

### 查询 BuildInfo 和 RpmRepo

```bash
curl -k https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/buildinfos
curl -k https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/rpmrepos
```

系统控制器也可以通过全局 API 跨 Project list：

```bash
curl -k "https://localhost:8443/apis/ebs/v1/buildinfos"
curl -k "https://localhost:8443/apis/ebs/v1/rpmrepos"
```

### Watch 全局 Job

调度器使用全局 API watch 全部 Project 下的 Job：

```bash
curl -k -N "https://localhost:8443/apis/ebs/v1/jobs?watch=true"
```

### 分页和 selector 查询

```bash
curl -k --get \
  --data-urlencode 'labelSelector=arch=x86_64,channel in (stable,testing)' \
  --data-urlencode 'fieldSelector=metadata.namespace=openeuler-22-03-lts' \
  --data-urlencode 'limit=100' \
  "https://localhost:8443/apis/ebs/v1/builds"
```

使用响应 `metadata.continue` 的原值请求下一页。ES-only 资源的 field selector 仅支持 `metadata.name` 和 `metadata.namespace`。

### Watch Project 下的 Job

```bash
curl -k -N "https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/jobs?watch=true"
```

### 更新 Job 状态

`PUT` 更新必须带当前对象的 `metadata.resourceVersion`。只更新 `status` 时，建议使用 merge patch：

```bash
curl -k -X PATCH https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/jobs/job-001/status \
  -H "Content-Type: application/merge-patch+json" \
  -d '{
    "status": {
      "phase": "Running",
      "runner": "runner-001"
    }
  }'
```

如果使用 `PUT`，需要先查询对象并把返回的 `metadata.resourceVersion` 填入请求体：

```bash
curl -k -X PUT https://localhost:8443/apis/ebs/v1/projects/openeuler-22-03-lts/jobs/job-001/status \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "ebs/v1",
    "kind": "Job",
    "metadata": {
      "name": "job-001",
      "resourceVersion": "<resourceVersion from GET>"
    },
    "status": {
      "phase": "Running",
      "runner": "runner-001"
    }
  }'
```

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--etcd-servers` | `http://etcd:2379` | etcd 服务器地址 |
| `--es-servers` | `http://elasticsearch:9200` | Elasticsearch 地址 |
| `--secure-port` | `8443` | HTTPS 监听端口 |

完整参数列表来自 `k8s.io/apiserver/pkg/server/options`。

## 存储路径

etcd 主数据路径：

```text
/registry/ebs/
├── jobs/{project}/{name}
└── runners/{name}
```

Job 的全局 list/watch 使用 `/registry/ebs/jobs`，Project API 使用对应的 `/registry/ebs/jobs/{project}` 前缀。

Elasticsearch 索引：

```text
ebs-projects
ebs-snapshots
ebs-builds
ebs-buildinfos
ebs-rpmrepos
```

服务启动时会检查并创建上述五个索引和显式 mapping。Project scoped 对象使用 `{project}/{name}` 作为文档 ID。ES-only 对象的 `resourceVersion` 只用于单对象乐观并发控制，不具备 etcd revision 或 watch 语义。

当前版本不包含旧双写数据的迁移工具。已有部署切换前必须单独迁移或清空旧数据。

## 相关文档

- [数据模型字段说明](../../docs/zh/design/data-models.md)
- [ebs-apiserver 设计](../../docs/zh/design/ebs-apiserver.md)
