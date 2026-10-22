# EulerMaker

中文 | [English](README.en.md)

EulerMaker 是面向 openEuler 及其衍生发行版的软件包构建系统，围绕工程、源码快照、构建任务、执行机和 RPM 仓库组织从源代码到二进制软件包的构建流程，支持操作系统定制场景。

项目采用 Go 多模块后端和 Vue 3 / TypeScript 前端，通过 Kubernetes 风格的 `metadata/spec/status` 资源 API 协调控制器、调度器与执行机。

## 主要能力

- 工程管理：维护软件包仓库、默认 Git ref、构建目标及 Bootstrap repositories，区分社区工程与个人工程。
- 构建管理：支持全量、增量、指定增量和 single 构建类型；single 可以指定多个包。
- 源码管理：通过 git-server 同步仓库，由 Snapshot Controller 将 ref 解析为固定 commit。
- 任务调度与执行：Scheduler 将 Job 分配给 Runner，Runner 执行任务并上报状态、日志和产物。
- 产物与仓库服务：管理上传清单、产物和实时日志，提供不可变过程仓版本创建及正式发布接口。
- 访问控制：Gateway 提供认证、工程级授权及机器账号访问；Web 控制台和 ebsctl 作为用户入口。
- 构建配置：使用集群级 `Config/build-target` 管理目标系统、架构和镜像，使用 `Config/build-resource` 管理构建资源规则。

当前 Controller Manager 已注册 Build、Snapshot、BuildInfo、RpmRepo、Job 和 Runner Controller。完整构建流程仍需结合部署配置和测试验证；实际支持范围以组件代码和测试为准。

## 组件与目录

| 目录 | 职责 |
| --- | --- |
| [api](api/README.md) | 独立 Go module，提供共享的 `ebs/v1` API 类型 |
| [backend/ebs-apiserver](backend/ebs-apiserver/README.md) | 资源 API、校验、状态更新与持久化 |
| [backend/ebs-gateway](backend/ebs-gateway/README.md) | 外部资源访问入口、认证、授权、限流与审计 |
| [backend/controller-manager](backend/controller-manager/README.md) | 控制器框架及资源调谐 |
| [backend/scheduler](backend/scheduler/README.md) | Job 调度与 Runner 绑定 |
| [backend/runner](backend/runner/README.md) | 任务执行、心跳及日志和产物上传 |
| [backend/git-server](backend/git-server/README.md) | Git 仓库同步、查询和克隆服务 |
| [backend/artifact-manager](backend/artifact-manager/README.md) | 产物、日志、过程仓和正式发布服务 |
| [frontend](frontend/README.md) | Vue 3 Web 控制台，支持中文与英文 |
| [tools/ebsctl](tools/ebsctl/README.md) | 面向 Gateway 的命令行客户端 |
| [hacks](hacks/docker-compose.yml) | 本地开发环境的 Docker Compose 配置 |

Job 和 Runner 存储在 etcd 中，支持 List/Watch；其他业务资源和 IAM 对象存储在 Elasticsearch 中，通过 List/Get 访问。构建文件和 Git 仓库保存在各自服务的持久化目录中。

## 开发环境

### 准备

推荐使用 Linux 主机、Docker Engine 和 Docker Compose v2。镜像构建需要能够访问对应镜像仓库及 Go/npm 依赖源。

在启动 [Compose](hacks/docker-compose.yml) 前：

1. 准备 `hacks/ebs-gateway-jwt-secret`，内容为 Base64 编码的随机 HMAC 密钥，生成方法见 [Gateway README](backend/ebs-gateway/README.md)。
2. 创建并检查以下宿主机目录，确保对应容器用户具有读写权限。绑定挂载不会自动继承镜像内目录的所有权。

| 宿主机目录 | 用途 |
| --- | --- |
| `/srv/etcd` | etcd 数据 |
| `/srv/es` | Elasticsearch 数据 |
| `/srv/git` | Git 仓库 |
| `/srv/artifact` | 产物、日志及 RPM 仓库 |
Runner 独立部署前，还需在系统中创建有效的 MachineAccount，并将其 `clientID` 和 `clientSecret` 写入 `hacks/runner-machine-credential.json`，格式见 [Runner README](backend/runner/README.md)。不要提交凭据文件。Runner 使用宿主机 Docker socket：设置 `DOCKER_GID` 为该 socket 的实际组 ID，并确认宿主机存在 `/usr/bin/docker`。在 Runner 节点准备 `/var/lib/ebs-runner`，且宿主机与容器内必须使用相同的绝对路径。

