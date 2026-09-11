# 0006：Failure、Panic 与 Shutdown

状态：Accepted
最后更新：2026-09-11
适用阶段：M1–M2
依赖：[核心执行模型](0001-core-execution-model.md) · [Sink Handoff](0005-sink-handoff-and-completion.md)

本文是 Operator Work Failure Policy、暂停、FailJob、panic 分类、错误因果与有界关闭的权威契约。

## 1. 失败、暂停与恢复

### 1.1 错误不直接等于退出

Job 在 `JobBuilder` 阶段为 Operator work 选择 FailJob 或 Retry。该策略不覆盖 Source、Sink
或 Runtime 内部错误。M2 不提供按 error 类型
分类的用户函数，也不提供自定义 BackoffFunc；这些扩展只有在后续真实需求证明内置策略不足时
才重新讨论。未配置 Retry 的默认行为仍是 FailJob，升级 Runtime 不得使既有 Job 自动重复执行。

Runtime 不提供 Skip/Discard record 终态，也不在每个 Transformation 上提供 `OnError`。
用户函数负责在业务逻辑附近识别可忽略的业务错误，并把它收敛为正常计算结果：Filter 可以
返回不保留，FlatMap 可以返回零输出，自定义 Operator 可以不 Emit 并返回 nil。Map 的成功
语义保持严格一进一出；若某项计算可能在业务错误时产生零输出，应使用 FlatMap 或自定义
Operator 表达。只有未被用户吸收的 error 才进入 Job 级 Retry/FailJob 策略。

正常零输出属于 Success，允许输入 completion 和连续 safe position 推进；它不被 Runtime
伪装成独立的 Discarded 终态。将来需要保留坏数据时，应作为显式业务输出、Side Output、
分支或专用 Sink 设计，而不是通过通用失败策略静默丢弃。

重试可以在时间上持续，但数据、goroutine、队列、timer 和并发请求始终有界，并且 Runtime 始终响应宿主取消。

### 1.2 Retry admission fence

任意 work 的 Failure Policy 决定 Retry 时，Runtime 立即暂停整个 Source 的新业务 admission，
而不是按 split、partition 或 position 选择性暂停。Retry 是 Runtime work 失败语义，不能假设
Source 提供 position，也不能要求 Source driver 在读取前知道下一条记录属于哪个 partition。

暂停只关闭新业务输入：

- Runtime 不再调用 Reader 获取新业务记录，Connector 不扩大业务预取；
- Kafka 等 Connector 仍维持 poll、heartbeat、session 和 control event 等外部协议活动；
- 已经越过 admission 线性化点的所有 work 继续竞争 execution lane、完成 Chain 并尽量交给
  Sink，不因另一个 work Retry 而取消；
- retry work 在 backoff 期间不占 execution lane，但保留原 work、输入、completion responsibility
  和 permit；空闲 lane 不从 Source 补入新 work；
- 不同 retry work 可以在 Parallelism 限制内并发执行，同一 work 任意时刻最多一个 active attempt；
- 所有 active retry blocker 都成功消失后，Runtime 才原子恢复 Source admission；有限 Retry
  耗尽触发 FailJob，无限 Retry 可以使 Source admission 无限期暂停，直到成功或外部取消。

Failure Policy 判定 Retry 后，必须先登记 active failure、把 blocker 从零变为一并安装 admission
fence，之后才能释放 lane 或安排 backoff。已经跨过 admission 线性化点的并发读取属于已接纳 work；
尚未跨过的读取必须被 fence 阻止。恢复时，active failure set 更新与 gate 状态属于同一个 Runtime
协调状态机；最后一个 blocker 成功与另一个 blocker 失败并发时，不得短暂错误打开 gate，也不得
丢失恢复通知。

该策略把问题影响限制在失败发生时已经接纳的有限 work 内，避免 backoff 期间持续扩大 commit gap、
潜在重复写入窗口和 completion tracking 范围；代价是任意 work Retry 都会暂停该 Run 的全部新业务
输入，即使 Source 内部包含多个互相独立的 split。

### 1.3 Operator work attempt Retry

