# Runner Controller 设计

## 1. 定位与范围

Runner Controller 是 `controller-manager` 中负责 Runner 健康状态收敛的控制器。它参考 Kubernetes Node Lifecycle Controller 的健康判定思路，但只实现当前 EBS 数据模型需要的最小能力：根据 Runner 持久化的最后心跳时间识别失联实例，并把 Runner 标记为 `Offline`。

首版职责：

- 监听 Runner 的创建、更新和删除事件；
- 根据 `status.heartbeat`、`metadata.creationTimestamp` 和当前时间计算下一次健康检查时间；
- 对超过心跳超时阈值且处于可处理状态的 Runner 进行权威 GET；
- 再次确认仍超时后，仅将 `status.phase` 更新为 `Offline`；
- 正确处理 Runner 心跳与 Controller 写入之间的并发、冲突和结果未知；
- 暴露必要的结构化日志和指标。

首版不负责：

- 不创建、删除或重新注册 Runner；
- 不生成或校验 `spec.instanceId` 的格式；该字段由 Runner 注册流程和 apiserver 校验，Controller 只在响应身份比较时保持其不变；
- 不根据 Runner 数量扩缩执行机；
- 不修改 `spec.unschedulable`、taints、labels 或其他 spec/metadata 字段；
- 不把 `Offline` Runner 自动恢复为 `Booting`、`Idle` 或 `Running`，恢复由 Runner agent 的下一次成功状态上报完成；
- 不读取、更新或删除 Job。已绑定 Job 的失败收敛由 Job Controller 处理；
- 不根据 Runner 上是否存在 Job推导 `Idle` 或 `Running`；
- 不实现节点驱逐、Pod eviction、污点管理、zone health 或 Kubernetes Node Controller 的其他集群能力；
- 不清理长期离线的 Runner。若以后需要历史 Runner 回收，应作为独立、显式启用的保留策略设计。

Controller 之间的关系为：

```text
Runner agent --heartbeat--> Runner.status
                              |
                              v
                    Runner Controller
                              |
                     phase = Offline
                              |
               +--------------+--------------+
               |                             |
               v                             v
          Scheduler                    Job Controller
      不再选择该 Runner          已绑定 Job 进入失联宽限期
```

Runner Controller 的离线超时与 Job Controller 的 Runner 丢失宽限期是两个连续阶段。前者判断 Runner 是否失联，后者在 Runner 已经明确 `Offline` 后给执行恢复保留额外时间。二者不能合并，否则 Job Controller 会重新承担心跳判定职责。

## 2. 依赖与代码组织

建议实现目录：

```text
components/controller-manager/pkg/controllers/runner/
  client.go       # 最小 API Client 和共享客户端适配器
  controller.go   # 初始化、事件处理和队列接入
  reconciler.go   # 超时判断、权威确认和状态更新
  metrics.go
```

Runner 是集群级资源且支持 List/Watch。Initializer 必须通过共享 `WatchSourceFactory` 获取 `source.RunnersGVR` 对应的 `source.CachedSource`，并在 `Manager.Run` 之前注册事件处理器：

```go
type Controller struct {
    *controller.BaseController
    runners source.CachedSource
    client  Client
    clock   clock.Clock
    config  Config
}
```

生产环境注入 `clock.RealClock{}`，测试注入 `clocktesting.FakeClock`。一次 `Sync` 只调用一次 `clock.Now()`，后续判断和状态时间边界全部使用该值；业务代码不得直接调用 `time.Now()`、`time.Since()` 或 `time.Until()`。

Runner key 等于 `metadata.name`，不包含 namespace。`Sync` 使用 `runners.GetByKey(key)` 读取缓存中的最新深拷贝。类型不符属于内部错误；`source.ErrCacheNotSynced` 按临时错误重试，不能退化为 API List。

Controller 声明最小客户端接口：

```go
type Client interface {
    GetRunner(ctx context.Context, name string) (*ebsv1.Runner, error)
    UpdateRunnerStatus(ctx context.Context, runner *ebsv1.Runner) (*ebsv1.Runner, error)
}
```

共享 API Client 适配器分别调用：

