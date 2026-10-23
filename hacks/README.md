# Docker Compose 开发部署

`docker-compose.yml` 是基础开发配置。`docker-compose.tls.yml` 是覆盖文件，继承基础文件中的镜像构建、网络、端口和数据挂载，不能单独启动。所有命令在仓库根目录执行。

## 极简开发调试：仅使用基础 Compose

只调试功能、不验证内部 TLS 认证时，直接使用 `docker-compose.yml`，无需生成证书、ES 密码或 API Key。该模式关闭 apiserver 客户端证书认证，内部客户端跳过其服务端证书校验；etcd、ES 使用无认证的 HTTP。Gateway 的用户登录和权限校验仍然保留。

先准备 `/srv/etcd`、`/srv/es`、`/srv/git`、`/srv/artifact` 数据目录，确保容器用户可写；ES 使用 UID 1000，Git Server 和 Artifact Manager 使用各自镜像内的 `ebs` 用户。已有目录应保留数据和权限，不要直接覆盖。

构建本地镜像，并在 Gateway JWT Secret 不存在时生成一次：

```bash
docker compose -f hacks/docker-compose.yml build
(
  umask 077
  set -o noclobber
  openssl rand -hex 32 > hacks/ebs-gateway-jwt-secret
)
```

如果 Secret 已存在，跳过生成步骤。文件型 Secret 保留宿主机权限，可按实际镜像用户设置读取权限：

```bash
gateway_uid=$(docker compose -f hacks/docker-compose.yml run --rm --no-deps --entrypoint id ebs-gateway -u)
sudo chown "$gateway_uid" hacks/ebs-gateway-jwt-secret
sudo chmod 0400 hacks/ebs-gateway-jwt-secret

docker compose -f hacks/docker-compose.yml up -d
docker compose -f hacks/docker-compose.yml logs -f
```

默认通过 `http://localhost` 访问前端；端口冲突时可用 `EULERMAKER_FRONTEND_PORT=5173 docker compose -f hacks/docker-compose.yml up -d`。修改后端代码后，使用 `docker compose -f hacks/docker-compose.yml up -d --build <服务名>` 重建对应服务。

基础配置不包含 Runner，实际执行构建需另行启动 `docker-compose.runner.yml`，并在 `Config/service-config` 中配置 Runner 和 Job 容器可访问的服务地址。`RUNNER_SERVICE_ACCESS_MODE` 默认 `internal`，使用两类服务的 `runnerInternalURL`；设为 `external` 时使用制品服务的 `runnerExternalURL`，源码直接访问 payload 的 `origin_url`，保留固定 `commit_id`，无需 Git Server 外网地址。同机宿主机网络可使用初始化的回环地址，异机部署须修改服务地址，并设置 `RUNNER_GATEWAY_URL` 和唯一的 `RUNNER_NAME`。这套配置仅适用于隔离的开发环境：映射的 etcd、ES 和 apiserver 端口不应暴露给不可信网络。不要直接复用 TLS 部署的数据目录切换模式。

## TLS 联调

| 链路 | TLS 覆盖文件的配置 |
| --- | --- |
| Gateway / Scheduler / Controller Manager → apiserver | mTLS，分别使用组件客户端证书 |
| apiserver → etcd | mTLS，使用独立的 etcd 客户端证书 |
| etcd peer | 双向 TLS；Compose 仍是单成员开发集群 |
| apiserver → Elasticsearch | HTTPS 校验服务端证书，API Key 认证 |
| Elasticsearch transport | 双向 TLS；Compose 仍是单节点开发集群 |

Gateway 对外入口、Git Server、Artifact Manager 和前端保持基础配置，不因该覆盖文件自动启用 HTTPS。Runner 独立部署，配置见 `docker-compose.runner.yml`。

### 1. 准备证书和 Secret

需要 Docker Compose、OpenSSL、curl 和 jq。基础配置中的 `/srv/etcd`、`/srv/es`、`/srv/git`、`/srv/artifact` 应已准备好，并允许对应容器用户读写；ES 数据目录的容器 UID 为 1000。

```bash
bash hacks/generate-internal-certs.sh hacks/certs
```

脚本拒绝覆盖已有目录，生成的证书有效期为 3650 天（约 10 年），仅用于开发验证：

| 文件（相对于 `hacks/certs/`） | 用途 |
| --- | --- |
| `private/*.key` | CA 私钥，仅保存在宿主机，不挂载到容器 |
| `runtime/server-ca.crt`、`client-ca.crt` | apiserver 服务端和内部组件客户端信任链 |
| `runtime/ebs-apiserver.{crt,key}` | apiserver 服务端身份 |
| `runtime/{ebs-gateway,scheduler,controller-manager}.{crt,key}` | 内部组件客户端身份 |
| `runtime/etcd-ca.crt`、`etcd.{crt,key}` | etcd 信任链和单节点服务端、peer、自连接及健康检查身份 |
| `runtime/ebs-apiserver-etcd.{crt,key}` | apiserver 访问 etcd 的客户端身份 |
| `runtime/es-ca.crt`、`es-http.{crt,key}`、`es-transport.{crt,key}` | ES 信任链、HTTPS 和 transport 身份 |

服务端 SAN 包含 Compose 服务名、`localhost` 和 `127.0.0.1`；访问其他主机名或 IP 时应重新签发包含对应 SAN 的证书，不要跳过校验。