Operator 返回 error 或用户 `Process`/回调 panic 包装成 `PanicError` 后，统一遵守 Job 的
Retry/FailJob 配置。context 取消与 shutdown 不 Retry；yaspe 内部 panic 强制 FailJob；Source
读取错误没有对应 work，不进入 work Retry。Sink effect 的恢复属于 Sink Connector 内部；
Runtime 不以本策略重试 Sink effect。

Retry 的最小恢复单位是整个 work attempt：

- 任一 Operator 失败后，丢弃当前 attempt 的全部暂存输出，不向 Sink 交接部分结果；
- 新 attempt 使用新的 attempt identity，从原始输入 Record 重新执行完整 Operator Chain；work
  identity、Source position 和 generation 保持不变；
- retry attempt 可以由任意 execution lane 执行，不保证 lane affinity，也不保证回到同一个
  Operator 包装实例；backoff 到期后它与已接纳 work 公平竞争 lane；
- 原始输入及其可达引用数据由 Runtime 保留到 work 终结。Runtime 不复制任意泛型输入；用户
  Operator 必须把输入视为只读，原地修改导致后续 attempt 输入变化时，结果不受保证；
- attempt 之间的次数和 elapsed time 连续累计，成功或最终失败时才销毁该 work 的 retry 状态。

M2 不自动重新 Open Source/Sink 或重启整个 Job。work Retry 耗尽后当前 Run FailJob 并返回 error；
是否创建新 Runtime 再次运行 Job 由宿主决定。

### 1.4 Retry budget 与 backoff

Retry budget 支持两种明确模式：

- finite：至少配置正数 `MaxRetries` 或正数 `MaxElapsedTime`，也可以同时配置，两者任一先耗尽
  即停止；`MaxRetries` 不包含首次 attempt；
- unlimited：显式无限 Retry，不与次数或时间上限混用，也不通过零值暗示无限。

有限预算耗尽后唯一动作是 FailJob。正常 Stop、跳过 work 和 Dead Letter 都会引入 M2 未提供的
终态或业务输出能力，因此不作为 exhaustion 配置。`MaxElapsedTime` 从首次 attempt 开始累计，
包含 attempt 执行与 backoff；deadline 到期时取消当前 attempt context、禁止启动新 attempt，
等待用户代码协作返回后 FailJob。Runtime 无法安全强杀忽略 context 的用户 goroutine，也不得在
它仍访问 Collector/Record 时假装 attempt 已终结。

M2 内置 no-backoff、fixed 和 exponential 三种策略，不提供用户 BackoffFunc。指数退避第 `n`
次 Retry 的 nominal delay 为：

```text
min(Initial * Multiplier^(n-1), Max)
```

比例 jitter `j` 的范围是 `[0, 1]`，实际 delay 为：

```text
clamp(nominal * (1 + uniform(-j, j)), 0, Max)
```

第一次 Retry 同样等待 `Initial`；backoff 从上一次 attempt 返回后开始。Runtime 使用自身可取消
clock/timer 和可注入随机源，测试使用确定序列。如果 retry deadline 早于计算结果，则只等待到
deadline 并耗尽，不再启动 attempt。具体 timer heap/queue 属于私有实现，但 timer、goroutine 和
调度状态必须有界。

`Build` 必须拒绝 finite 无有效上限、非正预算、unlimited 与有限预算混用、负 fixed delay、
非法 Initial/Multiplier/Max/Jitter 等配置；公开类型名和构造函数可由实现细化，不得改变上述语义。

### 1.5 多 work failure collection

第一个 work 进入 Retry 后，已接纳的其他 work 仍可能失败。Runtime 为每个 active failed work
保存一条 first-failure entry；同一 work 后续 attempt 不替换第一次 error，只更新 attempts、
last error 和 elapsed 等有界摘要。entry 数量受 `MaxInFlightWorks` 限制。

work Retry 成功后从 active set 移除，并可通过日志/observer 发布恢复事实；Runtime 不永久保存
整个 Run 中所有已恢复错误。任一 work 耗尽触发 FailJob 时，对 active set 生成不可变快照，区分：