```text
GET /apis/ebs/v1/runners/{name}
PUT /apis/ebs/v1/runners/{name}/status
```

`UpdateRunnerStatus` 使用完整的最新 Runner 对象和当前 `metadata.resourceVersion`。适配器必须保留 `client.WriteError` 的 `WriteRejected`、`WriteNotSent`、`WriteUnknown` 分类以及 HTTP 状态判断能力。

## 3. 配置

Go 配置结构放在独立的 Controller 层级，命令行名称保持扁平：

```go
type RunnerControllerOptions struct {
    HeartbeatTimeout   time.Duration
    StartupGracePeriod time.Duration
}

type Config struct {
    HeartbeatTimeout   time.Duration
    StartupGracePeriod time.Duration
    MaxRetries         int
}
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--runner-heartbeat-timeout` | `2m` | 从最后一次持久化心跳起允许无新心跳的时长 |
| `--runner-startup-grace-period` | `5m` | 新建 Runner 尚未写入首次心跳时的最大启动时长 |

两个时间值都必须大于 0，`Config.MaxRetries` 必须大于等于 0。Runner 默认心跳周期为 30 秒，`2m` 允许连续丢失数次心跳并吸收短暂网络波动。生产环境应保证心跳超时显著大于正常心跳周期。

Runner Controller 不增加独立的重试次数命令行参数。`MaxRetries` 的唯一来源是 controller-manager 全局的 `ManagerOptions.ControllerMaxRetries`，对应 `--controller-max-retries`。`pkg/options` 只解析和校验 Options；`cmd/controller-manager` 或 `pkg/app` 作为组装层，在创建 initializer map 时显式完成转换：

```go
runnerConfig := runnercontroller.Config{
    HeartbeatTimeout:   options.Runner.HeartbeatTimeout,
    StartupGracePeriod: options.Runner.StartupGracePeriod,
    MaxRetries:         options.Manager.ControllerMaxRetries,
}

initializers[runnercontroller.Name] = runnercontroller.Initializer(runnerConfig)
```

`Initializer` 闭包持有完整且已经校验的业务 `Config`。公开的 `New` 保持测试友好的默认入口；内部 `newController` 额外接收 BaseController 选项，使生产组装能够传入 Manager 的全局慢速退避配置：

```go
func New(runners source.CachedSource, client Client, clk clock.Clock, config Config) (*Controller, error) {
    return newController(runners, client, clk, config)
}

func newController(
    runners source.CachedSource,
    client Client,
    clk clock.Clock,
    config Config,
    baseOptions ...controller.Option,
) (*Controller, error) {
    if err := config.validate(); err != nil {
        return nil, err
    }
    c := &Controller{runners: runners, client: client, clock: clk, config: config}
    base, err := controller.New(Name, c.sync, config.MaxRetries, baseOptions...)
    if err != nil {
        return nil, err
    }
    c.BaseController = base
    // 注册 Runner handler。
    return c, nil
}
```

Initializer 从 `manager.InitContext.Config` 读取已经由 Manager 校验的全局慢速退避参数，并在创建生产实例时显式传给 BaseController：

```go
func Initializer(config Config) manager.InitFunc {
    return func(_ context.Context, init manager.InitContext) (controller.Controller, bool, error) {
        runners, err := init.Dependencies.WatchFactory.ForResource(source.RunnersGVR)
        if err != nil {
            return nil, false, err
        }
        value, err := newController(
            runners,
            newAPIClient(init.Dependencies.Client),
            clock.RealClock{},
            config,
            controller.WithSlowRetry(
                init.Config.SlowRetryInitial,
                init.Config.SlowRetryMax,
                init.Config.SlowRetryJitter,
            ),
        )
        return value, err == nil, err
    }
}
```

Runner Controller 不得直接依赖 `options.ManagerOptions`、读取 flag 或在内部写死重试次数。`MaxRetries` 只表示进入慢速阶段前允许的快速连续重试次数；慢速退避参数不复制到 Runner Controller 的业务 `Config`，只能通过 `InitContext.Config -> controller.WithSlowRetry` 传入。测试直接调用 `New` 时使用 BaseController 默认值；需要验证慢速调度边界时调用内部 `newController` 并注入 `controller.WithSlowRetry`、`controller.WithClock` 和 `controller.WithJitter`。

