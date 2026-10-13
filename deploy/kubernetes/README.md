# CCE 部署 Manifest（自建集群适配模板）

本目录提供 EulerMaker 的 CCE 部署模板。etcd 和 Elasticsearch 各部署 3 个成员，每个 etcd 成员独占 100Gi EVS 卷，每个 Elasticsearch 成员独占 1Ti EVS 卷；Git Server 和 Artifact Manager 分别使用单实例独占 PVC。容量和资源请求是起始值，上线前按数据量与压测结果调整。自建 Kubernetes 集群须按实际环境调整存储、节点和网络，不直接应用 CCE 专用 StorageClass。

以下按首次部署顺序组织。不要对整个目录执行 `kubectl apply -f`：`elasticsearch-bootstrap.yaml` 只能用于首次建群。已有数据的集群须制定独立迁移和恢复方案。

## 1. 初始化

确认 CCE 已安装 Everest CSI，所选可用区提供 SSD EVS。为至少 3 个 etcd 节点设置空值标签 `node-role.kubernetes.io/etcd=`，为另外至少 3 个 Elasticsearch 节点设置 `node-role.kubernetes.io/elasticsearch=`；两个 StatefulSet 都要求副本分布在不同节点。可用 `kubectl get nodes` 的 ROLES 列核对。

在 ES 节点持久设置 `vm.max_map_count >= 262144`，核对文件描述符上限，并确保成员间 `:2380`、`:9300` 可达。通过 VPC 和安全组限制数据服务端口，不要暴露到公网。`ebs-disk-ssd-retain` 使用 `WaitForFirstConsumer` 和 `Retain`；删除 PVC 不会自动删除底层 EVS。

先创建基础资源。自研组件的镜像统一使用 `swr.cn-north-4.myhuaweicloud.com/eulermaker/` 前缀；部署各组件前将 Manifest 中的 `latest` 替换为已推送且节点可拉取的版本或 digest。

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
