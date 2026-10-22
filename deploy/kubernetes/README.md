# CCE 部署 Manifest（自建集群适配模板）

本目录提供 EulerMaker 的 CCE 部署模板。etcd 和 Elasticsearch 各部署 3 个成员，每个 etcd 成员独占 100Gi EVS 卷，每个 Elasticsearch 成员独占 1Ti EVS 卷；Git Server 使用独占 PVC，Artifact Manager 使用专用节点的 `/srv/artifacts` 本地目录。容量和资源请求是起始值，上线前按数据量与压测结果调整。自建 Kubernetes 集群须按实际环境调整存储、节点和网络，不直接应用 CCE 专用 StorageClass。

以下按首次部署顺序组织。不要对整个目录执行 `kubectl apply -f`：`elasticsearch-bootstrap.yaml` 只能用于首次建群。已有数据的集群须制定独立迁移和恢复方案。

## 1. 初始化

确认 CCE 已安装 Everest CSI，所选可用区提供 SSD EVS。为至少 3 个 etcd 节点设置空值标签 `node-role.kubernetes.io/etcd=`，为另外至少 3 个 Elasticsearch 节点设置 `node-role.kubernetes.io/elasticsearch=`；两个 StatefulSet 都要求副本分布在不同节点。可用 `kubectl get nodes` 的 ROLES 列核对。

ebs-apiserver、Gateway、frontend、Scheduler、Controller Manager 和 Git Server 只调度到带 `node-role.kubernetes.io/ebs-control-plane=` 标签的节点。部署前为目标节点设置该标签并核对容量。

在 ES 节点持久设置 `vm.max_map_count >= 262144`，核对文件描述符上限，并确保成员间 `:2380`、`:9300` 可达。通过 VPC 和安全组限制数据服务端口，不要暴露到公网。`ebs-disk-ssd-retain` 使用 `WaitForFirstConsumer` 和 `Retain`；删除 PVC 不会自动删除底层 EVS。

先创建基础资源。`namespace.yaml` 只创建控制面 `ebs` 命名空间；首个 Artifact Manager 实例的 `art-01` 命名空间由其自身 Manifest 创建，本模板只部署这一个实例。自研组件的镜像统一使用 `swr.cn-north-4.myhuaweicloud.com/eulermaker/` 前缀，当前 Manifest 暂用 `dev` tag 和 `imagePullPolicy: Always`；部署前确认镜像已推送且节点可拉取，正式部署时改用不可变 digest。

```sh
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -f deploy/kubernetes/storageclass.yaml
kubectl apply -f deploy/kubernetes/pdb.yaml
```

## 2. 部署 etcd

### 2.1 签发证书

在受控环境运行脚本，输出目录必须不存在：

```sh
bash deploy/kubernetes/generate-etcd-apiserver-certs.sh /secure/path/to/etcd-certs
```

脚本按 `ebs` Namespace 和 `cluster.local` 域名签发三套独立 CA：客户端入口服务端 CA、客户端 CA 和成员间 peer CA。三个成员共用一套入口服务端证书和一套 peer 证书；etcd 自身连接客户端入口时使用独立客户端证书。`--ca-days` 默认 3650 天，`--client-days` 默认 365 天，服务端及 peer 证书默认 365 天。更改 Service 名称、Namespace 或集群域名时，先调整脚本中的 SAN 再签发。

| 用途 | 输出文件 |
| --- | --- |
| apiserver 信任 etcd 服务端 | `runtime/etcd-server-ca.crt` |
| etcd 验证 apiserver 客户端 | `runtime/etcd-client-ca.crt` |
| etcd 成员共用的入口服务端 | `runtime/etcd-server.crt`、`runtime/etcd-server.key` |
| etcd 自身连接客户端入口 | `runtime/etcd-internal-client.crt`、同名 `.key` |
| etcd 成员间双向认证 | `runtime/etcd-peer-ca.crt`、`runtime/etcd-peer.crt`、同名 `.key` |
| apiserver 访问 etcd | `runtime/ebs-apiserver-etcd-client.crt`、同名 `.key` |

