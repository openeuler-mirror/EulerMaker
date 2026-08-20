# ebsctl 命令行工具设计

## 一、定位与目标

`ebsctl` 是 EulerMaker 面向用户和运维人员的命令行客户端，交互方式参考 `kubectl`，但只暴露 EulerMaker 已定义的资源和数据能力。它不是控制器，不直接访问 etcd、Elasticsearch，也不使用内部 mTLS 身份。

首版目标：

- 通过 `ebs-gateway` 登录并操作 `ebs/v1` 资源；
- 使用 context 保存服务地址、当前用户和默认 Project；
- 支持资源的 get、list、create、replace、patch、delete 和 watch；
- 支持 YAML/JSON 输入以及 table、wide、yaml、json、name 输出；
- 所有命令可用于终端交互，也可在脚本和 CI 中稳定使用。

首版不实现插件、端口转发、远程 exec、资源编辑器和 kubectl 全量兼容。命令、配置和输出格式只保证 `ebsctl` 自身的兼容性。

二进制和 Go module 目录建议为：

```text
tools/ebsctl/
├── cmd/ebsctl/
│   └── main.go
└── pkg/
    ├── cli/          # 命令组装、参数校验和退出码
    ├── config/       # context 与凭据文件
    ├── client/       # Gateway HTTP 客户端
    ├── resource/     # 资源映射、编解码和请求构造
    ├── printer/      # table/yaml/json/name 输出
    └── stream/       # watch 和断线续接
```

## 二、访问架构

```text
用户 / CI
   |
   v
 ebsctl
   `-- 登录、资源 CRUD、list/watch --> ebs-gateway --> ebs-apiserver
```

首版所有请求始终发送给当前 context 的 Gateway，不直接访问 apiserver、Artifact Manager 或其他内部组件。日志查看和 Artifact 查询下载留待后续版本设计，不作为首版命令或配置的一部分。

## 三、命令模型

### 3.1 通用语法

```text
ebsctl [全局参数] <命令> [资源] [名称] [命令参数]
```

全局参数：

| 参数 | 说明 |
|------|------|
| `--config` | 配置文件，默认 `$HOME/.config/ebs/config.yaml` |
| `--context` | 覆盖当前 context |
| `-p, --project` | 覆盖默认 Project |
| `--gateway` | 临时覆盖 Gateway 地址，不写配置 |
| `--request-timeout` | 普通 HTTP 请求超时，默认 `30s`；watch 不使用该总超时 |
| `--insecure-skip-tls-verify` | 仅测试环境跳过 TLS 校验 |
| `--verbose` | 输出请求诊断信息，但不输出密码、Token 或响应中的敏感字段 |

命令和资源名称大小写不敏感，输出中的 Kind 保持 API 定义。资源支持单数、复数和固定短名：

| 资源 | 单数/复数 | 短名 | 作用域 |
|------|-----------|------|--------|
| Project | `project/projects` | `proj` | 集群级 |
| Snapshot | `snapshot/snapshots` | `snap` | Project |
| Build | `build/builds` | `build` | Project |
| Job | `job/jobs` | `job` | Project |
| BuildInfo | `buildinfo/buildinfos` | `bi` | Project |
| RpmRepo | `rpmrepo/rpmrepos` | `repo` | Project |

首版使用编译期静态资源表，不依赖 Kubernetes discovery API。客户端版本新增资源时同步更新资源表；遇到未知 `apiVersion` 或 Kind 必须报错，不能猜测请求路径。

### 3.2 登录与 Context

```bash
ebsctl login https://ebs.example.com --username alice

ebsctl config get-contexts
ebsctl config use-context production
ebsctl config set-project openeuler-mainline
ebsctl logout
```

`login` 默认从终端安全读取密码，不回显；也允许 `--password-stdin`，不提供 `--password` 参数，避免密码进入 shell history 和进程参数。成功后调用 `/auth/login`，保存 access token 和服务地址。context 默认名由 `--context-name` 指定，否则从 Gateway host 生成。

`logout` 删除当前 context 的本地 Token；当前服务端没有 Token 吊销接口，因此 logout 不表示服务端 Token 立即失效。CI 可以通过环境变量 `EBS_TOKEN` 注入 Token，环境变量优先于配置文件，且不落盘。

### 3.3 资源查询

```bash
ebsctl get projects
ebsctl get projects --mine
ebsctl get jobs -p openeuler-mainline
ebsctl get job build-kernel -p openeuler-mainline -o yaml
ebsctl get jobs -l package=kernel --field-selector status.phase=Running
ebsctl get jobs --watch
```

`get <resource>` 调用 collection list；`get <resource> <name>` 调用单对象 GET。Project 级资源必须能从 `--project` 或当前 context 获得 Project，否则在发起请求前报错。Project 自身忽略默认 Project。`ebsctl` 不提供 Runner 的 get、list、watch 或写操作。

支持参数：

- `-o table|wide|yaml|json|name`；终端默认 `table`，管道中仍默认 table，不根据 TTY 隐式改变格式；
- `-l, --selector` 映射为 `labelSelector`；
- `--field-selector` 映射为 `fieldSelector`；
- `--limit` 和 `--continue` 原样使用服务端分页；
- `--mine` 仅用于 `get projects`，通过 `/auth/check` 获取当前可信用户，并在客户端保留 owner 或 member labels 与该用户匹配的 Project；该参数只是显示过滤，不构成服务端权限边界；
- `-w, --watch` 建立 watch；
- `--watch-only` 不先输出初始列表，只显示后续事件。

watch 输出 table 时增加 `EVENT` 列；JSON/YAML 模式逐事件输出完整 WatchEvent。连接正常超时后使用最后的 `resourceVersion` 重连；收到资源版本过期响应时重新 list。用户主动中断返回 0，无法恢复的认证或协议错误返回非 0。

### 3.4 创建和更新

```bash
ebsctl create -f project.yaml
ebsctl create -f manifests/
ebsctl replace -f job.yaml
ebsctl patch job build-kernel -p openeuler-mainline \
  --type merge --patch '{"spec":{"priority":100}}'