首版不需要独立的轮询周期。每次事件根据对象时间戳计算 `RequeueAfter`；超过单次安全定时上限时按 5.2 节截断并定期复查。进程重启后，初始 List 会重新为全部 Runner 计算截止时间。

## 4. 字段所有权

Runner agent 与 Runner Controller 都会更新 `/status`，必须明确字段所有权：

| 写入方 | 拥有的字段或迁移 |
|--------|------------------|
| Runner agent | `phase` 的注册、启动、运行和恢复迁移；`conditions`、`capacity`、`allocatable`、`addresses`、`info`、`heartbeat` |
| Runner Controller | 仅在心跳超时确认后执行当前 `phase -> Offline` |

Runner Controller 更新前必须 DeepCopy 权威 GET 返回的对象，只修改：

```go
request.Status.Phase = "Offline"
```

以下字段必须原样保留：

- `status.heartbeat`；
- `status.conditions`；
- `status.capacity` 和 `status.allocatable`；
- `status.addresses` 和 `status.info`；
- 全部 spec、labels、annotations 和服务端 metadata。

其中包括不可变的 `spec.instanceId`。Runner Controller 不使用 `instanceId` 代替 `metadata.uid` 进行乐观并发或对象重建判断。

Controller 不把心跳改为当前时间，也不清空旧容量信息。`Offline` 表示旧信息不再可用于调度，不表示历史上报数据需要删除。

Runner agent 允许通过带有更新心跳的状态写入将 `Offline` 恢复为 `Booting`、`Idle` 或 `Running`。Runner Controller 对 `Offline` 对象直接结束，不阻止或覆盖后续恢复。

## 5. 健康判定

### 5.1 有效时间基准

只有以下 phase 参与心跳超时判定：

```text
Registering | Booting | Idle | Running
```

`Offline` Runner 直接结束。phase 为空或不是上述合法值时，视为非法对象：记录结构化日志和指标，返回永久错误，不计算截止时间，也不尝试修复其状态。虽然 apiserver 会拒绝新写入的非法 phase，Controller 仍必须安全处理历史数据和异常响应。

对可处理的 Runner，截止时间按以下规则计算：

| 条件 | 截止时间 |
|------|----------|
| `status.heartbeat` 非零 | `heartbeat + HeartbeatTimeout` |
| `status.heartbeat` 为零且 `creationTimestamp` 非零 | `creationTimestamp + StartupGracePeriod` |
| 两者均为零 | 对象非法，返回永久错误并记录指标，不写状态 |

`heartbeat` 晚于本次 `now` 时，视为尚未超时，并按 `heartbeat + HeartbeatTimeout` 安排检查。Controller 不在首版推断时钟漂移，也不修改未来时间戳；应记录时钟偏移指标和限速日志，供运维排查 Runner 与控制面时钟同步。

`Registering` 和 `Booting` Runner 可能尚无首次心跳，使用创建时间和启动宽限期。`Idle` 或 `Running` 但心跳为零的数据也采用同一规则；若已经超过启动宽限期，则允许标记为 `Offline`，不能永久保持可调度。

### 5.2 截止时间安全计算

缓存判断、权威 GET 和 `WriteUnknown` 确认读取不得各自拼接时间逻辑，必须调用同一个纯函数：

```go
const maxHealthRequeueAfter = 24 * time.Hour

type DeadlineResult struct {
    Deadline     time.Time
    RequeueAfter time.Duration
    Expired      bool
    Basis        string // "heartbeat" 或 "creationTimestamp"
    FutureBasis  bool
}

func calculateHealthDeadline(
    runner *ebsv1.Runner,
    now time.Time,
    config Config,
) (DeadlineResult, error)
```

计算步骤固定为：

