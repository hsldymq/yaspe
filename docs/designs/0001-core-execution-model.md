# 0001：核心执行模型总览

状态：Accepted
最后更新：2026-08-26
适用阶段：M0–M2

> 本文件只维护跨能力的共同语言、总体执行形态、资源不变量、阶段保证和 Design 关系。
> 每项能力的完整行为契约见 [Design Map](design-map.md) 指向的权威 Design；当前进度和
> 唯一下一步见 [Current Status](../status.md)。

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

本总览固定跨能力的行为、责任、所有权和恢复语义，不固定 Go 方法名、channel 布局、状态
枚举或物理对象分配方式。只有对应能力 Design 明确声明已经定稿的公开 API 名称属于例外；
其中的参考代码仍按各文件自己的声明判断，不能反向把私有实现固化为契约。

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


## 4. 详细 Design 的权威边界

- [Job Definition 与 Runtime 实例化](0002-job-definition-and-runtime-instantiation.md)：type-state API、Factory、Build、生命周期、复用和类型擦除；
- [Source Reader、Admission 与 Memory Source](0003-source-reader-and-admission.md)：非阻塞读取、通知竞态、reservation、ownership 与 Memory Source；
- [Operator Attempt 与 Collector](0004-operator-attempt-and-collector.md)：Collector scope、Emit ownership、Chain 与 attempt；
- [Sink Handoff 与 Completion](0005-sink-handoff-and-completion.md)：整组交接、Memory Sink、异步 completion 与关闭；
- [Failure、Panic 与 Shutdown](0006-failure-panic-and-shutdown.md)：Failure Policy、FailJob、panic、错误因果与 deadline；
- [Position、Ownership 与 Kafka Rebalance](0007-position-and-kafka-rebalance.md)：safe position、generation 与 rebalance；
- [Runtime 验证与可观测性](0008-runtime-verification-and-observability.md)：指标、确定性测试、race/leak、fault 与 benchmark。

本总览可以摘要这些契约及其关系，但不复制完整规则。发生冲突时，以对应能力 Design 为
预期行为的权威来源；代码和测试仍是已实现行为的证据。

## 5. 端到端回压与资源预算

### 5.1 permit 生命周期

Runtime 接受 Source 输入前取得 in-flight permit。permit 持续到输入终结，不能在以下时刻提前释放：

- Worker 完成计算；
- terminal output 形成；
- Sink 接受或入队；
- 外部请求发出。

只有记录进入策略允许的终态后才释放 permit。

### 5.2 回压链路

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

### 5.3 第一版数量预算

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


## 6. 长期演进

### 6.1 Collector 与 Task/Operator Chain

未来若引入 Flink 式单线程 Task，内部 edge/output 可以按 Task 复用；当前输入身份可以由轻量 execution scope 提供。只要公开生命周期、所有权和并发契约不变，物理 Collector 可以内联、复用或池化。

### 6.2 Async Operator 与 Stage

真实 workload 若要求单条输入内部并行或独立 Stage 并行度，应由 Runtime 托管的 Async Operator、fan-out/fan-in 或显式 chain boundary 提供，而不是允许普通 Operator 任意并发使用 Collector。

### 6.3 checkpoint epoch

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

### 6.4 重新评估条件

- Collector 或 completion 的分配/调度成为主要性能瓶颈；
- 当前 Reader 边界无法支持重要 Source；
- 引入 keyed state、多输入 Join、Timer、shuffle 或网络 exchange；
- checkpoint barrier 需要调整 Source/Runtime 控制协议；
- 分布式 split 管理需要 Coordinator/Reader 模型；
- Sink 只能提供事务/epoch completion，无法提供逐 work completion；
- 性能数据证明当前责任交接或末端暂存造成不可接受且无法优化的开销。

## 7. 已接受的保证与非保证

### 7.1 第一版保证

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

### 7.2 第一版不保证

- 并行度大于一时的全局输出顺序；
- 用户 Operator 内部副作用的撤销、幂等或事务；
- Sink 已接管后的通用回滚；
- 没有事务或幂等 Sink 时的 exactly-once；
- rebalance、结果未知和 position 提交前崩溃时完全无重复；
- 不可重放 Source 的无丢失恢复；
- 普通 Collector 的并发或异步使用。


## 8. 设计形成与统一说明

本节记录多轮讨论分别形成的主要决定，以及合并时对潜在冲突采用的统一表述。

