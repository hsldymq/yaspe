# 0001：核心执行模型

状态：Accepted（已确定条款作为当前设计；“开放问题”仍待后续收敛）
最后更新：2026-08-25
适用阶段：M0–M2

> 本文件是当前正式 Design，由多轮设计讨论与
> [Source 数据进入架构决策](../decisions/0001-runtime-controlled-source-ingestion.md)
> 合并形成。
> 文档状态、决策追踪和接力规则遵循 [Documentation Governance](../governance.md)，重要局部
> 决定索引见 [Decision Index](../decisions/README.md)。

## 1. 目的与范围

本文描述 yaspe 近期核心执行模型，使实现者不依赖原始讨论记录，也能准确回答：

- 外部 Source 的数据何时进入 Runtime；
- 谁拥有 Connector 预取、Runtime input、Operator 输出和 Sink 请求；
- 一条输入如何执行同步 Operator Chain；
- Collector 的生命周期和并发边界是什么；
- 一次 work attempt 失败时哪些输出可以撤销；
- 异步批量 Sink 如何接管输出并报告 completion；
- Sink 变慢时如何把回压传回 Source；
- 失败、暂停、终止和 Kafka rebalance 中如何继续推进安全进度；
- 近期逐记录 completion 如何演进到长期 checkpoint。

当前适用于单进程、线性 Pipeline、record 级并行和单条输入内同步 Operator Chain。完整 DAG、shuffle、有状态 Operator、checkpoint 恢复和分布式执行不在当前实现范围内，但近期边界不得主动阻断这些方向。

本文固定行为、责任、所有权和恢复语义，不固定 Go 方法名、channel 布局、状态枚举或物理对象分配方式。

### 1.1 首个工作负载

yaspe 首个真实工作负载是 `lightning-log-filter` 的 Kafka 日志 ETL：

```text
Kafka
  → 解析
  → 过滤
  → 业务事件提取
  → 异步批量写入 ClickHouse
```

它要求：

- Map、Filter、FlatMap 等短计算能够利用单机多核；
- Worker 将最终输出交给 Sink 后，不必等待落库即可处理下一条输入；
- Sink 写不赢时，内存、队列、goroutine 和 timer 不会无限增长；
- Sink 尚未明确成功时，Kafka position 不得提前推进；
- 第一版优先避免静默丢失，可以接受故障边界的重复；
- Kafka heartbeat/session 不因业务回压被错误阻塞；
- 业务 Operator 不感知 Kafka、ClickHouse 或 Source 的物理读取模型。

### 1.2 阶段落地边界

- M1：非阻塞 Memory Reader、有界 permit 和队列、完整 work-attempt 边界、末端暂存、同步 Memory Sink、FailJob 以及取消回收；
- M2：Kafka split/position/ownership、异步批量 Sink、completion tracker、生产级重试恢复和 rebalance 收尾；
- M1 的接口和所有权边界不得阻断 M2，但不得为了远期目标提前实现 Kafka、生产 Sink 或 checkpoint。

## 2. 设计原则与术语

### 2.1 计算与执行分离

Operator 只描述业务计算：一条输入产生零条、一条或多条输出。Runtime 拥有：

- Source admission；
- Pipeline Worker 和并行度；
- Collector 和内部 edge；
- 队列、permit、背压和取消；
- completion、safe position 和 generation fence；
- Retry、FailJob 和宿主取消；
- Source、Sink 和 Job 生命周期。

Operator 不创建 Worker Pool，不提交 Source position，也不决定 Job 级恢复策略。

### 2.2 关键术语

- **split**：可独立读取和推进 position 的 Source 分片；Kafka 中对应 topic-partition。
- **ownership**：当前 Runtime 对一个 split 的处理和提交权责。
- **generation**：同一 split 的一次 ownership 任期；重新获得同一 split 也是新任期。
- **generation fence**：拒绝旧任期的迟到完成或提交污染当前 ownership。
- **work**：Runtime 已接管的一条原始输入及其端到端责任状态；从 Source admission 持续到输入终结，一个 work 可以经历一次或多次 attempt。
- **in-flight permit**：Runtime 接受一条尚未终结输入所占用的端到端容量名额。
- **work attempt**：一条 Runtime 已接受的原始输入执行完整同步 Operator Chain 的一次尝试。
- **Pipeline Worker**：Runtime 预先创建并长期复用的固定 goroutine；顺序执行多个不同 work，不与某个 work 永久绑定。
- **execution slot**：当前可执行 Operator Chain 的并发名额；第一版与 Pipeline Worker 一一对应，数量等于 `Parallelism`。
- **execution lane**：一条独立并行执行通道；第一版由一个 Pipeline Worker、一个 execution slot 和该通道独占的 Operator Chain 实例组成。
- **terminal output**：一次 Chain 成功后形成、尚未交给 Sink 的最终输出集合。
- **completion**：一条输入的全部必要下游效果是否进入明确结果。
- **safe position**：一个 split 中连续完成、恢复时可以从其后继续的位置。
- **committed position**：外部 Source 已持久化确认的 safe position。

### 2.3 三个不同的完成时刻

```text
processing finished
    Operator Chain 已执行并形成最终输出

record completed
    所有必要 Sink effect 已明确成功或进入策略允许的终态

position committable
    split 中不存在阻挡连续位置推进的前序空洞
```

Worker 可以在适当的责任交接后复用；in-flight permit 只能在记录终结后释放；Source position 只能在连续完成后推进。

### 2.4 Work、Worker、slot 与 permit 的关系

```text
取得 in-flight permit
    ↓
Runtime 接管输入并创建 work
    ↓
等待 execution slot
    ↓
Pipeline Worker 执行一次 work attempt
    ↓
completed work 成功进入 terminal queue
    ↓
execution slot 释放，Worker 可执行下一个 work
    ↓
Sink 接管并异步完成必要 effect
    ↓
work terminal
    ↓
in-flight permit 释放
```

work 是状态与责任载体，不是 goroutine。Retry 为同一 work 创建新的 attempt，继续使用原 permit。第一版一个 Worker goroutine 对应一个 execution slot，并与一套独立 Operator Chain 共同组成一条 execution lane；Worker 阻塞在 terminal queue Put 时仍占用该 slot，Put 成功后即释放执行关系，不需要跟随 work 等待 Sink completion。

## 3. 近期执行形态

M1/M2 采用多条并行完整 Pipeline，而不是为每个 Operator 建立独立队列和 Worker Pool：

```text
                         ┌─ Pipeline Worker 1 ─┐
Bounded Source Boundary ─┼─ Pipeline Worker 2 ─┼─→ Terminal/Sink Boundary
                         └─ Pipeline Worker N ─┘

每个 Worker 内：
Map → Filter → FlatMap → terminal output
```

选择原因：

- 短计算在同一 goroutine 内同步调用，避免逐 Operator 调度；
- 不同输入由多个 Worker 并行；
- 单条输入内部的输出顺序和错误传播清晰；
- M1 的同步 Memory Sink 容易验证；
- M2 只在异步批量 Sink 处引入受控异步边界；
- 以后可以根据真实 workload 增加显式 chain boundary，而不是现在实现完整物理 DAG。

并行度大于一时，第一版不保证不同输入之间的全局输出顺序。

每条 execution lane 拥有独立创建的 Operator Chain；不同 lane 不共享 Operator 实例。同一
实例只由所属 lane 的 Pipeline Worker 串行调用，因此普通 Operator 无需为了 `Process`
并发调用自行加锁，也不得被 Runtime 同时用于多条 lane。Job Definition 必须保留创建每条
Chain 所需的信息，而不能仅依赖一个待共享的现成 Operator 对象。

### 3.1 第一版线性 Job Definition

第一版使用 Go 1.27 泛型方法提供类型安全的 fluent API。`JobBuilder` 是逻辑定义的所有者，
`Stream[T]` 是指向当前 `Transformation` 的类型安全句柄；Map、Filter、FlatMap 和自定义
Operator 接入都会添加新的 Transformation，而不是在定义阶段添加一个已实例化并由所有
lane 共享的 Operator。

概念调用形态为：

```go
builder := yaspe.NewJob("lightning-log-filter")

builder.
    From(source).
    Map(parse).
    Filter(validate).
    FlatMap(extract).
    To(sink)

job, err := builder.Build()
```

