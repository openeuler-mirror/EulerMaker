# Git Server 设计

## 1. 定位

Git Server 是 EulerMaker 内部使用的源码仓库镜像服务。它从远端同步 Git 仓库到本地裸仓库，并提供同步状态查询、有限的 Git 内容查询和只读 clone 能力。

```text
Controller / internal client
          |
          | HTTP API
          v
      Git Server --------> Remote Git repository
          |
          +-------------> Local bare repository
          |
          +-------------> Read-only git daemon
```

首版约束：

- 单实例部署并使用本地持久化目录；
- 不提供仓库写入和 `git push`；
- 不实现仓库分片、多副本同步和分布式锁；
- 调用方负责根据同步结果更新业务对象。

## 2. 功能范围

1. 接收仓库 URL，将其加入同步队列；
2. 首次同步时创建 bare repository；
3. 后续同步时 fetch 远端 refs；
4. 查询仓库最近一次成功同步时间；
5. 异步删除本地仓库；
6. 执行构建流程需要的有限 Git 只读查询；
7. 通过 git daemon 提供只读 clone。

首版只持久化最近成功同步时间，不持久化队列意图、错误和重试状态；不支持 Git LFS、submodule 递归同步、Snapshot commit 保留、磁盘配额和自动 GC。

## 3. 仓库存储路径

本地路径保持可读，运维人员查看数据卷时可以直接识别仓库：

```text
${dataDir}/{host}/{repositoryPath}
```

以下两个地址：

```text
https://gitee.com/src-openeuler/gcc.git
git@gitee.com:src-openeuler/gcc.git
```

均映射为相同 key 和目录：

```text
gitee.com/src-openeuler/gcc.git
${dataDir}/gitee.com/src-openeuler/gcc.git
```

协议、HTTP(S) 默认端口、SSH 默认端口和 SSH 用户名不属于仓库 key。

### 3.1 支持的 URL 形式

首版支持：

```text
https://host[:port]/owner/repo[.git]
http://host[:port]/owner/repo[.git]
ssh://user@host[:port]/owner/repo[.git]
user@host:owner/repo[.git]
git://host[:port]/owner/repo[.git]
```

最后一种 `user@host:path` 按 SCP 风格 SSH 地址解析。`file://`、本地绝对路径、相对路径和其他协议均拒绝。

### 3.2 转换算法

服务必须按以下顺序转换，所有 sync、status、delete 和 command 请求共用同一个实现：

1. 去除输入首尾空白；中间包含空白、NUL 或控制字符则拒绝。
2. 识别标准 URL 或 SCP 风格 SSH 地址，得到 scheme、userinfo、host、port 和 path；解析失败立即拒绝。
3. 按上一节校验 scheme 和 userinfo；query 和 fragment 必须为空，不能静默删除。
4. host 转为小写并移除末尾的 DNS 根点。host 不能为空。
5. 去除协议的默认端口：HTTP 为 80、HTTPS 为 443、SSH 为 22、Git 为 9418。非默认端口保留，并生成目录段 `{host}~{port}`。
6. 移除 path 开头和结尾的 `/`，将连续 `/` 合并成一个分隔符；结果至少包含仓库名。
7. 每个 path 段只进行一次百分号解码。拒绝解码失败、解码后为 `.`/`..`、包含 `/`、`\`、NUL 或控制字符的段；因此 `%2F`、`%5C`、`%2E%2E` 均不能绕过校验。
8. 对解码后的每个 UTF-8 path 段重新编码为文件名：`A-Z`、`a-z`、`0-9`、`.`、`_`、`-` 保持可读，其他字节使用大写 `%HH`。字面 `%` 编码为 `%25`，保证转换结果唯一。
9. 最后一个 path 段没有 `.git` 后缀时补充 `.git`；后缀比较区分大小写。
10. 使用独立路径段拼接 `${dataDir}/{hostPort}/{repositoryPath}`，再通过 `filepath.Rel` 验证结果仍位于 `${dataDir}` 下。

IPv4 地址按普通 host 处理。IPv6 地址去掉 URL 方括号后按上述文件名规则编码，例如 `2001:db8::1` 形成 `2001%3Adb8%3A%3A1`。

示例：

| 输入 URL | 仓库 key |
|----------|----------|
| `https://gitee.com/src-openeuler/gcc.git` | `gitee.com/src-openeuler/gcc.git` |
| `git@gitee.com:src-openeuler/gcc.git` | `gitee.com/src-openeuler/gcc.git` |
| `ssh://git@gitee.com:22/src-openeuler/gcc` | `gitee.com/src-openeuler/gcc.git` |
| `https://gitee.com:8443/team/repo.git` | `gitee.com~8443/team/repo.git` |
| `https://git.example.com/team/a%20b.git` | `git.example.com/team/a%20b.git` |
| `https://user:password@gitee.com/team/repo.git` | 拒绝 |
| `file:///srv/repo.git` | 拒绝 |
| `https://gitee.com/team/%2E%2E/repo.git` | 拒绝 |

同一个 key 的不同协议地址共享一份裸仓库。第一次成功 clone 的地址作为仓库的 `origin`；后续请求映射到同一个 key 时不自动替换 origin，只使用现有 origin 执行 fetch，避免调用 status 或 delete 时改变远端。需要切换协议或凭证时由运维显式修改仓库 remote 或重新创建仓库。

所有最终仓库的创建、打开、移动和删除都必须使用 3.3 节定义的、锚定 `dataDir` 目录文件描述符的安全路径操作，不能用一次 `Lstat` 后再按字符串路径操作。不同 host 或非默认端口默认视为不同仓库，不设计 alias 机制。

同步临时目录由 `${dataDir}` 固定推算为 `${dataDir}/.tmp`，不提供独立配置。`.tmp` 是保留目录，仓库 key 的第一个目录段不得为 `.tmp`。clone 临时目录使用 `clone-{uuid}`，删除隔离目录使用 `delete-{uuid}`，Git 子进程所需的临时认证材料目录使用 `auth-{uuid}`；UUID 必须由密码学安全随机源生成。成功 clone 后将临时目录原子移动到最终路径。

### 3.3 文件系统安全与原子发布

首版只支持 Linux。服务启动时将 `dataDir` 清理为绝对路径，并以 `O_RDONLY|O_DIRECTORY|O_CLOEXEC|O_NOFOLLOW` 打开一次，使用 `fstat` 确认它是真实目录；`dataDir` 是符号链接、无法打开或文件系统不支持本节要求的系统调用时启动失败。服务在整个生命周期持有 `dataDirFD`，后续安全判断以该目录 FD 为根，不依赖字符串前缀比较。

