# EulerMaker Frontend

EulerMaker 的基础 Web 控制台，使用 Vue 3、TypeScript、Vite、Vue Router 和 Pinia。

当前包含：首页工程概览、工程列表、工程详情、账号登录，以及中文和 English 国际化。资源与认证请求统一通过 `ebs-gateway`。

运维页面提供“脚本管理”，Ops/Admin 可查看和搜索全局脚本、创建脚本、编辑正文并删除非默认脚本；默认 `rpmbuild` 不显示删除入口。编辑保存携带原 resourceVersion，删除请求携带 UID 和 resourceVersion 前置条件，冲突时提示刷新。脚本通过 `/apis/ebs/v1/scripts` 管理，不在浏览器中执行；BuildInfo Controller 创建 Job 时记录脚本引用，由 Runner 拉取并执行。

## 本地开发

```bash
npm install
npm run dev
```

开发服务器默认将 `/apis` 和 `/auth` 转发到 `http://localhost:8080`，将 Job 日志使用的 `/artifacts` 转发到 `http://localhost:8081`。这两个地址都从运行 Vite 的机器访问；访问不到 8081 时，日志请求会在 Vite 代理层失败。使用其他后端地址时：

```bash
VITE_EULERMAKER_GATEWAY=http://gateway.example:8080 \
VITE_EULERMAKER_ARTIFACT_MANAGER=http://artifact.example:8081 npm run dev
```

如果后端部署在 `ssh dev` 对应的机器，而本地不能直连 Artifact Manager，可以在运行 Vite 的机器上另开终端建立隧道，再启动或重启 Vite：

```bash
ssh -N -L 8081:127.0.0.1:8081 dev
```

## Docker Compose

在仓库根目录准备 Gateway 与 Runner 所需 secret 后，构建并启动完整环境：

```bash
docker compose -f hacks/docker-compose.yml up -d --build
```

前端默认地址为 `http://localhost/`（宿主机 80 端口）。可通过 `EULERMAKER_FRONTEND_PORT` 修改宿主机端口：

```bash
EULERMAKER_FRONTEND_PORT=8088 docker compose -f hacks/docker-compose.yml up -d --build
```

前端容器通过同源路径代理 `/auth`、`/apis`、`/artifacts` 和 `/repositories`，浏览器不需要感知 Compose 内部服务地址。

## 检查和构建

```bash
npm run typecheck
npm run build
```