`private/` 中的 CA 私钥不得放入 Pod 或提交仓库。共用 peer 证书不能区分具体成员；若需成员级隔离，应改为逐 Pod 证书与 Secret 分发。正式环境应安排受控 CA 和证书轮换流程。

### 2.2 创建 Secret

```sh
etcd_cert_dir=/secure/path/to/etcd-certs
kubectl -n ebs create secret generic etcd-client-tls \
  --from-file=server.crt="$etcd_cert_dir/runtime/etcd-server.crt" \
  --from-file=server.key="$etcd_cert_dir/runtime/etcd-server.key" \
  --from-file=client-ca.crt="$etcd_cert_dir/runtime/etcd-client-ca.crt" \
  --from-file=internal-client.crt="$etcd_cert_dir/runtime/etcd-internal-client.crt" \
  --from-file=internal-client.key="$etcd_cert_dir/runtime/etcd-internal-client.key"

kubectl -n ebs create secret generic etcd-peer-tls \
  --from-file=peer.crt="$etcd_cert_dir/runtime/etcd-peer.crt" \
  --from-file=peer.key="$etcd_cert_dir/runtime/etcd-peer.key" \
  --from-file=peer-ca.crt="$etcd_cert_dir/runtime/etcd-peer-ca.crt"

kubectl -n ebs create secret generic ebs-apiserver-etcd-tls \
  --from-file=server-ca.crt="$etcd_cert_dir/runtime/etcd-server-ca.crt" \
  --from-file=client.crt="$etcd_cert_dir/runtime/ebs-apiserver-etcd-client.crt" \
  --from-file=client.key="$etcd_cert_dir/runtime/ebs-apiserver-etcd-client.key"
```

### 2.3 部署与检查

```sh
kubectl apply -f deploy/kubernetes/etcd.yaml
kubectl -n ebs rollout status statefulset/etcd --timeout=10m
kubectl -n ebs get pods -l app.kubernetes.io/name=etcd
kubectl -n ebs get pvc
```

`etcd` 是 Headless Service，提供稳定成员 DNS；apiserver 通过三个成员地址和客户端证书访问。etcd 的探针使用独立的 metrics 端口。备份和运维客户端也须使用受信任证书，不能继续使用无证书的 `etcdctl`。

## 3. 部署 Elasticsearch

### 3.1 签发证书

`elasticsearch.yaml` 启用了安全功能：三个成员共用 transport 证书并验证专用 CA，因此 transport 使用 `verification_mode: certificate`；HTTP 使用独立证书。在受控环境签发，输出目录必须不存在：

```sh
bash deploy/kubernetes/generate-elasticsearch-certs.sh /secure/path/to/es-certs
```