仓库 key 必须先经过 3.2 节规范化并拆分为独立路径段。创建最终仓库父目录时，从 `dataDirFD` 开始逐级执行：

1. 使用 `mkdirat(currentFD, segment, 0750)` 创建单级目录；`EEXIST` 只表示需要继续检查，不能直接视为成功；
2. 使用 `openat2` 打开该段，设置 `O_RDONLY|O_DIRECTORY|O_CLOEXEC` 和 `RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS|RESOLVE_NO_MAGICLINKS`；
3. 打开失败、路径是符号链接或不是目录时返回 `RepositoryConflict`；
4. 将新目录 FD 作为下一层的 currentFD，直到取得最终仓库父目录 FD。

所有目录 FD，包括 `dataDirFD`、`tmpDirFD`、最终父目录 FD、临时仓库 FD 和启动扫描使用的目录 FD，都统一使用 `O_RDONLY|O_DIRECTORY|O_CLOEXEC` 打开，并继续使用适用的 `openat2 RESOLVE_*` 约束。不得使用 `O_PATH`，从而这些 FD 可以直接用于目录遍历和 `fsync`；不再额外按字符串路径重新打开目录。

不能使用 `MkdirAll`、`EvalSymlinks` 或“先 `Lstat`、再 open/rename”的组合替代上述过程。多个 Worker 并发创建公共父目录时，允许其中一个 `mkdirat` 成功，其他 Worker 收到 `EEXIST` 后通过 `openat2` 取得同一安全目录。

`.tmp` 也必须通过 `mkdirat(dataDirFD, ".tmp", 0700)` 和上述 `openat2` 约束创建或打开，并在进程生命周期持有 `tmpDirFD`。`clone-{uuid}`、`delete-{uuid}` 和 `auth-{uuid}` 只能作为 `.tmp` 的单级直接子项，通过 `mkdirat(tmpDirFD, name, 0700)` 创建；临时名称完全由服务生成，存在时生成新 UUID，不能清空或复用。传给 Git 子进程的临时绝对路径只能由已经打开并校验的 dataDir、`.tmp` 和随机名称组成，不能包含 OriginURL 或仓库 key 的任何路径段。

首次 clone 完成后，在持有该仓库 operationLock 写锁期间执行以下发布流程：

1. 通过 `openat2(tmpDirFD, cloneName, ...)` 重新打开临时仓库，使用 `fstat` 记录 device/inode，并确认它是真实目录和合法 bare repository；
2. 重新按上述规则取得最终父目录 FD，不能复用未经校验的字符串路径；
3. 使用 `renameat2(tmpDirFD, cloneName, finalParentFD, repositoryName, RENAME_NOREPLACE)` 原子移动，禁止覆盖任何已存在对象；
4. rename 成功后，通过 `openat2(finalParentFD, repositoryName, ...)` 打开最终仓库，确认仍是合法 bare repository，且 device/inode 与移动前一致；
5. 对最终仓库的必要数据和 `finalParentFD` 执行 `fsync`。全部成功后才更新 SyncTime 并提供 CloneURL。

`RENAME_NOREPLACE` 返回 `EEXIST` 时，必须重新安全打开并分类最终对象：合法 bare repository 表示已有可用仓库，当前 Worker 清理自己的临时目录，并在同一操作内按该仓库保存的 origin 执行 fetch，只有 fetch 成功才把本次 sync 记为成功；符号链接、普通文件或非法仓库返回 `RepositoryConflict`。不得覆盖、跟随或自动删除冲突对象。理论上源和目标都位于 dataDir 的同一文件系统；出现 `EXDEV` 视为内部存储配置错误。

删除同样在 operationLock 写锁内完成：通过最终父目录 FD 安全打开并记录仓库 device/inode，在 `.tmp` 中生成不存在的 `delete-{uuid}` 名称，然后使用 `renameat2(finalParentFD, repositoryName, tmpDirFD, deleteName, RENAME_NOREPLACE)` 隔离。移动后重新打开隔离目录并核对 device/inode；核对成功即为逻辑删除。后台递归清理必须从 `tmpDirFD` 和单级随机名称开始，逐级使用目录 FD 和 `O_NOFOLLOW`/`openat2`，不得跟随符号链接。

operationLock 只约束本进程内同仓库操作，不能替代上述文件系统原语。`RENAME_NOREPLACE` 负责防止进程外对象或异常残留被覆盖，目录 FD 和 `RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS` 负责消除路径检查与使用之间的竞态及路径逃逸。

启动恢复扫描也必须以 `dataDirFD` 为根：跳过安全打开的 `.tmp`，逐项使用 `openat2` 且不跟随符号链接；遇到符号链接、普通文件或结构异常的仓库候选项只记录告警，不进入 RepositoryState。任何扫描、清理或 API 流程都不得根据字符串拼接结果直接递归删除路径。

## 4. HTTP API

仓库同步、状态查询和删除统一使用动作式 POST。仓库 URL 放在 JSON body 中，不出现在 query string 和常规访问日志中。

三个接口使用相同的请求结构：

```go
type RepositoryRequest struct {
    OriginURL string `json:"origin_url"`
}
```

请求必须使用 `Content-Type: application/json`，拒绝未知字段、空 `origin_url` 和请求正文后的多余 JSON 数据。

除 `/command` 已经成功启动 Git 子进程后的命令结果外，所有非 2xx 响应统一使用以下结构：

```go
type ErrorResponse struct {
    Code    string `json:"code"`
    Message string `json:"message"`
}
```

`code` 是供调用方判断的稳定机器码，`message` 是经过脱敏的简短说明，不保证文本稳定，也不能包含 Git 原始 stderr、凭证或认证临时路径。响应必须使用 `Content-Type: application/json`。通用错误映射如下：

| HTTP 状态码 | code | 适用场景 |
|------------|------|----------|
| `400 Bad Request` | `InvalidRequest` | JSON、Content-Type、字段、URL 或命令请求格式非法 |
| `403 Forbidden` | `Forbidden` | 请求的 Git 命令不在允许列表中，不涉及调用方身份鉴权 |
| `404 Not Found` | `RepositoryNotFound` | 仓库从未出现、已删除或尚无可用本地副本 |
| `409 Conflict` | `RepositoryConflict` | 最终路径被异常文件占用或当前文件系统状态与请求冲突 |
| `413 Content Too Large` | `RequestTooLarge` | HTTP 请求正文超过限制 |
| `500 Internal Server Error` | `InternalError` | 无法启动子进程、状态损坏或未分类的内部错误 |
| `503 Service Unavailable` | `NotReady` | 服务尚未 ready |
| `503 Service Unavailable` | `QueueClosed` | 关闭期间无法记录新的异步意图 |