- primary trigger：真正耗尽并使 Job 此刻停止的 work 及其第一次 error；
- failure collection：当时所有仍在 Retry 的 work 及其第一次 error 和摘要。

单个 work 无论经历多少 attempt，其第一次 error 始终是该 work 的根因。多错误集合采用
[§1.7 的公开 RunError](#17-公开-runerror)，通过 `Unwrap() []error` 和查询接口暴露；内部从
第一版开始保留上述因果信息和有界摘要。

### 1.6 Sink 最终失败

Sink 以 `SinkAccepted, nil` 接管 items 后自行决定是否以及如何 Retry。Runtime 不解析 Sink
error、不提供 retryable/permanent classifier、不重新提交 item，也不重新执行已经成功的
Operator Chain。Sink 只在内部恢复结束后通过 reporter 报告最终的 `SinkSucceeded`、
`SinkNotApplied` 或 `SinkUnknown`；完整 outcome 契约见
[Sink Design §1.4](0005-sink-handoff-and-completion.md#14-completion-结果)。

Runtime 收到一个 work 的第一个最终 `SinkNotApplied` 或 `SinkUnknown` 时立即触发 FailJob，
不等待同一 work 的其他 item 全部完成后才停止 admission。该 error 是此次停止的 primary
trigger；随后从同一或其他已接管 work 到达的最终失败作为 secondary errors 保留。已经接管的
其余 item 仍按 §2 的统一 shutdown deadline 有限 drain，可信 success 继续形成 completion
事实，但失败 work 不形成 Success，也不推进其 Source position。

这条固定 FailJob 路径不受 Job 的 Operator Work Failure Policy 影响。未来若引入 checkpoint
驱动的自动 Run/region 恢复或通用异步 Sink 基础设施，必须另行设计恢复单位、重复边界和状态
恢复协议；M2 不把这些能力伪装成当前 work Retry。

### 1.7 公开 RunError

`Runtime.Run` 的所有非正常结束统一返回 `*RunError`，不根据底层错误数量在原始 error 与聚合
error 之间切换。Operator 失败、Sink 最终失败、Source read error、startup/Open error、用户或
内部 panic、宿主 context 取消、shutdown timeout 和 Close error 都遵守这一入口；正常有界
Source 完成返回 nil。`Build` 等定义期校验不属于 Run，仍直接返回其配置错误。

Source 经 `SourceContext.ReportFailure` 独立报告的最终错误也进入此因果模型；其输入错误
是故障原因，返回错误只是报告接收状态，不能替换原因。Reader 与独立报告对同一最终失败
不得重复触发流程，完整规则见 [Source Design §1.1.2](0003-source-reader-and-admission.md#112-独立的最终失败报告)。
Connector 在最终报告前执行已接受的有限会话恢复，不属于 Operator Retry；报告最终失败
后不能在当前 Run 中复活 Source。

`RunError` 是 `Run` 返回前冻结的不可变快照，具体类型与查询方法见
[runtime_error.go](../../runtime_error.go)。当前 FailJob 路径已实现 primary 与 secondary 因果；
由于 Operator Retry 尚未实现，实际运行返回的 ActiveFailures 为空，不能将类型定义或快照
查询测试当作 active failure collection 的运行验证。

`Primary()` 恰好返回一个非 nil error，表示第一个使 Run 开始停止或最终不能成功返回的原因：

- 首个触发 FailJob 的 Operator、Sink、Source 或 Runtime error 成为 primary；
- 宿主取消若先触发停止，`context.Canceled` 或 `context.DeadlineExceeded` 成为 primary；
- 业务处理正常完成后，首个使 Run 失败的 Close error 或 shutdown timeout 成为 primary；
- 多个终止事件并发到达时，以 Runtime 串行协调状态机接受的第一个事件为准；
- 停止开始后出现的更严重错误也不得按严重程度改写已经成立的 primary。

`ActiveFailures()` 只保存停止线性化时仍处于 Operator Retry 的其他 work，不重复包含已经成为
primary trigger 的 work。每项只公开首次错误、最后错误、attempt 数和 elapsed time；不公开
`WorkID`、attempt identity、Source position 或 generation，因为调用方不能用这些内部身份重新
提交、确认或跨 Run 关联 work。返回 slice 是副本，调用方不能修改冻结快照。

`Secondary()` 保存停止开始后形成的其他错误，例如其余 Sink item 的最终失败、Close error、
shutdown timeout 或 internal panic。返回 slice 同样是副本。primary、active failure 与 secondary
三类不得混为一个无角色集合，因为它们分别表达停止原因、停止前已经存在的并发故障和停止过程
中的附加故障。

`Unwrap() []error` 返回新 slice，使标准 `errors.Is/As` 可以遍历 primary、每个 active failure
的 first/last error 和全部 secondary error；nil 不进入结果，同一 `WorkFailure` 的 first 与 last
是同一 error 时只放一次。unwrap 顺序不表达 primary，调用方必须通过 `Primary()` 查询因果角色。
`Error()` 只格式化 primary 与 active/secondary 数量摘要，不拼接所有底层错误，避免并发失败使
错误字符串无界膨胀。迟到 callback 在 `RunError` 冻结后只能命中失效 fence，不能修改快照。

## 2. FailJob、取消与关闭

### 2.1 FailJob

FailJob 是用户策略认为当前 Job 不应继续恢复的最终动作。它终止当前 Runtime 并使 `Run` 返回根因错误，但不杀死嵌入 yaspe 的宿主进程。

普通 FailJob 按 work 当时已经越过的责任边界停止，而不根据 Source 是否带 position
选择另一套路径：

| work 阶段 | FailJob 动作 |
|---|---|
| 只有 reservation、Reader 尚未返回 ready | 释放空 reservation，不创建 work |
| 已接管输入但仍在 queued | 不启动 Operator，取消并终结该 work |
| Operator 已 started | 取消 attempt context；不为填补 position gap 继续执行 |
| 已形成 terminal outputs、尚未进入 Sink | 丢弃本组 outputs，不开始新的 Sink handoff |
| 已经进入同步 `Accept` 或已由 Sink 接管 | 不撤回，在 shutdown deadline 内等待明确结果 |
| 已 completed | 保留已经成立的结果 |

取消是协作式的：Runtime 不能强制终止不响应 context 的用户代码或卡住的 Connector。
FailJob 使用一个统一、可配置的 shutdown deadline，并受宿主更早 deadline 约束；Source、
Operator、Sink drain 和 Close 都共享这份总预算，而不是各自重新获得完整期限。期限内
可信的 Sink success 仍可更新 completion、safe position 并尽快提交；到期仍未知的操作
不得标记成功。超时后 `Run` 返回可识别的 `ShutdownTimeoutError`，其中报告尚未退出的
阶段或组件，且不得把相应 goroutine 伪装成已经终止。所有迟到路径必须被 fence，不能在
`Run` 返回后修改 Runtime 状态、推进 completion/position 或发起新的外部 effect。

position tracker 只根据最终已经成立的 completion 事实计算安全位置，不反过来驱动普通
FailJob 为填补 position gap 而 drain Operator。未提交输入由可重放 Source 在后续执行中
重放。Kafka revoke 为减少 commit gap 而允许 started work 有限收敛，仍是独立的
ownership 收尾协议，不改变普通 FailJob 规则。

正常停止与 FailJob 共用同一套有界关闭协调机制，但收敛范围不同：

- 正常 EOF 或显式优雅停止在期限内完成应保留的输入与输出；stdio 的显式停止还需交付
  Source 已缓冲的记录、处理 queued work，详细顺序见
  [stdio Design §4](0010-stdio-and-graceful-stop.md#4-优雅停止顺序)；
- 普通 FailJob 不启动 queued work，取消尚未把 terminal output 交给 Sink 的 started work，
  不再制造新的外部 effect；
- 两者都在期限内 drain 已由 Sink 接管的操作，并允许可信 completion 推进和
  提交 safe position；
- FailJob 的 `Run` 结果始终保留第一个触发终止的 error 作为 primary/root cause；停止期间
  出现的取消、Close、超时或其他错误作为 secondary errors 附加，聚合结果必须允许调用者
  通过 `errors.Is/As` 识别 primary 与每个 secondary error。

显式优雅停止是独立于宿主 context 取消的行为，决定见
[ADR-0009](../decisions/0009-separate-graceful-stop-from-cancellation.md)。当前 Runtime 已实现
正常 EOF drain 和 context 取消后的失败收尾，尚未实现外部请求“停止读取后继续处理”的入口。
首次 Ctrl+C 的信号接线不得直接复用 Run context 的取消来声称支持优雅停止。

### 2.2 panic 边界与分类

Runtime 只在它主动调用用户代码的最外层受控入口设置窄 recover boundary。M1 至少
包括 Operator factory 和 `Operator.Process`；内置 Operator 在 `Process` 内调用的
transform/predicate 由外层 `Process` 边界覆盖，不重复嵌套 recover。M2 Failure Policy 只由
内置值策略构成，不调用 error classifier 或 BackoffFunc 等用户回调。

`Operator.Process` 及其用户回调 panic 时：

- Runtime 捕获 panic value 和当次 stack，将其包装为可识别的 `PanicError`；
- `PanicError` 只描述失败事实，与其他未被吸收的 error 一样进入 Job 级 Failure Policy，
  由策略选择 Retry 或 FailJob；
- 每次 panic 的 value 与 stack 都保留为该 attempt 的诊断；
- Operator factory panic 发生在可执行实例建立前，作为启动失败直接返回；内置 Failure Policy
  若 panic 属于 yaspe 内部缺陷，按 `InternalPanicError` 强制 FailJob；
- 用户代码 panic 不穿透 Runtime 并终止嵌入 yaspe 的宿主进程。

yaspe 内部 panic 表示本不应发生的引擎缺陷。Runtime 监督边界可 recover 以便诊断和
有界收尾，但必须：

- 捕获原始 panic value 和 stack，形成 `InternalPanicError`；
- 强制 FailJob，不进入 Failure Policy、不 Retry、不恢复 Worker 继续处理；
- 立即停止 admission 和业务处理，仅执行受 deadline 限制的资源收尾；
- 不再依据 panic 后的 completion 状态推进或提交 position；
- 如果该 panic 首先触发 FailJob，`InternalPanicError` 是 primary error；如果它发生在已有
  FailJob 的停止过程中，原触发 error 仍是 primary，`InternalPanicError` 连同完整 stack
  作为高严重度 secondary error，不因严重程度改写因果顺序；
- `Run` 返回的聚合错误可识别该 `InternalPanicError`。这类情况必须被定位和修复，recover
  不是继续运行的容错机制。

### 2.3 context 与阻塞点

宿主取消必须最终解除或终结 Runtime 管理的所有等待路径，包括：

- Connector 内部阻塞 I/O；
- Reader 可用性通知；
- permit 和队列容量；
- terminal output 容量；
- Sink 接收容量和 completion 等待；
- retry timer；
- graceful shutdown drain。

Runtime 必须保证自身可控的等待都响应取消并回收。若用户代码或 Connector 违反取消契约，
shutdown deadline 防止 `Run` 永久卡住；此时允许仍无法强制终止的 goroutine 存活，但必须
返回 `ShutdownTimeoutError`、报告泄漏位置并用终态 fence 隔离其迟到动作。实现和测试不得
把这种结果报告为正常、完整回收。


## 3. 实现与验证证据

FailJob、停止预算与最终冻结见 [Runtime](../../runtime.go)，组件启动回滚和逆序关闭见
[执行流程](../../runtime_execute.go)，panic 边界见 [类型适配](../../runtime_adapter.go)。
[核心测试](../../runtime_test.go) 和 [控制测试](../../runtime_control_test.go) 覆盖取消、关闭错误、
Source 根因去重、同步 Sink 多错误、用户/内部 panic、关闭期限和迟到事件隔离。

Operator Retry 和异步 Sink 在途恢复仍待实现。超时测试刻意保留不响应取消的组件，断言
Run 返回超时且冻结结果不变，再由测试释放组件；不声称 Go 可以强制终止任意用户 goroutine。
