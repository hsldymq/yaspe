# 0001：核心执行模型

状态：Accepted（已确定条款作为当前设计；“开放问题”仍待后续收敛）
最后更新：2026-08-22
适用阶段：M0–M2

> 本文件是当前正式 Design，由多轮设计讨论与
> [Source 数据进入架构决策](../decisions/0001-runtime-controlled-source-ingestion.md)
> 合并形成。

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

- M1：非阻塞 Memory Reader、有界 permit 和队列、完整 work-attempt 边界、末端暂存、同步 Memory Sink、FailJob/Skip 以及取消回收；
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
- Fail、Skip、Retry、Dead Letter；
- Source、Sink 和 Job 生命周期。

Operator 不创建 Worker Pool，不提交 Source position，也不决定 Job 级恢复策略。

### 2.2 关键术语

- **split**：可独立读取和推进 position 的 Source 分片；Kafka 中对应 topic-partition。
- **ownership**：当前 Runtime 对一个 split 的处理和提交权责。
- **generation**：同一 split 的一次 ownership 任期；重新获得同一 split 也是新任期。
- **generation fence**：拒绝旧任期的迟到完成或提交污染当前 ownership。
- **in-flight permit**：Runtime 接受一条尚未终结输入所占用的端到端容量名额。
- **work attempt**：一条 Runtime 已接受的原始输入执行完整同步 Operator Chain 的一次尝试。
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

### 4.3 可用性通知与控制事件

Reader 使用可等待的可用性通知避免忙轮询。通知只是提示；Runtime 取得 permit 后若未读到记录，必须归还名额。

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

Connector 预取必须同时在记录数和字节数上有界。Runtime 无容量时，Connector 可以阻塞、暂停业务读取、使用 credit、保留有界缓存或采用协议等价方式，但不得继续扩大积压。

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

- `Emit(nil)` 表示当前 Collector 已接受输出并取得后续责任；
- Collector 接受不等于最终 Sink 已经完成；
- `Emit` 失败表示本次输出未被接受；
- FlatMap 首次 Emit 失败后停止后续输出；
- context 取消不撤回此前已经成功接受的输出；
- `Emit` 必须能够传播下游同步处理错误、Runtime 取消和容量边界错误；
- 具体实现可以在同步 Chain 中直接调用下一个 Operator，也可以在明确边界处等待容量；
- yaspe 不承诺所有端到端背压都必须表现为 `Emit` 长期阻塞。

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

它不是事务日志，也不提供外部原子性。终端暂存的记录数和字节数必须纳入全局资源预算。

Operator 内自行产生的外部副作用不受末端暂存保护。重新执行 Chain 可能重复这些副作用，责任由 Operator 作者承担。

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

### 7.2 有界通知驱动交接

Runtime 与 Sink 之间使用有界、通知驱动的交接边界：

- Runtime 判断哪些 work 当前有资格产生 Sink effect；
- Sink 判断自身 buffer 和异步请求是否有容量；
- M2 首版由 Sink 在有容量时非阻塞取得一个完整 eligible work；
- 没有 work 或容量时等待状态变化通知，不得忙轮询；
- pull 是阶段调度策略，不是稳定公开语义；
- 未来可以改为 push、DAG edge 或跨进程 exchange，只要责任转移和有界语义不变。

### 7.3 异步 completion

Sink 的等待 buffer、并发请求、待重试项和 timer 都有上限。Worker 在 terminal output 被 Sink 整组接管后可以处理下一条输入，但父输入仍保持 in-flight，直到所有必要 Sink effect 明确完成。

外部异步 callback 不直接并发修改 work、completion、safe position 或 generation。callback 只报告事件，由 Runtime 协调路径串行且幂等地应用。

迟到、乱序或重复通知不得导致：

- work 重复终结；
- permit 重复释放；
- safe position 错误跨越空洞；
- 旧 generation 推进当前 position。

### 7.4 completion 结果

Sink completion 至少区分三种事实：

1. 确认成功；
2. 外部协议能够证明未生效；
3. 结果未知，可能已经生效。

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

### 8.3 总预算

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