同一请求只能写入一次响应。handler 在服务未 ready 时不读取或修改 RepositoryState，直接返回 `NotReady`。

### 4.1 请求同步

```http
POST /api/v1/repo/sync
Content-Type: application/json

{
  "origin_url": "https://gitee.com/src-openeuler/gcc.git"
}
```

成功加入队列或任务已经存在时返回 `202 Accepted`：

```json
{
  "key": "gitee.com/src-openeuler/gcc.git",
  "clone_url": "git://git-server:9418/gitee.com/src-openeuler/gcc.git",
  "sync_time": "2026-09-09T02:30:00Z"
}
```

sync 只表示服务已经记录同步意图，不保证本次同步已经完成。仓库曾成功同步且尚未删除时返回 `clone_url` 和 `sync_time`；首次同步尚未成功时只返回 `key`。调用方不区分重复 sync 请求，也不等待某一次请求完成：存在 `sync_time` 和 `clone_url` 时可以尝试 clone，否则稍后重新查询 status。

URL 非法返回 `400 Bad Request`，队列已经关闭返回 `503 Service Unavailable`。相同 key 已经排队、执行或等待自动重试的相同 sync 请求直接合并，返回当前状态且不创建新同步周期；同步周期终结后再次请求 sync 才创建新周期。sync 与 delete 相互切换时仍更新为最新意图。队列保证同一个仓库不会被并行同步。

### 4.2 查询仓库

```http
POST /api/v1/repo/status
Content-Type: application/json

{
  "origin_url": "https://gitee.com/src-openeuler/gcc.git"
}
```

同步成功后返回：

```json
{
  "key": "gitee.com/src-openeuler/gcc.git",
  "clone_url": "git://git-server:9418/gitee.com/src-openeuler/gcc.git",
  "sync_time": "2026-09-09T02:30:00Z"
}
```

仓库存在于状态 map 时返回 `200 OK`。最近一次后台操作失败时额外包含 `error` 和 `retry_count`；已有仓库同步失败不会清除上一次成功的 `sync_time` 和 `clone_url`，因此旧内容仍可 clone。仓库从未出现，或已完成删除且当前没有待处理 sync 意图时返回 `404 Not Found`。

返回 `clone_url` 后，调用方可以直接执行 `git clone clone_url`，不读取本地存储路径，也不自行实现 URL 到仓库 key 的转换。后续 fetch 与 clone 可以并发，因此 clone 可能读取到同步前或同步后的 refs；调用方已明确不要求关联某一次 sync。`sync_time` 只表示最近一次成功同步时间。

### 4.3 删除仓库

```http
POST /api/v1/repo/delete
Content-Type: application/json

{
  "origin_url": "https://gitee.com/src-openeuler/gcc.git"
}
```

合法请求加入队列后返回 `202 Accepted`，响应只包含 `key`。同步和删除使用相同仓库队列串行执行。删除不存在的仓库直接返回 `204 No Content`。

sync 和 status 使用统一的状态响应字段；尚无成功同步结果时省略 `clone_url` 和 `sync_time`：

```go
type RepositoryResponse struct {
    Key        string           `json:"key"`
    CloneURL   string           `json:"clone_url,omitempty"`
    SyncTime   *time.Time       `json:"sync_time,omitempty"`
    RetryCount int              `json:"retry_count,omitempty"`
    Error      *RepositoryError `json:"error,omitempty"`
}

type RepositoryError struct {
    Code      string `json:"code"`
    Message   string `json:"message"`
    Retryable bool   `json:"retryable"`
}
```

sync 响应只确认最新意图已经被记录，不代表操作完成。调用方不要求精确确认某一次重复请求，因此外部 API 不暴露 phase、操作 ID、generation 或 observed generation。

### 4.4 Git 查询

首版保留 `/command` 接口，但只允许构建流程使用的只读命令：

```text
git-rev-parse
git-show
git-log
git-ls-tree
```

```http
POST /command
Content-Type: application/json

{
  "repo": "https://gitee.com/src-openeuler/gcc.git",
  "command": ["git-show", "0123456789abcdef:gcc.spec"]
}
```

成功返回 `200 OK`：

```json
{
  "stdout": "commit 0123456789abcdef\n",
  "stderr": "",
  "exit_code": 0,
  "stdout_truncated": false,
  "stderr_truncated": false
}
```

响应结构固定为：

```go
type CommandResponse struct {
    Stdout          string `json:"stdout"`
    Stderr          string `json:"stderr"`
    ExitCode        int    `json:"exit_code"`
    StdoutTruncated bool   `json:"stdout_truncated"`
    StderrTruncated bool   `json:"stderr_truncated"`
}
```

stdout 和 stderr 必须分别捕获，不能合并。Git 子进程正常退出时返回其真实退出码；`exit_code=0` 表示命令成功，非零退出返回 `422 Unprocessable Entity`，响应正文仍使用 `CommandResponse`，保留用于诊断的 stdout、经过统一脱敏的 stderr 和真实退出码。

命令超过服务端命令超时时，服务必须终止整个子进程组并等待回收，返回 `504 Gateway Timeout`，响应仍使用 `CommandResponse`；此时 `exit_code` 固定为 `-1`，同时返回终止前已经捕获的 stdout 和 stderr。客户端断开或服务关闭导致请求 context 取消时也必须终止并回收子进程，但不保证还能向客户端写回响应。服务内部无法创建或启动子进程时返回 `500 Internal Server Error`，使用统一错误响应而不是 `CommandResponse`。

`--max-command-output-bytes` 是单次请求 stdout 与 stderr 捕获数据之和的上限。服务应并发读取两个管道，按实际读取字节累计，避免任一管道阻塞子进程。当累计输出首次超过限制时，立即取消命令、终止整个子进程组并等待回收，返回 `413 Content Too Large`；保留限制以内已经捕获的内容，将发生截断的流对应 `*_truncated` 设置为 `true`，`exit_code` 固定为 `-1`。如果到达上限时两个流仍可能同时产生数据，允许两个截断标志均为 `true`。

