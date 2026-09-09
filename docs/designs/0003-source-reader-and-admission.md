# 0003：Source Reader、Admission 与 Memory Source

状态：Accepted
最后更新：2026-09-09
适用阶段：M1–M2
依赖：[核心执行模型](0001-core-execution-model.md) · [ADR-0007](../decisions/0007-layered-source-prefetch-budgets.md) · [ADR-0006](../decisions/0006-source-failure-reporting-and-session-recovery.md)

本文是 Runtime-facing 非阻塞 Reader、可用性通知、完整 reservation、ownership 交接与 M1 Memory Source 的权威契约。

## 1. Source 物理模型与 Runtime Reader

### 1.1 外部 pull/push 中立

yaspe 不要求所有外部系统采用统一的物理 pull、push、callback 或订阅模型：

- Kafka Connector 可以在内部 poll broker；
- 文件 Connector 可以执行阻塞读取；
- callback/subscription Source 可以接收外部推送；
- 测试 Source 可以直接提供内存记录。

统一的是 Connector 面向 Runtime 的有界责任交接，不是外部系统的物理读取方式。业务 Operator 不感知 Source 类型、Kafka poll API、partition consumer 或 callback 对象。

#### 1.1.1 Source 生命周期

Source、Reader 与 SourceContext 的具体接口定义见 [source.go](../../source.go)。
Memory Source 的生命周期实现见 [内存 Source](../../connector/memory/source.go)；Runtime
提供环境、协调启动与关闭的行为仍待实现。

每次 Run 通过 Factory 创建一个新 Source。Runtime 最多调用一次 `Open`，成功前不调用
`TryRead`；`Open` 可以启动 Connector 自己的 I/O、session 或 callback goroutine。
`LifecycleContext` 可由 Source 保存但 cancel function 只归 Runtime。停止时 Runtime 先停止
admission、取消 lifecycle context，再以独立 shutdown-deadline context 调用一次 `Close`。
Runtime 只 Close 成功 Open 的 Source；Open 在部分初始化后失败时由 Source 自行清理半成品。
Close 开始后不再调用 TryRead，Close 不伪装成正常 finished，也不提交不安全 position。通用
Source 不要求 Close 幂等，M1 Memory Source 保留其已接受的幂等保证。

#### 1.1.2 独立的最终失败报告

`SourceContext` 是 Runtime 实现并在 `Open` 时提供给 Source 的运行环境，名称表示其使用者。
Source 实现读取和外部系统适配，Runtime 提供 lifecycle、control 与最终失败报告能力；
增加此方法不要求每个 Source 实现一套 Runtime 失败处理器。

Source 可能在 Runtime 因背压或暂停不再调用 `TryRead` 时发生最终失败。只依赖 Reader
返回 error，会使会话恢复超时、权限错误等无法及时触发 FailJob。因此公开以下独立入口：

```go
reportErr := sourceContext.ReportFailure(sourceErr)
```

- `sourceErr` 必须非 nil，表示 Source 确定无法继续的最终失败；nil 返回参数错误，不构成
  有效报告。暂时错误由 Connector 按自身已接受的恢复策略处理，不提前作为最终失败报告；
- 返回 nil 仅表示 Runtime 已接收报告，不表示 Source 恢复、Job 成功或关闭流程已经完成。
  非 nil 返回值描述报告本身的问题，例如入口已失效；不得用它替换原始 `sourceErr`；
- 报告入口并发安全，快速登记并通知 Runtime，不等待业务 permit、队列空位、下一次
  `TryRead` 或 Job 关闭，也不通过无限队列或每次报告创建 goroutine 实现通知；
- 每个 Source instance 保留首个最终失败，重复报告不覆盖根因、不无限累积错误。
  `TryRead` 仍可返回 error，但 Connector 的最终失败锁存与 Runtime 的登记须保持一致，
  同一失败经两个入口被观察不能生成两条独立失败流程；