Transformation 记录逻辑计算语义、拓扑身份、上游关系、用户函数和创建运行实例所需的
信息，但不处理 Record、不启动 goroutine，也不持有运行期队列或 session。`Stream[T]`
持有当前 Transformation 的内部引用；每次转换返回指向新尾节点的 `Stream[O]`，旧 Stream
仍可保留。内部必须保留 Transformation 身份和引用关系，不能只把整条路径复制成一个
Operator factory 切片，以免阻断未来的分支和 DAG 演进。

内置 Map、Filter 和 FlatMap Transformation 可以共享用户提供的函数值，但 Runtime 为每条
execution lane 分别创建包装该函数的 Operator 实例。同一 Operator 实例只由所属 lane 串行
调用；多个 lane 可能并发调用同一个用户函数值。函数捕获的变量和外部依赖不会因 Operator
实例化而复制，其并发安全、幂等性和副作用由用户负责。需要 lane-local 实例状态的自定义
Operator 通过 factory 接入。

`To` 只向 Builder 添加 Sink Transformation，不直接封闭并返回 Job。`Build` 对当前
Transformation 拓扑创建不可变快照、执行结构校验并返回 Job；Builder 后续变化不影响已经
构建的 Job，Builder 本身不保证并发安全。第一版 Build 只接受恰好一个 Source、零个或多个
Operator Transformation、恰好一个 Sink，且它们构成一条无分支、无合流、无悬空节点的
线性链。未来支持多 Source、分支和多 Sink 时通过放宽 Build 校验并增加图编译阶段演进，
无需改变 `From`、Transformation 方法和 `To` 的基本模型。

`Transformation`、内部引用、ID、类型擦除和 factory 的具体 Go 类型名仍可通过实现原型
细化；这些实现选择不得改变以上定义期、编译期和运行期边界。

## 4. Source 物理模型与 Runtime Reader

### 4.1 外部 pull/push 中立

yaspe 不要求所有外部系统采用统一的物理 pull、push、callback 或订阅模型：

- Kafka Connector 可以在内部 poll broker；
- 文件 Connector 可以执行阻塞读取；
- callback/subscription Source 可以接收外部推送；
- 测试 Source 可以直接提供内存记录。

统一的是 Connector 面向 Runtime 的有界责任交接，不是外部系统的物理读取方式。业务 Operator 不感知 Source 类型、Kafka poll API、partition consumer 或 callback 对象。

### 4.2 非阻塞 Reader

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

- `err != nil` 时 Runtime 忽略 `ReadResult`，释放尚未转交的 permit，并把错误交给
  Failure Policy；
- `ReadReady` 是唯一使 `Value` 有效并转移记录 ownership 的状态；
- `ReadUnavailable` 表示当前暂时无数据，Runtime 释放 permit 并等待可用性通知；
- `ReadFinished` 表示 Source 已永久正常结束，后续不得再返回记录；
- `ReadStateInvalid` 是用于捕获零值 `ReadResult[T]{}` 和未知枚举值的防御性哨兵，不是
  Reader 可主动返回的正常状态；
- Runtime 把 invalid/unknown state 包装为可识别的 `InvalidReadResultError`，释放 permit 后交给
  Failure Policy。M1 只有 FailJob 时它直接终止 Job；M2 引入 Retry 后遵守用户策略，
  不在 Runtime 内硬编码处置动作。

当 Source 正常结束时，Connector 先交付已经预取的有界缓存记录，缓存清空后才
返回 `ReadFinished`，且该状态永久保持。当读取失败与缓存记录同时可观察时，失败优先：
当次 `TryRead` 返回 error，不在该调用中继续交付缓存记录。M1 随后 FailJob 并在关闭时
丢弃尚未交接的 Source-owned 缓存；M2 选择 Retry 后是否保留并继续交付原缓存，留待 Retry/
Connector 恢复契约决定。

### 4.3 可用性通知与控制事件

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

### 4.4 Source admission 与所有权

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

### 4.5 Kafka session 特殊约束

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

### 4.6 Source 值、Record 与 Runtime Envelope 的边界

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

### 4.7 M1 Memory Source

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

#### 4.7.1 提交与背压

- 有容量时提交立即接受；缓冲已满时等待容量，并必须响应调用 context 取消；
- 提交返回 `nil` 是 value 及其可达引用数据的 ownership 转移点；返回 error 则不转移；
- 容量恢复与 context 取消并发时不承诺固定胜者，但返回结果必须与线性化结果一致：
  不得成功入队后返回 error，也不得返回 `nil` 却未保存记录；
- 每次成功提交只交付一次。Source 进入 finishing、failed 或 closed 后，新提交立即返回
  可区分的生命周期错误，不再等待容量；
- M1 只需可取消的阻塞提交，不同时承诺非阻塞 `TrySubmit` 语义。

#### 4.7.2 正常结束

`Finish` 只声明生产方不再提交新记录，不等待 Source 缓存、Operator、Sink 或整个
Job 完成。它使 Source 进入 finishing，拒绝并唤醒正在等待容量的提交，并唤醒
正在等待可用性的 Runtime。Reader 继续交付已缓存记录；缓存清空后才进入永久
finished。

`Finish` 对 finishing/finished 幂等。它不替代 Job 等待：有限 Memory Source 呈现 finished 后，
Runtime 仍必须处理所有已接纳 work，只有 work、Sink 和资源收尾按契约完成后，`Run`
或未来的 Job `Wait` 才能返回 `nil`。

#### 4.7.3 失败注入

`Fail` 要求非 nil 根因，将 Source 转为 failed，拒绝新提交，唤醒容量和可用性
等待者，并使 Reader 优先返回该错误。它只声明故障，不等待 Runtime 完成 FailJob。
已转给 Runtime 的 work 仍由 Runtime 负责；M1 中尚未交接的缓存不再优先交付，并在
Runtime 关闭 Source 时丢弃。`Run` 的主根因必须保留注入的原始错误。

#### 4.7.4 Close 与终态竞争

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

## 5. Collector 生命周期与并发

### 5.1 每次 Process 逻辑独立

Runtime 为每次 `Operator.Process` 调用提供一个逻辑上独立的 Collector：

```text
create logical Collector
    ↓
Process begins
    ↓
Emit 0..N times
    ↓
Process returns
    ↓
Collector becomes invalid
```

具体约束：

- Collector 仅在对应 `Process` 调用期间有效；
- Operator 不得保存 Collector；
- Operator 不得在 `Process` 返回后继续调用 Collector；
- Collector 可以关联当前 work attempt/execution scope；
- `Emit` 成功后，Runtime 取得输出的后续处理责任；
- 逻辑独立不等于必须为每次调用执行独立堆分配。

物理实现可以是栈上小对象、内联 edge、长期复用的 output 加当前 execution scope，或者经验证安全的池化对象。优化不能改变公开生命周期。

### 5.2 Collector 仅串行使用

- 单个 Collector 只能在对应 `Process` 的调用 goroutine 中串行使用；
- Collector 不保证线程安全；
- Operator 不得启动 goroutine 并发调用 Collector；
- Operator 不得异步保存 Collector；
- `Emit` 按调用发生顺序处理；
- 不同输入可以由不同 Pipeline Worker 并行处理。

普通 Operator 内部若任意创建 goroutine，会使真实并行度脱离 Runtime 控制，并引入输出顺序、错误竞争和生命周期问题。需要异步能力时，应由未来 Runtime 托管的 Async Operator 或显式 Stage 提供。

### 5.3 Emit 契约

- `Collector.Emit` 的公开形式为 `Emit(record)`，不接收调用方传入的 context；
- Runtime 创建 Collector 时将当前 work attempt 的 `Process` context 绑定到 Collector；
- `Emit` 在等待下游容量、传播背压和解除阻塞时只使用该绑定 context。用户
  Operator 不能用无关 context 脱离当前 attempt 的取消边界；
- `Process` 仍显式接收 context，供用户计算、I/O 和派生操作使用；
- `Emit(nil)` 表示当前 Collector 已接受输出并取得后续责任；
- Collector 接受不等于最终 Sink 已经完成；
- `Emit` 失败表示本次输出未被接受；
- FlatMap 首次 Emit 失败后停止后续输出；
- context 取消不撤回此前已经成功接受的输出；
- `Emit` 必须能够传播下游同步处理错误、Runtime 取消和容量边界错误；
- 具体实现可以在同步 Chain 中直接调用下一个 Operator，也可以在明确边界处等待容量；
- yaspe 不承诺所有端到端背压都必须表现为 `Emit` 长期阻塞。