stdout 和 stderr 按原始字节捕获，但 JSON 字符串必须是合法 UTF-8：无效 UTF-8 字节使用 Unicode replacement character `U+FFFD` 替换。输出限制按替换前的原始字节数计算。服务不得在日志中记录完整 stdout 或 stderr。

实现要求：

- `command[0]` 必须与允许列表完全匹配；
- 不通过 shell 执行；
- 工作目录只能是服务端计算的仓库路径；
- 设置命令超时；
- 限制参数数量、参数长度和输出大小；
- 拒绝包含 NUL 的参数；
- 仓库不存在返回 `404 Not Found`；
- 命令不在允许列表中时返回 `403 Forbidden`；
- 执行失败返回 `422 Unprocessable Entity`；
- 输出超过限制返回 `413 Content Too Large`；
- 超时返回 `504 Gateway Timeout`。

`/command` 在 Git 子进程成功启动后使用 `CommandResponse` 表达 `200`、`422`、`413` 和 `504`，这是统一错误结构的唯一例外；对应语义分别为成功、`CommandFailed`、`OutputLimitExceeded` 和 `CommandTimeout`。请求校验失败、仓库不可用、命令不在允许列表中或子进程无法启动时仍返回通用 `ErrorResponse`。因此 `413` 对请求正文使用 `ErrorResponse{code=RequestTooLarge}`，对已经运行的命令输出超限使用 `CommandResponse`，两者可由响应结构区分。

后续可以将其拆成 resolve、tree 和 blob 接口，但不作为首版前置条件。

`/command` 仅供 EulerMaker 内部受信任组件调用，不接入 ebs-gateway，也不向普通用户或集群外网络开放。首版接受白名单 Git 命令的部分参数可能访问仓库附加信息或尝试写入本地文件的风险，不实现逐命令参数语法解析；仍必须保留命令名白名单、不经过 shell、固定工作目录、非 root 运行、超时、参数数量及长度限制和输出限制，并通过 NetworkPolicy 限制调用来源。容器除 `${dataDir}` 和必要临时目录外不得拥有可写路径。若以后向非受信任调用方开放，必须先增加逐命令参数校验，或将接口替换为 resolve、tree、blob 等结构化只读 API。

## 5. 同步逻辑

### 5.1 内存状态

服务为每个仓库 key 保存一条内存状态：

```go
type RepositoryAction string

const (
    ActionSync   RepositoryAction = "Sync"
    ActionDelete RepositoryAction = "Delete"
)

type RepositoryState struct {
    Key           string
    OriginURL     string // 最新 sync 请求 URL，仅在需要 clone 时使用
    DesiredAction RepositoryAction
    revision      uint64
    SyncTime      *time.Time
    RetryCount    int
    Error         *RepositoryError
    resetBackoff  bool
    operationPending bool
    operationLock sync.RWMutex
}
```

`SyncTime != nil` 表示最终路径存在一份至少成功同步过一次、尚未完成删除的裸仓库；此时 API 可以返回 CloneURL。同步成功时更新 SyncTime；后续同步失败不清除它；删除成功时必须清除 SyncTime。`Error` 至少包含稳定的 `code`、脱敏后的 `message` 和 `retryable`，表示最近一次后台操作错误。`operationPending` 表示当前 revision 尚在队列、执行中或等待自动重试；同一状态下的重复 sync 请求据此合并。创建新的非合并显式意图时清除旧的 Error 和 RetryCount。

状态 map 的值必须是 `*RepositoryState`。对象一经创建，在进程生命周期内不得替换或从 map 删除；这样同仓库的 operationLock 始终是同一个锁。读取任务参数时只复制 DesiredAction、OriginURL 和内部 revision，禁止复制包含互斥锁的 RepositoryState。

队列意图、错误、重试次数和 SyncTime 都只保存在内存，不向裸仓库写入额外状态文件。同步成功时，Worker 使用本次操作完成时的 UTC 时间更新 SyncTime。

服务启动时递归扫描 `${dataDir}`，跳过 `.tmp`，将结构合法的 bare repository 恢复到状态 map，并从 `remote.origin.url` 恢复 OriginURL；内部 revision 为 0，SyncTime 固定恢复为空。重启后即使本地仓库完整存在，status 也暂不返回 `sync_time` 和 `clone_url`；调用方重新提交 sync，下一次 fetch 成功后重新生成 SyncTime 并恢复 clone URL。裸仓库格式非法、origin 缺失或无法解析时记录错误且不加入可用状态，不自动删除。扫描完成前 readiness 失败。

### 5.2 队列和最新意图

服务使用一个 typed rate-limiting workqueue，队列元素只包含仓库 key。Worker 数量由配置指定。workqueue 的 dirty/processing 语义保证同一个 key 不会被两个 Worker 同时处理；服务不自行实现第二套 dirty 集合。

RepositoryState map 由一个全局互斥锁保护，锁内只修改内存字段，不执行文件系统、Git、队列阻塞操作或网络请求。每个仓库的 `operationLock` 保护本地裸仓库；全局锁与 operationLock 不得同时持有。

sync 和 delete 使用最新意图模型；相同 sync 在一个未终结周期内具有合并语义：

1. handler 规范化 URL并取得全局锁；
2. 创建或读取 RepositoryState；
3. 若请求为 sync，且 `DesiredAction=Sync && operationPending=true`，直接返回当前状态：不增加 revision、不清除 Error/RetryCount、不重置退避，也不再次 Add；排队、执行中和等待自动重试均属于未终结周期；
4. 否则创建新显式意图：内部 `revision++`，更新 DesiredAction 并设置 `operationPending=true`；sync 请求同时记录本次 OriginURL，delete 请求保留该字段。OriginURL 只是未来需要 clone 时的输入，不代表覆盖已有仓库的 origin；
5. 清除上一意图的错误和 RetryCount，设置 `resetBackoff=true` 后释放锁；
6. 调用 `queue.Add(key)`；如果 key 正在处理，workqueue 将其标记为 dirty；如果 key 正在延迟队列中，本次 Add 使其立即可用；
7. 返回当前状态，不暴露内部 revision。

Worker 在当前 revision 成功、不可重试失败或重试耗尽时设置 `operationPending=false`。安排 `AddRateLimited` 后仍保持 true；发现 revision 已变化时不修改该字段，因为它已经表示更新意图的未终结周期。终结后的新 sync 请求会增加 revision 并重新入队。

