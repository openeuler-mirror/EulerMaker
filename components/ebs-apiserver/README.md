# ebs-apiserver

`ebs-apiserver` 是 EulerMaker 的资源 API 服务，基于 `Kubernetes GenericAPIServer` 实现。服务使用 etcd 和 Elasticsearch 持久化资源，并可通过 `--enable-iam` 启用内置 IAM 模块。

## 构建与测试

在当前目录执行：

```bash
go test ./...
CGO_ENABLED=0 go build -o ebs-apiserver ./cmd/server
```

在仓库根目录构建容器镜像：

```bash
docker build -f components/ebs-apiserver/Dockerfile -t eulermaker/ebs-apiserver:dev .
```

## Build 创建互斥

full、incremental、specified 构建按 Project + OS + Arch 通过 ES 原子占用互斥，冲突返回 409；single 不占用目标。apiserver 在终态写入或实际删除成功后释放占用，并每 30 秒扫描补偿，不需要 controller 直接访问 ES。

内部索引 alias 为 `ebs-build-target-claims`，仅保存目标占用。调用方保证 Build 名称不复用，apiserver 不保存历史名称记录。未知创建且无法确认结果时保留占用，不按超时自动释放；可通过 `ebs_build_claim_unconfirmed`、`ebs_build_claim_oldest_unconfirmed_seconds` 和结构化日志观察。

升级时先暂停所有 Build 创建、停止旧版实例，检查存量非终态构建、建立对应目标占用，再开放创建。禁止新旧协议混跑、直接写入 Build、在线无协调切换内部索引或清空占用解除阻塞。创建结果未知时，人工清理前必须排除仍可能提交的迟到写入。

## OpenAPI 代码生成

修改 `../../api/ebs/v1` 或 `pkg/apis/iam/v1` 下的 API 类型后，需要重新生成 OpenAPI 定义：

```bash
./hacks/update-openapi.sh
```

脚本使用固定版本的 `openapi-gen`，并将两个 API 包的定义统一写入`pkg/generated/openapi/zz_generated.openapi.go`。生成文件不应手工修改；API 类型、字段或校验标记发生变化时，应运行脚本并一并提交生成结果。

首次执行可能需要下载生成工具。提交前应再次执行脚本，并检查生成文件的格式和差异：

```bash
./hacks/update-openapi.sh
git diff --check
git diff -- pkg/generated/openapi/zz_generated.openapi.go
```

## 本地运行

先启动 etcd 和 Elasticsearch，然后执行：

```bash
./ebs-apiserver \
  --etcd-servers=http://localhost:2379 \
  --es-servers=http://localhost:9200 \
  --enable-iam \
  --secure-port=8443
```

服务启动后可检查 API 和健康状态：

```bash
curl -k https://localhost:8443/healthz
curl -k https://localhost:8443/apis/ebs/v1
```

完整命令行参数由 `k8s.io/apiserver/pkg/server/options` 和本组件的启动选项共同提供，可通过以下命令查看：

```bash
./ebs-apiserver --help
```