### 8.1 第一轮讨论形成的决定

- 为什么近期选择并行完整 Pipeline，而不是每 Operator Stage Worker；
- 每次 Process 逻辑独立 Collector；
- Collector 生命周期、单 goroutine 串行使用和非线程安全；
- Collector 物理复用与 Flink Task/Operator Chain 的长期方向；
- completion 的 processing/record/position 三层区分；
- permit 持续到 record terminal；
- 从逐记录 completion 演进到 checkpoint epoch；
- Async Operator 和显式 Stage 的重新评估方向。

### 8.2 第二轮讨论形成的决定

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

### 8.3 Source 架构决策补充的约束

- 外部系统物理 pull/push/callback 中立；
- Operator 不感知 Source 物理模型；
- callback Source 到非阻塞 Reader 的有界适配解释；
- Kafka 业务回压与 heartbeat/session 分离；
- 不同 Connector 可以采用不同 pause/resume/credit 机制；
- 取消必须覆盖 Connector 阻塞 I/O 和所有等待路径；
- Source 与 Operator 的独立测试边界；
- Source 边界的专项验证和重新评估条件。

### 8.4 线性 Job Definition 讨论形成的决定

- `JobDraft → Stream[T] → JobBuilder → Job` 用不同方法集合表达构建阶段；
- 使用 Go 1.27 泛型方法表达 Map、Filter、FlatMap 等相邻类型关系；
- Transformation 是描述逻辑计算和拓扑关系的定义期对象，不是运行时 Operator；
- 内置 Transformation 共享用户函数值，但为每条 lane 创建独立 Operator 包装实例；
- 用户函数捕获和外部依赖的并发安全、幂等性与副作用不由实例隔离保证；
- `From`/`Transform`/`SinkTo` 及 Func 变体接入 Factory，`SinkTo` 封口后由 `Build` 校验并
  快照为不可变 Job；
- Job 可重复并发运行，Runtime 一次性使用；私有 typed adapter 承担异构类型擦除；
- 第一版 Build 只接受单 Source、线性 Operator Chain 和单 Sink，内部引用模型保留未来 DAG 演进能力。
- Runtime 不提供 Skip/Discard record 动作或 Transformation `OnError`；可忽略业务错误由用户函数收敛为正常零输出；
- 未被用户函数吸收的 error 只进入 Job 级 Retry/FailJob 策略，正常零输出按 Success 完成并可推进连续 position。

### 8.5 近期候选方案与取舍

以下内容记录近期决定背后的主要理由。它不是新的执行规范；规范仍以上文对应章节为准。

- **共享 Operator 实例 vs. lane-local Operator 实例**：不共享包装 Operator，因为共享实例会把
  `Process` 并发、内部状态同步和锁成本强加给所有实现；选择每条 lane 独立实例，使一个实例
  始终串行调用。代价是需要保留实例创建信息，而且它无法隔离共享用户函数捕获的变量或外部
  依赖；这些对象的并发安全和副作用仍由用户负责。
- **线性 factory 切片 vs. Transformation 引用图**：不把当前路径复制成单纯 factory 切片，
  因为它会丢失节点身份和上游关系，使未来分支、合流和多 Sink 需要重做定义模型；选择
  Transformation 引用图，并在第一版 `Build` 时限制为线性结构。代价是内部需要处理异构节点、
  类型擦除和图校验。
- **共享可变 Builder vs. 不可变 type-state 派生**：选择不同阶段的方法集合和不可变派生，
  使缺失 Source、Sink 后继续转换等结构错误尽量在编译期排除，旧 Stream 仍能形成另一个
  独立 Job。代价是公开阶段类型更多，`Build` 仍需防御非法零值和内部不变量损坏。
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

### 8.6 对潜在冲突的统一表述

Source 架构决策中的“Source 通过 Runtime 边界提交”和第二轮讨论中的“Runtime 从 Reader 取走”统一为：

> Connector 与 Runtime 之间执行有界责任交接；物理调度可以是 pull、push、callback、credit 或 notification + try-read。

Source 架构决策中的“背压由 Collector.Emit 传播”和第二轮讨论中的“permit/Sink capacity 传播”统一为：

> 背压属于 Runtime，通过 Source admission、permit、terminal output、Sink capacity 等全部有界边界共同传播；Collector.Emit 传播同步下游错误、取消和其所在边界的容量状态，但不承诺全部端到端背压都表现为 Emit 阻塞。