同一个 key 的多个请求以最后分配的内部 revision 为最终意图。例如 sync 正在执行时收到 delete，当前 sync 可以完成，但随后必须执行 delete；delete 正在执行时收到 sync，delete 完成后必须再次 clone。中间意图可以被更新的 revision 合并，服务只保证最终状态符合最新意图。

同仓库操作遵循以下不变量：

- 任意时刻至多有一个 sync/delete 持有写锁；
- 任意数量的 `/command` 可以并发持有读锁，但不能与 sync/delete 并发；
- 不同仓库之间不共享 operationLock，可以由不同 Worker 并发处理；
- workqueue 的串行语义用于合并意图，operationLock 负责保护文件系统，两者都必须保留；
- API 请求的到达顺序以持有全局状态锁并分配内部 revision 的顺序为准，而不是网络连接建立或响应返回顺序；
- 旧 revision 允许完成已经开始的本地操作，但其结果不得终结更新 revision。

Worker 处理规则：

1. 从队列取得 key，并在全局锁内复制当前 DesiredAction、OriginURL 和 revision；如果 `resetBackoff=true`，同时清除此标志并记录本周期需要重置退避；
2. 释放全局锁；需要重置退避时，由 Worker 在锁外调用 `Forget(key)`；
3. 获取该仓库 operationLock 的写锁并执行对应操作；
4. 操作结束后释放 operationLock，再取得全局锁；
5. 先根据已经完成的文件系统操作更新物理事实：sync 成功则更新 SyncTime，delete 成功则清除 SyncTime；即使 revision 已更新，这两项也必须反映当前磁盘状态；
6. 比较当前 revision 与本周期复制的 revision；如果 revision 已更新，丢弃本周期的 Error 和 RetryCount 结果，不得用旧错误覆盖新意图，也不得根据旧错误调用 `AddRateLimited`；workqueue 会因处理期间发生的 Add 再次返回该 key；
7. 如果 revision 未更新，根据操作结果更新 Error、RetryCount 和重试行为；
8. Worker 是 `Done`、`Forget`、`AddRateLimited` 的唯一调用者。

自动重试不增加 revision，使用确定性的指数退避，不叠加全局令牌桶，也不增加 jitter：

```text
delay(n) = min(retryBaseDelay * 2^(n-1), retryMaxDelay)
```

其中 `n` 从 1 开始，表示即将安排的第 `n` 次自动重试。默认 `retryBaseDelay=1s`、`retryMaxDelay=1m`。`max-retries=5` 表示首次执行失败后最多再执行 5 次，因此单个 revision 最多执行 6 次；设置为 0 表示不自动重试。`RetryCount` 表示当前 revision 已经安排或开始执行的自动重试序号，首次执行时为 0，第一次调用 `AddRateLimited` 后为 1，最大不超过 `max-retries`。操作成功、新的非合并显式意图到达时将 RetryCount 和 Error 清零；合并的 sync 请求保持当前值；不可重试失败和重试耗尽时保留最终 RetryCount 和 Error 供 status 查询。

可重试错误发生且当前 `RetryCount < max-retries` 时，Worker 先令 RetryCount 加一、保存 Error，再调用 `AddRateLimited`；到期取出后 RetryCount 不再增加，只有该次执行再次失败并确实安排下一次重试时才增加。网络超时、临时 DNS/连接错误和远端 5xx 属于可重试错误；URL/认证错误、仓库格式错误和本地权限错误默认不可重试。不可重试错误或重试耗尽时记录 Error 并调用 `Forget`。新的显式 sync/delete 请求增加 revision，可以重新激活已停止重试的 key，并在 Worker 处理该 revision 前清除旧 rate limiter 计数。

Worker 每次只终结一次队列项：成功、不可重试失败、重试耗尽以及发现更新 revision 时调用 `Forget` 后 `Done`；仅当前 revision 的可重试失败调用 `AddRateLimited` 后 `Done`。handler 和只读请求不得调用 `Done`、`Forget` 或 `AddRateLimited`。

### 5.3 同步和删除

首次同步：

1. 校验 OriginURL 并选择凭证；
2. 按 3.3 节通过 `tmpDirFD` 创建本次同步的唯一 `clone-{uuid}` 临时目录，目录已存在时生成新 UUID，禁止复用或清空已有路径；
3. 执行 `git clone --mirror`；
4. 校验结果是 bare repository；
5. 按 3.3 节使用 `renameat2(RENAME_NOREPLACE)` 原子发布到最终路径，完成发布后校验并 `fsync` 最终路径的父目录；
6. 返回成功，由 Worker 更新内存中的 SyncTime。

首次 clone 成功后，该 URL 被 Git 写入裸仓库的 `remote.origin.url`，作为此仓库后续同步的唯一 origin。

已有仓库同步：

1. 校验目标是 bare repository；
2. 读取仓库已有的 `remote.origin.url`；
3. 根据该 origin 选择凭证；
4. 执行 `git fetch --prune '+refs/*:refs/*'`；
5. 返回成功，由 Worker 更新内存中的 SyncTime。

相同 key 的后续 sync 必须从本地裸仓库读取 `remote.origin.url` 并使用该地址 fetch，不得使用请求携带的 URL，也不得修改已有 origin。后续请求中的 URL 只用于计算 key。即使不同协议或用户名的 URL 映射到同一个 key，仍以首次 clone 保存的 origin 为准；需要切换 origin 时必须先完成 delete，再提交新的 sync。

删除按 3.3 节在写锁内将最终仓库原子移动到 `.tmp` 下的 `delete-{uuid}` 并核对 device/inode，成功即视为逻辑删除并清除内存中的 SyncTime；释放写锁后由当前 Worker 尽力清理该临时目录，清理失败只记录日志并由后台清理器或下次启动继续处理，不改变逻辑删除结果。目标不存在也视为删除成功并清除 SyncTime。移动失败按文件系统错误分类决定重试，不能直接递归删除未经校验的请求路径。

### 5.4 临时目录清理

服务只允许通过启动时安全取得的 `tmpDirFD` 清理 `.tmp` 的直接子目录，并且目录名必须与 `clone-{uuid}`、`delete-{uuid}` 或 `auth-{uuid}` 完整匹配。目标必须通过 3.3 节的目录 FD 操作打开并确认不是符号链接；未知文件、未知目录和不符合命名规则的条目不得自动删除，只记录告警。任何清理逻辑都不得接受请求 URL、仓库 key 或外部输入作为待删除路径。

进程维护一个由独立互斥锁保护的 active temporary directory 集合：

