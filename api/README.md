# ebs-api

`ebs-api` 包含 EulerMaker 组件共享的版本化 EBS API 类型。apiserver、controller、scheduler 和命令行工具应依赖此 module。

当前公共 API 位于 `ebs/v1`。修改 API 类型后，运行以下命令更新 DeepCopy 代码：

```bash
./hacks/update-codegen.sh
```

随后在 `components/ebs-apiserver` 下运行 `./hacks/update-openapi.sh`，同步更新服务端 OpenAPI 定义。