### 5.4 Emit 值的 ownership 与复制

`Record[T]` 按值传递不表示其引用数据被复制。`T` 可以包含 slice、map、pointer、interface，
以及由这些值间接引用的任意对象；Runtime 无法对任意 `T` 实施通用、安全且语义正确的深拷贝。
第一版采用成功交接即转移 ownership 的约定：

- `Emit` 成功时，当前 `Record[T]` 及其可达引用数据的 ownership 立即转给 Runtime，不延迟到
  `Process` 返回；
- 调用方从成功的 `Emit` 返回起不得再修改、复用或释放相关引用数据和 backing storage，也
  不得将其放回对象池；普通变量随后改为指向其他值不属于复用；
- `Emit` 失败表示本次输出未被接受，ownership 仍属于调用方，调用方可以修改、复用或释放
  相关数据；此前成功 Emit 的其他输出不受这次失败影响；
- Runtime 默认不复制 `T`，也不提供通用 copier/serializer。需要复用原存储的用户代码必须
  在 Emit 前自行创建独立值；
- Runtime 和后续接收方取得生命周期管理责任，但把业务值视为逻辑不可变，不依赖 ownership
  对其原地修改。

该规则是 Go 类型系统无法完全强制的实现约定。成功 Emit 后仍通过别名修改或复用数据属于
用户实现错误；yaspe 不保证检测或阻止，也不保证此时的输出内容、确定性或并发安全。可能结果
包括已暂存输出被覆盖、多次输出意外共享最终内容、data race，以及对象池提前复用造成的数据
污染。race detector 只能发现其中一部分并发违规。

## 6. Operator Chain 与 work-attempt 边界

### 6.1 两个观察层级

单个 Operator/Collector 层级：

- FlatMap 第三次 Emit 失败，不会从这个 Collector 的测试观察中撤销前两次 Emit；
- 此前接受的输出仍然对本次 Collector 可见。

完整 work-attempt 层级：

- 中间 Collector 同步驱动下一个 Operator；
- 中间层不建立持久恢复队列；
- Chain 的最终输出在 attempt 成功前保存在 Runtime 可撤销的有界末端边界；
- 任一 Operator 失败时，Runtime 丢弃该 attempt 尚未转移给 Sink 的全部末端输出。

这两个层级并不矛盾：前者定义 Operator 契约，后者定义 Runtime 是否已经产生不可撤销的外部责任。

### 6.2 末端暂存

```text
Runtime-owned input
    ↓ work attempt
Synchronous Operator Chain
    ↓ attempt success
Bounded terminal output
    ↓ eligible handoff
Sink-owned output
```

末端暂存的作用是：

- Chain 失败时，避免把部分最终输出提前交给 Sink；
- 策略允许重试时，可以使用保留的原始输入重新执行整条 Chain；
- 暂停、position gap 或 generation 变化时，可以阻止尚未产生外部 effect 的 work 继续交接。

它不是事务日志，也不提供外部原子性。第一版终端暂存按 work 数量保持有界；字节预算属于后续增强。

Operator 内自行产生的外部副作用不受末端暂存保护。重新执行 Chain 可能重复这些副作用，责任由 Operator 作者承担。

近期实现使用有界 terminal queue 保存已经成功完成 Chain、尚待 Sink 接管的 work。Worker 以可取消的 `Put(ctx, completedWork)` 整组提交；队列已满时，Worker 阻塞并继续持有当前 completed work，不处理下一条输入。队列腾出或 context 取消后 Put 才返回。这样 Sink 回压会依次填满 terminal queue、阻塞固定数量的 Worker、耗尽 input queue/permit，最终传回 Source。

blocked Worker 当前持有的 work、terminal queue、Sink Coordinator 当前 work 和 Sink 已接管的 in-flight effect 都必须计入资源预算。Sink Coordinator 和 completion 协调路径使用独立执行路径，不得依赖可能全部阻塞在 terminal queue 的 Pipeline Worker，否则会形成循环等待。

### 6.3 零、一和多输出

- Map：形成一个派生输出；
- Filter 保留：继续传递原记录；
- Filter 丢弃：成功 work 零输出，可直接完成；
- FlatMap：形成有限个派生输出；
- 任一 Operator 返回错误：attempt 失败，尚未交给 Sink 的末端输出可撤销。

## 7. Sink 交接与 Completion

### 7.1 整组责任转移

Chain 成功后，Runtime 才允许 terminal output 进入 Sink 边界。一个 work 的最终输出必须整组交接：

```text
Sink 接受该 work 的全部输出
或
Sink 一个也不接受
```

这是责任转移原子性，不是外部写入事务原子性，也不要求一个 work 独占一个物理 batch。Sink 接管后可以把多个 work 的输出自由组批。

成功交接前，输出由 Runtime 持有，可以暂缓或撤销；成功交接后，输出责任转给 Sink，Runtime 不再假设可以撤回。

Sink 接管后失败时，优先保留已经形成的 terminal output，并在 Sink 边界恢复，而不是重新执行 Operator Chain。

交接 API 不向 Sink Connector 暴露 Runtime 内部的 `Work`。Runtime 把每个 terminal output 包装为 `SinkItem[T]`，其中包含业务 `Record[T]` 和仅供 Runtime 关联 completion 的不透明身份。Connector 接收该 work 产生的完整 `[]SinkItem[T]`；`T` 必须与 Sink 声明的输入类型一致，并由 Go 泛型在组装 Pipeline 时约束。

Connector 使用 `item.Record` 转换目标系统需要的请求，并让原 `SinkItem[T]` 跟随该请求直到 callback，再通过 reporter 原样报告对应 item 的结果。Connector 不解释不透明身份、不维护 records index，也不依赖业务值相等性；因此内容相同的多条 Record 仍可被 Runtime 准确区分。目标系统自己的结构应使用 `KafkaRequest`、`PostgresRow` 等具体名称，避免与 `SinkItem` 混淆。

attempt、generation、Source position、completion 状态和调度信息仍由 Runtime 保存。原子接管方法采用每次交接传入绑定式结果报告器的方案，当前拟定 API 为：

```go
type SinkContext interface {
    LifecycleContext() context.Context
    CapacityNotifier() SinkCapacityNotifier
}

type SinkCapacityNotifier interface {
    NotifyAvailable()
}

type Sink[T any] interface {
    Open(runtime SinkContext) error

    Accept(
        ctx context.Context,
        items []SinkItem[T],
        reporter SinkResultReporter[T],
    ) (SinkAcceptStatus, error)

    Close(ctx context.Context) error
}
```

Runtime 创建 `SinkContext` 并对每个 Sink 实例调用一次 `Open`；只有 Open 成功后才启动 Sink Coordinator、Pipeline Worker 和 Source admission。运行期仅 Sink Coordinator 调用 `Accept`。Runtime 最多调用一次 `Close`，Open 失败时不开放数据入口，并按逆序清理已经打开的其他组件。

`LifecycleContext()` 返回由 Runtime 控制的 Sink 生命周期 context，Sink 可以保存并用于后台任务和已接管的异步操作，但不能取得其 cancel function。它不同于 `Accept` 的单次接管 context；Accept 返回成功后，异步操作不得因该调用 context 生命周期结束而被取消。`Close(ctx)` 的 context 独立控制有限 drain/资源释放，因此即使生命周期 context 已取消，关闭仍可在单独 deadline 内收尾。`SinkContext` 第一版只提供生命周期 context 和 capacity notifier，未来能力按实际需求扩展。

`SinkResultReporter` 已经绑定当前内部 work，异步 Sink 可以保存它并在外部 callback 中报告完成事实，不需要取得或保存 work ID。它与 Collector 的调用期生命周期不同：可以在 `Accept` 返回后使用、可以跨 goroutine 调用，并且必须并发安全。

只有 `SinkAccepted, nil` 才会使 reporter 对该次交接有效；返回 `SinkBackpressured` 或 error 时，Sink 不得保存或调用它。Runtime 还必须正确处理外部客户端在 `Accept` 返回前同步触发 callback 的竞态：提前到达的报告只能暂存，确认接管成功后才能应用；若最终没有接管，则不得据此终结 work。