1. Worker 先生成随机名称并将绝对路径登记为 active，再通过 `Mkdir` 创建目录；创建失败时撤销登记；
2. clone 原子移动成功，或者临时目录清理成功后，撤销 active 登记；
3. 清理失败时也撤销登记，使后台清理器可以再次处理；
4. 后台清理器只处理符合命名规则且不在 active 集合中的目录；
5. active 集合锁只保护集合，不得在锁内执行 Git、文件遍历或递归删除，也不得与全局状态锁或 operationLock 同时持有。

各场景的处理规则：

- **clone 失败或超时**：终止并回收 Git 子进程后，当前 Worker 立即尝试删除对应 `clone-{uuid}`。清理失败不改变原同步错误分类，也不单独增加同步重试次数；记录结构化告警并交给后台清理器重试。
- **clone 成功**：临时目录原子移动到最终仓库路径后，原临时路径自然消失，不再执行递归删除。如果最终路径已存在则不能覆盖；按 3.3 节重新分类目标，合法 bare repository 在清理自己的临时目录后改为 fetch，其他对象返回 `RepositoryConflict`。
- **删除仓库**：最终仓库成功原子移动到 `delete-{uuid}` 后，逻辑删除已经完成。当前 Worker 释放 operationLock 后立即尝试递归删除隔离目录；清理失败不恢复 SyncTime，也不把删除转换为操作失败，由后台清理器继续处理。
- **认证材料**：每次 Git 网络操作结束、启动失败、超时或 context 取消后都立即清理对应 `auth-{uuid}`。清理失败记录结构化告警并交给后台清理器；认证材料清理失败不改变原 Git 操作的结果。
- **进程崩溃**：正在使用的 `clone-*` 或尚未清完的 `delete-*` 会留在磁盘。由于同步意图只保存在内存中，重启后不恢复这些操作，而是把所有可识别临时目录视为孤儿并清理。
- **正常关闭**：先按关闭顺序等待 Worker；已经完成或取消的 Worker 执行一次即时清理。超过优雅退出期限后允许留下临时目录，由下次启动处理，不延长进程退出时间。

服务启动时必须按 3.3 节创建或安全打开 `.tmp`，并在恢复最终仓库状态之前执行一次同步清理：通过 `tmpDirFD` 删除其中全部可识别的 `clone-*`、`delete-*` 和 `auth-*` 孤儿目录，然后扫描最终仓库。清理任一可识别目录失败时，服务可以启动 HTTP liveness，但 readiness 保持失败，不启动 Worker，也不接受管理请求；后台清理器按 `--temp-cleanup-interval` 重试，全部成功后再完成仓库扫描并置为 ready。未知条目不阻塞 readiness。

服务 ready 后，后台清理器按固定周期扫描 `.tmp`，用于重试运行时失败的清理。后台清理不修改 RepositoryState，不影响已经完成的 sync/delete 结果。清理失败记录路径、用途、错误类型和重试次数，但不得记录 origin URL 或凭证。

### 5.5 只读操作并发

`/command` 先在全局锁内取得稳定的 RepositoryState 指针，释放全局锁后再获取 operationLock 的读锁，最后校验最终路径是合法 bare repository。校验通过即可执行，不要求 SyncTime 非空；校验失败返回 `404 RepositoryNotAvailable`。command 和写操作的先后顺序以 operationLock 为线性化顺序：command 先取得读锁则先执行，否则等待写操作结束并重新检查目录。

同步和删除持有写锁，因此不会与 `/command` 并发。git daemon 不经过进程内锁：fetch 期间 Git 自身的引用更新保持原子，已有 clone 可以继续；仓库删除或重新创建期间，新 clone 可能短暂失败，首版接受该行为，客户端可以重试。

### 5.6 关闭

服务关闭时按以下顺序执行：

1. readiness 置为失败并停止接受新的 sync/delete/command 请求；
2. 关闭队列，不再开始新操作；
3. 等待正在运行的 Worker 和 command 在优雅退出期限内结束；
4. 超时后取消对应 context 并终止 Git 子进程；
5. 停止 git daemon；
6. 关闭 HTTP Server。

## 6. 凭证与安全

凭证只保存在由 Secret 挂载的 TOML 文件中，不复制到镜像，也不通过服务启动参数或服务级环境变量传递。Git 子进程执行期间允许按本节约定，通过只属于该子进程的环境向受控 helper 传递所需凭证。凭证规则包含协议、host 和可选仓库路径前缀；匹配时解析 URL 后逐字段比较，不使用字符串包含匹配。

SSH 使用私钥和 `known_hosts`，不得关闭 host key 校验。HTTPS 使用系统 CA 或挂载的 CA bundle。URL 中禁止携带密码或 token。

Git Server 是内部服务，HTTP API 不执行调用方身份认证或角色授权，包括 sync、status、delete 和 `/command`。服务只在 EulerMaker 受信任内部网络开放，通过网络隔离和 NetworkPolicy 限制访问，不接入 ebs-gateway 的普通用户 API，也不得暴露到公网。下文凭据配置仅用于 Git Server 访问远端 Git 仓库，不用于认证 API 调用方。

服务不配置远端 host 白名单，格式合法且能够连接的远端地址均可使用。因此管理 API 必须只对受信任的内部组件开放，不能允许普通用户直接提交任意仓库同步请求。

### 6.1 凭证加载与匹配

服务启动时一次性严格解析认证 TOML，拒绝未知字段、重复优先级、无效 PEM、无法解密的私钥、无效 `known_hosts` 和不适用于对应协议的字段。配置加载成功后，凭证只保存在进程内存中；运行期间不重新读取配置。

每次 clone 或 fetch 使用实际即将访问的 origin 选择凭证。首次 clone 使用请求中的 OriginURL；已有仓库 fetch 必须先读取 `remote.origin.url`，再根据该 URL 选择凭证。匹配过程如下：

1. SCP 风格地址视为 `ssh`，host 转为小写并移除 DNS 根点；scheme、host 和 path 使用与仓库 key 转换相同的解析及解码结果；
2. `path_prefix` 去除开头的 `/`，为空表示匹配该 host 下全部仓库，非空匹配完整路径段前缀，`team/` 不得匹配 `team-other/`；
3. 先匹配 scheme 和 host，再选择最长的 `path_prefix`；相同优先级存在多条规则属于启动配置错误；
4. HTTP、HTTPS 和 Git 协议没有匹配规则时按匿名方式访问，不回退尝试其他凭证；SSH 没有匹配规则时返回 `AuthNotConfigured`，避免隐式使用运行用户的 SSH 配置、私钥或 agent；
5. 认证失败属于不可重试错误，错误响应和日志不得包含用户名之外的认证内容。

