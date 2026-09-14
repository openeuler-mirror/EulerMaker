# Artifact Manager

Artifact Manager 接收 Runner 上传的构建产物和实时容器日志，并把正文、元数据、幂等记录和日志 JSON Lines 索引保存到独立的本地持久化目录。它同时接受 RpmRepo Controller 的内部请求，基于已经封账的 Job Manifest 生成不可变 RPM 过程仓，并从 Ready 过程仓生成和切换正式 release。

## 构建与测试

在当前目录执行：

```bash
go test ./...
CGO_ENABLED=0 go build -o artifact-manager ./cmd/server
```

构建容器镜像：

```bash
docker build -t eulermaker/artifact-manager:dev .
```

## 本地运行

先启动 Gateway，然后执行：

```bash
go run ./cmd/server \
  --listen=:8081 \
  --data-dir=/var/lib/ebs-artifacts \
  --gateway-url=http://localhost:8080
```

上述 HTTP Gateway 地址只适用于本地开发。生产环境应使用 HTTPS，并通过 `--gateway-ca` 配置信任的 CA。

仓库物化依赖本机的 `rpm` 和 `createrepo_c`。当前内部仓库管理接口不校验 Token，只允许部署在受信任的内部网络，后续接入统一的组件身份认证。

过程仓接口包括创建物理版本 `POST /internal/v1/repositories`、查询状态或删除 `GET/DELETE /internal/v1/repositories/{repositoryUID}`。Ready 过程仓通过只读地址 `GET /repositories/v1/{repositoryUID}/{path}` 提供内容。

正式发布接口包括创建 `POST /internal/v1/releases`、查询或删除 `GET/DELETE /internal/v1/releases/{buildName}`，以及激活或回滚 `POST /internal/v1/releases/{buildName}/activate`。稳定 DNF 地址为 `/repositories/{project}/{arch}/`，不可变历史版本地址为 `/repositories/releases/v1/{buildName}/`。

构建过程仓保存在 `${dataDir}/repositories/{project}/{arch}/history/{buildName}/steps/{repositoryUID}`，正式版本保存在 `${dataDir}/repositories/{project}/{arch}/releases/{buildName}`。Artifact Manager 通过原子替换架构目录中的 `current` 链接切换稳定入口。`--release-public-key` 可以指定复制到每个正式版本的只读公钥；历史版本由 `--release-history-count` 和 `--release-history-ttl` 控制。

服务启动后可以检查健康状态：

```bash
curl http://localhost:8081/healthz
curl http://localhost:8081/readyz
```

完整参数请执行：

```bash
go run ./cmd/server --help
```

## Docker Compose

在仓库根目录构建并启动完整开发环境：

```bash
docker compose -f hacks/docker-compose.yml up -d --build
```

Compose 环境中，Artifact Manager 在 `8081` 端口提供服务，并使用 `http://ebs-gateway:8080` 访问 Gateway。
