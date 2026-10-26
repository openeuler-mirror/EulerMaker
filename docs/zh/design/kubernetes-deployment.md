# Kubernetes 部署方案

本文定义 EulerMaker 当前架构的 Kubernetes 部署边界和首版落地方式，不将现有 Runner 改写成 Kubernetes Job 执行器。`hacks/docker-compose.yml` 仍用于本地开发；`deploy/kubernetes/` 中的 Manifest 以 CCE 为模板，自建 Kubernetes 集群需按下文适配。单副本约束是当前实现约束，不代表 Kubernetes 自身无法扩容。

## 部署边界

部署分为控制面、持久化服务和构建节点三部分：

```text
浏览器 ── 自行配置的入口 ── frontend ── ebs-gateway ── ebs-apiserver
                              │                 │                   ├── 专用 etcd：Job、Runner
                              │                 │                   └── Elasticsearch：其他资源与 IAM
                              │                 └── /auth/check
                              └── /artifacts、/repositories ── artifact-manager ── 节点本地目录

controller-manager ── ebs-apiserver、git-server、artifact-manager
scheduler ── ebs-apiserver
Runner 与其 Job 容器（Docker/containerd）── 可从构建节点访问的 Gateway、Artifact Manager、Git 源码入口
```

- 首版将前端、Gateway、apiserver、Scheduler、Controller Manager 和 Git Server 放在 `ebs` Namespace，Artifact Manager 单独放在 `art-01` Namespace。前端和 Controller Manager 使用 `artifact-manager.art-01` 访问它。构建节点可在集群外；不要求 Runner 使用集群 ServiceAccount，也不让 Job 容器直接访问 Kubernetes API。
- etcd 和 Elasticsearch 在目标集群中自部署，数据使用持久卷。
- 自研组件镜像从仓库内各组件 Dockerfile 构建，推送到 `swr.cn-north-4.myhuaweicloud.com/eulermaker/`。模板暂用 `:dev` 标签；投产时应固定到不可变 digest。应用参数由容器 `args` 传递；非敏感参数由部署模板管理，JWT 密钥、仓库凭据、签名凭据和私钥由 Secret 或外部密钥系统只读挂载。
- `Config` 和 `Script` 是 EulerMaker API 对象，不用 Kubernetes ConfigMap 替代。apiserver 初始化默认对象后，后续修改仍走 EulerMaker API。

## 工作负载与副本

| 组件 | Kubernetes 资源 | 首版副本 | 服务与探针 | 约束 |
| --- | --- | ---: | --- | --- |
| frontend | Deployment | 3 | ClusterIP `:8080`；`/healthz` | 调度到 `ebs-control-plane` 节点；`/art-xx/artifacts/...` 和 `/art-xx/repositories/...` 按实例 Namespace 转发，原有无前缀路径继续访问配置的 Artifact Manager |
| ebs-gateway | Deployment | 3 | ClusterIP `:8080`；`/healthz` | 调度到 `ebs-control-plane` 节点；所有副本使用同一个 JWT 签名密钥；当前限流是单副本本地计数 |
| ebs-apiserver | Deployment | 3 | 仅集群内 ClusterIP `:8443`；HTTPS `/livez`、`/readyz` | 三副本共用 ES 和 etcd 存储；不直接对外暴露 |
| scheduler | Deployment，`Recreate` | 1 | 仅内部 `:8080/healthz`、`/readyz`、`/metrics` | 当前没有 Leader Election，不允许两实例同时调度；更新时先停旧 Pod |
| controller-manager | Deployment，`Recreate` | 1 | 仅内部 `:8080/healthz`、`/readyz`、`/metrics` | 当前没有 Leader Election，不允许两实例同时运行控制器 |
| git-server | Deployment（`Recreate`）+ 独占 PVC | 1 | HTTP `:8080/healthz`、`/readyz`；Smart HTTP `:8080/git/` | 同一数据目录不能由两个写入实例共享；HTTP 管理口仅供 Controller Manager 访问 |
| artifact-manager | `art-01` Namespace 中的 Deployment（`Recreate`）+ 节点本地目录 | 1 | HTTP `:8081/healthz`、`/readyz` | 固定到唯一带 `node-role.kubernetes.io/art-01=` 标签的节点；Artifact、过程仓、正式仓及临时目录必须在同一文件系统 |
| etcd | StatefulSet + 每成员独立 EVS PVC | 生产 3；开发 1 | 仅集群内客户端和成员通信地址 | 跨可用区部署前验证成员间延迟 |
| Elasticsearch | StatefulSet + 每数据节点独立 EVS PVC | 生产至少 3；开发 1 | 集群内 HTTPS、API Key 认证和节点间双向 TLS | 自部署与当前应用兼容的版本；生产配置副本分片和节点反亲和，不以共享卷代替节点副本 |

