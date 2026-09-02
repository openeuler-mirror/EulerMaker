# ebs-apiserver

`ebs-apiserver` 是 EulerMaker 的资源 API 服务，基于 `Kubernetes GenericAPIServer` 实现。服务使用 etcd 和 Elasticsearch 持久化资源，并可通过 `--enable-iam` 启用内置 IAM 模块。

## 构建与测试

在当前目录执行：

```bash
go test ./...
CGO_ENABLED=0 go build -o ebs-apiserver ./cmd/server
```

构建容器镜像：

```bash
docker build -t eulermaker/ebs-apiserver:dev .
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