1. `runner == nil`、`now.IsZero()`、`HeartbeatTimeout <= 0` 或 `StartupGracePeriod <= 0` 属于调用方或启动配置错误；生产配置必须在 Controller 初始化时拒绝，函数仍返回错误以保护单元测试和内部调用。
2. `status.heartbeat` 非零时选择其为 `base`，使用 `HeartbeatTimeout`，`Basis="heartbeat"`；否则选择非零 `creationTimestamp`，使用 `StartupGracePeriod`，`Basis="creationTimestamp"`；两者都为零返回永久对象错误。
3. 时间计算统一去除单调时钟部分并转为 UTC：`base = base.UTC().Round(0)`、`now = now.UTC().Round(0)`。持久化 API 时间不应携带进程内单调时钟，不能混用不同来源的 monotonic component。
4. 执行 `deadline = base.Add(timeout)`。若 timeout 非正数、`deadline.Before(base)`，或者结果年份不在 RFC3339 可表达的 `[1, 9999]` 范围，视为时间溢出或非法时间并返回可分类错误，不能调用 `AddAfter`。调用方把对象时间错误转换为永久对象错误，并增加 `runner_controller_invalid_timestamp_total`。
5. 截止边界采用左闭语义：`now >= deadline` 即超时。实现必须写成 `expired := !now.Before(deadline)`，不能使用 `now.After(deadline)`，否则等于截止时间时会多等待一个周期。
6. 已超时时返回 `Expired=true`、`RequeueAfter=0`；不得返回负 duration。
7. 尚未超时时计算 `remaining := deadline.Sub(now)`。若在 `now < deadline` 的前提下得到 `remaining <= 0`，视为内部时间计算错误并返回错误。
8. `RequeueAfter = min(remaining, maxHealthRequeueAfter)`。截断只限制单次内存定时器长度，不修改真实 `Deadline`；24 小时后重新读取对象并按原始时间重新计算，不能因为截断而提前标记 Offline。
9. `base.After(now)` 时设置 `FutureBasis=true`，但仍按 `base + timeout` 计算；首版不把未来时间戳钳制为 now，也不直接判定 Offline。调用方在一次 Sync 中首次观察到该标记时增加一次 `runner_controller_future_heartbeat_total` 并输出限速日志，后续阶段再次观察到时不重复计数。

该函数不读取 clock、不访问 API、不修改 Runner，也不直接操作队列。一次 `Sync` 只取得一次 `now := clock.Now()`，所有阶段都把同一 now 传入；权威 GET 或结果未知确认耗费的网络时间不重新推进本周期的 now。下一次调谐会取得新的 now，因此不会永久延后超时。

函数错误分类固定为：Controller 初始化已经可以发现的 duration 配置错误导致启动失败；来自对象的零时间或溢出时间属于永久对象错误；nil Runner、零 now 或与函数约定矛盾的计算结果属于临时内部错误并报警。永久对象错误等待后续 Runner 更新事件重新激活，内部错误进入 BaseController 的快速及慢速退避。

### 5.3 缓存阶段

`Sync(key)` 首先读取缓存对象：

1. 对象不存在：成功结束；
2. `deletionTimestamp` 非空：成功结束；
3. `phase == Offline`：成功结束；
4. phase 为空或不属于 `Registering`、`Booting`、`Idle`、`Running`：返回永久错误；
5. 调用 `calculateHealthDeadline`；
6. `Expired=false`：返回函数给出的安全 `RequeueAfter`；
7. 已到期：进入权威确认阶段。

缓存只用于快速判断和安排时间，不能作为写 `Offline` 的最终依据。

### 5.4 权威确认阶段

到期后必须调用 `GetRunner`：

1. GET 返回 404：Runner 已被删除，成功结束；
2. GET 返回其他错误：按错误矩阵处理，不写状态；
3. 校验响应非 nil、name 与请求一致、UID 和 resourceVersion 非空；
4. 若权威对象 UID 与缓存 UID 不同，说明同名对象已重建，立即重新入队；
5. 若对象正在删除或已经 `Offline`，成功结束；
6. phase 为空或不属于可处理状态：返回永久错误，不写状态；
7. 使用同一个 `now` 和权威对象调用 `calculateHealthDeadline`；
8. `Expired=false` 说明心跳已经恢复，返回函数给出的安全 `RequeueAfter`；
9. 仍然到期才构造 `/status` 更新。