结果报告采用一个可增量、可批量调用的方法：

```go
type SinkOutcome uint8

const (
    SinkSucceeded SinkOutcome = iota
    SinkNotApplied
    SinkUnknown
)

type SinkItemResult[T any] struct {
    Item    SinkItem[T]
    Outcome SinkOutcome
    Err     error
}

type SinkResultReporter[T any] interface {
    Report(results []SinkItemResult[T])
}
```

`Report` 可以调用一次或多次，每次报告任意数量的 item，不要求按原顺序或一次覆盖整个 work。`SinkSucceeded` 必须携带 nil `Err`；`SinkNotApplied` 和 `SinkUnknown` 必须携带非 nil `Err`。如果外部协议只提供结果状态而没有底层异常，Connector 使用 yaspe 提供的标准哨兵错误；因此 Runtime、日志和失败策略始终能得到具体错误值，但 outcome 仍是外部事实的权威分类。

调用 `Report` 时，`results` slice 的所有权转移给 Runtime。调用返回后 Connector 不得读取、修改、缩短、扩展、复用其 backing array，也不得放回对象池。该约定允许 Runtime 将结果直接投递到协调路径而不复制；`SinkItem` 和 `SinkItemResult` 对 Connector 都是逻辑不可变值。违反约定属于 Connector 实现错误，可使用 race detector 和 Connector 契约测试发现。

`Report` 不返回 error，可以由任意 goroutine 调用，并由 Runtime 幂等处理重复、迟到、越界、外来或互相矛盾的 item 结果；这些违规不得在 callback goroutine 中 panic，应被忽略并记录诊断。容量恢复通知属于整个 Sink 的容量状态，不属于某个 work，因此不放入 `SinkResultReporter`。

#### 7.1.1 M1 同步 Memory Sink

Memory Sink 的定位是 Runtime 同步参考 Sink、结果断言工具、benchmark 终点和本地示例
输出。它用来验证 terminal output、整组责任交接、并发调用、Sink 失败与 FailJob，
不提供外部 I/O、异步 completion、batch flush、partial success、position、Retry、持久化或
exactly-once。下列 `Accept`、`Groups`、`Records` 和 Close 等名称只表达已接受语义，
不锁定最终公开 Go API。

Runtime 只在整条 Operator Chain 成功后，把一个 work 的全部 terminal outputs 通过一次
同步调用交给 Memory Sink：

```text
Accept(group) returns nil
    → Sink atomically owns and has synchronously completed the whole group

Accept(group) returns error
    → Sink owns and retains none of the group
```

成功返回是整组 `Record` 及其可达引用数据的 ownership 转移点；Runtime 之后不得
修改或复用。返回 error 时 Memory Sink 不得保存组内任何 Record 或引用，ownership 仍属于
Runtime，错误进入 Failure Policy。不存在先保存部分记录再返回 error 的合法路径。

零输出 work 没有 Sink effect，Runtime 不调用 Memory Sink，直接将该 work 标记为 Success 并
释放 permit。这是 Filter 不保留、FlatMap 返回空结果或自定义 Operator 正常不 Emit 的
成功语义，不是 Skip/Discard。

Memory Sink 可以配置固定、确定性失败计划，例如第 N 次接管失败、所有接管失败或
按预先给定的 error 序列失败。M1 不接受任意用户失败回调，以避免多出一套用户代码
生命周期、panic 和 records 逃逸契约。每次接管在同一线性化区内先检查生命周期和
失败计划；若失败则不保存任何值，若成功则一次追加整组。

Memory Sink 必须线程安全并保留 work grouping。单组内记录顺序严格保持；并发 work 的
group 按实际接管线性化顺序保存。该顺序是可观察实现事实，不是 `Parallelism > 1`
时的业务顺序保证；`Parallelism = 1` 时应保持线性 work 顺序。M1 不承诺并发等待者公平性。

结果读取在语义上提供保留 work 边界的 groups 视图和按实际接管顺序扁平化的 records
视图。两者均返回新的外层与组内 slice 结构，调用方修改 slice 不得破坏 Sink 内部结构；
yaspe 不对任意 `T` 做深拷贝，因此快照中 Record 及可达引用数据仍必须视为只读，读取快照
不转移 ownership。运行期快照只包含该调用线性化前已完成的整组接管；`Run` 返回后
结果集合稳定。

Close 由 Runtime 调用，幂等、同步且快速，不清空已接管结果，Close 后仍可读取快照。
Memory Sink 没有后台 goroutine、外部 in-flight、flush 或可注入 Close failure。正常关闭顺序为：

```text
Runtime stops new Sink handoff
    ↓
already-entered synchronous Accept calls finish
    ↓
Memory Sink closes
    ↓
results remain readable and stable
```

接管与 Close 使用同一线性化边界。接管先生效时，其整组成功或失败结果完整形成后
Close 才继续；Close 先生效时，后续接管返回可识别的 closed error、不保存数据也不转移
ownership。正常 Runtime 不应在 Close 后调用接管；该错误不得被静默丢弃。

某个并发接管返回 error 并触发 FailJob 时，其他已在线性化点成功的 groups 仍然有效，
不回滚；Runtime 得知 FailJob 后阻止尚未开始的新 Sink handoff，等待已进入的同步调用返回，
再 Close。Memory Sink 不提供跨 work 事务或回滚。

### 7.2 有界通知驱动交接

Runtime 与 Sink 之间使用有界、通知驱动的交接边界：

- Runtime 为每个 Sink 实例提供独立的 Sink Coordinator；只有 Coordinator 调用 `Accept`，Pipeline Worker 不直接调用 Sink，也不维护 Sink 容量状态；
- Worker 只向有界 terminal queue 提交 completed work；交接成功后可以处理下一条，但端到端 permit 仍持续到 Sink effect 完成；
- Sink Coordinator 判断哪些 work 当前有资格产生 Sink effect，并负责选择、等待、公平性和重试调度；
- Sink Connector 只被动接收一个 work 的完整 `[]SinkItem[T]`，不感知 `Work` 内部状态，也不感知 Runtime 采用 pull、push、mailbox 还是 event loop；
- Sink 对容量的判断和整组责任接管必须是一个原子操作，方法返回 `(SinkAcceptStatus, error)`；
- `SinkAccepted, nil` 表示 Sink 已取得该 work 全部输出的后续责任；
- `SinkBackpressured, nil` 是流量控制结果，不是处理失败，此时责任仍在 Runtime，Sink 不得接管部分输出；
- 任何非 `nil` error 都表示 Sink 一个输出也没有接管，status 被忽略，错误交给 Runtime 的失败策略处理；
- Connector 不得在接管部分输出后返回 `SinkBackpressured` 或 error；
- 不提供先调用 `IsBackpressured`、再提交 work 的分离式协议，避免两步之间容量状态发生变化；
- Sink 在观察到接管容量恢复或增加时调用 `SinkCapacityNotifier.NotifyAvailable()`，报告调用瞬间存在可用容量；它不负责拉取或选择 work；
- 通知不预留容量，也不保证 Coordinator 稍后调用 `Accept` 时仍能成功；Runtime 必须把它视为重新检查触发器，并以新的原子 `Accept` 结果为准；
- Connector 不需要判断 Runtime 是否正在等待；notifier 可从任意 goroutine 并发调用、不返回 error，并应快速完成；Runtime 容忍重复、合并以及到达时已不再可用的通知；
- 永远无法被当前 Sink 接受的 item group 必须返回真实 error，不得以 `SinkBackpressured` 等待一个永远不会发生的容量通知；
- 容量通知使用单调递增的 generation/version，而不是可被旧 `Accept` 结果覆盖的 available 布尔值；
- Coordinator 在调用 `Accept` 前取得 observed version；若返回 `SinkBackpressured` 时版本已经变化，则立即重试，否则原子等待 version 大于 observed；
- 版本检查与等待必须避免 check-then-wait 窗口，可使用“版本号 + 每代关闭并替换的 channel”等内部机制；通知允许合并和伪唤醒，但不得丢失；
- Runtime 在容量变化后重新尝试交接；没有容量或状态变化时不得忙轮询；
- 未来可以在 Runtime 内部改用其他调度方式、DAG edge 或跨进程 exchange，只要不改变责任转移、有界背压和完成语义。

### 7.3 异步 completion