端口不参与认证规则匹配，同一个 host 的默认端口和非默认端口使用同一组规则。需要按端口隔离凭证时应使用不同域名；首版不增加端口匹配字段。

### 6.2 Git 子进程公共环境

所有访问远端的 Git 子进程都直接通过 `exec` 启动，不经过 shell，并显式构造环境。除最小系统环境外至少设置：

```text
GIT_TERMINAL_PROMPT=0
GIT_CONFIG_NOSYSTEM=1
GIT_CONFIG_GLOBAL=/dev/null
GIT_OPTIONAL_LOCKS=0
```

服务必须清除继承环境中的 `GIT_ASKPASS`、`SSH_ASKPASS`、`GIT_SSH`、`GIT_SSH_COMMAND`、`GIT_CONFIG_*`、`GIT_DIR`、`GIT_WORK_TREE`、`SSH_AUTH_SOCK`、credential helper 和代理认证变量，再按本次匹配结果加入允许的变量。普通网络代理变量是否保留由部署环境决定，但日志不得输出完整子进程环境。

每次需要文件形式认证材料的 Git 操作都在 `${dataDir}/.tmp/auth-{uuid}` 创建独立目录，目录权限为 `0700`，其中的私钥、`known_hosts` 和 CA 文件权限为 `0600`。目录先登记到 active temporary directory 集合，再创建和写入；不得复用其他操作的目录。操作结束后的清理规则见 5.4 节。

### 6.3 HTTPS 认证

HTTPS 用户名和密码通过受控的 askpass helper 提供，不拼接到 URL，不写入 Git config，也不作为 Git 命令参数。helper 是 Git Server 二进制的内部运行模式，依据 Git 传入的提示只返回本次操作的用户名或密码；无法识别的提示直接失败。

父进程为子进程设置：

```text
GIT_ASKPASS=<git-server askpass helper>
GIT_TERMINAL_PROMPT=0
```

凭证值通过仅对子进程可见的环境变量传给 helper；变量名固定，但变量值不得写入日志、错误响应或指标。子进程退出后不得把该环境保存到任何长期对象。首版接受同一 Pod、同一 Unix 用户下具有 `/proc` 读取能力的进程可能观察子进程环境，因此 Git Server 必须独占容器且不得与不受信任进程共享 PID namespace。

配置了自定义 CA 时，将 CA 内容写入本次 `auth-{uuid}/ca.pem` 并设置 `GIT_SSL_CAINFO`。未配置时使用系统信任库。不得设置 `GIT_SSL_NO_VERIFY`。HTTP 协议可以使用 username/password，但不提供传输加密；生产配置默认禁止带凭证的 HTTP 规则，只有显式的 `--allow-insecure-http-auth=true` 才允许加载。

### 6.4 SSH 认证

SSH 私钥和 `known_hosts` 分别写入本次 `auth-{uuid}/identity` 和 `auth-{uuid}/known_hosts`。服务将 `GIT_SSH` 指向 Git Server 二进制的内部 SSH wrapper 模式，并通过仅对子进程可见的环境变量传递两个文件路径。wrapper 校验 Git 传入的 host 和远端命令参数后，直接以参数数组启动 OpenSSH，其等价语义为：

```text
ssh -i <identity> -o IdentitiesOnly=yes -o UserKnownHostsFile=<known_hosts> \
    -o GlobalKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes \
    -o BatchMode=yes
```

wrapper 不通过 shell 拼接命令，不接受调用方提供额外 SSH option；认证文件路径必须由服务生成且不含外部输入。URL 中已有 SSH 用户名时使用 URL 用户名；否则使用认证规则的 `username`。两者都不存在时配置无效，不能依赖运行用户名称。

未加密私钥使用 `BatchMode=yes`。私钥配置了 passphrase 时，改用受控 SSH askpass helper：设置 `SSH_ASKPASS`、`SSH_ASKPASS_REQUIRE=force` 和仅本次操作可见的 passphrase 环境变量，在无控制终端的新会话中启动 Git，并关闭 `BatchMode`。helper 只响应私钥口令提示，其他提示失败；host key 确认绝不能交给 askpass。无论是否配置 passphrase，都必须启用 `StrictHostKeyChecking=yes`，不得使用 `InsecureIgnoreHostKey` 或 `/dev/null` 代替规则提供的 `known_hosts`。

### 6.5 origin 与敏感信息处理

首次 clone 的 OriginURL 已经通过 URL 校验且禁止 HTTP userinfo。认证信息仅通过上述运行时通道注入，因此 Git 写入的 `remote.origin.url` 不包含密码、token、私钥或 passphrase。clone 完成后必须重新读取并解析 `remote.origin.url`；若其中出现 HTTP userinfo、非支持协议或与请求计算出不同的仓库 key，本次 clone 失败并清理临时仓库。

Git stderr 在进入 RepositoryError、`CommandResponse`、日志或指标标签前必须经过统一脱敏器处理，至少替换：配置中的 username/password/passphrase、完整私钥内容、Authorization header、包含 userinfo 的 URL，以及认证临时目录绝对路径。除内部 `/command` 返回经过脱敏的 stderr 外，API 只返回稳定错误码和脱敏摘要，不返回 Git 原始 stderr。允许记录远端 host、仓库 key、Git 退出码和错误分类。

## 7. 只读 clone

首版保留只读 git daemon：

```text
git daemon --base-path=${dataDir} --export-all --reuseaddr
```

访问示例：

```text
git://git-server:9418/gitee.com/src-openeuler/gcc.git
```

git daemon 使用 `--export-all`，不要求仓库包含 `git-daemon-export-ok`。它没有认证和加密，只能通过内部 Service 和 NetworkPolicy 对受信任组件开放，不得暴露到集群外，也不得启用 receive-pack。

由于 `${dataDir}/.tmp` 位于 git daemon 的 base path 下，知道完整随机临时路径的内部客户端理论上可以在 clone 完成、原子移动之前访问临时裸仓库。首版接受该短暂风险：git daemon 不提供仓库目录枚举，临时目录名必须使用具有足够熵的随机值，失败和过期临时目录应及时清理，且端口只对受信任的内部组件开放。

HTTP API 和 git daemon 使用同一 Pod 的两个容器，或由主进程明确管理子进程生命周期；Dockerfile 不使用 shell 后台运行两个进程。

