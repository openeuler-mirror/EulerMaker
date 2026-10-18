# ebs-api

`ebs-api` 包含 EulerMaker 组件共享的版本化 EBS 和 IAM API 类型。apiserver、gateway、controller、scheduler 和命令行工具按需依赖此 module。

公共资源类型分别位于 `ebs/v1` 和 `iam/v1`。IAM 的认证、凭证管理和权限策略仍由各组件实现。修改 API 类型后，运行以下命令更新 DeepCopy 代码：

```bash
./hacks/update-codegen.sh
```

随后在 `backend/ebs-apiserver` 下运行 `./hacks/update-openapi.sh`，同步更新服务端 OpenAPI 定义。

各 API 包的 DeepCopy 方法由生成器写入该包的 `zz_generated.deepcopy.go`。脚本使用专用临时目录，重复执行不会在仓库根目录生成额外的 import-path 目录。