ebsctl delete job build-kernel -p openeuler-mainline
```

- `-f` 接受单文件、目录或 `-`（stdin）；目录只读取 `.yaml`、`.yml` 和 `.json` 文件，按文件名排序，不递归；
- YAML 文件允许 `---` 分隔多个对象；每个对象必须包含 `apiVersion`、`kind` 和 `metadata.name`；
- Project 级对象的 `metadata.namespace` 可以省略，由 `--project` 或 context 补齐；若文件值与命令行 Project 不同则拒绝；
- `create` 只执行 POST，已存在返回冲突；
- `replace` 先要求输入包含 `metadata.resourceVersion`，再执行 PUT，不自动覆盖并发修改；
- `patch --type merge --patch <JSON>` 使用 `application/merge-patch+json`；首版只支持 merge patch。`-p` 已作为全局 Project 参数，不复用于 patch 内容；
- `delete` 默认要求交互确认仅限危险的批量删除；指定单个名称直接删除，`--all` 必须显式给出，并支持 `--yes` 跳过确认。

`ebsctl` 不提供 `apply`。声明式对象的创建和更新分别使用 `create` 与 `replace`，局部字段更新使用 `patch`。

### 3.5 状态与诊断

```bash
ebsctl describe job build-kernel -p openeuler-mainline
ebsctl wait job build-kernel -p openeuler-mainline \
  --for=jsonpath='{.status.phase}'=Completed --timeout=30m
ebsctl version
```

`describe` 只基于资源对象生成稳定的分区文本，不额外推测服务端事件；在系统增加 Event 资源后再展示关联事件。`wait` 首先 GET，然后基于 watch 等待条件，watch 中断后从最后版本恢复。首版支持 `condition=<type>`、`delete` 和受限 JSONPath 等值判断。

`version` 输出 client version；带 `--server` 时额外请求 Gateway 健康和 API 版本信息。若服务端尚无版本端点，只显示连通性，不把 client version 当作 server version。

## 四、配置与凭据

配置文件格式：

```yaml
apiVersion: config.ebs/v1
kind: Config
currentContext: production
contexts:
  production:
    gateway: https://ebs.example.com
    user: alice
    project: openeuler-mainline
    tls:
      caFile: /home/alice/.config/ebs/ca.pem
      serverName: ebs.example.com
credentials:
  production:
    token: <access-token>