全局预算至少需要考虑记录数和字节数。仅限制 channel 长度或记录数不足以约束大小差异很大的日志。retry 原则上继续占用原 completion responsibility 和资源预算。

具体预算分配、公平性和动态大小调整仍是开放问题。

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

### 11.2 context 与阻塞点

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

Revoke 开始后，Runtime 立即停止该 split 的新读取、新交接和尚未开始的 work。在 ownership 失效前，允许在有限期限内：

- 收敛已开始的 Chain；
- 等待已被 Sink 接受的操作；
- 推进并提交连续 safe position。

收尾不得无限阻塞 rebalance。

Ownership 失效后：

- Connector 丢弃该 split 尚未交接的缓存；
- Runtime 不启动该 split 已接受但未执行的 work；
- 正在计算的 work 可以收到取消；
- 迟到 terminal output 不得转移给 Sink；
- 已被 Sink 接受的操作无法假定可撤销，但不得再推进当前 position；
- 已完成但未在旧 ownership 内提交的进度不得失效后补交。

新 owner 从最后成功持久化的 safe position 恢复。旧 ownership 已产生外部效果但未提交的记录可能重复，这是 at-least-once 边界。

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
- 队列和 Sink 饱和时，记录数、字节数和 goroutine 保持有界；
- Connector 内部预取数量和字节可配置或有明确上限；
- callback/push Source 也能通过有界适配层响应 admission；
- Kafka Connector 在业务回压期间仍满足 heartbeat/session 生命周期。

### 15.2 取消与资源回收

- context 取消可以解除所有 Runtime 管理的等待路径；
- Connector 阻塞 I/O 能被取消或通过受控关闭结束；
- Runtime 退出后不存在 Source、Worker、Sink 或 retry goroutine 泄漏；
- deadline 到期的未知 Sink operation 不被错误标记成功。

### 15.3 attempt、Sink 与 completion

- attempt 失败不会把 terminal output 部分交给 Sink；
- Sink 整组交接不会发生部分责任转移；
- 乱序、迟到和重复 callback 不会重复终结或释放 permit；
- 零输出 work 能直接完成；
- 多输出 work 只在全部必要 effect 完成后终结；
- Sink 入队、外部完成、输入终结和 position 提交可以分别观测和测试。

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

- Sink 整组交接和 completion 对应的具体 Go API；
- 全局 in-flight budget 与 Connector 预取、input queue、attempt output 和 Sink buffer 的具体配额关系；
- `Record` metadata 与 Runtime Envelope 的边界；
- Emit 成功后的引用数据 ownership 与复制规则；
- Operator 实例是否允许被多个 Worker 并发调用，还是每个 lane 独立实例；
- 用户回调的线程安全契约；
- 第一版线性 Job Definition；
- Skip 是否为允许推进 position 的终态；
- Kafka revoke 的默认收尾期限；
- 失败策略的具体动作、等待节奏和恢复范围；
- `Collector.Emit` 的 context 长期显式传递还是绑定到 Process scope；
- M2 position 和 Runtime Envelope 使用泛型、opaque token 还是内部 adapter。

## 17. 实现前审核点

实现或评审 M1/M2 时必须能回答：

- 一条记录从何时开始由 Runtime 承担 completion responsibility；
- 每层预取、队列、暂存、请求和 retry 的数量/字节上限；
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
- Sink pull eligible work 的阶段调度方案；
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

### 18.4 对潜在冲突的统一表述

Source 架构决策中的“Source 通过 Runtime 边界提交”和第二轮讨论中的“Runtime 从 Reader 取走”统一为：

> Connector 与 Runtime 之间执行有界责任交接；物理调度可以是 pull、push、callback、credit 或 notification + try-read。

Source 架构决策中的“背压由 Collector.Emit 传播”和第二轮讨论中的“permit/Sink capacity 传播”统一为：

> 背压属于 Runtime，通过 Source admission、permit、terminal output、Sink capacity 等全部有界边界共同传播；Collector.Emit 传播同步下游错误、取消和其所在边界的容量状态，但不承诺全部端到端背压都表现为 Emit 阻塞。
