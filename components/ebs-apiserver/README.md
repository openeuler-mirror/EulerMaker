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

通过 `GET /apis/ebs/v1/configs/build-target` 读取，使用 PUT、JSON Merge Patch 或 JSON Patch 更新。只有 `visibility: Public` 的对象可经 Gateway 匿名具名读取；列表和写入仅允许 Ops/Admin/System。Gateway 不允许删除内置 Config；apiserver 使用通用资源路由，不支持 watch 和 status 子资源。创建 Build 时目标未配置返回 422，配置读取或内容解析失败返回 503，均不申请构建目标占用。

资源 GET/LIST 支持 `includeFields=metadata.name,status.phase` 和 `excludeFields=spec` 等响应字段选择参数；两者同时指定时先选择再排除。字段路径相对于单个资源对象，LIST 的顶层分页元数据保留。ES 资源的安全字段选择会下推至 `_source` filtering，复杂路径仍在响应端裁剪；不影响服务端过滤或存储。Watch 不支持。

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
    exec /usr/local/bin/build-rpm --config /workspace/payload.json
```

上述为接口示例；apiserver 使用内置的 [rpmbuild Script](pkg/server/default-rpmbuild-script.yaml) 初始化同名对象。通过 `POST /apis/ebs/v1/scripts` 创建，`GET /apis/ebs/v1/scripts` 列表，`GET/PUT/PATCH /apis/ebs/v1/scripts/{name}` 读取和更新，`DELETE /apis/ebs/v1/scripts/{name}` 删除。删除可选带 `preconditions.uid` 和 `preconditions.resourceVersion` 以防止误删；apiserver 不按脚本名称限制删除。PUT 必须携带 GET 返回的 resourceVersion；PATCH 支持 JSON Merge Patch 和 JSON Patch，并使用 ES 乐观锁避免并发覆盖。列表支持分页和 label/metadata.name 过滤。

正文不设独立大小上限，要求 UTF-8、无 NUL、首行是指定绝对解释器路径的 shebang（LF 换行）；apiserver 使用通用请求体大小限制（当前配置为 5 MiB）。经 Gateway 访问时还受其请求体限制。不支持 watch、status 或 Project-scoped 路径；通用路由不注册 HEAD。

apiserver 在就绪前从内置模板初始化 `rpmbuild` Script，仅创建不存在的对象；已有对象不被覆盖，初始化失败则启动失败，无需启动参数。Gateway 已限制仅 Ops/Admin/System 可创建和修改 Script，普通登录用户只读、Runner 仅可读取具名对象；BuildInfo Controller 将创建时观察到的 Script 元数据写入新 Job 的 `spec.scriptRefs`，Runner 按引用拉取并执行脚本。apiserver 仍属于内部服务，不应直接对外暴露。

内置 [rpmbuild Script 模板](pkg/server/default-rpmbuild-script.yaml) 可用于简单 RPM 包：读取 `/workspace/payload.json`，按 `spec_url` 和 `commit_id` 检出仓库；`use_git_lfs: true` 时改从固定地址 `https://atomgit.com/src-openeuler/${package_name}.git` 克隆，并在检出 `commit_id` 后拉取 LFS 对象。默认先从 Job 仓库安装 `gcc_secure`，`unuse_gcc_secure: true` 或包名为 `gcc-10` 时跳过；若 payload 含 `preinstall` 包名数组，再执行 `dnf install`，随后通过 `dnf builddep` 安装依赖；`use_kmod_libs: true` 时再安装 `kmod-libs`。`SOURCES` 下的非隐藏目录在构建前另打包为 `.tar.gz`，`use_xz: true` 时改为 `.tar.xz`，最后以 `rpmbuild -ba` 构建。默认仅 `rpmbuild` 以 `eulermaker` 用户运行，`use_root: true` 时改为 root；其他准备和收尾步骤仍以 root 运行。二进制 RPM 和源码 RPM 均放入 `/results/packages/`。构建镜像需预装 Bash、Git、curl、rpm-build、`dnf builddep` 插件、`tar`、`useradd` 和 `runuser`；使用 Git LFS 的镜像还需预装 `git-lfs`，使用 `use_xz` 时还需预装 `xz`。模板仅处理仓库根目录的 spec、仓库内文件及可直接下载的 HTTP(S) Source/Patch，不覆盖旧脚本的高级构建规则。

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