```

要求：

- 配置目录权限为 `0700`，包含 Token 的文件权限为 `0600`；权限过宽时拒绝使用并给出修复提示；
- 保存前使用临时文件、fsync 和 rename，防止并发写入或崩溃损坏；
- 不保存用户密码；诊断信息和错误中不打印 Authorization header；
- `EBS_TOKEN`、`EBS_GATEWAY` 可覆盖当前 context，适用于 CI；
- CA 和 Token 分离存储可作为后续增强，首版允许同一受限配置文件；
- `--insecure-skip-tls-verify` 不持久化，除非用户显式执行 config set；设置时输出警告。

优先级从高到低为：命令参数、环境变量、指定 context、当前 context、内置默认值。地址必须包含 `http` 或 `https` scheme，不接受 URL userinfo、fragment 和非空查询参数。

## 五、输出与脚本兼容性

### 5.1 标准输出约束

- stdout 只输出请求结果；提示、进度和诊断写 stderr；
- `-o json` 输出合法单个 JSON 对象，list 保持 List 结构；
- `-o yaml` 保持 API 字段，不输出本地计算字段；
- `-o name` 使用 `resource/name`，Project 级资源不重复拼入 Project；
- table 的列是稳定的人类界面，不承诺可作为脚本协议；脚本应使用 JSON、JSONPath 或 name；
- `--no-headers` 仅对 table 生效。

不同资源的默认列：

| 资源 | 默认列 |
|------|--------|
| Project | NAME、DISPLAY NAME、PHASE、AGE |
| Snapshot | NAME、PHASE、AGE |
| Build | NAME、PHASE、SNAPSHOT、AGE |
| Job | NAME、PHASE、STAGE、RUNNER、AGE |
| BuildInfo | NAME、STATUS、AGE |
| RpmRepo | NAME、STATUS、AGE |

### 5.2 退出码

| 退出码 | 含义 |
|--------|------|
| `0` | 成功，或用户正常中断持续 watch |
| `1` | 服务端业务错误、部分批处理失败或等待条件失败 |
| `2` | 命令参数、配置或输入对象错误 |
| `3` | 认证失败或登录状态失效 |
| `4` | 网络、TLS、超时或服务不可用 |
| `5` | 乐观锁或字段冲突 |

批量 `-f` 默认继续处理独立对象并汇总失败，任一失败返回 1；`--fail-fast` 在首个失败时停止。stderr 中错误格式固定包含资源、名称、HTTP 状态、服务端 error code、message 和 requestID（若有），不回显敏感请求体。

## 六、客户端内部设计

### 6.1 请求链路

```text
Command
  -> Config resolution
  -> Resource mapping and validation
  -> Request builder
  -> Authentication transport
  -> Retry transport
  -> Gateway
  -> Decoder
  -> Printer
```

普通 GET 在网络错误、429 和 503 时允许带抖动的有限指数退避；遵循 `Retry-After`。非幂等 POST 不自动重试。PUT、PATCH、DELETE 只有在请求明确未发送时才能自动重试，结果未知时返回错误交由用户重新读取对象判断。401 不使用密码静默重新登录，清除内存 Token 并提示执行 login。

每次请求生成 `X-Request-ID`，接收服务端 requestID 并在错误中展示。HTTP response body 设置读取上限；watch 响应使用流式 reader，不进入通用 JSON 缓冲。

### 6.2 编解码

资源对象使用与 `ebs-apiserver` 相同版本的 API Go 类型，避免手写重复结构；CLI 层通过 runtime Scheme 完成 GVK 注册。输入启用未知字段检查，提供 `--validate=false` 仅跳过本地 schema 校验，服务端校验始终生效。

YAML 转 JSON 时保留整数精度。任何输出都不得包含 IAM credential、密码或完整 Token。Secret 类资源如果以后引入，printer 默认脱敏。

### 6.3 并发和取消

所有网络操作接受根 `context.Context`。第一次 SIGINT 取消当前请求并执行必要的流关闭；第二次 SIGINT 立即退出。printer 只由单 goroutine 写 stdout，避免多对象输出交错。

## 七、兼容性策略

- CLI 与服务端通过 `apiVersion` 协商对象格式，不用二进制版本字符串推断兼容性；
- 新增 JSON 字段向后兼容，删除或改变字段语义必须提升 API 版本；
- 命令参数废弃至少保留一个次版本并输出 stderr 警告；
- table 列允许新增，但已有列名和含义在同一 CLI 主版本内保持稳定；
- 配置文件包含独立 `apiVersion`，升级时先读取旧版本并原子迁移，无法降级时保留备份。

## 八、开发阶段

### 8.1 MVP

1. config、login、logout 和安全 Token 存储；
2. 静态资源表及 get/create/replace/patch/delete；
3. table/json/yaml/name 输出、selector 和分页；
4. Project 范围的 Job watch；
5. Linux amd64/arm64 的单二进制发布。

### 8.2 后续能力

- shell completion 和 man page；
- OS keyring 凭据存储；
- 日志查看和 Artifact 查询下载；
- Event 资源和增强版 describe；
- Windows/macOS 构建与签名。

## 九、测试要求

| 类别 | 必测内容 |
|------|----------|
| 配置 | context 优先级、0600 权限、原子写入、环境变量覆盖、损坏配置 |
| 认证 | 登录成功/失败、Token 不进入诊断输出、401、logout、password-stdin |
| 资源映射 | 所有单复数/短名、Project 路由、未知 GVK、namespace 冲突 |
| CRUD | JSON/YAML、多文档、stdin、resourceVersion 冲突、merge patch、批量错误汇总 |
| 输出 | table 列、JSON/YAML 合法性、name、stdout/stderr 分离 |
| Watch | BOOKMARK、断线恢复、410 重新 list、取消、慢消费者 |
| 安全 | TLS CA、insecure 警告、URL 校验、敏感字段脱敏、响应体上限 |
| 兼容性 | 新增未知响应字段、旧配置迁移、客户端与服务端 API 版本不匹配 |

端到端测试使用真实 Gateway 和 apiserver，至少覆盖：登录后创建 Project、创建并 watch Job，以及无权限访问其他用户 Project 被拒绝。