- 已接收的最终失败进入现有 FailJob 路径，不使用 Operator Work Retry。全局 primary 仍
  按 [Failure Design §1.7](0006-failure-panic-and-shutdown.md#17-公开-runerror) 确定；若 Job
  已因其他错误停止，Source 报告不能覆盖该根因；
- 入口在 Source Open 阶段即须可用；初始化失败仍遵循 §1.1.1 的部分资源自行清理规则。
  Source 报告入口在关闭完成或最终终止失效后，迟到调用快速返回关闭错误，不能 panic、
  阻塞或重新启动失败处理，也不能改变已返回的 RunError；
- Connector callback 只报告失败，不同步等待 Runtime 或 Kafka 客户端关闭，防止生命周期
  相互等待。关闭期间的附加错误与最终冻结仍遵循统一 RunError/关闭契约。

该入口只补充数据 admission 之外的最终失败通知，不改变 ready 记录先绑定再观察停止的
所有权规则，也不把控制事件或暂时错误混入业务 Record。Kafka 恢复预算与错误分类见
[Kafka Design §3.6](0007-position-and-kafka-rebalance.md#36-会话建立与恢复)，跨组件取舍见
[ADR-0006](../decisions/0006-source-failure-reporting-and-session-recovery.md)。

### 1.2 非阻塞 Reader

M1/M2 阶段，Runtime 面向非阻塞 Reader。Reader 只返回：

- 已经可用的记录；
- 当前暂时没有数据；
- 正常结束；
- 读取失败或控制状态变化。

Runtime 的 Reader 调用不等待外部阻塞 I/O。阻塞读取、批量 poll、callback 接收和 session 维护由 Connector 内部适配，必要时使用受 Runtime 管理的专用 I/O goroutine和有界缓存。

非阻塞 Reader 是阶段实现选择，不代表外部系统必须物理 pull。callback Source 可以把推送结果放入 Connector 自身有界缓存，再由 Reader 非阻塞取走。

Reader、ReadResult 与 ReadState 的具体定义见 [source.go](../../source.go)。Reader 用
`TryRead` 返回当前结果，用 `Available` 提示读取方重新检查；结果包括读取状态、业务值及
可选的恢复位置。

`TryRead` 必须立即返回且不接收 context；阻塞等待只发生在 availability、Connector 内部 I/O
和生命周期边界。公开 struct 允许 Connector 直接构造结果，Runtime 仍必须验证 state 和字段
组合。`Positioned` 的契约见 §1.6.1。

`ReadResult` 只表达正常读取状态，`error` 表达读取失败：

- `err != nil` 时 Runtime 忽略 `ReadResult`，释放尚未转交的 permit，并把错误交给 Job 级
  failure 路径；它没有对应 work，不进入 Operator work Retry；
- `ReadReady` 是唯一使 `Value` 有效并转移记录 ownership 的状态；
- `ReadUnavailable` 表示当前暂时无数据，Runtime 释放 permit 并等待可用性通知；
- `ReadFinished` 表示 Source 已永久正常结束，后续不得再返回记录；
- `ReadStateInvalid` 是用于捕获零值 `ReadResult[T]{}` 和未知枚举值的防御性哨兵，不是
  Reader 可主动返回的正常状态；
- Runtime 把 invalid/unknown state 包装为可识别的 `InvalidReadResultError`，释放 permit 后进入
  Job 级 failure 路径。M1/M2 都直接 FailJob；未来若增加 Source/Job 恢复必须独立设计，不得
  借用没有 work identity 的 Operator work Retry。

当 Source 正常结束时，Connector 先交付已经预取的有界缓存记录，缓存清空后才
返回 `ReadFinished`，且该状态永久保持。当读取失败与缓存记录同时可观察时，失败优先：
当次 `TryRead` 返回 error，不在该调用中继续交付缓存记录。M1 随后 FailJob 并在关闭时
丢弃尚未交接的 Source-owned 缓存；M2 同样由 FailJob 关闭当前 Source，不在当前 Run 中保留
缓存并尝试重建。未来 Source/Job 恢复若需要复用缓存，必须另行定义 ownership 与 fence。
此处的 error 是 Source 对 Runtime 报告的最终失败；Connector 尚未报告最终失败之前的
有限客户端会话恢复不等于恢复一个已经失败的 Source。独立报告入口见 §1.1.2。

### 1.3 可用性通知与控制事件

Reader 使用可等待的可用性通知避免忙轮询。通知只是提示；Runtime 取得 permit 后若未读到记录，必须归还名额。

可用性通知必须消除 `TryRead -> unavailable -> wait` 之间的 check-then-wait 窗口。
若 Connector 已把预取数据放入内部缓冲，却没有使等待者可观察到通知，Runtime 可能
永久等待，而 Source 同时等待 Runtime 取走数据。因此第一版必须保证：

- Reader 从当前不可读变为可能可读、正常结束、失败或关闭时，必须发布通知；
- 通知不依赖 Runtime 先观察到 `unavailable`，Connector 不需要判断当前是否存在等待者；
- Connector 必须先在同步保护下发布新的 Reader 状态，再发送通知；先通知后发布状态
  可能使 Runtime 醒来后仍读不到数据，并随后错过真正的状态变化；
- `Available()` 在 Reader 生命周期内始终返回同一个容量为 1、receive-only 且永不关闭的
  notification channel。Connector 发送时执行 non-blocking send；channel 已满表示已有一个
  足以触发重新检查的未消费通知；
- 通知允许合并、重复和过期，不预留记录也不证明当前一定可读；非阻塞 Reader 的下一次
  原子结果才是状态事实的权威；
- 对于单 admission loop，在新状态发布后，必须始终满足二选一：Runtime 的下一次读取
  能观察到新状态，或 notification channel 中已有可消费的通知。

Runtime 取得 permit 后读到 `unavailable` 时立即释放 permit，然后等待通知；醒来后
重新竞争 permit 并重新读取。它不得因为收到一次通知就认定已有记录。

finished、failure 和 close 都先发布状态再尝试发送一次通知，不通过关闭 channel 表达终态。
永不关闭避免重复 Close、迟到 producer 或通知 goroutine发生 send-on-closed-channel panic；
Runtime 等待 notification 时同时监听 lifecycle cancellation。

业务数据与以下控制事件分离：

- available/no-data/end；
- read failure；
- assignment/revoke；
- ownership/generation 变化；
- host cancellation。

使读取资格失效的控制事件优先于新数据交接。Runtime 一旦知道 ownership 已失效，不得再接受该 generation 的记录。

#### 1.3.1 Split control 边界

动态 split ownership 使用独立于 Reader 和 availability 的控制边界。`SplitID` 是当前 Source
instance 内唯一、非空、可比较和可诊断的 string；Runtime 只比较，不解析内容：

```go
type SplitID string

type SourceControlReporter interface {
    Assign(context.Context, []SplitID) error
    BeginRevoke(context.Context, []SplitID, RevokeOptions) (RevokeHandle, error)
    Lost(context.Context, []SplitID) error
}

type RevokeOptions struct {
    CommitReserve time.Duration
}

type RevokeHandle interface {
    Positions() []SplitPosition
    Complete(commitErr error) error
}
```

不支持动态 split 的 Source 可以完全不调用 reporter。每次 control slice 必须非空，不能包含
空 ID 或重复项；Runtime 在改变状态前验证整批并复制 IDs。Connector 必须串行发起 control
calls，Runtime reporter 并发安全并拒绝非法重叠。状态机为：

```text
unowned --Assign--> owned(new Runtime generation)
owned  --BeginRevoke--> revoking --Complete/deadline--> unowned
owned/revoking --Lost--> immediately fenced --> unowned
```

Assign 已 owned、Revoke 未 owned、Lost 未 owned以及其他非法转换都是可识别的 control protocol
error并触发 FailJob；Runtime 不悄悄重置 generation。cooperative rebalance 只报告实际变化的
split，retained split 不进入调用。generation 只由 Runtime 创建，Connector 和 Reader 都不能
指定。

`BeginRevoke` 先暂停整个 Source admission，对目标 splits 有限 drain 并冻结最终 safe
positions，再返回 handle；目标 generation 此时处于 revoking 且不再产生新可提交进度。
Connector 在自己的 control/callback goroutine 中提交 `Positions()` 返回的副本，之后恰好调用
一次 `Complete`。nil 表示外部 commit 成功；非 nil 表示失败，Runtime 不更新 committed
frontier、fence generation 并 FailJob。重复 Complete 返回 completed-handle error。原 context
到期仍未 Complete 时 Runtime 自动 fence 并使 handle 失效；迟到 Complete 不得更新 position。
Lost 不 drain、不返回 handle且禁止旧 position commit。

##### Revoke 时间预算

Connector 提供整个 revoke 的有限 context deadline，以及 `CommitReserve`：在总预算内为
最终 position 提交、`Complete` 和控制回调返回预留的时长。Runtime 负责执行 drain，不再
配置独立的 revoke drain 最长时长，也不要求 Connector 读取 Runtime 配置。

```text
drain deadline = 有效 revoke 总 deadline − CommitReserve

BeginRevoke → Runtime drain → 冻结 positions、返回 handle → Connector commit → Complete
                              ↑ drain 截止                                  ↑ 总截止
```

有效总 deadline 不得晚于调用 context 或已有 shutdown 的更早期限。排队、等待已有普通
提交、drain、最终提交与 callback 返回共享该绝对期限，不在阶段切换时重新计时。Connector
的默认预算及外部期限约束见 [Kafka Design §3.4](0007-position-and-kafka-rebalance.md#34-kafka-revoke-配置)。

- `ctx` 必须有 deadline，`CommitReserve` 不得为负；参数错误在改变 ownership 前拒绝；
- 通用接口允许 `CommitReserve == 0`，表示不额外预留；需要外部提交的 Connector 应使用正值；
- 若剩余时间不大于预留时长，不等待业务继续完成，立即冻结当前安全位置并返回 handle；
- 工作提前收敛就提前返回，预留时长不是提交自身的 timeout，也不是必须等待的时间；
- drain 到期本身不作为 `BeginRevoke` 失败：停止等待、取消尚未交给 Sink 的未完成工作，
  冻结可提交的成功前缀并返回有效 handle；Sink-owned 未知结果不得算作成功；
- 冻结后的 completion 不得再改变 handle 的 positions 或产生该 revoking scope 的新 safe
  progress。Sink-owned 操作仍按 completion/fence 契约收敛，不能假定可以撤销；
- drain 截止不取消传入 context、不使 handle 失效；总 context 取消或有效总期限到期则使
  handle 失效并 fence。调用开始时 context 已结束，不返回可继续提交的有效 handle；
- 合法 revoke 一旦开始，不因 drain 到期恢复为 owned/readable。`Complete` 或最终 fence
  才结束旧 ownership，恢复其他 split 还须满足 Job 未失败及没有其他暂停原因。

选择显式 options 是因为预留时长直接控制本次操作，调用方可发现、Runtime 可验证。只用
`context.WithDeadline` 提前取消会同时缩短 handle 有效期；用 context value 附带另一期限虽
可实现，但会隐藏操作参数。直接要求 Connector 计算绝对 drain deadline 也可实现，第一版
选择提供预留时长、由 Runtime 统一推导，避免重复预算算法。不使用动态 revoke 的 Source
无需处理此 options。若未来出现独立于 Source 总预算的业务等待限制，再评估 Runtime 上限。

原 `BeginRevoke(ctx, splits)` 与独立 `RevokeDrainTimeout` 没有显式区分 drain 截止和 handle
失效，容易用尽预算后才进入提交。它们由以上契约替代；跨组件取舍及旧决定历史见
[ADR-0005](../decisions/0005-connector-owned-revoke-budget.md)。

Reporter 从 `Open(SourceContext)` 调用期间即有效，以支持同步初始 assignment。shutdown 后拒绝
新 Assign；有效 ownership 的 BeginRevoke 合并到现有 shutdown drain并使用所有 deadline 中
最早者，Lost 仍可立即 fence。Open 失败、Close 完成、最终 deadline fence 或 internal panic
禁止 position progress 后，reporter 失效；迟到调用快速返回 `ErrSourceControlClosed`，不
阻塞、panic、创建 goroutine或修改 Runtime 状态。

Connector 必须先改变本地读取资格，再报告 control event：Assign 返回后才能使 split
readable；revoke/lost 时先原子标记目标 split non-readable，停止扩大预取，再调用 Runtime。
在该本地线性化点前已经 ready 的记录仍须由 Runtime 绑定和追踪，之后不得再交付目标 split。
Lost 同时丢弃尚未交接缓存；revoke 缓存在决议后丢弃，retained 缓存保持但在全局 admission
暂停期间不交付。

### 1.4 Source admission 与所有权

完整责任交接为：

```text
External Source
    ↓ Connector 读取，可能批量预取
Connector-owned bounded data
    ↓ Runtime 先取得 in-flight permit，再成功取走一条
Runtime-owned input
```

Source 已读取不等于 Runtime 已接受。只有 Runtime 取得 permit 并完成交接后，才承担把该输入跟踪到明确终态的责任。

为消除 Reader 已交出记录、Runtime 却尚未建立追踪责任的窗口，admission 必须先预留
完整 reservation，再调用非阻塞 Reader。reservation 不只是一个计数 permit，而是已在
Runtime 内受控登记的空 work slot，至少预留：

- 一个 record-level in-flight permit；
- work identity 与最小追踪状态；
- 把记录保留在 admitted/pending 状态所需的有界容量；
- 后续调度到 Worker 所需的队列或等价容量资格。

只预留 permit，却允许后续因 work registry 或 Worker queue 已满而拒绝登记，不构成完整
reservation。reservation 在读取前受 Runtime 追踪，但仍为空且不承担任何 Source 记录的
completion responsibility。

非阻塞读取的结果按以下顺序处理：

```text
reserve complete admission slot
    ↓
optionally observe cancellation before reading
    ↓
non-blocking Reader call
    ├─ unavailable / finished / error / invalid → release empty reservation
    └─ ready → bind record to reservation without ordinary failure
                     ↓
                 observe cancellation
                     ├─ cancelled → mark tracked work cancelled
                     └─ active    → schedule tracked work
```

Reader 成功返回 ready 是记录 ownership 和 completion responsibility 从 Reader 转给 Runtime 的
线性化点。从该返回起：

- Reader 不得再次交付、修改或复用该 value 及其可达引用数据；
- Runtime 的第一个动作必须是把 value 绑定到已预留 reservation，使空 slot 进入
  admitted 受追踪状态；
- 绑定必须同步、不阻塞、不再申请容量、不调用用户代码，且不能返回普通控制流
  error；
- Runtime 不得在 ready 返回与绑定之间检查 context 并直接返回；context 取消即使与
  ready 并发，也必须先绑定，再把已追踪 work 明确标记为 cancelled 并交由 shutdown
  协调器收敛；
- Worker 暂时无空闲不是登记失败；work 可留在已预留的有界 admitted/pending 状态等待
  调度，始终受 Runtime 追踪。

ready 后的绑定不承诺对 yaspe 内部 bug、进程崩溃或硬件故障实现内存事务。若该
最小内部操作 panic，它是 `InternalPanicError`：Runtime 强制 FailJob、不推进 position，由可重放
Source 在重启后重放未提交输入。Memory Source 不提供进程级恢复，这是其已声明的非保证。

Source 各层须明确自己的容量及计数单位：Connector 已取出、转换中或等待交接的记录按
记录数限制；外部客户端内部预取可以按有限请求/批次单位约束，具体契约由 Connector
Design 指定。Runtime 无容量时，Connector 可以阻塞、暂停业务读取、使用 credit 或协议
等价方式，但不能借客户端预取或新增私有队列持续扩大积压。

Kafka 第一版按客户端 fetch 数与 Connector 记录数分层约束，不承诺整个 Source 的统一
记录数或字节上限，不新增 yaspe 字节/解压限制，详见
[Kafka Design §3.2](0007-position-and-kafka-rebalance.md#32-分层缓存与背压)。Memory Source
仍按 §1.7 使用严格的记录数缓存上限。原统一预取记录数要求与替代理由见
[ADR-0007](../decisions/0007-layered-source-prefetch-budgets.md)。

### 1.5 Kafka session 特殊约束

“停止业务读取”不等于停止整个 Kafka client/session 循环。回压期间 Connector 仍必须完成维持 Consumer Group membership 所需的 poll/heartbeat 和控制事件处理。

Kafka Connector 应将：

```text
业务记录是否可交接
```

与：

```text
session、heartbeat、assignment、revoke 是否继续推进
```

分开处理。具体 pause/resume 和 poll 策略留给 Kafka Connector Design。

### 1.6 Source 值、Record 与 Runtime Envelope 的边界

Source Connector 从外部客户端取得原始数据后，由配置的 deserializer/parser 将其转换为
业务值 `T`。Source 特有且业务需要观察的信息，例如 Kafka key、headers、topic 或时间戳，
由 deserializer 按所选输出模型放入 `T`；同一个 Connector 因而可以产出简单值，也可以
产出携带丰富 Source metadata 的业务类型。

Deserializer 只产生 `T`。完成正式交接前，Connector 在自身有界缓冲中持有该值；Runtime
取得 in-flight permit 后，通过前述非阻塞 Reader 取走 `T`，并统一创建 `Record[T]`、内部
Envelope 和 Work。第一版
`Record[T]` 仍只有 `Value T`，不增加通用 metadata 容器。

M2 positioned Reader 的 ready 结果还携带 Connector 定义的不透明 split/position 信息；
Runtime 根据当前 assignment 绑定 ownership，不能由 Reader 自行填写 generation。完整表示、
有序交接、identity 和 safe position 契约见
[Position Design §1](0007-position-and-kafka-rebalance.md#1-position-与第一版一致性保证)。M2
第一版一个 positioned Source element 必须恰好产生一个 `T`，不得在 Connector 内静默过滤；
Source 级零/多输出需要未来独立的 element-level completion 协议。

普通 Map 把输入转换为新的输出类型时，只有被 transform 明确保留在输出值中的 Source
metadata 才会继续到达下游。这是类型转换的显式语义，不由 Runtime 隐式复制。split、
position、ownership generation、work identity、attempt、completion 和 permit 等正确性
metadata 永远只存在于 Runtime Envelope，不进入 `Record[T]` 或业务值 `T`。

Event time 不是所有 Source 都存在，也不能仅凭 Kafka timestamp 推断为业务事件时间。
第一版不把 event time 加入 `Record[T]`；只有在 window、watermark、timer 等真实需求出现，
并同时定义产生、传播和变换语义后，才重新评估是否增加可选的通用 event-time 字段。

#### 1.6.1 Positioned ready 与 position commit

`ReadResult.Positioned == nil` 表示 unpositioned ready；positioned ready 使用：

```go
type PositionedRead struct {
    Split    SplitID
    Position any
}

type SplitPosition struct {
    Split    SplitID
    Position any
}

type PositionCommitter interface {
    CommitPositions(context.Context, []SplitPosition) error
}
```

只有 `ReadReady` 可以携带 Positioned，且 Split 非空、Position 非 nil。Position 由 Connector
定义，Runtime 不解析、比较或序列化，只保存并在 safe frontier 前进时原样交回；Connector 将
其视为不可变值。ready Split 必须处于当前 owned generation，同一 split 必须按恢复顺序交付。

会返回 positioned ready 的 Source 必须实现 `PositionCommitter`，实现该接口的 Source 也不得
混合返回 unpositioned ready；Runtime 在启动时识别 capability，并把违反组合视为契约错误。
`CommitPositions` 的 nil 返回是外部持久化成功，不只是进入 Connector 队列；每批同一 split
最多一个 position，Runtime 可以合并多次 frontier 前进。正常提交频率、Kafka client 适配与
commit failure 策略见 [Kafka Design §3.3](0007-position-and-kafka-rebalance.md#33-offset-提交与-revoke-交接)，不改变这一公共边界。

### 1.7 M1 Memory Source

Memory Source 的定位是 Runtime 参考 Source、确定性测试设施、benchmark 输入与本地
示例数据源。它用来在不依赖 Kafka 或网络的情况下验证 admission、permit、背压、
`unavailable -> notification -> ready`、正常结束、读取失败、取消和 goroutine 回收。
它不是生产级内存消息队列，不提供跨进程交付、持久化、position、replay、checkpoint、
无界缓存或多 Reader 竞争消费。

Memory Source 把 Runtime-facing Source/Reader 与 producer-facing SourceProducer 分开。Runtime 只观察标准
非阻塞 Reader 契约；测试或本地生产者通过 SourceProducer 动态提交记录、声明正常结束
或注入失败。动态有界模式是验证通知竞态与背压的权威参考实现。
具体构造与读取 API 见 [Source](../../connector/memory/source.go)，生产 API 见
[SourceProducer](../../connector/memory/source_producer.go)，用法见
[可执行示例](../../connector/memory/example_test.go)。工厂每次应创建一对新的 Source 和
SourceProducer；本实现不把某次运行的 SourceProducer 当作可复用工厂。

SourceProducer 可在 Open 前提交或声明结束。Open 前注入的失败先在本地锁存，Open 绑定
SourceContext 后报告；Open 后的首个 Fail 直接通过独立入口报告，不等待下一次 TryRead。
报告接收错误不替换输入根因，Reader 和报告入口始终使用同一个首因。

内部缓冲必须显式配置且按记录数有界；M1 不提供无界模式，参考实现要求容量大于零。
SourceProducer 允许多 goroutine 并发提交，Reader 仍只有一个 Runtime admission loop。成功提交
形成内部全序，Reader 按该顺序 FIFO 交付；不承诺并发调用按 goroutine 启动时间排序，
也不承诺等待者公平性。

#### 1.7.1 提交与背压

- 有容量时提交立即接受；缓冲已满时等待容量，并必须响应调用 context 取消；
- 提交返回 `nil` 是 value 及其可达引用数据的 ownership 转移点；返回 error 则不转移；
- 容量恢复与 context 取消并发时不承诺固定胜者，但返回结果必须与线性化结果一致：
  不得成功入队后返回 error，也不得返回 `nil` 却未保存记录；
- 每次成功提交只交付一次。Source 进入 finishing、failed 或 closed 后，新提交立即返回
  可区分的生命周期错误，不再等待容量；
- M1 只需可取消的阻塞提交，不同时承诺非阻塞 `TrySubmit` 语义。

#### 1.7.2 正常结束

`Finish` 只声明生产方不再提交新记录，不等待 Source 缓存、Operator、Sink 或整个
Job 完成。它使 Source 进入 finishing，拒绝并唤醒正在等待容量的提交，并唤醒
正在等待可用性的 Runtime。Reader 继续交付已缓存记录；缓存清空后才进入永久
finished。

`Finish` 对 finishing/finished 幂等。它不替代 Job 等待：有限 Memory Source 呈现 finished 后，
Runtime 仍必须处理所有已接纳 work，只有 work、Sink 和资源收尾按契约完成后，`Run`
或未来的 Job `Wait` 才能返回 `nil`。

#### 1.7.3 失败注入

`Fail` 要求非 nil 根因，将 Source 转为 failed，拒绝新提交，唤醒容量和可用性
等待者，并使 Reader 优先返回该错误。它只声明故障，不等待 Runtime 完成 FailJob。
已转给 Runtime 的 work 仍由 Runtime 负责；M1 中尚未交接的缓存不再优先交付，并在
Runtime 关闭 Source 时丢弃。`Run` 的主根因必须保留注入的原始错误。

#### 1.7.4 Close 与终态竞争

Close 属于 Runtime-facing Source 生命周期，不暴露给 SourceProducer。Runtime 先终止 admission
loop 和 Reader 等待，再执行 Close。Close 幂等、不伪装成正常 finished，并且：

- 拒绝新提交和读取，唤醒所有容量与可用性等待者；
- 丢弃尚未交接的 Source-owned 缓存，不影响已转给 Runtime 的 work；
- 不等待 Operator、Sink 或整个 Job，也不覆盖正常停止或 FailJob 的主根因；
- Memory Source 自身没有需要异步 drain 的外部资源，因此 Close 应同步且快速；通用
  Connector 使用已经接受的 `Close(context.Context) error` 并受统一 shutdown deadline 限制。

提交、Finish、Fail、Reader 状态转换和 Close 在同一状态机上线性化。并发操作不承诺
固定调度顺序，但任何返回成功的 ownership 或生命周期变化都必须已生效。具体终态规则为：

- 提交先于 Finish 成功时，记录必须进入缓存并在 finishing 期间交付；Finish 先生效时，
  提交返回结束错误且不转移 ownership；
- Fail 可以把 open 或尚未 drain 完的 finishing 改为 failed；一旦 Reader 已发布永久
  finished，后来的 Fail 不得改写已发布的正常结束事实；
- Fail 先生效时，后续 Finish 不得把 failed 覆盖为正常结束；
- 重复 Finish 幂等；重复 Fail 不改变状态且保留第一个根因，后续不同 error 只可作为
  附加诊断；failed 后 Finish、finished 后 Fail 和 closed 后的 SourceProducer 操作返回可区分的
  生命周期错误；
- Close 使生命周期最终进入 closed，但不改写既有正常或失败根因。


### 1.8 Memory Source 实现与验证证据

实现见 [Source](../../connector/memory/source.go) 与
[SourceProducer](../../connector/memory/source_producer.go)。内部缓冲按记录数有界，
Source 本身不创建 goroutine，构造不执行外部 I/O；数据结构以代码为准。

- [基础测试](../../connector/memory/source_test.go)：容量和生命周期校验、FIFO 与引用交接、
  正常 drain、失败优先、首因保持、关闭释放缓存及不持锁调用失败报告；
- [SourceProducer 测试](../../connector/memory/source_producer_test.go)：容量背压、取消与 deadline、
  所有等待者唤醒、Open 前等待者绑定生命周期、通知先于/晚于等待、通知合并与过期、
  多生产者逐项交付和顺序；
- [竞态测试](../../connector/memory/source_race_test.go)：Submit 与终态变化、失败与永久 finished、
  Open 与并发 Fail，以及 Close 与在途失败报告；
- [可执行示例](../../connector/memory/example_test.go)：独立使用 Source / SourceProducer 的基本流程。

并发等待使用标准库 `testing/synctest` 安排，不依赖 `time.Sleep` 猜测时序；关键状态顺序
分别验证，同时触发的竞争只断言合法结果，不假定固定胜者。测试需等待所有创建的
生产者和读取 goroutine 退出。

`go test -race -count=1 -timeout 30s ./...` 与 `go vet ./...` 已通过。失败报告使用可控的
SourceContext 验证，不能据此声明 Runtime 的 admission、FailJob、关闭协调或端到端链路
已经实现或验证。