权威 GET 是必要步骤：Watch 缓存可能暂时落后，而 Runner 的新心跳可能已经成功写入 apiserver。

## 6. 并发与写入结果

### 6.1 与 Runner 心跳并发

Runner Controller 的 PUT 携带权威对象的 `resourceVersion`。若 Runner 在 GET 后写入了新心跳，Controller 的旧版本 PUT 应返回 Conflict；Controller 不覆盖新心跳，立即重新入队并重新判断。

仍存在不可消除的窗口：Runner 的新心跳可能在 Controller 的 `Offline` PUT 成功之后到达。该情况由 Runner agent 的状态写入自然恢复，不需要补偿事务。

为了避免旧 Runner 实例覆盖同名新对象，GET 响应 UID 必须有效，成功响应 UID 必须与请求 UID 相同。Runner 删除重建导致 resourceVersion 或 UID 变化时，以新对象为准。

### 6.2 成功响应校验

共享 API Client 首先执行通用成功响应校验：响应对象非 nil；name、namespace、UID 与请求一致；resourceVersion 非空。Runner Controller 随后执行资源专属校验，固定实现为：

```go
func validateOfflineStatusResponse(response, request *ebsv1.Runner) error {
    if response == nil || request == nil {
        return fmt.Errorf("runner status response and request are required")
    }
    if response.Name != request.Name ||
        response.Namespace != request.Namespace ||
        response.UID != request.UID ||
        response.ResourceVersion == "" {
        return fmt.Errorf("runner status response identity is invalid")
    }
    if !apiequality.Semantic.DeepEqual(response.Spec, request.Spec) {
        return fmt.Errorf("runner spec changed in status response")
    }
    if response.Status.Phase != "Offline" ||
        !apiequality.Semantic.DeepEqual(response.Status.Conditions, request.Status.Conditions) ||
        !apiequality.Semantic.DeepEqual(response.Status.Capacity, request.Status.Capacity) ||
        !apiequality.Semantic.DeepEqual(response.Status.Allocatable, request.Status.Allocatable) ||
        !apiequality.Semantic.DeepEqual(response.Status.Addresses, request.Status.Addresses) ||
        !apiequality.Semantic.DeepEqual(response.Status.Info, request.Status.Info) ||
        !response.Status.Heartbeat.Equal(&request.Status.Heartbeat) {
        return fmt.Errorf("runner status response differs outside phase")
    }
    return nil
}
```

其中 `request` 是基于权威 GET 对象 DeepCopy 后仅把 `Status.Phase` 改为 `Offline` 的实际请求对象。因此响应字段规则为：

| 字段 | 成功响应要求 |
|------|--------------|
| `metadata.name`、`metadata.namespace`、`metadata.uid` | 必须与请求完全一致；Runner namespace 应为空 |
| `metadata.resourceVersion` | 必须非空，允许由服务端更新，不要求与请求值相同 |
| `metadata.managedFields` 和其他服务端维护 metadata | 不参与 Controller 的资源专属语义比较，不能对整个 metadata 使用 `reflect.DeepEqual` |
| `spec`，包括 `instanceId`、type、arch、unschedulable、taints | 使用 `apiequality.Semantic.DeepEqual` 与请求完整比较，任何变化均为异常 |
| `status.phase` | 必须为 `Offline` |
| `status.conditions` | 必须与请求语义相同 |
| `status.capacity`、`status.allocatable` | 必须与请求语义相同 |
| `status.addresses`、`status.info` | 必须与请求语义相同 |
| `status.heartbeat` | 必须使用 `metav1.Time.Equal` 与请求比较相同 |

不得只校验 `phase`，也不得使用整个 Runner 的 `reflect.DeepEqual`。`apiequality.Semantic.DeepEqual` 是上述字段的固定比较方式，不自行把 nil map/slice 与空 map/slice 归一化；apiserver 若意外改写这些字段，必须进入确认流程。