文件型 Compose Secret 保留宿主机权限。脚本将运行时私钥设为 `0444`，供非 root 容器读取；证书根目录及其 `private`、`runtime` 子目录保持 `0700`，CA 私钥保持 `0600`。宿主机其他用户无法穿过这些目录读取密钥。各容器只挂载自己的文件，不挂载 CA 私钥或整个 runtime 目录。[Compose Secret 权限说明](https://docs.docker.com/reference/compose-file/services/#secrets)

为首次启动的 ES 创建随机管理员密码。已有文件时保留原值：

```bash
(
  umask 077
  set -o noclobber
  openssl rand -base64 32 > hacks/certs/runtime/es-elastic-password
)
sudo chown 1000:0 hacks/certs/runtime/es-elastic-password
sudo chmod 0400 hacks/certs/runtime/es-elastic-password
```

ES 镜像要求 `ELASTIC_PASSWORD_FILE` 为 `0400` 或 `0600`，且容器 UID 1000 能读取，所以密码文件不能沿用运行时证书私钥的 `0444`。上面的所有权调整仅针对该密码文件，不修改 CA 私钥。读取它时也需要相应的宿主机权限。

如果尚未创建 Gateway JWT Secret，再执行以下命令；不要覆盖已有 JWT Secret：

```bash
(
  umask 077
  set -o noclobber
  openssl rand -hex 32 > hacks/ebs-gateway-jwt-secret
)
```

JWT 文件也需要容器用户可读。可将受 `0700` 父目录保护的副本用于 TLS 覆盖配置：

```bash
install -m 0444 hacks/ebs-gateway-jwt-secret hacks/certs/runtime/ebs-gateway-jwt-secret
```

### 2. 先启动 etcd 和 Elasticsearch

```bash
docker compose -f hacks/docker-compose.yml -f hacks/docker-compose.tls.yml \
  up -d --wait etcd elasticsearch
```

etcd 健康检查使用客户端证书。ES 健康检查验证 HTTPS 可达且匿名请求返回 401；它只表示认证入口已启动，不代替下方经过认证的集群健康检查。

通过受控方式查看 `hacks/certs/runtime/es-elastic-password`，在 curl 提示时输入密码，不将密码直接写在命令行中：

```bash
curl --fail --show-error --cacert hacks/certs/runtime/es-ca.crt -u elastic \
  'https://localhost:9200/_cluster/health?wait_for_status=yellow&timeout=60s'
```

确认响应 `timed_out=false`，且状态为 `yellow` 或 `green`。单节点下有副本分片时 `yellow` 正常。

`ELASTIC_PASSWORD_FILE` 用于初始化密码，不会覆盖已有安全索引中的密码。已有 ES 数据应使用现有密码，必要时通过 `elasticsearch-reset-password --url https://localhost:9200 -i -u elastic` 重置。已有 etcd 数据会保留成员 peer URL；从明文切换到 TLS 时需先规划成员 URL 更新，不能仅修改 `--initial-cluster`，也不要为切换 TLS 直接删除数据。

### 3. 创建 apiserver 的 ES API Key

以下命令由终端提示输入 `elastic` 密码，API Key 不输出到终端。需要 `monitor` 权限完成 apiserver 启动检查，`ebs-*` 索引权限用于资源读写、alias 和索引初始化。[Elasticsearch API Key 接口](https://www.elastic.co/docs/api/doc/elasticsearch/v8/operation/operation-security-create-api-key)

```bash
(
  set -euo pipefail
  umask 077
  test ! -e hacks/certs/runtime/es-api-key
  es_api_key=$(curl --fail --silent --show-error \
    --cacert hacks/certs/runtime/es-ca.crt -u elastic \
    -H 'Content-Type: application/json' \
    -X POST https://localhost:9200/_security/api_key \
    -d '{"name":"ebs-apiserver","role_descriptors":{"ebs-storage":{"cluster":["monitor"],"indices":[{"names":["ebs-*"],"privileges":["all"]}]}}}' \
    | jq -er '.encoded | select(type == "string" and length > 0)')
  printf '%s\n' "$es_api_key" > hacks/certs/runtime/es-api-key
  chmod 0444 hacks/certs/runtime/es-api-key
)
```

Key 文件保存的是接口返回的 `encoded` 值，不是单独的 `id` 或 `api_key`。apiserver 仅挂载 API Key，不挂载 `elastic` 密码；通过 `SSL_CERT_FILE` 信任 ES CA。密码和 API Key 均放在私有父目录下，不提交仓库。证书或 API Key 轮换后需重新创建相关容器以重新挂载、加载文件。

### 4. 启动应用组件

```bash
docker compose -f hacks/docker-compose.yml -f hacks/docker-compose.tls.yml config --quiet
docker compose -f hacks/docker-compose.yml -f hacks/docker-compose.tls.yml up -d --build
```

覆盖文件的 `command` 整体替换基础命令，移除 apiserver 的匿名客户端开关和内部客户端的跳过服务端证书校验开关。后续 `up`、`logs`、`down` 等操作都使用同一组 `-f` 文件。

当前内部证书认证只识别组件身份，尚未实现逐接口授权；Compose TLS 配置不是完整的生产安全部署方案。