Sink 的等待 buffer、并发请求、待重试项和 timer 都有上限。Worker 在 terminal output 被 Sink 整组接管后可以处理下一条输入，但父输入仍保持 in-flight，直到所有必要 Sink effect 明确完成。

外部异步 callback 不直接并发修改 work、completion、safe position 或 generation。callback 只报告事件，由 Runtime 协调路径串行且幂等地应用。

这里的 callback 是 Kafka、HTTP、数据库等外部异步客户端在请求完成、失败或超时时执行的回调，包括可能在注册时同步触发、早于 `Accept` 返回的回调。它只能通过 `SinkResultReporter.Report` 和 `SinkCapacityNotifier.NotifyAvailable` 提交事实，不能直接操作 Runtime 状态。

每个 Sink 使用一个 Sink Coordinator 串行处理 Accept、capacity 和 item completion，但两类事件采用不同入口：

- capacity 使用独立的版本化 signal，version 在 `NotifyAvailable` 调用期间同步推进，不通过普通 FIFO 事件累计；
- 每个 work reporter 使用独立、线程安全且有界的 result inbox；`Report` 先验证 item 身份，并只保留每个 item 的第一个有效结果；
- reporter 可以接收多次增量 Report，但通过 `wakePending` 或等价机制合并唤醒：Coordinator drain 前，同一 reporter 在 wakeup queue 中最多占一个位置；
- Coordinator 取得 reporter wakeup 后一次 drain 当前 pending results，再允许后续结果安排新的 wakeup；实现必须处理 drain 与新 Report 并发发生的竞态，不得丢失结果；
- 重复、外来、越界、迟到或互相矛盾的结果在进入 pending 存储前丢弃并记录诊断，不能借此无限扩大队列；
- 活跃 reporter 数量受全局 in-flight work 上限约束，因此每 reporter 一个 pending wakeup 的总空间也有界；
- capacity signal 和 result inbox 不强制共享一个普通 channel 或统一事件结构。

reporter 在 `Accept` 返回前处于 pending-accept 状态。提前到达的有效结果可以暂存，但只有 `SinkAccepted, nil` 才激活并允许 Coordinator 应用；若最终返回 Backpressured 或 error，则丢弃提前结果并记录 Connector 违反契约。Sink Coordinator 将同一 work 的 item outcome 聚合成 work-level completion，再交给 Runtime Completion Tracker；后者负责输入终态、permit 释放、position gap 和 safe position，不理解物理 Sink batch。

当前稳定边界先采用 work 级 reporter。未来提供跨 work 组批的通用异步 Sink 基础设施时，可以在其内部增加类似 Flink `ResultHandler` 的物理请求级 handler，由基础设施负责物理 batch 与多个 work reporter 之间的结果聚合；这不改变 Runtime 与 Sink 的 `Accept` 边界。yaspe 只借鉴绑定式回调和 mailbox 式串行应用，不把“是否重试”混入结果事实。

迟到、乱序或重复通知不得导致：

- work 重复终结；
- permit 重复释放；
- safe position 错误跨越空洞；
- 旧 generation 推进当前 position。

### 7.4 completion 结果

Sink completion 至少区分三种事实：

1. `SinkSucceeded`：确认成功，`Err` 必须为 nil；
2. `SinkNotApplied`：外部协议能够证明未生效，`Err` 必须非 nil；
3. `SinkUnknown`：结果未知、可能已经生效，`Err` 必须非 nil。

超时、断连等不能自动解释为“未写入”。错误临时或永久、是否重试是失败策略的另一维度，不能由这三种事实直接推导。

如果 Sink 能可靠报告每个输出的结果：

- 已确认成功的部分保持成功；
- 只重试确认未生效或仍需处理的部分；
- 原始 work 等所有必要输出都完成后才终结。

整批重试是外部协议无法提供可靠细粒度结果时的退化方案。

### 7.5 completion 关联

概念上可以用“封口 + 未完成计数”理解多个派生输出：

```text
initial: sealed=false, pending=0
accepted child: pending++
attempt sealed: sealed=true
child complete: pending--

sealed && pending == 0
→ input complete
```

最终实现不必采用同名字段，但必须避免 pending 暂时为零、attempt 尚可能继续产生输出时提前完成。

### 7.6 有限关闭与迟到事件隔离

Sink 一旦以 `SinkAccepted, nil` 接管一个 work 的输出，就承担把所有 item 推进到明确结果的
责任。关闭不能把“已接管但仍在内部 buffer”解释为可以丢弃；buffer、已组 batch 和外部
in-flight 请求都属于有限 drain 的范围。

Runtime 关闭 Sink 时采用以下顺序：

```text
停止新的 Accept
    ↓
Sink 在 Close deadline 内 flush 已接管 buffer
并等待外部 in-flight 请求
    ↓
逐 item 形成 Succeeded / NotApplied / Unknown
    ↓
deadline 到期或 drain 完成后，使 reporter/notifier 失效隔离
    ↓
释放 Connector 与 Runtime 协调资源
```

结果必须表达外部事实，而不是为了结束关闭而选择方便的状态：

- 明确完成外部效果的 item 报告 `SinkSucceeded`；
- Connector 能证明尚未提交或取消发生在外部效果之前的 item 报告 `SinkNotApplied`；内部
  buffer 在释放前必须先以该事实完成报告；
- 已发出请求但在 deadline 内无法确认效果的 item 报告 `SinkUnknown`；超时本身不能证明
  `NotApplied`；
- `Close` 返回的整体 error 只表达关闭过程或资源释放结果，不能替代每个已接管 item 的
  completion 事实，也不能授权静默丢弃 buffer。

`Close` 是有期限的尽力完成，不是无限等待。Connector 必须停止自身能够控制的 admission、
goroutine、timer、内部队列和 callback 注册，并在 deadline 内尽量收敛全部已接管 item；但
yaspe 不要求 Connector 证明外部客户端在 `Close` 返回后绝不触发迟到 callback。

Runtime 在关闭边界将相关 reporter 和 notifier 标记为失效（fence）。这里的 fence 是结果资格
隔离，不是终止 callback 或撤销外部效果：对象仍可被迟到 callback 安全调用，但 fence 生效后
尚未应用的事件只产生有界诊断，不得阻塞、panic、重新终结 work、释放 permit 或推进 safe
position。fence 与结果应用必须在同一串行协调路径中建立明确顺序，避免 callback 先观察 active、
再越过并发 fence 提交结果的 check-then-act 竞态。迟到 callback 自身持有的轻量 reporter 可以
自然存活，但 Runtime 不为它保留完整 work、Sink Coordinator 或 position tracker。

callback/push Source 采用同一责任划分：Connector 有界停止自身可控的接收与缓存，Runtime 在
admission/ownership 失效后拒绝迟到数据和 availability notification。外部回调线程始终不能
绕过 Connector 边界直接修改 Runtime 状态。

关闭时的本地完成进度与外部 Source commit 仍然分离。只有 fence 前已经应用、并在 split 内
形成连续前缀的成功结果才能推进 safe position；还必须成功持久化该位置，重启后才能从其后
恢复。例如 Kafka offset 120–140 已连续完成，而 141–150 在关闭期限内仍为 `NotApplied` 或
`Unknown`，最多提交 next offset 141；若该 commit 也未成功，恢复位置会早于 141，但绝不能
因为 Sink 曾接管 141–150 而跳到 151。`Unknown` 或尚未提交的成功效果可能在恢复后重复，这是
当前 at-least-once 边界。

## 8. 端到端回压与资源预算

### 8.1 permit 生命周期

Runtime 接受 Source 输入前取得 in-flight permit。permit 持续到输入终结，不能在以下时刻提前释放：

- Worker 完成计算；
- terminal output 形成；
- Sink 接受或入队；
- 外部请求发出。

只有记录进入策略允许的终态后才释放 permit。

### 8.2 回压链路

```text
Sink slows down
    ↓
Sink buffer/request/retry reaches limits
    ↓
Sink stops accepting eligible work
    ↓
terminal output and in-flight permits fill
    ↓
Runtime stops admitting source records
    ↓
Connector stops expanding business prefetch
```

Kafka session/control loop仍应继续运行。

### 8.3 第一版数量预算

端到端资源包括：

```text
Connector prefetch
+ Runtime input queue
+ running attempts
+ terminal output
+ Sink buffer
+ external requests
+ retry state and timers
```