### 启动与检查

完成上述配置后，在仓库根目录执行：

```bash
docker compose -f hacks/docker-compose.yml up -d --build
docker compose -f hacks/docker-compose.yml ps
docker compose -f hacks/docker-compose.yml logs --tail=100
```

前端默认通过宿主机 **80** 端口访问，即 `http://localhost/`。修改端口：

```bash
EULERMAKER_FRONTEND_PORT=8088 docker compose -f hacks/docker-compose.yml up -d --build
```

Gateway 默认监听宿主机 8080 端口，可检查：

```bash
curl -fsS http://localhost:8080/healthz
```

Compose 从当前工作区构建应用镜像。默认配置使用开发用 TLS 跳过校验、未启用 Elasticsearch 安全认证，并暴露多个内部服务端口，**不适合直接用于公网或生产部署**。生产环境需要独立规划证书、网络隔离、凭据管理、持久化与备份。Docker socket 访问也应限制在受信任的 Runner。

构建服务地址由 `Config/service-config` 管理，可由 Ops/Admin 在运维页面的“服务配置”中修改。控制面的 `internalURL` 使用 Compose 服务名；Runner 通过 Gateway 获取 `runnerInternalURL` 或 `runnerExternalURL`。同机、宿主机网络的 Runner 可使用初始化默认值：Artifact Manager 为 `http://127.0.0.1:8081`，Git Server 为 `http://127.0.0.1:8084`；异机 Runner 必须改为构建节点及 Job 容器可达的地址。所选网络需能访问 Gateway `8080`、Artifact Manager `8081` 和 Git Smart HTTP `8084` 端口，不应将内部端口直接开放到公网。

```bash
docker compose -f hacks/docker-compose.yml up -d --build
DOCKER_GID="$(stat -c %g /var/run/docker.sock)" \
  docker compose -p eulermaker-runner -f hacks/docker-compose.runner.yml up -d --build
```

Runner 单独使用 [Compose 文件](hacks/docker-compose.runner.yml)，默认通过宿主机网络访问 `127.0.0.1:8080`，不会随控制面启动。跨主机运行时，在 Runner 节点设置可达的 `RUNNER_GATEWAY_URL` 和唯一的 `RUNNER_NAME`。`RUNNER_SERVICE_ACCESS_MODE` 默认为 `internal`，两类服务均使用 `runnerInternalURL`；设为 `external` 时均使用 `runnerExternalURL`，不会自动回退。先填写所选模式的服务地址并确认控制面就绪，再启动 Runner。可用相同的 `-p eulermaker-runner -f hacks/docker-compose.runner.yml` 参数查看状态和日志。

## 本地开发与验证

后端为多个独立 Go module，部分通过相对 `replace` 引用根目录 `api`；请保持仓库目录结构，在目标 module 内执行命令，而不是在根目录直接运行 `go test ./...`。

```bash
(cd api && go test ./...)
(cd backend/ebs-apiserver && go test ./...)
(cd backend/controller-manager && go test ./...)
(cd tools/ebsctl && go test ./...)
```

前端建议使用 Node.js 22，与构建镜像保持一致：

```bash
cd frontend
npm ci
npm run typecheck
npm run build
npm run dev
```

修改共享 API 类型后，按 [API README](api/README.md) 更新 DeepCopy，并在 `backend/ebs-apiserver` 中执行 `bash hacks/update-openapi.sh` 更新 OpenAPI。各组件的构建命令和运行参数见对应 README。

## 设计文档

- [整体架构](docs/zh/design/architecture.md)
- [数据模型](docs/zh/design/data-models.md)
- [API Server](docs/zh/design/ebs-apiserver.md) 与 [Gateway](docs/zh/design/ebs-gateway.md)
- [Controller Manager](docs/zh/design/controller-manager.md) 与 [Scheduler](docs/zh/design/scheduler.md)
- [构建配置](docs/zh/design/data-models~config.md)、[构建脚本](docs/zh/design/data-models~script.md)与[标签约定](docs/zh/design/labels.md)
- [Git Server](docs/zh/design/git-server.md) 与 [Artifact Manager](docs/zh/design/artifact-manager.md)

## 参与贡献

1. Fork 仓库并创建功能分支。
2. 保持改动聚焦，同步更新相关测试与文档。
3. 运行受影响模块的测试；前端修改执行类型检查和构建。
4. 提交 Pull Request，说明改动目的、验证结果以及 API 或存量数据影响。
