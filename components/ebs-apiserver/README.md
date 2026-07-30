# ebs-apiserver

`ebs-apiserver` 是 EulerMaker 的资源 API 服务，基于 `k8s.io/apiserver` 的 `GenericAPIServer` 实现，提供 REST API、`/status` 子资源、list/watch 能力和 etcd 持久化。

服务使用 etcd 和 Elasticsearch 作为组合主存储：etcd 负责对象持久化、resourceVersion 和 list/watch，Elasticsearch 负责对象索引、搜索字段和增强数据。

## 架构

```
client / ebs-gateway
        |
        v
ebs-apiserver
        |
        +-- etcd              主存储：对象与 watch
        |
        +-- Elasticsearch     主存储：索引与增强数据
```

## 资源列表

| 资源 | Project API | 全局 API | 子资源 |
|------|-------------|----------|--------|
| Project | `/apis/ebs/v1/projects` | - | `/status` |
| Snapshot | `/apis/ebs/v1/projects/{project}/snapshots` | `/apis/ebs/v1/snapshots` | `/status` |
| Build | `/apis/ebs/v1/projects/{project}/builds` | `/apis/ebs/v1/builds` | `/status`, `/abort` |
| BuildInfo | `/apis/ebs/v1/projects/{project}/buildinfos` | `/apis/ebs/v1/buildinfos` | `/status` |
| RpmRepo | `/apis/ebs/v1/projects/{project}/rpmrepos` | `/apis/ebs/v1/rpmrepos` | `/status` |
| Job | `/apis/ebs/v1/projects/{project}/jobs` | `/apis/ebs/v1/jobs` | `/status` |
| Runner | `/apis/ebs/v1/runners` | - | `/status` |

`Snapshot`、`Build`、`BuildInfo`、`RpmRepo`、`Job` 的 Project API 表达业务归属；全局 API 用于调度器和控制器跨 Project list/watch。Project API 会在 apiserver 内部重写到 scoped storage 路径，因此 Project 名需要满足 DNS1123 label 约束，只能使用小写字母、数字和 `-`，不能包含 `.`。

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
│       └── hybrid/
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
  --secure-port=8443
```

### Docker 构建

```bash
docker build -t eulermaker/ebs-apiserver:dev .
```

## API 示例

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

`specCommits`、`buildTargets` 和 `packageRepos` 为必填字段：

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
      }],
      "packageRepos": [{
        "name": "gcc",
        "url": "https://example.com/src-openeuler/gcc.git",
        "commitId": "0123456789abcdef"
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

系统控制器也可以通过全局 API 跨 Project list/watch：

```bash
curl -k -N "https://localhost:8443/apis/ebs/v1/buildinfos?watch=true"
curl -k -N "https://localhost:8443/apis/ebs/v1/rpmrepos?watch=true"
```

### Watch 全局 Job

调度器使用全局 API watch 全部 Project 下的 Job：

```bash
curl -k -N "https://localhost:8443/apis/ebs/v1/jobs?watch=true"
```

### Watch 全局 Build

```bash
curl -k -N "https://localhost:8443/apis/ebs/v1/builds?watch=true"
```

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
├── projects/{name}
├── snapshots/{project}/{name}
├── builds/{project}/{name}
├── buildinfos/{project}/{name}
├── rpmrepos/{project}/{name}
├── jobs/{project}/{name}
└── runners/{name}
```

Project 子资源按 `{project}/{name}` 存在各自的资源全局前缀下，全局 list/watch 直接监听对应资源前缀，例如 `/registry/ebs/builds`。

Elasticsearch 索引：

```text
ebs-projects
ebs-snapshots
ebs-builds
ebs-buildinfos
ebs-rpmrepos
ebs-jobs
ebs-runners
```

服务启动时会预创建 Project、Snapshot、Build、Job 和 Runner 的索引。`BuildInfo`、`RpmRepo` 在首次写入时使用上表中的索引名；部署环境需要允许 Elasticsearch 自动创建索引，或预先创建这两个索引。

## 相关文档

- [数据模型字段说明](../../docs/zh/design/data-models.md)
- [ebs-apiserver 设计](../../docs/zh/design/ebs-apiserver.md)