HTTP 2xx 并不代表 Controller 可以无条件结束成功。共享 Client 通用校验失败时直接返回 `WriteUnknown`；资源专属校验失败时，Runner Controller 将该异常成功响应按 `WriteUnknown` 交给 6.4 节的确认读取流程。两种情况都不能作为 `WriteRejected` 直接重放原 PUT，也不能计入 Offline 成功指标。

### 6.3 写入结果矩阵

| 结果 | 处理 |
|------|------|
| 2xx 且响应校验通过 | 成功结束，增加 Offline 计数 |
| 404 | 对象已删除，成功结束 |
| 409 / 412 | 返回 `Requeue=true`，读取最新对象 |
| 408 / 429 | 返回临时错误，由工作队列限速重试 |
| 401 / 403 | 永久错误，不对该 key 自动重试；日志和指标必须可见 |
| 其他 4xx | 永久错误 |
| 5xx | 临时错误，限速重试 |
| 请求明确未发送且原因可重试 | 临时错误 |
| 请求明确未发送且配置或编码错误 | 永久错误 |
| 请求已发送但响应超时、连接中断或响应异常 | 进入结果未知确认 |

### 6.4 结果未知确认

发生 `WriteUnknown` 时：

1. 若 context 已取消，直接返回 context 错误，不发起额外请求；
2. 调用 `GetRunner`；
3. 404：成功结束；
4. GET 失败：返回该读取错误；
5. UID 已变化：旧写入不再相关，返回 `Requeue=true`；
6. `phase == Offline`：认为目标状态已经生效，成功结束；
7. phase 为空或不属于可处理状态：返回永久错误，不写状态；
8. 对象处于可处理状态且 `calculateHealthDeadline` 返回 `Expired=false`：返回函数给出的安全 `RequeueAfter`；
9. 对象处于可处理状态且仍超时：返回 `Requeue=true`，重新执行完整流程。

结果未知确认不得直接重放原 PUT，否则可能在未读取最新心跳的情况下重复写入。

## 7. 事件与队列语义

Runner 事件处理器只更新队列，不执行 API 写入：

- `Add`：所有 Runner 入队，用于安排首次心跳或超时检查；
- `Update`：仅当下述 `shouldEnqueueRunnerUpdate` 返回 true 时，将新对象的 name 入队；
- `Delete`：不需要入队。队列中已有 key 后续读取不到对象会自然结束。

入队条件固定实现为：

```go
func shouldEnqueueRunnerUpdate(oldRunner, newRunner *ebsv1.Runner) bool {
    return oldRunner.UID != newRunner.UID ||
        oldRunner.ResourceVersion == newRunner.ResourceVersion ||
        deletionTimestampChanged(oldRunner.DeletionTimestamp, newRunner.DeletionTimestamp) ||
        oldRunner.Status.Phase != newRunner.Status.Phase ||
        !oldRunner.Status.Heartbeat.Equal(&newRunner.Status.Heartbeat)
}

func deletionTimestampChanged(oldTime, newTime *metav1.Time) bool {
    if oldTime == nil || newTime == nil {
        return oldTime != newTime
    }
    return !oldTime.Equal(newTime)
}
```

各条件语义如下：

- UID 变化表示同名 Runner 已被删除重建，必须立即按新对象重新计算；
- `old.ResourceVersion == new.ResourceVersion` 表示 informer resync，即使业务字段没有变化也要重新计算截止时间；
- `deletionTimestamp` 的 nil/非 nil 转换或时间值变化必须入队，以便停止处理正在删除的对象；
- phase 或 heartbeat 变化直接影响健康状态或截止时间，必须入队；
- `Heartbeat.Equal` 同时正确处理零值和时间语义，不能比较指针地址或格式化字符串。

若 resourceVersion 正常变化，但 UID、deletionTimestamp、phase 和 heartbeat 均未变化，则不入队。capacity、allocatable、conditions、addresses、info、spec、labels、annotations 或其他 metadata 的单独变化不影响 Runner 健康截止时间，因此不作为 Update 入队条件；已有 `AddAfter` 和 BaseController 慢速重入不受影响。

