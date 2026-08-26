# 0003：Source Reader、Admission 与 Memory Source

状态：Accepted
最后更新：2026-08-26
适用阶段：M1–M2
依赖：[核心执行模型](0001-core-execution-model.md) · [ADR-0001](../decisions/0001-runtime-controlled-source-ingestion.md)

本文是 Runtime-facing 非阻塞 Reader、可用性通知、完整 reservation、ownership 交接与 M1 Memory Source 的权威契约。

## 1. Source 物理模型与 Runtime Reader

### 1.1 外部 pull/push 中立

yaspe 不要求所有外部系统采用统一的物理 pull、push、callback 或订阅模型：

- Kafka Connector 可以在内部 poll broker；
- 文件 Connector 可以执行阻塞读取；
- callback/subscription Source 可以接收外部推送；
- 测试 Source 可以直接提供内存记录。

统一的是 Connector 面向 Runtime 的有界责任交接，不是外部系统的物理读取方式。业务 Operator 不感知 Source 类型、Kafka poll API、partition consumer 或 callback 对象。

### 1.2 非阻塞 Reader

M1/M2 阶段，Runtime 面向非阻塞 Reader。Reader 只返回：

- 已经可用的记录；
- 当前暂时没有数据；
- 正常结束；
- 读取失败或控制状态变化。

Runtime 的 Reader 调用不等待外部阻塞 I/O。阻塞读取、批量 poll、callback 接收和 session 维护由 Connector 内部适配，必要时使用受 Runtime 管理的专用 I/O goroutine和有界缓存。

非阻塞 Reader 是阶段实现选择，不代表外部系统必须物理 pull。callback Source 可以把推送结果放入 Connector 自身有界缓存，再由 Reader 非阻塞取走。

M1 Reader 的读取边界在语义上返回 `ReadResult[T], error`。下列代码只是帮助实现和
讨论的参考形状，不是最终定稿的公开 Go API；方法名、类型名、通知载体和可见性在
实现阶段商议，但不得改变本节定义的结果、错误和生命周期语义：

```go
type Reader[T any] interface {
	TryRead() (ReadResult[T], error)
	Available() <-chan struct{}
}

type ReadResult[T any] struct {
	State ReadState
	Value T
}

const (
	ReadStateInvalid ReadState = iota
	ReadReady
	ReadUnavailable
	ReadFinished
)
```

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

### 1.3 可用性通知与控制事件

Reader 使用可等待的可用性通知避免忙轮询。通知只是提示；Runtime 取得 permit 后若未读到记录，必须归还名额。

可用性通知必须消除 `TryRead -> unavailable -> wait` 之间的 check-then-wait 窗口。
若 Connector 已把预取数据放入内部缓冲，却没有使等待者可观察到通知，Runtime 可能
永久等待，而 Source 同时等待 Runtime 取走数据。因此第一版必须保证：

- Reader 从当前不可读变为可能可读、正常结束、失败或关闭时，必须发布通知；
- 通知不依赖 Runtime 先观察到 `unavailable`，Connector 不需要判断当前是否存在等待者；
- Connector 必须先在同步保护下发布新的 Reader 状态，再发送通知；先通知后发布状态
  可能使 Runtime 醒来后仍读不到数据，并随后错过真正的状态变化；
- M1 的参考实现可使用每个 Reader 一个长期存在、容量为 1 的 notification channel。发送时执行
  non-blocking send；channel 已满表示已有一个足以触发重新检查的未消费通知；
- 通知允许合并、重复和过期，不预留记录也不证明当前一定可读；非阻塞 Reader 的下一次
  原子结果才是状态事实的权威；
- 对于单 admission loop，在新状态发布后，必须始终满足二选一：Runtime 的下一次读取
  能观察到新状态，或 notification channel 中已有可消费的通知。

Runtime 取得 permit 后读到 `unavailable` 时立即释放 permit，然后等待通知；醒来后
重新竞争 permit 并重新读取。它不得因为收到一次通知就认定已有记录。

业务数据与以下控制事件分离：

- available/no-data/end；
- read failure；
- assignment/revoke；
- ownership/generation 变化；
- host cancellation。

