# EulerMaker

中文 | [English](README.en.md)

EulerMaker 是面向 openEuler 及其衍生发行版的软件包构建系统，围绕工程、源码快照、构建任务、执行机和 RPM 仓库组织从源代码到二进制软件包的构建流程，支持操作系统定制场景。

项目采用 Go 多模块后端和 Vue 3 / TypeScript 前端，通过 Kubernetes 风格的 `metadata/spec/status` 资源 API 协调控制器、调度器与执行机。

## 主要能力

- 工程与源码管理：维护发行版、构建架构、软件包仓库和 Bootstrap 仓库；Git Server 同步源码，Snapshot 固定构建 commit。
- 构建管理：支持全量、增量、指定增量和 single 构建；single 可选择多个包，仅选择一个包时可通过 `ebs.io/git-ref` 指定 PR 等完整引用；非 single 构建可控制是否发布。
- 调度与执行：支持构建优先级、资源匹配、标签选择及污点容忍，通过门禁资源池隔离任务；Runner 支持 Docker 和 containerd 原生客户端。
- 产物与仓库服务：提供日志查看与下载、产物上传、可选 RPM 签名、过程仓物化及正式仓发布。
- 访问控制：支持本地或 OAuth 登录、工程 owner/member 授权及 Runner/Service 机器账号；Service 账号按工程范围和操作权限接入自动化流程。
- 集中运维：Web 控制台支持构建状态、依赖图谱、Runner 详情及标签/污点管理，统一维护构建配置、资源规则、服务地址、脚本和账号；同时提供 ebsctl。
- 软件包清单同步：可选 Release Management 插件周期读取社区清单，更新工程软件包列表，提供差异预览和手动同步。

控制器以持久化资源状态持续调谐，通过检查点、幂等操作及重试恢复未完成流程；Runner 支持上传恢复，
资源删除通过 finalizer 逐层收敛。部署约束与安全注意事项见各组件及部署文档。

## 组件与目录

| 目录 | 职责 |
| --- | --- |
| [api](api/README.md) | 独立 Go module，提供共享的 `ebs/v1`、`iam/v1` API 类型 |
| [backend/ebs-apiserver](backend/ebs-apiserver/README.md) | 资源 API、校验、状态更新与持久化 |
| [backend/ebs-gateway](backend/ebs-gateway/README.md) | 外部资源访问入口、认证、授权、限流与审计 |
| [backend/controller-manager](backend/controller-manager/README.md) | 控制器框架及资源调谐 |
| [backend/scheduler](backend/scheduler/README.md) | Job 调度与 Runner 绑定 |
| [backend/runner](backend/runner/README.md) | 任务执行、心跳及日志和产物上传 |
| [backend/git-server](backend/git-server/README.md) | Git 仓库同步、查询和克隆服务 |
| [backend/artifact-manager](backend/artifact-manager/README.md) | 产物、日志、过程仓和正式发布服务 |
| [backend/release-management](backend/release-management/README.md) | 可选的软件包清单周期同步插件 |
| [frontend](frontend/README.md) | Vue 3 Web 控制台，支持中文与英文 |
| [tools/ebsctl](tools/ebsctl/README.md) | 面向 Gateway 的命令行客户端 |
| [hacks](hacks/README.md) | Docker Compose 开发部署、TLS 联调及辅助脚本 |
| [deploy/kubernetes](deploy/kubernetes/README.md) | Kubernetes 部署模板、证书配置与升级操作 |

Job 和 Runner 存储在 etcd 中，支持 List/Watch；其他业务资源和 IAM 对象存储在 Elasticsearch 中，通过 List/Get 访问。构建文件和 Git 仓库保存在各自服务的持久化目录中。

## 部署与版本文档

- [版本变更记录](CHANGELOG.md)：各版本能力、接口变化和升级注意事项。
- [Kubernetes 部署](deploy/kubernetes/README.md)：按组件部署，包含存储、证书和 Mapping 升级说明。
- [Compose 开发部署与 TLS 联调](hacks/README.md)：配置、凭据及目录权限准备；[TLS 覆盖文件](hacks/docker-compose.tls.yml)须与基础 Compose 一起使用。
- [Release Management 使用说明](backend/release-management/README.md)：清单来源、机器凭据和同步操作。启用前先执行 `plan`，确认整个包列表替换的影响。

## 快速开始：本地开发环境

### 准备

推荐使用 Linux 主机、Docker Engine 和 Docker Compose v2。镜像构建需要能够访问对应镜像仓库及 Go/npm 依赖源。

按 [Compose 开发部署说明](hacks/README.md) 准备 `hacks/ebs-gateway-jwt-secret`，以及 `/srv/etcd`、`/srv/es`、`/srv/git`、`/srv/artifact` 数据目录。密钥需允许 Gateway 容器用户读取，数据目录需允许对应容器用户读写；绑定挂载不会自动继承镜像内目录的所有权。不要提交凭据文件。

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

