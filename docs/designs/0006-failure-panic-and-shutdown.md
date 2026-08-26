# 0006：Failure、Panic 与 Shutdown

状态：Accepted（M1 FailJob 条款已定；M2 Retry 细节仍待收敛）
最后更新：2026-08-26
适用阶段：M1–M2
依赖：[核心执行模型](0001-core-execution-model.md) · [Sink Handoff](0005-sink-handoff-and-completion.md)

本文是 Failure Policy、暂停、FailJob、panic 分类、错误因果与有界关闭的权威契约。

## 1. 失败、暂停与恢复

### 1.1 错误不直接等于退出

用户失败策略可以根据错误、阶段、历史尝试、当前时段和业务信息决定等待、重试或最终 FailJob。

Runtime 不提供 Skip/Discard record 终态，也不在每个 Transformation 上提供 `OnError`。
用户函数负责在业务逻辑附近识别可忽略的业务错误，并把它收敛为正常计算结果：Filter 可以
返回不保留，FlatMap 可以返回零输出，自定义 Operator 可以不 Emit 并返回 nil。Map 的成功
语义保持严格一进一出；若某项计算可能在业务错误时产生零输出，应使用 FlatMap 或自定义
Operator 表达。只有未被用户吸收的 error 才进入 Job 级 Retry/FailJob 策略。

正常零输出属于 Success，允许输入 completion 和连续 safe position 推进；它不被 Runtime
伪装成独立的 Discarded 终态。将来需要保留坏数据时，应作为显式业务输出、Side Output、
分支或专用 Sink 设计，而不是通过通用失败策略静默丢弃。

重试可以在时间上持续，但数据、goroutine、队列、timer 和并发请求始终有界，并且 Runtime 始终响应宿主取消。

### 1.2 暂停

一条记录触发暂停后：

- Source 不再向 Runtime 交接新记录；
- Connector 不扩大业务预取；
- Kafka session/control 仍继续；
- 已接受但尚未开始的 work 保留输入和 permit，不分配给 Worker；
- 已开始的 Chain 可以继续收敛到 terminal boundary；
- 同一 split 中位于未解决失败之后的新 Sink effect 暂缓；
- 位于失败之前、能够填补连续空洞的 work 可以继续进入 Sink；
- 不同 split 按各自连续完成进度判断；
- 已被 Sink 接受的操作不撤回，继续等待明确结果；
- 暂停不冻结 completion、safe position 计算和安全提交。

同一暂停期间的多个失败进入一次 Job 级恢复过程，但每条失败保留独立错误、记录上下文和恢复状态。统一协调重试并发、共享依赖探测、日志和报警。

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

- 正常停止在期限内允许已开始的 work 完成 Chain 并把 terminal output 交给 Sink；
- 普通 FailJob 不启动 queued work，取消尚未把 terminal output 交给 Sink 的 started work，
  不再制造新的外部 effect；
- 两者都在期限内 drain 已由 Sink 接管的操作，并允许可信 completion 推进和
  提交 safe position；
- FailJob 的 `Run` 结果始终保留第一个触发终止的 error 作为 primary/root cause；停止期间
  出现的取消、Close、超时或其他错误作为 secondary errors 附加，聚合结果必须允许调用者
  通过 `errors.Is/As` 识别 primary 与每个 secondary error。

### 2.2 panic 边界与分类

Runtime 只在它主动调用用户代码的最外层受控入口设置窄 recover boundary。M1 至少
包括 Operator factory、`Operator.Process` 和 Failure Policy；内置 Operator 在 `Process` 内调用的
transform/predicate 由外层 `Process` 边界覆盖，不重复嵌套 recover。

`Operator.Process` 及其用户回调 panic 时：

- Runtime 捕获 panic value 和当次 stack，将其包装为可识别的 `PanicError`；
- `PanicError` 只描述失败事实，与其他未被吸收的 error 一样进入 Job 级 Failure Policy，
  由策略选择 Retry 或 FailJob；
- 每次 panic 的 value 与 stack 都保留为该 attempt 的诊断；
- Operator factory panic 发生在可执行实例建立前，作为启动失败直接返回；Failure Policy 自身
  panic 不能再递归询问同一策略，直接触发 FailJob；
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