## 8. 配置

运行参数全部通过命令行传入：

```text
git-server \
  --listen-address=:8080 \
  --data-dir=/srv/git \
  --workers=20 \
  --operation-timeout=10m \
  --max-retries=5 \
  --retry-base-delay=1s \
  --retry-max-delay=1m \
  --temp-cleanup-interval=10m \
  --max-request-body-bytes=65536 \
  --max-command-output-bytes=16777216 \
  --git-daemon-enabled=true \
  --git-daemon-address=:9418 \
  --clone-base-url=git://git-server:9418 \
  --allow-insecure-http-auth=false \
  --auth-config=/etc/git-server/auth.toml
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--listen-address` | `:8080` | HTTP API 监听地址 |
| `--data-dir` | `/srv/git` | 裸仓库根目录，临时目录固定为 `{data-dir}/.tmp` |
| `--workers` | `20` | 同步和删除 Worker 数量 |
| `--operation-timeout` | `10m` | 单次 Git 操作超时 |
| `--max-retries` | `5` | 首次执行之外的最大自动重试次数，允许为 0 |
| `--retry-base-delay` | `1s` | 第一次自动重试的等待时间，必须大于 0 |
| `--retry-max-delay` | `1m` | 指数退避上限，不能小于 `retry-base-delay` |
| `--temp-cleanup-interval` | `10m` | 临时目录清理失败后的重试周期，必须大于 0 |
| `--max-request-body-bytes` | `65536` | HTTP JSON 请求正文的最大字节数 |
| `--max-command-output-bytes` | `16777216` | `/command` 最大输出字节数 |
| `--git-daemon-enabled` | `true` | 是否启动只读 git daemon |
| `--git-daemon-address` | `:9418` | git daemon 监听地址 |
| `--clone-base-url` | `git://git-server:9418` | 生成响应中 `clone_url` 的基础地址 |
| `--allow-insecure-http-auth` | `false` | 是否允许通过明文 HTTP 使用用户名和密码；生产环境必须保持关闭 |
| `--auth-config` | 无 | 远端 Git 认证 TOML 文件，未配置时只允许匿名访问远端仓库 |

`clone-base-url` 必须来自启动参数，不能根据 HTTP 请求的 `Host` header 推断。`workers`、超时、重试次数、退避参数和输出限制必须在启动时完成范围校验，非法值导致进程启动失败。

只有远端 Git 认证信息保存在 TOML 文件中：

```toml
[[auth]]
scheme = "ssh"
host = "gitee.com"
path_prefix = "src-openeuler/"
username = "git"
private_key = """-----BEGIN OPENSSH PRIVATE KEY-----
...
-----END OPENSSH PRIVATE KEY-----"""
passphrase = ""
known_hosts = "gitee.com ssh-ed25519 AAAA..."

[[auth]]
scheme = "https"
host = "git.example.com"
path_prefix = "team/"
username = "builder"
password = "example-password"
ca = """-----BEGIN CERTIFICATE-----
...
-----END CERTIFICATE-----"""
```

每条 `auth` 规则按 `scheme`、`host` 和 `path_prefix` 匹配，选择最长的 path prefix；多条规则具有相同匹配优先级时配置无效，服务拒绝启动。SSH 和 HTTPS 不使用的字段必须为空，未知字段拒绝加载。

认证 TOML 作为一个整体通过 Secret 挂载，只读权限设置为 `0400`，不得通过 ConfigMap、命令行参数或环境变量传递。服务不得打印 TOML 内容。配置只在启动时加载，修改后需要重启服务生效。

## 9. 部署

`hacks/docker-compose.yaml` 增加 Git Server、`git-data` 持久卷、凭证挂载以及调用方使用的地址。

生产部署要求：

- 单副本并使用 PVC，不依赖固定节点的 hostPath；
- 使用非 root 用户和只读根文件系统；
- 数据目录和临时目录显式可写；
- 配置 liveness、readiness 和优雅终止；
- 分别限制 HTTP 管理端口和 git daemon 端口的访问来源。

readiness 检查数据目录可读写且 Worker 已启动；单个远端仓库不可访问不影响整个服务的 readiness。

## 10. 可观测性

结构化日志记录仓库 key、操作类型、Worker、耗时、重试次数和稳定错误类型，不记录凭证、Authorization header、私钥内容或完整命令输出。

首版指标包括：

- 同步和删除队列深度；
- 同步成功、失败和重试次数；
- clone、fetch、删除和查询耗时；
- 当前工作的 Worker 数量；
- 本地仓库数量。

## 11. 测试要求

至少覆盖：

- HTTPS 和 SSH 地址映射为相同 key 和目录；
- URL 形式、HTTP userinfo、端口、百分号编码、路径穿越和符号链接校验；
- 父目录并发创建、符号链接替换、`RENAME_NOREPLACE` 冲突和 rename 前后 device/inode 校验；
- 启动扫描、发布、删除和临时目录清理均不能逃逸 dataDir 或跟随符号链接；
- 首次 clone、重复同步和 fetch；
- 服务重启后恢复已有 bare repository 和 origin，但 SyncTime 为空；再次 fetch 成功后恢复 sync_time 和 clone_url；
- clone 失败、超时和进程重启后的临时目录清理；
- 删除隔离目录清理失败不恢复 SyncTime，并能由后台清理器重试；
- 后台清理器跳过 active 和未知条目，不跟随符号链接；
- 相同仓库处于排队、执行或自动重试期间的重复 sync 请求合并，不增加 revision、不重置错误和退避；同步周期终结后的新 sync 创建新 revision；
- 同步和删除串行；
- sync 执行中收到 delete、delete 执行中收到 sync 时，最终状态符合最新内部 revision；
- 旧 revision 的成功和失败均不能覆盖新 revision，也不能触发旧请求的退避；
- 同仓库 command 与写操作互斥，不同仓库可以并发；
- 显式请求立即唤醒退避项，并重置自动重试计数；
- Git 命令允许列表、超时和输出限制；
- SSH host key 和 HTTPS CA 校验；
- 认证规则最长 path prefix 匹配、无匹配时匿名访问以及重复优先级启动失败；
- HTTPS askpass、SSH wrapper、加密私钥 askpass 和临时认证目录清理；
- origin 不包含凭证，Git stderr、错误响应和日志完成敏感信息脱敏；
- 重试耗尽后由新请求重新激活；
- 关闭时 Worker 和 Git 子进程正确退出。