脚本默认 CA 3650 天、叶证书 365 天，可用 `--ca-days`、`--cert-days` 调整。它按 `ebs` Namespace 和 `cluster.local` 域名签发证书；地址变化时先修改脚本中的 SAN。HTTP 证书须覆盖 `elasticsearch.ebs.svc.cluster.local`，transport 证书同时具备服务端和客户端认证用途。`private/` 中的 CA 私钥不能上传集群或提交仓库。共用 transport 私钥无法区分成员；如需成员级隔离，应使用逐成员证书与完整主机名校验。[Elastic transport TLS](https://www.elastic.co/docs/deploy-manage/security/set-up-basic-security)、[HTTPS 配置](https://www.elastic.co/docs/deploy-manage/security/set-up-basic-security-plus-https/)。

现有明文 ES 集群不能直接滚动更新到此配置：启用 transport TLS 须安排完整集群重启及回退窗口，先备份并确认数据兼容性。下面仅适用于首次建群。

### 3.2 创建 Secret

```sh
es_cert_dir=/secure/path/to/es-certs
kubectl -n ebs create secret generic elasticsearch-transport-tls \
  --from-file=ca.crt="$es_cert_dir/runtime/es-transport-ca.crt" \
  --from-file=tls.crt="$es_cert_dir/runtime/es-transport.crt" \
  --from-file=tls.key="$es_cert_dir/runtime/es-transport.key"

kubectl -n ebs create secret generic elasticsearch-http-tls \
  --from-file=ca.crt="$es_cert_dir/runtime/es-http-ca.crt" \
  --from-file=tls.crt="$es_cert_dir/runtime/es-http.crt" \
  --from-file=tls.key="$es_cert_dir/runtime/es-http.key"
```

### 3.3 首次建群与检查

```sh
kubectl apply -f deploy/kubernetes/elasticsearch-bootstrap.yaml
kubectl apply -f deploy/kubernetes/elasticsearch.yaml
kubectl -n ebs rollout status statefulset/elasticsearch --timeout=20m
```

ES 的 startup/readiness/liveness 探针只检查 TCP 端口，不能代表集群健康。首次启动后在 `elasticsearch-0` 本机交互设置 `elastic` 用户密码；重置工具使用节点本地临时用户，不能通过可能转发到其他节点的 Service 访问。再通过 HTTPS 和认证检查集群至少达到 yellow 且有三个节点：[Elastic 内置用户说明](https://www.elastic.co/guide/en/elasticsearch/reference/current/security-minimal-setup.html)。

```sh
kubectl -n ebs exec -it elasticsearch-0 -- \
  /usr/share/elasticsearch/bin/elasticsearch-reset-password -i -u elastic \
  --url https://localhost:9200

kubectl -n ebs exec -it elasticsearch-0 -- curl -fsS \
  --cacert /usr/share/elasticsearch/config/certs/http/ca.crt -u elastic \
  'https://elasticsearch.ebs.svc.cluster.local:9200/_cluster/health?wait_for_status=yellow&timeout=30s'
kubectl -n ebs exec -it elasticsearch-0 -- curl -fsS \
  --cacert /usr/share/elasticsearch/config/certs/http/ca.crt -u elastic \
  'https://elasticsearch.ebs.svc.cluster.local:9200/_cat/nodes?v'
```

确认三节点集群健康后，**必须移除** `cluster.initial_master_nodes` 并滚动重启，再次检查集群健康：

```sh
kubectl -n ebs delete configmap elasticsearch-bootstrap
kubectl -n ebs rollout restart statefulset/elasticsearch
kubectl -n ebs rollout status statefulset/elasticsearch --timeout=20m
kubectl -n ebs exec -it elasticsearch-0 -- curl -fsS \
  --cacert /usr/share/elasticsearch/config/certs/http/ca.crt -u elastic \
  'https://elasticsearch.ebs.svc.cluster.local:9200/_cluster/health?wait_for_status=yellow&timeout=30s'
```

后续重启或重新应用 `elasticsearch.yaml` 不会重新引导集群；**已有集群恢复时不要重新应用 `elasticsearch-bootstrap.yaml`**。[Elasticsearch 建群说明](https://www.elastic.co/guide/en/elasticsearch/reference/current/modules-discovery-bootstrap-cluster.html)。apiserver 创建的新 ES 索引设置 1 个副本；该配置不会自动更新已有索引。

## 4. 部署 ebs-apiserver

### 4.1 创建入站证书 Secret

第 2 节已创建 apiserver 访问 etcd 的 Secret。本组件还要求独立的入站证书 Secret：服务端证书须覆盖客户端使用的 Service DNS，客户端 CA 须信任 Gateway、Scheduler、Controller Manager 的证书。它们与 etcd 的 CA 相互独立。在受控环境签发证书，输出目录必须不存在：

```sh
bash deploy/kubernetes/generate-internal-certs.sh /secure/path/to/internal-certs
```

脚本默认 CA 有效期 3650 天、服务端与客户端证书 365 天，可用 `--ca-days`、`--cert-days` 调整。服务端证书覆盖 `ebs-apiserver`、`ebs-apiserver.ebs.svc` 和 `ebs-apiserver.ebs.svc.cluster.local`；三个客户端证书分别使用 Gateway、Scheduler、Controller Manager 的 URI SAN。脚本按 `ebs` Namespace 和 `cluster.local` 域名签发，地址变化时先修改 SAN。`private/` 中的 CA 私钥不能上传集群或提交仓库；只从 `runtime/` 创建 Secret。

```sh
internal_cert_dir=/secure/path/to/internal-certs
kubectl -n ebs create secret generic ebs-apiserver-serving-tls \
  --from-file=tls.crt="$internal_cert_dir/runtime/ebs-apiserver.crt" \
  --from-file=tls.key="$internal_cert_dir/runtime/ebs-apiserver.key"
kubectl -n ebs create secret generic ebs-apiserver-internal-client-ca \
  --from-file=ca.crt="$internal_cert_dir/runtime/client-ca.crt"
```

### 4.2 创建 ES API Key Secret

待 ES 集群健康后，在能解析集群内 Service DNS 的受控环境执行；`elastic` 密码由终端交互输入。API Key 的有效权限是角色描述与创建者权限的交集；`cluster: ["monitor"]` 用于 apiserver 启动时访问 ES 根路径的健康检查，`ebs-*` 索引权限用于资源读写和索引初始化。将响应中的 `encoded` 保存到受控文件，不提交仓库或放入 Pod 参数。[Elastic Create API Key API](https://www.elastic.co/docs/api/doc/elasticsearch/v8/operation/operation-security-create-api-key)。

```sh
curl --cacert "$es_cert_dir/runtime/es-http-ca.crt" -u elastic \
  -H 'Content-Type: application/json' \
  -X POST 'https://elasticsearch.ebs.svc.cluster.local:9200/_security/api_key' \
  -d '{
    "name": "ebs-apiserver",
    "role_descriptors": {
      "ebs-storage": {
        "cluster": ["monitor"],
        "indices": [{"names": ["ebs-*"], "privileges": ["all"]}]
      }
    }
  }'

kubectl -n ebs create secret generic ebs-apiserver-es-auth \
  --from-file=api-key=/secure/path/to/es-api-key \
  --from-file=ca.crt="$es_cert_dir/runtime/es-http-ca.crt"
```

`ebs-apiserver.yaml` 从 Secret 文件读取 API Key，并通过 `SSL_CERT_FILE` 信任 ES HTTP CA。轮换密钥或 CA 后须重启 apiserver Pod 以重新加载，不要关闭证书校验。

### 4.3 部署与检查

```sh
kubectl apply -f deploy/kubernetes/ebs-apiserver.yaml
kubectl -n ebs rollout status deployment/ebs-apiserver --timeout=10m
kubectl -n ebs get pods,pvc
```

apiserver 使用三个 etcd 成员地址，通过服务端 CA 校验成员身份；etcd 校验其客户端证书。确认 `/readyz` 正常、默认 Config/Script 和 ES 索引初始化成功，再由实际内部客户端验证 Job、Runner 读写及 List/Watch。

## 5. 部署 Scheduler

Scheduler 只依赖已就绪的 ebs-apiserver，通过独立的内部客户端证书访问 Job 和 Runner；此时尚无 Job 也可以启动。

```sh
kubectl -n ebs create secret generic scheduler-apiserver-tls \
  --from-file=ca.crt="$internal_cert_dir/runtime/server-ca.crt" \
  --from-file=tls.crt="$internal_cert_dir/runtime/scheduler.crt" \
  --from-file=tls.key="$internal_cert_dir/runtime/scheduler.key"
kubectl apply -f deploy/kubernetes/scheduler.yaml
kubectl -n ebs rollout status deployment/scheduler --timeout=10m
```

## 6. 部署 Git Server

`git-server.yaml` 创建普通 ClusterIP Service。Git Server 返回 `/git/...` 形式的 `clone_path`，无需预先配置或发现服务 IP；Controller Manager 客户端转换为 `git-server:///git/...` 逻辑地址。Runner 的 Git 逻辑地址解析尚未实现，补齐前不能使用该地址运行完整构建链路。当前不加载远端认证配置，只能匿名同步公开仓库；需要访问私有仓库时再通过 Secret 挂载 `auth.toml` 并配置 `--auth-config`。

Job 容器由宿主机 Docker 启动并使用宿主机网络，不在 Kubernetes Pod 网络内。启用 Snapshot Controller 前，必须从实际构建节点验证 Git Service ClusterIP 的 8080 端口可达；集群外构建节点通常不能直接访问 ClusterIP，若验证失败，应改用构建节点可达的固定内网入口。

克隆与管理 API 共用 HTTP 端口。配置集群外克隆入口时，仅转发 `/git/` 并使用 HTTPS，不暴露 `/api/v1/repo/*` 和 `/command`。手动克隆可将该入口与 `clone_path` 拼接，不能把逻辑地址直接交给 Git。

```sh
kubectl apply -f deploy/kubernetes/git-server.yaml
kubectl -n ebs rollout status deployment/git-server --timeout=20m
```

## 7. 部署 Gateway

Gateway 需要独立客户端证书和 JWT 签名密钥；证书须由 apiserver 信任的内部客户端 CA 签发，其 URI SAN 对应 Gateway 身份。在受控主机上生成一次 JWT 密钥文件，文件内容是单个 Base64 字符串，解码后为 32 字节随机密钥：

```sh
gateway_jwt_dir=/secure/path/to
install -d -m 0700 "$gateway_jwt_dir"
umask 077
openssl rand -base64 32 > "$gateway_jwt_dir/gateway-jwt-secret"
```

保管该文件并让所有 Gateway 副本使用同一份密钥；重新生成会使已签发的 Token 失效。创建 Kubernetes Secret 时直接使用文件内容，不要再次编码：

```sh
kubectl -n ebs create secret generic ebs-gateway-apiserver-tls \
  --from-file=ca.crt="$internal_cert_dir/runtime/server-ca.crt" \
  --from-file=tls.crt="$internal_cert_dir/runtime/ebs-gateway.crt" \
  --from-file=tls.key="$internal_cert_dir/runtime/ebs-gateway.key"
kubectl -n ebs create secret generic ebs-gateway-jwt \
  --from-file=secret="$gateway_jwt_dir/gateway-jwt-secret"
kubectl apply -f deploy/kubernetes/ebs-gateway.yaml
kubectl -n ebs rollout status deployment/ebs-gateway --timeout=10m
```

## 8. 部署 Artifact Manager

`artifact-manager.yaml` 先创建 `art-01` 命名空间，再在其中创建 Service 和 Deployment。Artifact Manager 通过 `ebs` 命名空间 Gateway 的 `/auth/check` 验证 Runner Token，部署前先确认 Gateway 已就绪。只给存放 Artifact 数据的节点设置空值标签 `node-role.kubernetes.io/art-01=`，并在该节点预先挂载 `/srv`、创建 `/srv/artifacts`。模板使用 `hostPath` 的 `Directory` 类型；目录不存在时 Pod 会启动失败，不会自动写入节点根文件系统。不要给多个持有不同数据的节点打同一标签；节点故障时须先恢复数据，再调整标签或启动 Pod。本地目录不提供自动故障转移，应单独安排备份。

在 Artifact 节点上以 root 执行以下命令。`findmnt /srv` 必须显示预期的数据盘，否则先停止，不要在节点根盘创建目录。容器内的 `ebs` 用户需要对挂载目录有写权限；模板的 `fsGroup: 1000` 不保证自动修改 `hostPath` 权限，因此将目录的组设为 `1000` 并开放组写入。`2` 设置 setgid，使新建子目录继承该组。

```sh
findmnt /srv
mkdir -p /srv/artifacts
stat -c '%U:%G %a %n' /srv/artifacts
chgrp 1000 /srv/artifacts
chmod 2770 /srv/artifacts
stat -c '%U:%G %a %n' /srv/artifacts
```

不要对已有大量产物递归执行 `chown`；若迁入了旧数据，需单独检查其中不可写的目录。当前模板由宿主机预置权限，不使用 root initContainer；如需在 Pod 启动时自动修复，只应针对挂载目录顶层做有限的权限调整。

```sh
kubectl label node <artifact-node> node-role.kubernetes.io/art-01=
kubectl get nodes -l node-role.kubernetes.io/art-01
```

RpmRepo Controller 根据 Project 的 `ebs.io/artifacts` annotation 从 `Config/service-config` 选择实例内部地址；BuildInfo Controller 也从该 Config 解析过程仓地址。前端 Nginx 将 `/art-01/artifacts/...` 和 `/art-01/repositories/...` 去掉实例前缀后转发到 `artifact-manager.art-01.svc.cluster.local:8081`；`NGINX_DNS_RESOLVER` 未配置时从 Pod 的 `/etc/resolv.conf` 读取 DNS 地址。原有 `/artifacts/...` 和 `/repositories/...` 仍转发到 `DEFAULT_ARTIFACT_MANAGER`，Kubernetes Deployment 将其设为 `artifact-manager.art-01:8081`，Docker Compose 使用默认短名。当前管理接口和仓库地址共用端口，不要将原始端口直接暴露到公网。

如果已经在 `ebs` 部署旧版 Artifact Manager，迁移前须停止旧 Deployment，确认其 Pod 已退出，避免两个实例同时写入同一 `/srv/artifacts`；保留宿主机数据目录。删除旧 Deployment 和旧 Service 后再应用下方 Manifest，并确认 `art-01` Pod 仍调度到原数据节点，不能因切换命名空间而指向空目录。前端和 Controller Manager 需使用本次更新的镜像或 Manifest，才能访问新地址。

```sh
kubectl apply -f deploy/kubernetes/artifact-manager.yaml
kubectl -n art-01 rollout status deployment/artifact-manager --timeout=20m
```

### 8.1 配置实例地址与 Runner 接入

apiserver 初始化的 `Config/service-config` 包含开发环境默认地址，已有对象不会被初始化流程覆盖。Kubernetes 部署后，由 Ops/Admin 通过 Gateway 更新该 Config 的 `spec.content`：配置 `gitServer.internalURL`，将 `artifactManagers` 中 `default` 和 `art-01` 的 `internalURL` 指向已部署的 `art-01` Service，并为 Runner 网络模式填写可达的 `runnerInternalURL` 或 `runnerExternalURL`。例如，Runner 在集群内且能够解析 Service DNS 时可使用：

```yaml
artifactManagers:
  default:
    internalURL: http://artifact-manager.art-01:8081
    runnerInternalURL: http://artifact-manager.art-01.svc.cluster.local:8081
    runnerExternalURL: ""
  art-01:
    internalURL: http://artifact-manager.art-01:8081
    runnerInternalURL: http://artifact-manager.art-01.svc.cluster.local:8081
    runnerExternalURL: ""
gitServer:
  internalURL: http://git-server:8080
  runnerInternalURL: http://git-server.ebs.svc.cluster.local:8080
  runnerExternalURL: ""
```

Git Server 的 Runner 地址使用服务入口，不附加 `/git`，代理路径前缀可保留。本阶段这两个地址仅保存配置，Gateway 尚未投影给 Runner，Runner 的 `git-server:///` 解析仍待实现。

`default` 是未设置 `ebs.io/artifacts` 的 Project 使用的实例；若将 Project 指向 `art-01`，创建或修改该 annotation 时也须确认 Config 中存在此键。BuildInfo Controller 创建 Job 时会将选中的实例名固化到 Job 的同名 annotation，Runner 据此选择日志和产物上传地址。不要将 Docker Compose 使用的 `127.0.0.1:8081` 原样用于异机 Runner。

本目录尚无 Runner Manifest。宿主机网络或集群外 Runner 不一定能解析 Service DNS，也不一定能访问 ClusterIP；应先从实际 Runner 节点验证 Gateway 和 Artifact Manager 地址。集群外 Runner 需要另外提供可达入口；使用 `--artifact-access-mode=external` 时读取 `runnerExternalURL`，允许 HTTP(S)，公网入口建议使用 HTTPS。Runner 启动时读取一次地址列表，之后约每 600 秒刷新；修改 Config 后可重启 Runner 使新地址立即生效。

Runner 在写入 `payload.json` 前，按逻辑 URL 中的实例名将 `repo[].url` 的 `artifact://{instance}/repositories/...` 转换为选定网络模式下的 HTTP(S) URL；省略实例名时使用 `default`。普通 HTTP(S) 仓库地址保持不变。仓库所在实例缺失或对应 Runner 地址为空时，Job 会报出明确的仓库解析错误，因此上线前应从构建节点验证所需实例的地址及仓库路径可达。

## 9. 部署 Controller Manager

客户端证书的 URI SAN 须对应 Controller Manager 身份，不能复用 Gateway 证书。确认 Git Server 和 Artifact Manager 的 Service 已创建，且 Controller Manager Pod 可访问它们。

Kubernetes 模板设置 `--job-history-action=archive`：终态 Job 默认保留 30 天，到期后先通过 apiserver 归档到 ES `ebs-oldjobs`，确认成功后再从 etcd 删除；归档失败则保留源 Job。归档仅包含 Job 对象，不包含日志和产物文件。

```sh
kubectl -n ebs create secret generic controller-manager-apiserver-tls \
  --from-file=ca.crt="$internal_cert_dir/runtime/server-ca.crt" \
  --from-file=tls.crt="$internal_cert_dir/runtime/controller-manager.crt" \
  --from-file=tls.key="$internal_cert_dir/runtime/controller-manager.key"
kubectl apply -f deploy/kubernetes/controller-manager.yaml
kubectl -n ebs rollout status deployment/controller-manager --timeout=10m
```

## 10. 部署前端

```sh
kubectl apply -f deploy/kubernetes/frontend.yaml
kubectl -n ebs rollout status deployment/frontend --timeout=10m
```

模板只创建前端的 ClusterIP Service。如需从集群外访问，请自行创建指向 `frontend:8080` 的 Ingress，或将前端 Service 配置为 NodePort；访问地址与 TLS 按实际环境配置。不要直接对外暴露 apiserver、Git Server HTTP 管理口或 Artifact Manager 内部接口。

## 运维边界

- 不直接修改 StatefulSet `replicas` 扩缩已有 etcd 集群；使用 etcd `member add/remove` 并调整配置，避免同时重启多数成员。ES 扩缩容须核对分片迁移与节点状态。
- 定期执行 etcd 逻辑快照和 ES 快照并备份到 OBS；本目录尚无备份任务或 OBS 快照仓库配置。EVS 快照不能代替应用一致性备份。
- 不要通过删除有数据的 PVC 解决启动故障。Compose 的 `/srv/etcd`、`/srv/es` 不能在服务运行时直接复制到 PVC；迁移前须停止写入并分别制定恢复流程。
- Secret 明文、TLS 私钥、Git 认证和 MachineAccount 凭据不能写入 ConfigMap、镜像或仓库；轮换证书时先更新信任链，再滚动重启相关组件。

CCE 的 [StorageClass 参数与回收策略](https://support.huaweicloud.com/usermanual-cce/cce_10_0380.html)以及 [etcd StatefulSet 部署示例](https://etcd.io/docs/v3.5/op-guide/kubernetes/)可用于核对集群版本差异。