显式开启 apiserver 的 `--insecure-allow-unauthenticated-clients=true` 时，PAT 和 OAuth 内部接口也可用于免认证开发环境；正常部署仍校验 Gateway 组件身份。

### 接入 Runner

控制面就绪后，由 Admin 创建 `type=runner` 的 MachineAccount，将 `clientID`、`clientSecret` 保存到 `hacks/runner-machine-credential.json`，格式见 [Runner README](backend/runner/README.md)。在 Runner 节点准备可写的 `/var/lib/ebs-runner`，宿主机与容器内必须使用相同的绝对路径。

Runner 使用[独立 Compose 文件](hacks/docker-compose.runner.yml)，不会随控制面启动。该模板通过原生 Docker 客户端连接宿主机 daemon，需挂载 Docker socket 并设置其实际组 ID；Runner 执行任务不依赖 Docker CLI。

```bash
DOCKER_GID="$(stat -c %g /var/run/docker.sock)" \
  docker compose -p eulermaker-runner -f hacks/docker-compose.runner.yml up -d --build
```

构建服务地址由 `Config/services` 管理，Ops/Admin 可在运维页面“服务配置”中修改。控制面的 `internalURL` 使用 Compose 服务名；Runner 通过 Gateway 获取访问地址：

- `RUNNER_SERVICE_ACCESS_MODE=internal`（默认）：制品服务和 Git Server 均使用 `runnerInternalURL`。
- `RUNNER_SERVICE_ACCESS_MODE=external`：制品服务使用 `runnerExternalURL`；源码直接使用 Job 的 `origin_url`，保留固定 `commit_id`，不需要 Git Server 外网地址。

同机宿主机网络模式默认使用 Gateway `http://127.0.0.1:8080`、Artifact Manager `http://127.0.0.1:8081` 和 Git Server `http://127.0.0.1:8084`。跨主机部署时，设置可达的 `RUNNER_GATEWAY_URL`、唯一的 `RUNNER_NAME`，并确保所选服务地址及源码地址可由 Runner 和 Job 容器访问；缺失地址不会自动回退到另一模式。containerd 接入方式见 [Runner 文档](backend/runner/README.md)与 [Kubernetes 模板](deploy/kubernetes/runner-containerd.yaml)。

## 本地开发与验证

后端为多个独立 Go module，部分通过相对 `replace` 引用根目录 `api`；请保持仓库目录结构，在目标 module 内执行命令，而不是在根目录直接运行 `go test ./...`。

```bash
(cd api && go test ./...)
(cd backend/ebs-apiserver && go test ./...)
(cd backend/ebs-gateway && go test ./...)
(cd backend/controller-manager && go test ./...)
(cd backend/release-management && go test ./...)
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

修改共享 API 类型后，按顺序更新生成代码：

```bash
(cd api && bash hacks/update-codegen.sh)
(cd backend/ebs-apiserver && bash hacks/update-openapi.sh)
(cd backend/ebs-gateway && bash hacks/update-openapi.sh)
```

修改 Gateway 路由或对外契约时，同步更新其 OpenAPI；执行 `bash hacks/update-openapi.sh --check` 检查生成结果。代码生成说明见 [API README](api/README.md)，各组件的构建命令和参数见对应 README。

## 设计文档

- [整体架构](docs/zh/design/architecture.md)
- [数据模型](docs/zh/design/data-models.md)
- [API Server](docs/zh/design/ebs-apiserver.md) 与 [Gateway](docs/zh/design/ebs-gateway.md)
- [Controller Manager](docs/zh/design/controller-manager.md) 与 [Scheduler](docs/zh/design/scheduler.md)
- [构建配置](docs/zh/design/data-models~config.md)、[构建脚本](docs/zh/design/data-models~script.md)与[标签约定](docs/zh/design/labels.md)
- [Git Server](docs/zh/design/git-server.md) 与 [Artifact Manager](docs/zh/design/artifact-manager.md)
- [Runner](docs/zh/design/runner.md) 与 [Release Management](docs/zh/design/release-management.md)

## 参与贡献

反馈问题或提出建议时，可在创建 Issue 时选择[问题反馈](.atomgit/ISSUE_TEMPLATE/bug_report.md)、[功能建议](.atomgit/ISSUE_TEMPLATE/feature_request.md)或[文档改进](.atomgit/ISSUE_TEMPLATE/documentation.md)模板。报告问题时请提供版本、复现步骤和相关日志，并移除密码、Token、Cookie、API Key 等敏感信息。

1. Fork 仓库并创建功能分支。
2. 保持改动聚焦，同步更新相关测试与文档。
3. 运行受影响模块的测试；前端修改执行类型检查和构建。
4. 提交 Pull Request，说明改动目的、验证结果以及 API 或存量数据影响。