使读取资格失效的控制事件优先于新数据交接。Runtime 一旦知道 ownership 已失效，不得再接受该 generation 的记录。

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

第一版 Connector 预取至少在记录数上有明确上限。Runtime 无容量时，Connector 可以阻塞、暂停业务读取、使用 credit、保留有界缓存或采用协议等价方式，但不得继续扩大积压。按字节限制预取属于后续增强，不是当前保证。

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
Envelope 和 Work。具体 Reader 接口与方法名留待 Source API 实现时确定。第一版
`Record[T]` 仍只有 `Value T`，不增加通用 metadata 容器。

普通 Map 把输入转换为新的输出类型时，只有被 transform 明确保留在输出值中的 Source
metadata 才会继续到达下游。这是类型转换的显式语义，不由 Runtime 隐式复制。split、
position、ownership generation、work identity、attempt、completion 和 permit 等正确性
metadata 永远只存在于 Runtime Envelope，不进入 `Record[T]` 或业务值 `T`。

Event time 不是所有 Source 都存在，也不能仅凭 Kafka timestamp 推断为业务事件时间。
第一版不把 event time 加入 `Record[T]`；只有在 window、watermark、timer 等真实需求出现，
并同时定义产生、传播和变换语义后，才重新评估是否增加可选的通用 event-time 字段。

### 1.7 M1 Memory Source

Memory Source 的定位是 Runtime 参考 Source、确定性测试设施、benchmark 输入与本地
示例数据源。它用来在不依赖 Kafka 或网络的情况下验证 admission、permit、背压、
`unavailable -> notification -> ready`、正常结束、读取失败、取消和 goroutine 回收。
它不是生产级内存消息队列，不提供跨进程交付、持久化、position、replay、checkpoint、
无界缓存或多 Reader 竞争消费。

Memory Source 把 Runtime-facing Source/Reader 与 producer-facing Controller 分开。Runtime 只观察标准
非阻塞 Reader 契约；测试或本地生产者通过 Controller 动态提交记录、声明正常结束
或注入失败。项目可提供预装 slice 并立即声明结束的便利构造，但动态有界模式是
验证通知竞态与背压的权威参考实现。下列 `Submit`、`Finish`、`Fail` 等名称只表达
已接受的语义，不锁定最终公开 Go API。

内部缓冲必须显式配置且按记录数有界；M1 不提供无界模式，参考实现要求容量大于零。
Controller 允许多 goroutine 并发提交，Reader 仍只有一个 Runtime admission loop。成功提交
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

Close 属于 Runtime-facing Source 生命周期，不暴露给 producer Controller。Runtime 先终止 admission
loop 和 Reader 等待，再执行 Close。Close 幂等、不伪装成正常 finished，并且：

- 拒绝新提交和读取，唤醒所有容量与可用性等待者；
- 丢弃尚未交接的 Source-owned 缓存，不影响已转给 Runtime 的 work；
- 不等待 Operator、Sink 或整个 Job，也不覆盖正常停止或 FailJob 的主根因；
- Memory Source 自身没有需要异步 drain 的外部资源，因此 Close 应同步且快速。通用
  Connector 的最终 Close 签名留待 Source API 实现审核。

提交、Finish、Fail、Reader 状态转换和 Close 在同一状态机上线性化。并发操作不承诺
固定调度顺序，但任何返回成功的 ownership 或生命周期变化都必须已生效。具体终态规则为：

- 提交先于 Finish 成功时，记录必须进入缓存并在 finishing 期间交付；Finish 先生效时，
  提交返回结束错误且不转移 ownership；
- Fail 可以把 open 或尚未 drain 完的 finishing 改为 failed；一旦 Reader 已发布永久
  finished，后来的 Fail 不得改写已发布的正常结束事实；
- Fail 先生效时，后续 Finish 不得把 failed 覆盖为正常结束；
- 重复 Finish 幂等；重复 Fail 不改变状态且保留第一个根因，后续不同 error 只可作为
  附加诊断；failed 后 Finish、finished 后 Fail 和 closed 后的 Controller 操作返回可区分的
  生命周期错误；
- Close 使生命周期最终进入 closed，但不改写既有正常或失败根因。