Update Handler 收到 nil、类型不符、`oldRunner.Name != newRunner.Name` 或新对象 name 为空时，记录 `UnexpectedRunnerUpdate` 内部错误和指标，不构造不可靠 key，也不尝试 API 写入。正常路径直接使用 `newRunner.Name` 作为集群级 key，不调用 namespace key 生成函数。

`BaseController` 的 dirty/processing 语义负责合并相同 Runner key。多个 worker 可以并发处理不同 Runner，同一 key 不会同时执行两个 `Sync`。Controller 不维护独立定时扫描 goroutine，也不直接调用工作队列的 `Done`、`Forget` 或 `AddRateLimited`。

临时错误先由 BaseController 执行有上限的快速限速重试，耗尽后进入框架统一的慢速指数退避并持续 `AddAfter`，直到成功、对象删除或永久错误。Runner Controller 不维护自己的失败次数或退避表。该机制是健康收敛的必要保障：已经失联的 Runner 不再产生心跳事件，不能在快速重试耗尽后仅等待下一次 Watch 事件。普通 Runner 事件可以立即唤醒慢速 key，但只有一次成功调谐才清除其慢速失败历史。

对象更新会重新 `Add` 相同 key。若此前存在较晚的延迟条目，立即事件必须使该 key尽快重新调谐；实现应使用 client-go delaying queue 的标准语义，不能自建一个无法被新事件提前唤醒的定时器表。

## 8. 错误分类

读取错误矩阵：

| 操作和结果 | 分类 |
|------------|------|
| cache 未同步 | 临时错误 |
| cache 对象类型错误 | 临时内部错误并报警 |
| GET 404 | 对象不存在，成功结束 |
| GET 408 / 429 / 5xx / 网络临时错误 | 临时错误 |
| GET 401 / 403 | 永久错误 |
| GET 其他 4xx | 永久错误 |
| GET 2xx 但对象身份或必要 metadata 非法 | 临时内部错误并报警，不写状态 |
| Runner phase 为空或不属于可处理状态 | 永久对象错误，不写状态 |
| 对象缺少 heartbeat 且 creationTimestamp 也为空 | 永久对象错误，不写状态 |

永久错误只终结当前事件产生的处理周期；后续 Runner 更新事件仍可重新入队并再次调谐。认证授权错误同时影响全部 Runner，进程健康检查不自动转为失败，但必须通过高可见度日志和指标报警。

临时错误不会因为超过 `--controller-max-retries` 而丢弃：快速阶段结束后由 BaseController 使用 `--controller-slow-retry-initial-delay`、`--controller-slow-retry-max-delay` 和统一抖动配置持续慢速重入。GET、状态写入和 `WriteUnknown` 确认读取中的临时错误均适用同一规则。

## 9. 重启与可用性

Runner Controller 不保存进程内的业务健康观察状态。BaseController 可以保存仅用于临时错误重试的慢速退避计数，该计数不参与 Runner 超时判定，进程重启后丢失也不改变业务截止时间。所有健康截止时间都由持久化对象计算，因此：

- 重启不会重新开始心跳超时；
- 初始 List 中已经超时的 Runner 会立即进入权威确认；
- 尚未超时的 Runner 会重新安排剩余时间；
- 多副本同时运行时依赖 resourceVersion 乐观并发，最多一个写入成功。

首版仍建议运行单个 active controller-manager 实例，因为框架尚未设计 leader election。多副本不会破坏 Runner 状态，但会产生重复 GET、Conflict、指标和日志；这不等价于完整高可用方案。

Runner Controller 不需要自定义 `HealthChecker`。Manager 的默认 ping checker 足够表示 worker 生命周期；Runner 是否存在、是否 Offline 以及 API 临时失败属于业务状态，不应令进程 `/healthz` 失败。共享 Source 的同步和陈旧状态继续由 Manager `/readyz` 负责。

## 10. 权限

最小权限：

```text
runners:        get, list, watch
runners/status: update
```

不需要 Runner create、普通对象 update/patch/delete，不需要 Job 或其他业务资源权限。Controller Manager 直接访问 ebs-apiserver，其服务身份必须与 Runner agent 身份隔离，不能复用某个 Runner 的 token。