第一版只按数量控制，不实现字节预算、动态借贷或单 work 输出数量限制。Runtime 公开的核心限制为：

```go
type RuntimeOptions struct {
    Parallelism      int
    MaxInFlightWorks int
}
```

- `Parallelism` 决定固定 Pipeline Worker goroutine 数量；Runtime 不为每个 work 创建 goroutine；
- `MaxInFlightWorks` 是端到端 work permit 总数，覆盖 input queue、Worker current、terminal queue、Coordinator current、Sink-owned in-flight 和 retry；
- work 在这些位置间移动时沿用同一个 permit，不重复计数；Worker 将 completed work 成功 Put 到 terminal queue 后即可执行下一条，但 permit 持续到 work terminal；
- input queue、terminal work queue 和 reporter wakeup 等局部容量由 Runtime 根据这两个值推导为有界内部默认值，第一版不作为用户配置；
- 第一版 `terminalWorkQueueCapacity = min(MaxInFlightWorks, 2 * Parallelism)`，用于吸收约两轮 Worker 同时完成的短暂突发；倍数是可通过 benchmark 调整的内部参数，未来改为 1 倍或其他值不改变公开语义；
- terminal queue 按 completed work 计数，一个 work 的完整 `[]SinkItem[T]` 只占一个 queue slot；
- Sink 通过自身 `MaxBufferedItems` 或等价配置限制已接管 item 数，Source Connector 通过 `PrefetchItems` 或等价配置限制未交接预取；
- retry 继续占用原 completion responsibility 和同一个 work permit。

第一版明确不保证单条 Record、单 work FlatMap 输出或系统总驻留数据的字节数有界。业务应为 Sink 配置足以原子接管正常 item group 的容量；永久超过 Sink 最大接管能力的 group 返回真实 error，不得无限 Backpressured。后续只有在实际数据证明需要时，再增加字节预算或 `MaxOutputsPerWork`。

## 9. Position 与第一版一致性保证

### 9.1 连续 safe position

记录可以乱序完成，但 Source position 只能推进到 split 内连续完成位置：

```text
100 complete
101 pending
102 complete
103 complete

safe frontier: 100
resume offset: 101
```

101 完成后可以一次推进到 103。Kafka Connector 将 Runtime safe position 转换为 Kafka offset commit。

### 9.2 generation fence

每次 partition ownership 带 generation。旧 ownership 的迟到 completion、Sink callback 或 commit 请求不能推进新 owner 的 position。

### 9.3 at-least-once

Sink 未明确成功时不能推进 position。结果未知时，为避免丢失，第一版选择重试或失败恢复，而不是提前确认。这可能产生重复。

第一版目标是边界明确的 at-least-once：

- 不因 Source 已读取、Collector 已接受或 Sink 已入队而提前确认；
- 崩溃后从未安全提交位置重放；
- 外部结果未知时优先避免丢失；
- 没有事务或幂等 Sink 时不承诺 exactly-once；
- 不可重放 Source 不保证无丢失恢复。

## 10. 失败、暂停与恢复

### 10.1 错误不直接等于退出

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

### 10.2 暂停

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

## 11. FailJob、取消与关闭

### 11.1 FailJob

FailJob 是用户策略认为当前 Job 不应继续恢复的最终动作。它终止当前 Runtime 并使 `Run` 返回根因错误，但不杀死嵌入 yaspe 的宿主进程。

最终终止时：

- 停止新读取、新交接、新重试和尚未开始的 work；
- 已被 Sink 接受但结果未定的操作在有限期限内等待；
- deadline 可配置且不得超过宿主更早的 deadline；
- 期限内成功继续更新 completion、safe position 并尽快提交；
- 到期仍未知的操作不标记成功；
- 未提交输入由可重放 Source 在后续执行中重放。

正常停止与 FailJob 共用同一套有界关闭协调机制，但收敛范围不同：

- 正常停止在期限内允许已开始的 work 完成 Chain 并把 terminal output 交给 Sink；
- 普通 FailJob 不启动 queued work，取消尚未把 terminal output 交给 Sink 的 started work，
  不再制造新的外部 effect；
- 两者都在期限内 drain 已由 Sink 接管的操作，并允许可信 completion 推进和
  提交 safe position；
- FailJob 的 `Run` 结果保留触发终止的根因。

### 11.2 panic 边界与分类

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
- `Run` 返回 `InternalPanicError`。这类情况必须被定位和修复，recover 不是继续运行的
  容错机制。

### 11.3 context 与阻塞点

宿主取消必须最终解除或终结 Runtime 管理的所有等待路径，包括：

- Connector 内部阻塞 I/O；
- Reader 可用性通知；
- permit 和队列容量；
- terminal output 容量；
- Sink 接收容量和 completion 等待；
- retry timer；
- graceful shutdown drain。

Runtime 退出后不得遗留 Source、Worker、Sink 或 retry goroutine。

## 12. Kafka Rebalance

Revoke 开始后，第一版暂停该 Kafka Source 所有 split 的新业务 admission，让现有资源优先
用于收尾；Kafka heartbeat、session 和必要的 poll/control 处理必须继续运行。暂停全局
admission 不等于撤销所有 ownership：只有 Kafka 报告的 revoked split 进入 drain、commit、
缓存清理和 generation fence，retained split 保留当前 ownership generation 和有界未交接
缓存，rebalance 收尾后恢复 admission 并优先交接已有缓存。

对于 revoked split，Runtime 冻结收尾集合：尚未开始 Operator Chain 的 work 不再启动；
已经开始的 work 允许在期限内完成 Chain、把 terminal output 原子交给 Sink，并等待 Sink
completion；已经被 Sink 接受的操作继续等待明确结果。正常零输出且已经完成的 work 保留
Success。这样可以尽量填补并发处理形成的 position gap，减少新 owner 重放已经产生 Sink
effect 的较大 position。在 ownership 失效前，Runtime 允许：

- 收敛已开始的 Chain；
- 将这些 Chain 成功形成的 terminal output 交给 Sink；
- 等待已被 Sink 接受的操作；
- 推进并提交连续 safe position。

默认 `RevokeDrainTimeout` 为 30 秒。实际收尾 deadline 取用户配置和 Kafka Connector 当前
可用 rebalance deadline 中较早者，并为最终 safe position commit、控制回调返回和协议推进
预留安全时间；收尾不得无限阻塞 rebalance。期限到期时取消仍未交给 Sink 的 work，Sink-owned
unknown 不得标记成功，只提交当时连续的 safe position。

Ownership 失效后：

- Connector 丢弃该 split 尚未交接的缓存；
- Runtime 不启动该 split 已接受但未执行的 work；
- 正在计算的 work 可以收到取消；
- 迟到 terminal output 不得转移给 Sink；
- 已被 Sink 接受的操作无法假定可撤销，但不得再推进当前 position；
- 已完成但未在旧 ownership 内提交的进度不得失效后补交。

新 owner 从最后成功持久化的 safe position 恢复。旧 ownership 已产生外部效果但未提交的记录可能重复，这是 at-least-once 边界。

Kafka offset 按 consumer group、topic 和 partition 独立保存，committed offset 表示下一条
应读取的 offset；Connector 负责把 Runtime 的最后连续完成 position 转换成该语义。被
revoked 的 split 即使重新分配给同一实例也创建新 generation、丢弃旧预取并从 committed
offset 恢复；retained split 不重置读取位置或 generation。

同一机制同时支持 eager 和 cooperative rebalance：eager 通常把全部当前 assignment 作为
revoked 集合处理，cooperative 只处理实际移动的子集。正确性不得依赖 cooperative 一定启用，
但生产默认可以优先使用 cooperative 以减少暂停和重放。Kafka 报告 split 已 lost 时，旧
ownership 可能已被其他 Consumer 接管，Connector 必须立即 fence generation、清理本地缓存
且不再提交旧 position，不执行正常 revoke drain。

## 13. 长期演进

### 13.1 Collector 与 Task/Operator Chain

未来若引入 Flink 式单线程 Task，内部 edge/output 可以按 Task 复用；当前输入身份可以由轻量 execution scope 提供。只要公开生命周期、所有权和并发契约不变，物理 Collector 可以内联、复用或池化。

### 13.2 Async Operator 与 Stage

