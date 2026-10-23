# EulerMaker Frontend

EulerMaker 的基础 Web 控制台，使用 Vue 3、TypeScript、Vite、Vue Router 和 Pinia。

当前包含：工程列表、工程详情、账号登录，以及中文和 English 国际化。资源与认证请求统一通过 `ebs-gateway`。

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

Artifact 和仓库访问还支持 `/art-01/artifacts/...`、`/art-01/repositories/...` 形式：前端 Nginx 去掉实例前缀后转发到对应 Namespace 的 `artifact-manager` Service。未带实例前缀的 `/artifacts/...`、`/repositories/...` 保持转发到 `DEFAULT_ARTIFACT_MANAGER`，Docker Compose 默认是 `artifact-manager:8081`。动态路由的 DNS Resolver 默认读取容器 `/etc/resolv.conf`，也可通过 `NGINX_DNS_RESOLVER` 显式指定；实例命名匹配 `art-` 加数字编号。

### Runner 经前端上传

Runner 可将 `--gateway` 指向前端入口，通过 `/auth/`、`/apis/` 访问 Gateway；在 `Config/service-config` 中将所用制品实例的 `runnerInternalURL` 或 `runnerExternalURL` 设为同一入口（如 `https://ebs.example/art-01`），Runner 会在该前缀后拼接日志、产物和 Manifest 接口。地址末尾不要添加 `/artifacts`。使用 external 地址时设置 `--service-access-mode=external`，源码使用 Job payload 的 `origin_url`，保留固定 `commit_id`。构建容器须能访问上游仓库并获取该提交，无需配置 Git Server 外网地址；服务配置页面仅展示 Git Server 管理面和内网 Runner 地址，前端当前不代理 Git Server。

Artifact Manager 的默认及 `art-{数字}` 路由支持 HTTP/1.1 流式上传，关闭请求和响应缓冲，保留 Authorization、幂等键及日志分块头，不由 Nginx 自动重试上游请求。单次请求正文上限为 2 GiB，包含 multipart 元数据及边界开销，因此可上传文件须略小于 2 GiB；文件、Job 总大小和鉴权仍由 Artifact Manager 校验。经前端上传时同时受代理与后端限制，采用更严格的上限。

连接上游超时为 10 秒，读取上传正文、向上游发送请求和向客户端发送响应的空闲超时均为 300 秒，等待上游响应的空闲超时为 7200 秒。这些不是整个上传的总时限；Runner 和 Artifact Manager 仍各自执行上传超时。设置含义参见 [Nginx 请求正文配置](https://nginx.org/en/docs/http/ngx_http_core_module.html#client_body_timeout)及[代理配置](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_request_buffering)。外层负载均衡或 Ingress 若存在，也需允许相应的正文大小、流式转发和超时。`/auth/`、`/apis/` 保持原有配置。

## 检查和构建

```bash
npm run typecheck
npm run build
```

上传代理集成测试使用本机 Docker、已构建的前端镜像及 `node:22-alpine`，创建隔离的临时网络和容器并在结束时清理：

```bash
NGINX_TEST_IMAGE=eulermaker/frontend:dev node --test tests/artifactProxy.test.mjs
```

测试检查两类路径的超过 1 MiB 产物上传、未结束请求的实时转发、日志与 Manifest 路由、认证及幂等头透传。未设置 `NGINX_TEST_IMAGE` 时，常规测试仅验证代理配置。