## 11. 日志与指标

关键日志统一包含：

```text
controller=runner key=<runner> uid=<uid> resourceVersion=<rv> reason=<reason>
```

建议指标：

```text
runner_controller_heartbeat_checks_total
runner_controller_heartbeat_timeouts_total
runner_controller_offline_updates_total
runner_controller_status_update_conflicts_total
runner_controller_status_update_unknown_total
runner_controller_future_heartbeat_total
runner_controller_invalid_timestamp_total
```

不把 Runner name 作为指标 label，避免高基数。具体对象信息只写结构化日志。

## 12. 测试要求

单元测试至少覆盖：

- 有心跳 Runner 在截止时间前返回准确的 `RequeueAfter`；
- `now == deadline` 时立即判定超时，未返回负数或额外等待；
- 极远未来时间按 24 小时截断单次 `RequeueAfter`，复查时仍使用原始 deadline，不提前 Offline；
- deadline 加法溢出或超出 RFC3339 年份范围时返回永久对象错误且不入延迟队列；
- UTC 规范化和去除 monotonic component 后，缓存、权威确认和结果未知确认得到一致结论；
- 零心跳 Runner 使用 creationTimestamp 和启动宽限期；
- heartbeat 和 creationTimestamp 均缺失时不写状态；
- phase 为空或未知时返回永久错误且不写状态；
- Offline 或正在删除的 Runner快速结束；
- 缓存显示超时、权威 GET 已有新心跳时不写 Offline；
- 缓存与权威对象 UID 不同时重新调谐新对象；
- 权威对象仍超时时只修改 phase，完整保留其他字段；
- Controller PUT 前 Runner 心跳更新导致 Conflict 时重新读取；
- 成功响应允许 resourceVersion 和服务端 metadata 变化，但 spec 或 phase 之外任一 status 字段变化时进入 `WriteUnknown` 确认；
- `WriteUnknown` 确认已 Offline、已恢复、仍超时和 GET 失败四类结果；
- 401/403、429、5xx、网络错误和异常成功响应的分类；
- 快速重试耗尽后进入慢速退避，持续故障时延迟指数增长并封顶，恢复成功后清除慢速状态；
- Add、UID 替换、删除时间、phase、heartbeat、相同 resourceVersion resync，以及仅无关字段变化时不入队的行为；
- Update 事件对象类型错误、nil、name 变化或 name 为空时只记录内部错误且不入队；Delete 事件不入队；
- FakeClock 推进能够触发到期调谐，测试不使用真实 sleep；
- `go test -race` 下多 Runner 并发和同 key 重复事件无数据竞争。

集成测试至少覆盖：

1. Runner 持续心跳时不会被标记 Offline；
2. 停止 Runner agent 后，超过心跳超时会进入 Offline；
3. Offline 事件使 Scheduler 不再选择该 Runner；
4. 已绑定 Running Job随后由 Job Controller 按自身宽限期进入 Failed；
5. 在 Runner Controller 写入前恢复心跳时，resourceVersion 冲突或权威 GET 能阻止错误覆盖；
6. Controller Manager 重启后，已超时 Runner 不会重新获得完整超时窗口；
7. Runner agent恢复并成功上报新心跳后，可以把 Offline 状态恢复为有效运行状态。

## 13. 实施顺序

1. 为 Controller Manager client 增加 Runner GET 和 Runner `/status` 更新适配，并复用现有写入结果分类。
2. 实现纯健康判定函数和响应校验，以 FakeClock 完成边界测试。
3. 实现事件处理、延迟重入、权威确认和结果未知确认。
4. 增加 `RunnerControllerOptions`，由组装层把 Runner 时间参数和全局 `ManagerOptions.ControllerMaxRetries` 转换为 `runnercontroller.Config`，并在 initializer map 中注册 `runner`。
5. 增加指标、README 参数说明和 dev Compose 集成验证。

完成上述内容后，Runner Controller 可以为 Job Controller 提供稳定的 `Offline` 状态生产者，形成“心跳失联判定”和“已绑定 Job 失败收敛”相互独立的两级控制流程。