真实 workload 若要求单条输入内部并行或独立 Stage 并行度，应由 Runtime 托管的 Async Operator、fan-out/fan-in 或显式 chain boundary 提供，而不是允许普通 Operator 任意并发使用 Collector。

### 13.3 checkpoint epoch

逐记录 completion 是无状态单进程阶段的轻量起点，长期演进为：

```text
per-record completion
    ↓ aggregate into checkpoint epoch
Source position snapshot
+ Operator state snapshot
+ required in-flight metadata
+ Sink prepare/committable
    ↓
checkpoint completion and recovery
```

端到端 exactly-once 仍需要 transactional 或 idempotent Sink。checkpoint 不会自动使任意外部副作用 exactly-once。

### 13.4 重新评估条件

- Collector 或 completion 的分配/调度成为主要性能瓶颈；
- 当前 Reader 边界无法支持重要 Source；
- 引入 keyed state、多输入 Join、Timer、shuffle 或网络 exchange；
- checkpoint barrier 需要调整 Source/Runtime 控制协议；
- 分布式 split 管理需要 Coordinator/Reader 模型；
- Sink 只能提供事务/epoch completion，无法提供逐 work completion；
- 性能数据证明当前责任交接或末端暂存造成不可接受且无法优化的开销。

## 14. 已接受的保证与非保证

### 14.1 第一版保证

- 外部物理 pull/push 差异由 Connector 适配，Operator 不感知；
- Connector 预取、Runtime 队列、attempt output、Sink buffer/request/retry 均有界；
- Sink 变慢最终耗尽 permit 并把回压传回 Source；
- Kafka 回压期间仍维护必要的 session/control；
- 每次 Process 获得逻辑独立 Collector；
- Collector 仅在调用 goroutine 中串行使用，Process 返回后失效；
- Source 已读取、Collector 已接受、Sink 已入队都不等于输入完成；
- work attempt 失败时，未转移给 Sink 的 terminal output 可撤销；
- Sink 整组接管一个 work 的 terminal output；
- position 只推进到 split 内连续允许终结的位置；
- 旧 generation 迟到结果不污染新 ownership；
- Runtime 管理的 goroutine 和等待路径最终响应取消并回收。
- Sink 关闭会在 deadline 内 drain 所有已接管 item，而不只处理当前外部请求；不能完成的 item
  按可证明事实进入 `SinkNotApplied` 或 `SinkUnknown`，不得静默丢弃；
- reporter/notifier 失效后仍可被迟到 callback 安全调用，但不得再推进 completion 或 position。

### 14.2 第一版不保证

- 并行度大于一时的全局输出顺序；
- 用户 Operator 内部副作用的撤销、幂等或事务；
- Sink 已接管后的通用回滚；
- 没有事务或幂等 Sink 时的 exactly-once；
- rebalance、结果未知和 position 提交前崩溃时完全无重复；
- 不可重放 Source 的无丢失恢复；
- 普通 Collector 的并发或异步使用。

## 15. 验证要求

### 15.1 Source 与背压

- 慢 Sink 最终阻止 Source 继续扩大读取或预取；
- 队列和 Sink 饱和时，work/item 数量和 goroutine 保持有界；第一版不保证字节数有界；
- Connector 内部预取数量可配置或有明确上限；字节上限属于后续增强；
- callback/push Source 也能通过有界适配层响应 admission；
- Kafka Connector 在业务回压期间仍满足 heartbeat/session 生命周期。
- 确定性竞态测试必须把数据发布分别注入到 `TryRead` 返回 `unavailable` 之前、之后以及
  Runtime 实际开始等待通知的两侧，证明不会丢失唤醒；
- 测试覆盖旧通知已占满 channel、Runtime 并发消费旧通知、通知合并、伪唤醒，以及
  end/failure/close 在等待窗口内发生的情形；
- 测试不得依赖调度器时序或 `time.Sleep` 碰撞竞态，必须用可控 hook/barrier 精确重现每个
  check-then-wait 交错。
- Memory Source 测试覆盖满缓冲 Submit 的背压和取消、Submit/Finish 线性化、finishing drain、
  Fail 优先级、首根因保留、Close 唤醒等待者与 Source-owned 缓存丢弃；
- 有限 Memory Source 的端到端测试必须证明 `Finish` 返回不会提前终止 Runtime，`Run` 只在
  已缓存和已接纳 work 及 Sink 都按契约收敛后返回 `nil`。
- admission 竞态测试必须把 context 取消分别注入到 reservation 之前、预留之后/读取之前、
  ready 返回与绑定之间、绑定之后/调度之前，证明只有空 reservation 可直接释放，
  ready 记录必须先登记再取消；
- 队列与 Worker 饱和测试必须证明 ready 后的绑定不再等待或失败，work 可在有界 pending
  状态中受追踪等待调度；
- 不可重放 Memory Source 的取消测试必须把未处理记录显式归类为 cancelled/未完成，不得
  把它误报为 success 或声称可恢复。

### 15.2 取消与资源回收

- context 取消可以解除所有 Runtime 管理的等待路径；
- Connector 阻塞 I/O 能被取消或通过受控关闭结束；
- Runtime 退出后不存在 Source、Worker、Sink 或 retry goroutine 泄漏；
- deadline 到期的未知 Sink operation 不被错误标记成功。
- `Close` 返回后 Connector 自身可控的 goroutine、timer、队列和 callback 注册已经有界收敛；
  外部客户端迟到调用只命中失效的轻量 reporter/notifier，不保留完整 Runtime 协调状态；

### 15.3 attempt、Sink 与 completion

- attempt 失败不会把 terminal output 部分交给 Sink；
- Emit ownership 契约测试覆盖成功后发送方不得复用、失败后仍可复用，以及内置 Operator 不在
  成功 Emit 后修改输出；测试不得宣称能够检测所有违反契约的用户代码；
- Sink 整组交接不会发生部分责任转移；
- 乱序、迟到和重复 callback 不会重复终结或释放 permit；
- 零输出 work 能直接完成；
- 多输出 work 只在全部必要 effect 完成后终结；
- Sink 入队、外部完成、输入终结和 position 提交可以分别观测和测试。
- 关闭测试覆盖已接管 buffer、外部 in-flight、deadline 前部分成功、可证明未生效、结果未知、
  fence 前后并发 callback，以及 `Close` error 不能替代逐 item completion；
- Memory Sink 单元测试覆盖整组成功、固定失败计划下的全组拒绝、零输出不调用 Sink、
  group 顺序与扁平视图，以及快照 slice 与内部 slice 结构隔离；
- Memory Sink 并发测试使用内部 hook/barrier 精确控制接管线性化前后、接管与 Close 竞争、
  失败触发 FailJob 时已进入与尚未进入的调用；不使用 `time.Sleep` 碰撞时序；
- `go test -race` 必须覆盖 Accept、运行期快照与 Close 并发。Close 返回后快照必须稳定，已成功
  groups 不得因其他 work 失败而回滚。测试 barrier 只属于内部测试设施，不进入公开 Sink API。

### 15.4 position 与 ownership

- 较大 position 先完成时不会越过前序空洞；
- generation 失效后任何迟到路径都被 fence；
- revoke 收尾有期限，不会无限阻塞；
- 旧 owner 不会为新 owner 提交迟到位置。

### 15.5 可测试边界

- Operator 可不依赖真实 Source/Sink 独立测试；
- Connector 可用假的 Runtime admission boundary 测试；
- Runtime 可用 Memory Reader/Sink 和可控时钟测试；
- 慢 Sink、结果未知、重复 callback、取消和 rebalance 可以确定性注入。

## 16. 当前开放问题

当前 M0 的退出目标是先收敛所有影响 M1/M2 公共 API、所有权、并发和恢复正确性的设计，
再开始 Runtime 与生产 Connector 编码。局部命名、私有类型组织和可由受约束原型验证的实现
选择不需要在文档中预先固定。

### 16.1 M1 实现前必须收敛

- M1 FailJob 的停止顺序、started work、terminal output 和根因传播；
- `JobBuilder`、`Stream[T]`、Transformation、factory、`Build` 的具体公开 API 和内部类型擦除边界；
- M1 指标、确定性测试、race/leak 测试和 benchmark 的实现前审核。

### 16.2 M2 实现前必须收敛

