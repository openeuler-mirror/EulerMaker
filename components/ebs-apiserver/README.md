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

## 构建环境配置

启动时以 create-only 方式初始化集群级 `Config/build-target`（公开读取）和 `Config/build-resource`（仅 Ops/Admin/System 读取），不覆盖已有对象；ES alias 为 `ebs-configs`。两者的业务 YAML 保存在 `spec.content` 中。

```yaml
apiVersion: ebs/v1
kind: Config
metadata:
  name: build-target
  resourceVersion: "<GET 返回的版本>"
spec:
  visibility: Public
  content: |
    targets:
      openEuler-24.03-LTS-SP4:
        arches:
          x86_64:
            image: registry.example/build:24.03-x86_64
          aarch64:
            image: registry.example/build:24.03-aarch64
```

通过 `GET /apis/ebs/v1/configs/build-target` 读取，使用 PUT、JSON Merge Patch 或 JSON Patch 更新。只有 `visibility: Public` 的对象可经 Gateway 匿名具名读取；列表和写入仅允许 Ops/Admin/System。不支持删除、watch、status 子资源。创建 Build 时目标未配置返回 422，配置读取或内容解析失败返回 503，均不申请构建目标占用。

## 全局脚本资源

`Script` 是集群级资源，ES alias 为 `ebs-scripts`，不设置 namespace，spec 仅包含可修改的 `content`。

```yaml
apiVersion: ebs/v1
kind: Script
metadata:
  name: rpmbuild
spec:
  content: |
    #!/bin/bash
    set -euo pipefail
    exec /usr/local/bin/build-rpm --config /workspace/payload.yaml
```

示例入口需与构建镜像匹配，不是内置默认脚本。通过 `POST /apis/ebs/v1/scripts` 创建，`GET /apis/ebs/v1/scripts` 列表，`GET/PUT/PATCH /apis/ebs/v1/scripts/{name}` 读取和更新。PUT 必须携带 GET 返回的 resourceVersion；PATCH 支持 JSON Merge Patch 和 JSON Patch，并使用 ES 乐观锁避免并发覆盖。列表支持分页和 label/metadata.name 过滤。

正文不设独立大小上限，要求 UTF-8、无 NUL、首行是指定绝对解释器路径的 shebang（LF 换行）；apiserver 保留 2 MiB 请求体上限。经 Gateway 访问时还受其请求体限制。不支持删除、watch、status、dryRun 或 Project-scoped 路径。

可选启动参数 `--default-script-file=/path/to/script.yaml` 在就绪前加载一个 Script YAML，仅创建不存在的对象；已有对象不被覆盖，初始化失败则启动失败。未配置时不自动创建脚本。Gateway 已限制仅 Ops/Admin/System 可创建和修改 Script，普通登录用户只读、Runner 仅可读取具名对象；BuildInfo Controller 将创建时观察到的 Script 元数据写入新 Job 的 `spec.scriptRefs`，Runner 按引用拉取并执行脚本。apiserver 仍属于内部服务，不应直接对外暴露。

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
