# Release Management

独立的软件包清单同步插件，从 openEuler release-management 仓库读取清单，通过 Gateway 更新现有
Project 的 `spec.packageRepos`。使用机器凭据换取 Service Token，支持启动同步、周期同步及本地预览；Webhook 尚未实现。
所有绑定直接使用内置的 openEuler 清单解析逻辑，读取 YAML 的 `packages[].name`，无需选择解析器。

## 启动

复制 `config.example.yaml`，填写 Gateway、现有 Project 名称和全局 `machineCredentialFile`。可信内网可使用 HTTP；
机器凭据与 Token 会明文传输，跨公网或不可信网络必须使用 HTTPS，私有 CA 通过 gatewayCAFile 配置。
凭据文件采用 JSON，使用 Admin 创建的 `type=service` 机器账号：

```json
{"clientID":"release-management","clientSecret":"base64url-encoded-random-secret"}
```

将真实凭据通过 Secret 只读挂载；clientSecret 为至少 32 字节随机数据的无填充 Base64URL 编码。
插件向 `POST /auth/machine-token` 使用 Basic 凭据发送 `{}`，Token 仅保存在内存，
到期前 `min(5m, TTL/5)` 在下次写入时刷新，所有绑定共用一个机器账号和 Token 缓存，并发请求共享一次换取。
binding 不支持覆盖凭据；需要不同身份时部署管理不同工程的独立实例。
每次写请求重新读取凭据文件，原子替换后下一次请求换取新 Token。
GET Project 主动不带 Authorization；PATCH 携带 Service Token。401 最多重新换取并重试一次，
原正文及版本前置条件保持不变；403 不刷新，网络/5xx 错误不直接重放写入。刷新失败停止本次写入，按既有同步退避恢复。
工程范围与操作权限控制由独立的 apiserver/Gateway 改动提供，不包含在插件部署中。启用该控制后，机器账号的 `projects` 必须覆盖所有绑定工程，`permissions` 仅需 `projects:update`；此前 Gateway 的固定允许列表不限制工程范围，凭据仅应发给可信内部服务。
周期默认 10 分钟，附加最多 10% 抖动。完整 Project 默认限制为 1 MiB，应与部署的 Gateway/apiserver 上限一致。

```bash
go build -o /tmp/release-management ./cmd
/tmp/release-management plan --config config.yaml --binding oe2403-next-everything
/tmp/release-management serve --config config.yaml
```

首次启动前先预览并人工确认差异。软件包列表完全来自配置目录中的上游清单，服务管理整个 packageRepos 数组，
包括删除源清单已移除的软件包；不合并其他写入者的人工修改。plan 不写 Project，也不是可直接重放的批准单。
同步不限制移除包的数量或比例，但候选列表为空时禁止更新。
若需调整包的范围，应修改上游清单或选择的目录。已有人工设置的 URL/ref/commitID 会按生成结果覆盖，
需要保留逐包定制的工程不应启用该绑定。
plan 与同步都会重新获取来源 ref，并从同一固定 commit 读取所有配置目录的直接子级 YAML 文件。
绑定按 Project 名称生效，允许下一轮同步同名重建的工程。每轮读取当前 UID 和 resourceVersion 作为 PATCH 前置条件；
本轮结果确认发现 UID 变化时停止确认成功，下一轮重新读取和计算，仍执行差异计算及空列表保护。

运行中可以使用相同配置执行：

```bash
/tmp/release-management plan --config config.yaml --binding oe2403-next-everything
/tmp/release-management sync --config config.yaml --binding oe2403-next-everything
/tmp/release-management status --config config.yaml --binding oe2403-next-everything
```

命令通过 dataDir/control.sock 访问服务，sync 成功只表示入队。服务未启动时仅 plan 可以独立运行，并先取得数据目录锁。
退出码：0 成功，1 执行失败，2 参数/配置错误或预览被保护规则阻止。

## 部署约束

- 单副本、Recreate 策略、持久化 dataDir；同一工程不能由多个独立实例管理。
- 服务以非 root 用户运行；数据目录可写，配置、CA 与机器凭据只读挂载，Unix socket 权限为 0600。
- HTTP 监听默认 :8080，仅提供 GET /healthz、/readyz、/metrics，部署在内部网络。
- 来源只支持无 URL 凭据的 HTTP(S)，禁止重定向；解析并固定 DNS 地址，默认拒绝私网、回环、链路本地地址。
  受信任内网清单源需显式配置 source.allowPrivateNetwork=true。该选项不影响 Gateway 内网访问。
- 运行环境需要支持 http.curloptResolve 的 Git/libcurl；Git 子进程不继承全局 Git 配置、代理或凭据环境。
- 每个 YAML 上限 8 MiB，每轮总量 32 MiB、1000 个文件、10000 个不同软件包；拒绝 YAML 别名和重复键。
- 来源、权限、空列表或容量异常时保留原列表，通过 status 和限频日志定位；恢复后周期或手动触发继续收敛。

从仓库根目录构建镜像：

```bash
docker build -f backend/release-management/Dockerfile -t release-management:local .
```

部署模板见 [Compose](../../hacks/README.md#可选release-management-周期同步) 和
[Kubernetes](../../deploy/kubernetes/README.md#12-可选部署-release-management)。

测试：`go test -race ./...`。设计契约见 [release-management 设计](../../docs/zh/design/release-management.md)。
