# EulerMaker Frontend

EulerMaker 的基础 Web 控制台，使用 Vue 3、TypeScript、Vite、Vue Router 和 Pinia。

当前包含：首页工程概览、工程列表、工程详情、账号登录，以及中文和 English 国际化。资源与认证请求统一通过 `ebs-gateway`。

运维页面提供“脚本管理”，Ops/Admin 可查看和搜索全局脚本、创建脚本、编辑正文；不提供删除。编辑保存携带原 resourceVersion，冲突时保留草稿并提示人工合并。脚本通过 `/apis/ebs/v1/scripts` 管理，不在浏览器中执行；Controller/Runner 的脚本消费链路尚未接入。

## 本地开发

```bash
npm install
npm run dev
```

开发服务器默认将 `/apis` 和 `/auth` 转发到 `http://localhost:8080`。使用其他 Gateway 地址时：

```bash
VITE_EULERMAKER_GATEWAY=http://gateway.example npm run dev
```

## Docker Compose

在仓库根目录准备 Gateway 与 Runner 所需 secret 后，构建并启动完整环境：

```bash
docker compose -f hacks/docker-compose.yml up -d --build
```

前端默认地址为 `http://localhost:3000`。可通过 `EULERMAKER_FRONTEND_PORT` 修改宿主机端口：

```bash
EULERMAKER_FRONTEND_PORT=8088 docker compose -f hacks/docker-compose.yml up -d --build
```

前端容器通过同源路径代理 `/auth`、`/apis`、`/artifacts` 和 `/repositories`，浏览器不需要感知 Compose 内部服务地址。

## 检查和构建

```bash
npm run typecheck
npm run build
```