需要从集群外访问时，仅开放前端入口，以及供构建节点使用的 Gateway、Artifact Manager 和 Git Smart HTTP 地址。前端模板本身只提供 ClusterIP；部署方自行创建 Ingress 或将 Service 配置为 NodePort，并按环境配置访问地址与 TLS。Scheduler、Controller Manager 的健康端口及 apiserver、Git Server HTTP 管理口不作为公网 Service。Git Server 使用单副本 Deployment 和独立 PVC，采用 `Recreate` 更新以避免新旧 Pod 同时写入；持久卷不自动提供数据层高可用。

### Runner 执行环境与服务访问

Runner 在专用构建节点上以容器或宿主机服务运行，由 Kubernetes 管理控制面。每台节点使用独立 Runner 名称、MachineAccount 凭据和持久化的 `instanceID` 文件；重启后必须沿用同一身份。`--container-runtime` 支持 `docker` 和 `containerd`：前者通过 Docker Go Client 访问 daemon，后者通过原生 Go Client 访问 containerd。使用 containerd 时配置对应 socket、namespace 和 snapshotter；运行时配置与恢复规则见 [Runner 设计](runner.md)。

当前 Job 容器由 Runner 直接通过宿主机运行时创建，使用宿主机网络，不作为 Kubernetes Pod 运行。Git Server 返回与地址无关的 `clone_path`，管理面将其保存为 `git-server:///...` 逻辑地址。Runner 从 Gateway 的 `service-endpoints` 获取地址：internal 模式使用 `gitServer.runnerInternalURL` 解析克隆地址；external 模式使用 payload 的 `origin_url`，保留 `commit_id`。启用构建前，应从实际 Job 容器验证对应 Git 入口可达，不能假定其具有 Kubernetes Pod 的 DNS 和 Service 访问能力。