- Retry 的适用错误、backoff/jitter、次数或持续时间、耗尽动作和 Job 级恢复范围；
- Source split/position 的公共或内部表示，以及 Kafka committed offset 转换边界；
- Runtime Envelope 中 split、position、generation、work、attempt 和 completion identity 的组织；
- 非阻塞 Reader、availability notification、Source control event 和 Connector Open/Close 的最终接口；
- Sink `Open/Accept/Close`、原子接管、reporter、capacity notification 和 callback slice ownership 的最终接口；
- `SinkSucceeded`、`SinkNotApplied`、`SinkUnknown`、部分成功和迟到/重复 callback 的精确动作；
- Completion Tracker 的零/多输出、permit 释放、position gap 和 generation fence；
- Kafka 客户端适配、poll/pause/commit、assignment/revoke/lost 和 commit 失败规则；
- ClickHouse batch、flush、部分失败、unknown effect 和关闭 deadline；
- M2 指标、故障注入矩阵和 at-least-once 声明审核。

### 16.3 可由原型细化但不得改变语义的事项

- Transformation 私有接口、ID、引用和异构存储方式；
- Job/Source/Sink definition 与运行实例的最终 Go 类型名；
- 有界内部 queue 的具体数据结构和不改变公开保证的容量微调；
- 测试工具、fake clock 和 fault injection hook 的 package 组织。

## 17. 实现前审核点

实现或评审 M1/M2 时必须能回答：

- 一条记录从何时开始由 Runtime 承担 completion responsibility；
- 每层预取、队列、暂存、请求和 retry 的数量上限，以及哪些部分暂不承诺字节上限；
- attempt 失败时哪些输出可丢弃，哪些已转给 Sink；
- Sink 入队、外部完成、输入终结和 position 提交是否严格区分；
- 暂停、终止和 revoke 是否仍允许安全进度继续提交；
- 旧 generation 的所有迟到路径是否被 fence；
- Kafka session 是否独立于业务回压继续维持；
- 宿主取消是否能有界结束所有 Runtime 管理的 goroutine。

## 18. 设计形成与统一说明

本节记录多轮讨论分别形成的主要决定，以及合并时对潜在冲突采用的统一表述。

### 18.1 第一轮讨论形成的决定

- 为什么近期选择并行完整 Pipeline，而不是每 Operator Stage Worker；
- 每次 Process 逻辑独立 Collector；
- Collector 生命周期、单 goroutine 串行使用和非线程安全；
- Collector 物理复用与 Flink Task/Operator Chain 的长期方向；
- completion 的 processing/record/position 三层区分；
- permit 持续到 record terminal；
- 从逐记录 completion 演进到 checkpoint epoch；
- Async Operator 和显式 Stage 的重新评估方向。

### 18.2 第二轮讨论形成的决定

- 非阻塞 Reader 与 availability notification；
- split、ownership、generation、work attempt 术语；
- 同步 Chain 外层的 work-attempt 边界；
- 可撤销的有界 terminal output；
- Sink 对一个 work 最终输出的整组责任转移；
- Sink Connector 隐藏 pull/push、通过原子接管和通用容量通知完成交接；
- callback 事件由 Runtime 串行幂等应用；
- completion 的成功、证明未生效、结果未知三种事实；
- 暂停期间 position gap 前后 work 的处理；
- FailJob 有限关闭和 Kafka rebalance 收尾。

### 18.3 Source 架构决策补充的约束

- 外部系统物理 pull/push/callback 中立；
- Operator 不感知 Source 物理模型；
- callback Source 到非阻塞 Reader 的有界适配解释；
- Kafka 业务回压与 heartbeat/session 分离；
- 不同 Connector 可以采用不同 pause/resume/credit 机制；
- 取消必须覆盖 Connector 阻塞 I/O 和所有等待路径；
- Source 与 Operator 的独立测试边界；
- Source 边界的专项验证和重新评估条件。

### 18.4 线性 Job Definition 讨论形成的决定

- `JobBuilder` 持有逻辑定义，`Stream[T]` 是类型安全的 Transformation 句柄；
- 使用 Go 1.27 泛型方法表达 Map、Filter、FlatMap 等相邻类型关系；
- Transformation 是描述逻辑计算和拓扑关系的定义期对象，不是运行时 Operator；
- 内置 Transformation 共享用户函数值，但为每条 lane 创建独立 Operator 包装实例；
- 用户函数捕获和外部依赖的并发安全、幂等性与副作用不由实例隔离保证；
- `To` 添加 Sink Transformation，`Build` 校验并快照为不可变 Job；
- 第一版 Build 只接受单 Source、线性 Operator Chain 和单 Sink，内部引用模型保留未来 DAG 演进能力。
- Runtime 不提供 Skip/Discard record 动作或 Transformation `OnError`；可忽略业务错误由用户函数收敛为正常零输出；
- 未被用户函数吸收的 error 只进入 Job 级 Retry/FailJob 策略，正常零输出按 Success 完成并可推进连续 position。

### 18.5 近期候选方案与取舍

以下内容记录近期决定背后的主要理由。它不是新的执行规范；规范仍以上文对应章节为准。

- **共享 Operator 实例 vs. lane-local Operator 实例**：不共享包装 Operator，因为共享实例会把
  `Process` 并发、内部状态同步和锁成本强加给所有实现；选择每条 lane 独立实例，使一个实例
  始终串行调用。代价是需要保留实例创建信息，而且它无法隔离共享用户函数捕获的变量或外部
  依赖；这些对象的并发安全和副作用仍由用户负责。
- **线性 factory 切片 vs. Transformation 引用图**：不把当前路径复制成单纯 factory 切片，
  因为它会丢失节点身份和上游关系，使未来分支、合流和多 Sink 需要重做定义模型；选择
  Transformation 引用图，并在第一版 `Build` 时限制为线性结构。代价是内部需要处理异构节点、
  类型擦除和图校验。
- **`To` 立即产出 Job vs. `Build` 快照**：不让 `To` 封闭定义，因为这会把单 Sink 假设固化进
  fluent API；选择由 `Build` 统一校验并创建不可变快照。代价是缺失 Source、Sink 或非法结构
  要到 `Build` 才报告。
- **Runtime Discard/OnError vs. 用户函数正常零输出**：不增加 `SkipRecord`、`DiscardRecord`
  或逐 Transformation `OnError`，因为它们会建立第二套业务控制流、拆散计算与其局部错误处理，
  并使 fluent chain 充斥错误策略。选择由 Filter、FlatMap 或自定义 Operator 把可忽略业务情况
  表达为正常零输出；未吸收的 error 进入 Job 级恢复。代价是 Map 若要丢弃输入，需要改用
  FlatMap 或自定义 Operator；Dead Letter 与 Side Output 延后到图能力阶段讨论。
- **只暂停 revoked split vs. 暂停全部 Source admission**：第一版 revoke 期间暂停该 Source 的
  全部 admission，以减少新 work 对 drain 资源的竞争并简化正确性。代价是 retained split 也会
  暂时增加 lag；如果生产数据证明影响不可接受，再评估按 split admission。
- **取消全部未入 Sink work vs. 允许 started work 进入 Sink**：选择让已经开始执行的 revoked
  work 有限完成并进入 Sink，以填补 position 空洞、减少重放和重复；尚未开始的 work 不再启动。
  代价是 revoke 可能等待更久，因此必须受 drain deadline 限制。
- **固定无限等待 vs. 默认 30 秒有限 drain**：选择 30 秒作为初始默认值，在常见 Sink drain
  机会与 rebalance 可用性之间取平衡，并由 Connector 更早的实际 deadline 覆盖。该数值不是
  协议常量，应根据客户端约束、工作负载延迟和生产指标重新校准。

### 18.6 对潜在冲突的统一表述

Source 架构决策中的“Source 通过 Runtime 边界提交”和第二轮讨论中的“Runtime 从 Reader 取走”统一为：

> Connector 与 Runtime 之间执行有界责任交接；物理调度可以是 pull、push、callback、credit 或 notification + try-read。

Source 架构决策中的“背压由 Collector.Emit 传播”和第二轮讨论中的“permit/Sink capacity 传播”统一为：

> 背压属于 Runtime，通过 Source admission、permit、terminal output、Sink capacity 等全部有界边界共同传播；Collector.Emit 传播同步下游错误、取消和其所在边界的容量状态，但不承诺全部端到端背压都表现为 Emit 阻塞。