Controller Manager 从 `Config/services` 取得管理请求使用的集群内地址：`gitServer.internalURL` 配置 Git Server（同命名空间可用 `http://git-server:8080`），`artifactManagers` 配置 Artifact Manager 实例表；不再通过 `--git-server-addr` 指定地址。初始化及更新规则见 [服务配置](data-models~config.md#4-服务配置内容)。RpmRepo Controller 根据 Project 的 `ebs.io/artifacts` annotation 选择实例。下发给 Job 的过程仓保持 `artifact://{instance}/...` 逻辑 URL，由 Runner 使用其可访问的 Artifact Manager 地址解析。启用构建前须从 Runner 和实际 Job 容器验证该地址，不应直接将 Artifact Manager 原始管理端口暴露到公网。

## 存储与恢复

| 数据 | 挂载或服务 | 要求 |
| --- | --- | --- |
| Job、Runner | EulerMaker etcd；每成员独立 100Gi 高性能 EVS PVC | 定期生成 etcd 逻辑快照并备份到 OBS，演练恢复 |
| 其他 API 对象与 IAM | 自部署 Elasticsearch；每数据节点独立 1Ti SSD EVS PVC | 定期生成 Elasticsearch 快照并备份到 OBS；升级前核对当前索引 alias 和 mapping |
| Git 裸仓库 | Git Server PVC，挂载到 `/var/lib/ebs-git` | 单写者；容量和 IOPS 按镜像仓规模估算 |
| Artifact、RPM 仓库及发布目录 | 专用节点的 `/srv/artifacts` 通过 `hostPath` 挂载到 `/var/lib/ebs-artifacts` | 同一文件系统须支持硬链接与原子 rename；上传、过程仓、正式仓不能拆到不同卷；节点故障后须恢复本地数据 |
| Runner work/results/cleanup/instance ID | 每个构建节点的 `/var/lib/ebs-runner` | 节点本地持久化；磁盘容量、inode 和清理水位单独监控 |

各组件的首版 CCE Manifest、部署顺序和限制见 [部署说明](../../../deploy/kubernetes/README.md)。apiserver 新建的 ES 索引有 1 个副本；已有索引不会因代码升级自动改变副本数，迁移时须单独检查并更新。Manifest 已配置 apiserver → etcd 客户端入口、etcd peer、ES transport 的双向 TLS，以及 ES HTTPS/API Key；尚未包含备份任务和证书轮换自动化。证书边界见 [etcd 传输与客户端证书认证](./ebs-apiserver.md#etcd-传输与客户端证书认证)，正式投产前需验证。

etcd 和 Elasticsearch 的在线数据卷使用 CCE `csi-disk` 或具备可用区感知的 `csi-disk-topology`，通过 StatefulSet `volumeClaimTemplates` 为每个成员创建独立的 `ReadWriteOnce` PVC；优先选择满足实际延迟、IOPS 和容量需求的 SSD 云硬盘。Pod 应固定到与卷兼容的可用区；设置节点反亲和与拓扑分散，避免多个成员落在同一节点。生产需使用保留底层卷的回收策略，避免删除 StatefulSet/PVC 时意外丢失数据，并定期验证扩容、节点迁移和恢复流程。SFS、OBS 不作为 etcd 数据目录或 Elasticsearch 索引目录的在线存储；OBS 仅承载备份。CCE 对云硬盘的 [StatefulSet 挂载及 RWO 约束](https://support.huaweicloud.com/usermanual-cce/cce_10_0616.html)和 [etcd 的磁盘建议](https://etcd.io/docs/v3.5/op-guide/hardware/)是卷选型依据。

etcd 快照使用 `etcdctl snapshot save` 等逻辑备份方式；Elasticsearch 使用快照仓库，不以直接复制运行中的 PVC 代替应用一致性快照。Elasticsearch 快照到 OBS 所需的仓库插件或兼容接口、凭据配置应先在选定版本上验证。Artifact Manager 的物化会从 Artifact 和基础过程仓硬链接 RPM，并在同一数据根目录内原子重命名工作目录；节点本地目录必须支持这些语义。首版固定在一个持有数据的节点运行，不以多个节点的不同本地目录伪装高可用。etcd、Elasticsearch 和 Artifact Manager 的备份需要记录同一恢复点的关系；跨存储没有分布式事务，不能声称分别取得的快照天然一致。已有 Compose 数据迁移时先停止写入，备份 `/srv/etcd`、`/srv/es`、`/srv/git`、`/srv/artifact`，再分别导入对应 StatefulSet PVC、Git Server PVC 或 Artifact Manager 节点目录，并校验对象、仓库与下载链路；不直接复制运行中的目录。

## 网络与身份

| 流量 | 地址或入口 | 控制 |
| --- | --- | --- |
| 浏览器到 frontend | 默认仅 ClusterIP；外部入口自行选择 Ingress 或 NodePort | 仅开放前端；由现有 Nginx 转发 `/auth`、`/apis`、`/artifacts`、`/repositories` |
| Gateway、Scheduler、Controller Manager 到 apiserver | `https://ebs-apiserver:8443` ClusterIP | 服务端 CA 校验和各自客户端证书；apiserver 仅内部可达 |
| apiserver 到 etcd | `https://etcd-{0,1,2}.etcd:2379` 三个成员地址 | 验证 etcd 服务端 CA 与各成员 DNS SAN；etcd 验证 apiserver 客户端证书 |
| etcd 成员间 | 各成员 Headless Service FQDN 的 `:2380` | HTTPS 和独立 peer CA；当前三个成员共用一套 peer 证书，尚未按成员隔离私钥 |
| apiserver 到 Elasticsearch | `https://elasticsearch:9200` | 同 Namespace Service；校验 ES HTTP 服务端证书，并从 Secret 文件读取限定 `ebs-*` 索引的 API Key |
| Elasticsearch 成员间 | 各成员的 `:9300` | transport 双向 TLS；当前共用成员证书并验证专用 CA |
| Controller Manager 到 Git Server HTTP、Artifact Manager 内部接口 | 两者均使用集群内 Service | 限制来源为 Controller Manager；内部接口不得经前端公网路由暴露 |
| Runner 到 Gateway、Artifact Manager | 构建节点可达的 HTTPS 内网入口 | Runner Token；Artifact Manager 经 Gateway `/auth/check` 验证 Token |
| Job 容器到 Git Smart HTTP、Artifact 仓库 | 构建节点可达的内网域名 | Git Smart HTTP 只读；按构建节点网段配置防火墙和访问控制 |

内部客户端 mTLS 需要服务端证书包含客户端实际连接名的 SAN；若客户端使用 `ebs-apiserver.ebs.svc`，不能只签发 `ebs-apiserver`。开发脚本生成的长期证书不直接作为生产证书。正式部署须启用 apiserver 客户端证书校验，不能沿用 Compose 中的 `--insecure-skip-verify` 或无认证开关。当前组件身份授权若仍为“持证组件均可访问所有业务 API”，部署仍须通过目标环境的网络 ACL、安全组或节点防火墙限制访问；逐资源授权完成前不能宣称最小权限。

Gateway JWT、ES API Key、各组件 TLS 私钥、Runner MachineAccount 凭据及可选的 Git Server `auth.toml`、GPG/Signatrust 凭据分别放入 Secret，不写进 ConfigMap 或镜像。当前 Git Server 模板仅匿名访问公开仓库，不需要认证 Secret。ES 探针只检查 TCP 端口，部署时须单独用 HTTPS 和认证验证集群健康；步骤与限制见 [Elasticsearch 部署章节](../../../deploy/kubernetes/README.md#3-部署-elasticsearch)。etcd 服务端、客户端和 peer 证书使用独立信任链，签发 CA 私钥不挂载到工作负载。证书和密钥轮换先更新信任链，再滚动重启客户端及服务端；JWT 密钥轮换会使既有 Token 失效，需单独安排窗口。

## 自建 Kubernetes 集群适配

`deploy/kubernetes/` 提供 CCE 版数据服务和应用组件模板，但私有入口、凭据与证书、备份、数据服务剩余 TLS 等配置仍需落地，不是可直接投产的部署包。自建集群保留 `ebs` Namespace、服务名、StatefulSet 稳定成员名及应用访问路径；不要原样应用云厂商专用的存储配置。适配时逐项核对：

| 范围 | CCE 模板中的假设 | 自建集群需调整或验证 |
| --- | --- | --- |
| 存储类与 PVC | `storageclass.yaml` 使用 Everest CSI、EVS SSD；etcd、ES 的 `volumeClaimTemplates` 和 Git Server 的独立 PVC 引用 `ebs-disk-ssd-retain`；Artifact Manager 使用 `/srv/artifacts` 本地目录 | 换成已安装的 CSI 和适用的块存储类，或预置 PV；同步修改两个 StatefulSet 的 `volumeClaimTemplates` 和 Git Server PVC 的 `storageClassName`。确认独立 `ReadWriteOnce` 卷、`Retain` 回收策略、扩容能力及绑定模式；另在唯一 Artifact 节点准备可写的 `/srv/artifacts` 并安排备份，节点故障后不会自动迁移数据。 |
| 节点与调度 | etcd 使用空值标签 `node-role.kubernetes.io/etcd=`，ES 使用 `node-role.kubernetes.io/elasticsearch=`；apiserver、Gateway、前端、Scheduler、Controller Manager 和 Git Server 使用 `node-role.kubernetes.io/ebs-control-plane=`；Artifact Manager 使用 `node-role.kubernetes.io/art-01=` | 分别准备至少 3 个 etcd、3 个 ES 专属节点，并只给持有 `/srv/artifacts` 数据的节点打 `art-01` 标签；为业务控制面节点设置自定义标签并核对容量。通过 `kubectl get nodes` 的 ROLES 列核对。开发集群若减少 etcd 或 ES 副本，须同步调整初始成员、首次引导名单和 PDB 的 `minAvailable`，不能只改 `replicas`。 |
| 节点内核与镜像 | ES 需要宿主机 `vm.max_map_count >= 262144`；镜像来自 `quay.io` 和 `docker.elastic.co` | 在 ES 目标节点持久配置内核参数；确认目标架构有对应镜像，节点能拉取，或改成私有镜像仓库地址。按实际容量重新评估资源请求和 PVC 大小。 |
| 入口与构建网络 | 控制面使用集群 DNS；Runner 和 Job 容器需访问集群外也可达的 Gateway、Artifact Manager、Git Smart HTTP 地址 | 提供构建节点可达的入口、内部 DNS、证书和防火墙规则。`*.svc.cluster.local` 只用于集群内连接，不直接下发给集群外构建节点；限制 Artifact Manager 管理接口的网络可达范围。 |
| Runner 执行环境 | Runner 支持 Docker Go Client 和 containerd 原生 Go Client | 按节点运行时选择 `--container-runtime`，配置 socket 访问权限；containerd 还需匹配 namespace、snapshotter。验证镜像拉取、挂载、宿主机网络与重启恢复，详见 [Runner 设计](runner.md)。 |
| 备份与凭据 | 文档以 OBS 保存 etcd、ES 备份；Manifest 已配置 etcd 和 ES 的 TLS/认证，但尚无备份任务或自动轮换 | 将备份目的地换成实际可用的对象存储或备份系统，验证 etcd 逻辑快照与 ES 快照仓库恢复；为生产落实 Secret 管理和轮换。不能用在线 PVC 文件复制代替一致性备份。 |
| 应用组件 | 目录已有 frontend、Gateway、apiserver、Scheduler、Controller Manager、Git Server 和 Artifact Manager 模板；Runner 在 Kubernetes 外部署 | 提供实际镜像、Secret、内网入口与构建节点路由；仅应用模板不能替代私有入口隔离和端到端验收。 |

首次部署前先检查 `kubectl get storageclass`、`kubectl get nodes -L kubernetes.io/hostname,topology.kubernetes.io/zone`，并确认 CSI 能创建所需 PVC。沿用 CCE 模板的 3 成员集群时，仍按 [部署说明](../../../deploy/kubernetes/README.md) 的顺序仅执行一次 ES 初始引导；自建环境也不能在已有集群恢复时重新应用引导配置。适配完成后，至少验证三个 etcd 成员健康、ES 集群与分片健康、PVC 在节点故障后的恢复路径，以及从实际 Runner/Job 容器到各内部入口的访问。

## 部署顺序与验收

1. 准备镜像仓库、目标环境的 CSI/StorageClass、构建节点路由和 Secret；如需外部访问前端，另行准备入口 DNS 与 TLS。为 Artifact Manager 节点目录验证硬链接、rename、容量与备份能力。分别验证 Controller Manager 使用的集群内管理地址与 Runner 使用的构建节点可达地址。
2. 部署 etcd 和 Elasticsearch，确认其数据持久化及恢复流程；先配置证书，待 ES 集群健康后创建 API Key Secret，再以 `--etcd-servers=https://etcd-0.etcd:2379,https://etcd-1.etcd:2379,https://etcd-2.etcd:2379` 和 `--etcd-cafile`、`--etcd-certfile`、`--etcd-keyfile` 启动 apiserver。等待 `/readyz` 成功，并核对默认 Config/Script、ES 索引和 IAM 初始化。不得用固定 `sleep` 代替就绪检查。
3. 启动 Scheduler 和 Git Server；Gateway 就绪后再启动依赖其 `/auth/check` 的 Artifact Manager，最后启动单实例 Controller Manager。对 Git Server 使用其 `/readyz`；Artifact Manager 当前 `/readyz` 仅表示 HTTP 服务可响应，若要让其代表恢复完成，需先补齐实际就绪状态。
4. 按需配置前端外部入口（Ingress 或 NodePort），再接入一台专用 Runner；从真实 Job 容器验证 Gateway、Artifact Manager、Git clone 和基础 RPM 源的连通性，并跑通单包构建、日志上传、过程仓物化与下载。
5. 扩展 Runner 节点前，验证重启后 `instanceID` 延续、Job 恢复、中止与工作目录清理。对 Scheduler 和 Controller Manager 保持一个活动实例；部署滚动更新不得产生新旧实例重叠。

Pod 的存活探针检查进程是否可恢复，未就绪应从 Service 摘除而不是频繁重启；对 Git Server 和 Artifact Manager 的较长恢复窗口配置启动探针。探针路径须与各组件实际实现一致，并分别通过故障注入验证。[Kubernetes 探针文档](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)区分了启动、存活和就绪的用途。

首版验收至少覆盖：无证书直连 apiserver 被拒、跨组件证书身份识别、控制器或调度器重启恢复、单卷重挂载恢复、Git Server 重启后仓库可用、Artifact Manager 重启后过程仓和正式仓可下载、Runner 节点失联及恢复、存储用满时告警与清理、备份恢复后 API 对象和 RPM 文件一致。Scheduler/Controller Manager 选主、Artifact/Git 多写者以及 Kubernetes 原生 Job 执行器均不在首版承诺范围内。
