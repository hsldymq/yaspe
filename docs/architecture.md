# yaspe Living Architecture

文档状态：Living Document  
最后更新：2026-08-21
当前里程碑：M0 — 核心语义与项目基线  
关联文档：[Vision](vision.md) · [Roadmap](roadmap.md) · [Current Status](status.md)

## 1. 文档目的

本文保存 yaspe 的整体结构视图，描述引擎在不同演进阶段应当包含哪些核心概念、它们之间的关系、职责边界、所有权、生命周期和交互方式。

它主要服务两个场景：

- 指导当前和后续阶段的设计与编码，防止局部实现破坏整体边界；
- 在开发中断或开启新会话后，帮助开发者和协作者快速恢复项目上下文。

本文不是不可修改的最终蓝图。实际实现、真实工作负载和测试可能推翻当前假设。发生变化时，应同时更新本文、相关阶段设计、ADR 和测试，而不是让文档与代码长期分叉。

## 2. 如何阅读本文

本文使用三种成熟度标记：

| 标记 | 含义 |
|---|---|
| `Current` | 当前正在实现，或已经有代码事实 |
| `Planned` | 能力方向和主要职责已确定，但具体设计尚未完成 |
| `Exploratory` | 仅用于保持远期结构视野，是否实现及怎样实现仍待验证 |

还应区分三个层次：

```text
Architecture
    长期概念、关系和职责边界

Stage Design
    当前里程碑的具体选择、语义和取舍

Implementation
    Go 类型、方法、算法、goroutine、channel 和外部库
```

本文中的方框表示概念角色，不保证最终一定对应同名 Go struct 或 package。只有当前阶段设计接受后，概念才进入具体接口和实现。

## 3. 当前项目定位

yaspe 是一个使用 Go 编写的、类型安全、可嵌入的流处理引擎。

近期目标是以单进程 Runtime 承载 `lightning-log-filter` 的高吞吐、无状态、顺序无关 ETL。多个 Kubernetes Pod 通过 Kafka Consumer Group 协调 partition，每个 Pod 内运行一个相互独立的 yaspe Runtime。

远期目标包括 keyed state、checkpoint、event time、window、端到端一致性和 CEP。分布式控制平面目前只是探索方向。

## 4. 架构总览

长期概念架构分为四个平面：

```text
┌──────────────── Definition Plane ────────────────┐
│ User API → Job Definition → Logical Graph        │
└──────────────────────┬───────────────────────────┘
                       │ compile / validate
┌──────────────── Planning Plane ──────────────────┐
│ Planner → Physical Execution Graph               │
└──────────────────────┬───────────────────────────┘
                       │ instantiate
┌──────────────── Execution Plane ─────────────────┐
│ Runtime                                           │
│   Source → Mailbox/Edge → Task/Operator → Sink   │
│              Backpressure / Failure / Lifecycle   │
└──────────────────────┬───────────────────────────┘
                       │ coordinate / persist
┌──────────────── Reliability Plane ───────────────┐
│ Position · Completion · State · Time · Checkpoint │
└───────────────────────────────────────────────────┘
```

当前 M0/M1 只实现其中最小的一部分。图中出现远期组件不表示现在应该创建对应 package。

## 5. Definition Plane

Definition Plane 让用户表达“计算什么”，不直接决定 goroutine、队列和具体执行位置。

### 5.1 Job Definition

状态：`Planned`，M1 提供最小形式，M4 正式图化。

职责：

- 表达一份完整流处理作业；
- 持有 Source、Operator 连接关系、Sink 和作业级配置；
- 作为编译与运行入口；
- 保持声明式，不在构建过程中启动计算。

不负责：

- 不创建 Worker；
- 不直接读取外部数据；
- 不提交 Source position；
- 不持有运行期队列和客户端 session。

关系：

```text
Job Definition
├── one or more Sources
├── zero or more Operators
├── one or more Sinks
└── Job Options
```

第一版可以只支持线性 Pipeline，不应为了远期 DAG 在 M1 预建完整图优化器。

### 5.2 Typed Stream / DSL

状态：`Planned`。

职责：

- 使用 Go 泛型在编译期约束相邻 Operator 的输入输出类型；
- 提供 Map、Filter、FlatMap 等拓扑构建入口；
- 生成逻辑节点和边，而不是传输运行期数据。

不负责：

- 不启动 goroutine；
- 不保存实时 Record；
- 不提供背压；
- 不把 Kafka 或 ClickHouse 客户端暴露给用户 Operator。

Go 1.27 泛型方法适合构建用户 DSL，但 Runtime 内部不能依赖漂亮的链式 API 表示异构执行图。DSL 应编译为稳定的内部表示。

### 5.3 Logical Graph

状态：`Planned`，M4 正式引入。

职责：

- 保存用户定义的逻辑节点、边和稳定 identity；
- 验证拓扑完整性、类型兼容性和必要能力；
- 为状态恢复提供稳定 Operator identity；
- 与物理并行度、队列、goroutine 和网络位置解耦。

不负责：

- 不持有运行期 channel；
- 不持有 Connector session；
- 不记录瞬时处理状态；
- 不直接执行 Operator。

## 6. Planning Plane

### 6.1 Planner / Compiler

状态：`Planned`，M1 可能只有最小编译步骤，M4 正式引入。

职责：

- 验证 Job Definition；
- 将 Logical Graph 转换为 Physical Execution Graph；
- 决定安全的 Operator chaining；
- 应用并行度、分区方式和能力要求；
- 在运行前拒绝 nil function、断裂拓扑和不支持的组合。

不负责：

- 不运行 Job；
- 不在运行中处理业务 Record；
- 不实现故障策略；
- 不因为性能优化改变公开语义。

### 6.2 Physical Execution Graph

状态：`Planned`，M4 正式引入。

职责：

- 描述实际需要实例化的 execution nodes、tasks 和 edges；
- 表达节点并行度、forward/shuffle 关系和 chain；
- 作为 Runtime 的执行输入；
- 可被检查、测试和诊断。

不负责：

- 不拥有正在运行的 goroutine；
- 不保存 mutable operator state；
- 不直接执行 checkpoint。

关系：

```text
Logical Operator A ─┐
Logical Operator B ─┼─ Planner ─→ Chained Execution Node
Logical Operator C ─┘
```

Chaining 是执行优化，不应改变错误、输出和完成语义。

## 7. Execution Plane 核心概念

### 7.1 Record

状态：`Current`。

当前代码事实：

```text
Record[T]
└── Value T
```

职责：

- 表达 Operator 处理的类型化业务数据；
- 作为 Source、Operator 和 Sink 之间的业务值容器；
- 后续在明确设计后承载 event time 等属于数据本身的 metadata。

不负责：

- 不直接保存 Kafka consumer session；
- 不直接保存 retry attempt；
- 不直接保存 checkpoint ID；
- 不承担 Runtime acknowledgment；
- 不把 Kafka offset 当作所有 Source 的通用业务字段。

当前 `Record` 只有 `Value` 是刻意的最小设计。是否增加 event time、headers 或 key，需要在相关阶段设计中决定继承和变换语义。

### 7.2 Runtime Envelope

状态：`Planned`，M2 引入。

职责：

- 在 Runtime 内关联业务 Record 与执行 metadata；
- 保存来源 split、position、ownership generation 和完成跟踪所需信息；
- 让 Runtime 观察处理生命周期，而不污染业务 Operator API。

概念关系：

```text
Runtime Envelope[T]
├── Record[T]
├── Source Split Identity
├── Source Position
├── Ownership Generation
└── Completion Identity / Runtime Metadata
```

这只是概念模型。是否真的实现为一个 `Envelope[T]` struct、metadata 是否分层保存，留给 M2 设计决定。

### 7.3 Operator

状态：`Current`。

职责：

- 对一条输入 Record 执行业务计算；
- 通过 Collector 产生零条、一条或多条输出；
- 返回本次处理失败；
- 响应 Runtime 传入的 context。

不负责：

- 不自行创建 Worker Pool；
- 不决定并行度；
- 不拥有输入输出队列；
- 不决定 Fail、Skip、Retry 或 Dead Letter；
- 不提交 Source position；
- 不管理 Job 生命周期；
- 不恢复 panic 并静默继续；
- 不在 `Process` 返回后继续使用 Collector。

当前 Operator 关系：

```text
Runtime
  │ invokes
  v
Operator[I, O]
  │ emits 0..N
  v
Collector[O]
```

Map、Filter、FlatMap 是 Operator 语义的不同特化：

```text
Map      1 → 1
Filter   1 → 0..1
FlatMap  1 → 0..N (finite in early versions)
```

### 7.4 Collector

状态：`Current`，具体 Runtime 实现尚未出现。生命周期与并发决定见 [Core Execution Model](designs/0001-core-execution-model.md)。

职责：

- 接收 Operator 产生的输出；
- 将输出交给 Runtime 控制的下游边界；
- 传播背压、取消和下游接收错误；
- 在返回成功时取得该输出的后续处理责任。

不负责：

- 不决定重试；
- 不提交 Source position；
- 不等同于最终 Sink；
- 不允许 Operator 在 `Process` 返回后继续使用；
- 不允许 Operator 并发调用或跨 goroutine 使用同一个 Collector。

当前接受的语义：

- 第一版保留 `Collector.Emit(ctx, record)` 的显式 context 参数；
- 内置 Operator 默认将 `Process` 接收的 context 原样透传给 `Emit`；
- 普通 `Map`、`Filter` 和 `FlatMap` 构造 API 的使用方无需直接处理 context；
- 显式 context 为自定义 Operator 和未来算子保留使用派生 context 的能力，可在出现具体生命周期问题后重新评估；
- `Emit` 可以因为有界下游和背压而阻塞；
- context 用于解除阻塞和优雅取消；
- `Emit` 返回 `nil` 表示本次输出已被当前 Process 调用的 Collector 接受；
- `Emit` 返回错误表示本次输出未被接受；
- “被 Collector 接受”不等于“已经写入最终外部 Sink”。

FlatMap 的一次调用不是事务边界。它产生的多条输出是普通流记录，
按顺序逐条交给 Collector；首次 Emit 失败后不再发送剩余输出，
此前已被接受的输出在该次 Process 内仍然可见。Sink 可以为了吞吐和容量
组织物理 batch，但不应为了保留一次 FlatMap 的输出分组而改变 batch 边界。

M1 的线性同步 Pipeline 以一条 Runtime 已接受的输入作为一次 work attempt。
中间 Operator 通过 Collector 同步串联，不为每一级建立持久恢复缓存；Chain 的
最终输出在 attempt 成功前留在 Runtime 可撤销的末端边界，尚未转移给 Sink。
任一 Operator 失败时，Runtime 丢弃该 attempt 尚未转移的最终输出，并在策略
允许时用保留的原始输入重新执行整条 Chain。

Chain 成功后，最终输出才转移给 Sink；之后的失败优先在 Sink 边界恢复，
不重新执行 Operator Chain。末端暂存的数量和大小必须纳入端到端容量预算。
这一边界只用于减少 Runtime 能够明确避免的重复，不使 FlatMap 成为事务，
也不要求 Sink 保留 work attempt 的物理 batch 分组。

同一输入派生出的记录共同参与该输入的完成跟踪，但这种关联不等于
外部事务原子性。输出被 Sink 接受后不能假定可以撤回，重试仍可能产生重复。
更强的一致性应由可重放 Source、checkpoint 以及具备事务或幂等能力的 Sink
共同提供，而不是把 Collector 或 Sink batch 当作事务协议。

### 7.5 Source

状态：`Planned`，Memory Source 在 M1，Kafka Source 在 M2。

职责：

- 从有界或无界外部数据源读取数据；
- 将外部数据及来源位置转换为 Runtime 能理解的输入；
- 响应 Runtime 的容量、取消和 split lifecycle；
- 报告正常结束或读取失败。

不负责：

- 不根据“已经读取”判断业务处理完成；
- 不绕过 Runtime 直接调用业务 Operator；
- 不创建无限预取缓冲；
- 不自行决定最终提交位置；
- 不将外部客户端对象暴露给业务 Operator。

Source 采用受 Runtime 控制的数据进入模型，详见 ADR 0001：

```text
External Source
      │ connector-specific pull/push
      v
Source Connector
      │ bounded handoff
      v
Runtime
```

Connector 已从外部系统读取、但尚未完成受控交接的数据，仍由 Connector
持有，不占用 Runtime 的 record-level in-flight permit，也不进入 completion tracking。
这一边界允许 Kafka 批量 poll、网络预取和 callback Source 适配各自的物理读取模型，
但 Connector 内部的未交接数据仍必须在数量和字节上有界。

Job 临时暂停时，Connector 可以保留已读取的有界未交接数据，但不得
继续扩大预取；恢复后应先按 split 内原顺序交接这些数据，再继续读取新数据。
暂停业务交接不等于停止外部 session 维护；Kafka Connector 仍需维持必要的
heartbeat 或等价生命周期，但不能以此为由继续无界获取业务数据。

未交接数据始终从属于特定 split 的当前 ownership。split 被 revoke、
ownership 连续性无法确认或 Job 终止时，Connector 丢弃对应未交接数据；
丢弃不产生 completion，也不推进 position。即使同一 split 后来重新分配给同一
实例，也属于新 ownership，不复用旧 ownership 的未交接缓存。只有明确未发生
ownership 中断的 split 才可继续使用原缓存。

第一版面向 Runtime 采用非阻塞 Reader 模型。Reader 只立即返回已经可用的业务记录、
暂时无数据或 Source 结束等结果，不在 Runtime 的数据获取调用中等待外部 I/O。
暂时无数据时，Connector 通过可等待的可用性通知唤醒 Runtime，避免忙轮询。

外部系统的阻塞读取、批量 poll 和 session 维护由 Connector 内部适配，必要时可使用
专用 I/O goroutine。Runtime 只在获得 in-flight permit 后才从 Reader 取走记录；
成功取走即完成 Source 到 Runtime 的记录级责任交接。可用性通知只表示
“可能有数据”，如果 Runtime 取得 permit 后未能取到记录，应归还该容量。

业务记录与 Source 控制事件使用独立路径。可用性、正常结束、读取失败、
split assignment/revoke、ownership 变化和宿主取消不伪装成业务 Record，
在没有业务数据时也能及时唤醒 Runtime。会使读取资格失效的控制事件优先于
新的数据交接；一旦 Runtime 已知当前 ownership 失效，就不得再接受该 ownership 的记录。

这些条款固定 Source 驱动语义，不预先固定 Go 方法名、通知载体或内部缓冲实现。

### 7.6 Sink

状态：`Planned`，Memory Sink 在 M1，生产级 Sink 在 M2。

职责：

- 将处理结果写入外部系统或测试收集器；
- 明确一条或一批输出在什么时候完成；
- 报告写入、flush 和关闭错误；
- 在支持时参与幂等或事务协议。

不负责：

- 不隐瞒异步写入状态；
- 不把“进入 Sink 队列”报告为“外部写入完成”；
- 不独立宣称端到端 exactly-once；
- 不自行推进 Source position，除非通过 Runtime 协调协议。

Chain 成功后的最终输出先保留在 Runtime 的有界末端边界。一个 work 的输出在责任上
整组交接给 Sink：全部接受或全部不接受；这不要求它们在同一个物理 batch 中写入，
Sink 可以跨 work 组批。交接成功前由 Runtime 持有，成功后由 Sink 负责直到每个必要
外部效果得到明确结果。

Runtime 负责判断 work 是否具有交付资格，包括暂停、position gap 和 generation fence，
并负责选择、等待、公平性和重试调度。Sink Connector 只被动接收完整 work，不感知
Runtime 内部采用 pull、push、mailbox 还是 event loop。容量判断与整组责任接管必须原子完成：
接受后责任转给 Sink；发生回压时责任仍在 Runtime，且回压不等于处理失败。容量恢复由 Sink
通过 Runtime 提供的通用通知入口报告，Runtime 再次尝试交接，不使用忙轮询。未来可以改变
内部调度方式，只要不改变责任转移、有界背压和完成语义。

异步回调只向 Runtime 报告结果事件，由 Runtime 协调路径串行、幂等地更新 completion、
permit、safe position 和 generation 状态。结果区分确认成功、可证明未生效和可能已生效的
未知状态。若外部协议可靠支持逐项结果，可以保留成功部分并只重试未完成部分；否则按
整个 batch 处理是其特殊情况。重试与否仍由失败策略决定。

Collector 与 Sink 的区别：

```text
Collector
    Operator 的运行期输出边界

Sink
    拓扑中的终端处理角色，可能具有外部副作用
```

### 7.7 Runtime

状态：`Planned`，M1 实现第一版 Local Runtime。

职责：

- 实例化并驱动执行计划；
- 创建和管理 Source、Mailbox、Worker、Collector 和 Sink 生命周期；
- 控制并行度、在途数量和背压；
- 调用 Operator；
- 统一传播错误和取消；
- 应用 Failure Policy；
- 确保所有 Runtime goroutine 和资源最终被回收；
- 后续管理完成跟踪、状态、时间和 checkpoint。

不负责：

- 不实现业务转换；
- 不把 Connector 专有类型泄漏到 Operator；
- 不通过无限缓冲换取吞吐；
- 不在没有协议支持时宣称事务或 exactly-once。

### 7.8 Execution Task / Worker

状态：`Planned`，M1。

职责：

- 从 Runtime 控制的有界输入取得工作；
- 在当前 goroutine 中同步执行一条 record 的 Operator chain；
- 将结果和失败报告给 Runtime；
- 响应 Job 取消。

第一版并发模型：

```text
Different records       parallel across Workers
One record's chain      synchronous in one Worker
```

不负责：

- 不读取 Kafka；
- 不决定 offset commit；
- 不自行扩容；
- 不在业务 Operator 内继续派生无界 goroutine。

### 7.9 Mailbox / Edge

状态：`Planned`，M1 只需要最小有界输入边界，M4 正式成为图 Edge。

职责：

- 在执行节点之间传递数据或工作；
- 提供明确容量；
- 下游无法继续时传播背压；
- 在取消和关闭时解除阻塞。

不负责：

- 不静默丢弃记录；
- 不拥有业务错误策略；
- 不无限增长；
- 不自动把入队视为端到端处理完成。

### 7.10 Failure Policy

状态：`Planned`，M1 支持 FailJob/Skip，M2 扩展 Retry/Dead Letter。

职责：

- 根据错误阶段、记录上下文和 attempt 决定 Runtime 行为；
- 将 Operator 的“发生错误”与 Runtime 的“如何处置”分离。

候选终态：

```text
Success
Skipped
DeadLettered
Failed
Cancelled
```

候选动作：

```text
FailJob
SkipRecord
RetryRecord
SendToDeadLetter
```

重试不意味着回滚。只要此前已有 Emit 或外部副作用，就可能产生重复。策略必须了解失败阶段和下游能力。

当策略选择重试时，近期方向是优先暂停引入新数据，把恢复范围限制在
当前失败和已经受控进入的在途数据，避免持续扩大问题范围。重试可以在
时间上无限等待，是否根据次数、持续时间、当前时段或其他业务信息终止，
由用户的失败策略决定。但无论等待多久，保留的数据、goroutine、队列和其他
运行资源都必须有界，并且 Runtime 必须始终响应宿主取消。

暂停后，已经被 Runtime 接受但尚未开始的记录保留原记录和 in-flight
容量，不再分配给 Worker。已开始的 work 可继续完成当前 Chain 计算；对于
同一 split 中位于未解决失败之后的记录，其新的 Sink effect 在末端边界等待，
避免在已知无法推进 safe position 时制造可以避免的重复。位于失败之前、
能够填补连续完成缺口的记录可继续进入 Sink；不同 split 按各自的连续进度判断。
已被 Sink 接受的操作不撤回，继续等待明确结果。

暂停不冻结已完成进度。Runtime 继续处理 Sink completion、计算每个 split 的
连续 safe position，并在 ownership 仍有效时尽快持久化前进的安全位置。

同一暂停期间继续出现的失败纳入一次 Job 级恢复过程，而不由每条记录
创建不受协调的后台重试循环。每个失败仍保留独立的错误、记录上下文和
恢复状态；统一协调负责限制重试并发、避免对同一故障依赖形成惊群，并只在
所有阻塞项都进入允许继续的状态后恢复新数据。观察和报警可保留单条失败
细节，但应允许限频和按故障过程聚合。

具体失败动作集合、每条失败的等待节奏以及更细的恢复范围仍属于阶段设计问题，
不在本文预先固定为具体类型或接口。

## 8. Reliability Plane

### 8.1 Source Split

状态：`Planned`，M2。

职责：

- 表达可独立读取和推进位置的数据分片；
- 对 Kafka 对应 topic partition；
- 为文件 Source 可以对应文件或文件区间；
- 作为 position、ownership 和恢复的作用域。

### 8.2 Source Position

状态：`Planned`，M2。

职责：

- 表达特定 split 中的读取/恢复位置；
- 允许 Runtime 跟踪完成进度；
- 由 Connector 转换为外部系统 position。

需要区分：

```text
Record Position
    当前输入记录的位置

Resume Position
    恢复时下一条应读取的位置
```

这能避免 Kafka offset 的 off-by-one 含义泄漏到通用 Runtime。

### 8.3 Completion Tracker

状态：`Planned`，M2。

职责：

- 跟踪并发输入是否进入终态；
- 按 split 计算连续完成位置；
- 阻止较大 position 越过尚未完成的前序记录；
- 向 Source Connector 发布 safe resume position。

示例：

```text
position 100  in-flight
position 101  success
position 102  success

safe position 仍不能越过 100
```

### 8.4 Ownership Generation

状态：`Planned`，M2 Kafka Connector。

职责：

- 区分同一 split 的不同 ownership 生命周期；
- partition revoke 后拒绝旧 generation 的迟到提交；
- 为 rebalance 和 fencing 提供本地判断依据。

Ownership 表示当前 Runtime 对某个 split 的处理和提交权责，generation 用于区分
同一 split 在不同分配任期中的 ownership。Generation fence 只允许当前任期的
完成和提交影响当前进度，阻止旧任期的迟到 Worker、Sink callback 或 Connector
提交污染新 ownership。Fencing 只能阻止 Runtime 内部状态和 position 被旧任务更新，
不能撤销已经发生的外部 Sink effect。

### 8.5 State Backend

状态：`Planned`，M5。

职责：

- 保存 Operator State 和 Keyed State；
- 隔离 Job、Operator、namespace 和 key；
- 提供快照与恢复能力；
- 管理 serializer、schema、TTL 和资源使用。

Operator 不应直接依赖某个数据库或本地文件作为状态实现。

### 8.6 Checkpoint Coordinator

状态：`Planned`，M6。

职责：

- 创建 checkpoint identity；
- 协调 Source position、Operator state 和 Sink commit 状态；
- 判断 checkpoint 成功、失败或超时；
- 只发布完整一致的可恢复快照。

```text
Checkpoint Coordinator
      ├── Source Position Snapshot
      ├── Operator/Keyed State Snapshot
      └── Sink Prepare/Commit State
```

Checkpoint 提供一致恢复基础，不自动使任意外部 Sink exactly-once。

### 8.7 Time Service

状态：`Planned`，M7。

职责：

- 区分 processing time 和 event time；
- 传播和合并 watermark；
- 管理 processing/event-time timer；
- 让 timer 参与 checkpoint 和恢复；
- 为测试提供可控时间。

### 8.8 Sink Commit Protocol

状态：`Planned`，M9。

职责：

- 在支持的 Sink 中表达 prepare、commit、abort 和 recovery commit；
- 将 Sink transaction/batch identity 与 checkpoint 关联；
- 根据 Connector 能力准确声明 delivery guarantee。

不负责：

- 不为缺少事务或幂等能力的外部系统制造虚假 exactly-once；
- 不把 checkpoint 成功简单等同于外部副作用原子提交。

## 9. 当前阶段结构：M0

M0 当前代码结构：

```text
package yaspe
├── Record[T]
├── Collector[T]
└── Operator[I, O]

package operator
└── Map[I, O]
```

当前调用关系：

```text
Test / future Runtime
      │ Process(ctx, Record[I], Collector[O])
      v
Map[I, O]
      │ transform
      v
MapFunc[I, O]
      │ Emit(ctx, Record[O])
      v
Collector[O]
```

M0 应实现：

- 最小 `Record`；
- `Operator` 和 `Collector` 契约；
- Map 的正常、transform 失败、Emit 失败和 context 传播测试；
- 核心执行模型阶段设计；
- 关键 ADR 和文档入口。

M0 不应实现：

- Engine 巨型接口；
- Kafka；
- Worker Pool；
- checkpoint；
- 状态；
- 完整 DAG；
- 为未来组件创建空 package。

## 10. M1 目标结构：有界并发 Stateless Runtime

```text
Job Definition (initially linear)
          │ compile
          v
Local Execution Plan
          │
          v
Local Runtime
├── Source Runner
├── Bounded Input Mailbox
├── Worker Pool
├── Operator Chain(s)
├── Runtime Collectors
├── Failure Policy
└── Sink
```

数据路径：

```text
Memory Source
      │ bounded handoff
      v
Mailbox
      │
      ├── Worker 1 ─→ Operator Chain ─→ Sink
      ├── Worker 2 ─→ Operator Chain ─→ Sink
      └── Worker N ─→ Operator Chain ─→ Sink
```

M1 的关键限制：

- 不同 record 可以并行；
- 单条 record 的 chain 同步执行；
- 一条输入的整条 chain 构成一次 work attempt，最终输出在 attempt 成功前留在有界末端边界；
- attempt 失败时丢弃未转移的最终输出，不为每个 Operator 保留持久中间缓存；
- Source 面向 Runtime 使用非阻塞 Reader 和可等待的可用性通知，外部阻塞 I/O 在 Connector 内部适配；
- 输出顺序在并行度大于 1 时默认不保证；
- Source、队列和在途工作数量必须有界；
- Sink 在 M1 可以同步完成；
- FailJob 和 SkipRecord 由 Runtime 处理；
- 还没有生产级 Source position 和 checkpoint。

## 11. M2 目标结构：Position、Completion 与生产 Connector

```text
Kafka Consumer Group
          │ assignment / revocation
          v
Kafka Source Connector
├── Poll / Session Lifecycle
├── Split Ownership + Generation
└── Commit Adapter
          │ Runtime Envelope
          v
Local Runtime
├── Bounded Work
├── Workers
├── Completion Tracker
└── Failure Policy
          │
          v
Async Batching Sink
├── Bounded Queue
├── Batcher
├── Writer(s)
└── Completion Notification
          │
          v
ClickHouse
```

关键交互：

```text
Kafka record read
      ↓
Runtime processing
      ↓
all required Sink effects complete
      ↓
Completion Tracker marks terminal
      ↓
continuous safe position advances
      ↓
Kafka Connector commits resume offset
```

Sink 入队不是完成。Kafka 已读取也不是完成。

多 Pod 时：

```text
Kafka Group Coordinator
   ├── Pod A / yaspe Runtime A
   ├── Pod B / yaspe Runtime B
   └── Pod C / yaspe Runtime C
```

Pod 之间不直接通信；Kafka Consumer Group 负责 partition ownership，yaspe Runtime 负责记录完成，Kafka Connector 负责提交 safe position。

## 12. M4 目标结构：正式执行图与 KeyBy

```text
Typed DSL
   ↓
Logical Graph
   ↓ Planner
Physical Execution Graph
   ├── Forward Edge
   ├── Shuffle Edge
   ├── Chained Node
   └── Parallel Tasks
```

KeyBy 路由：

```text
Record[T]
   │ KeySelector
   v
Key
   │ Stable Partitioner
   v
Logical Partition
   │ ownership
   v
Execution Task
```

要求：同一 key 进入同一逻辑分区并保持声明的 key 内顺序，不同 key 可以并行。

## 13. M5–M6 目标结构：State 与 Checkpoint

```text
Execution Task
├── Operator
├── Key Context
├── State Access
└── Timer registration (later)
          │
          v
State Backend
          │ snapshot / restore
          v
Checkpoint Storage

Checkpoint Coordinator
├── Source positions
├── Operator/Keyed state
└── Runtime metadata
```

State ownership 必须与 logical partition ownership 对齐。恢复时，Source 不得从与状态快照矛盾的位置开始处理。

## 14. M7–M8 目标结构：Time、Window 与 Join

```text
Sources
   │ per-split watermarks
   v
Watermark Merge
   │
   v
Time Service ─── Timer State
   │
   ├── Window Operator
   ├── Aggregation Operator
   └── Join Operator
```

所有时间组件依赖可恢复 State 和 Timer，不应作为旁路 goroutine 实现。

## 15. M9 目标结构：端到端一致性

```text
Checkpoint N
├── Source position N
├── Operator state N
└── Sink transaction N
          │
          ├── prepare
          ├── commit
          ├── abort
          └── recovery commit
```

Kafka-to-Kafka 可以利用 Kafka transaction；Kafka-to-ClickHouse 可能只能通过稳定 ID、幂等写入、去重或 staging 获得业务可观察的 exactly-once 效果。每个 Connector 单独声明能力。

## 16. M10–M11 远期结构

### CEP

```text
Keyed Event Stream
      ↓
Pattern Compiler
      ↓
Pattern Runtime / NFA
├── Match State
├── Timer
├── Watermark
└── Checkpointed State
```

CEP 只有在 keyed state、event time、timer 和 checkpoint 稳定后才进入具体设计。

### Distributed Runtime

状态：`Exploratory`。

```text
Control Plane
├── Job Manager
├── Source Coordinator
├── Scheduler
└── Checkpoint Coordinator

Data Plane
├── Task Worker A
├── Task Worker B
├── Network Shuffle
└── State Placement
```

在此阶段前，yaspe 多 Pod 是多个独立单进程 Runtime，由 Kafka Consumer Group 等外部系统协调工作，不是 yaspe 自己的分布式集群。

## 17. 关键运行交互

### 17.1 正常处理

```text
Source reads input
   ↓
Runtime accepts bounded work
   ↓
Worker invokes Operator
   ↓
Operator emits through Collector
   ↓
Downstream/Sink accepts and completes
   ↓
Runtime marks input terminal
```

### 17.2 背压

```text
Sink slows down
   ↓
Sink buffer/request or end-to-end in-flight capacity exhausted
   ↓
new output submission or source admission blocks
   ↓
Workers and bounded mailbox stop making unbounded forward progress
   ↓
Mailbox fills to its finite capacity
   ↓
Source pauses or blocks bounded handoff
```

背压链路中任何缓冲都不能无限增长。

### 17.3 Operator 失败

```text
Operator returns error
   ↓
Runtime classifies failure stage
   ↓
Failure Policy
   ├── FailJob
   ├── SkipRecord
   ├── RetryRecord (when safe/allowed)
   └── Dead Letter (after DLQ succeeds)
```

Operator 不自行决定策略。M1 中 Operator Chain 失败会丢弃本次 work attempt
尚未转移给 Sink 的最终输出；已被 Sink 接受或用户在 Operator 内自行产生的
外部副作用不在该撤销边界内。

### 17.4 Job 取消和优雅停止

```text
Runtime receives cancellation / SIGTERM adapter
   ↓
stop accepting new source records
   ↓
cancel or drain in-flight work according to policy
   ↓
flush/close Sink within deadline
   ↓
commit only safe positions
   ↓
release Source ownership/session
   ↓
wait for all Runtime goroutines
   ↓
Run returns
```

当用户策略最终选择 FailJob，或宿主要求取消时，Runtime 停止新的重试、
新输入和尚未开始的 work。已被 Sink 接受但结果未定的操作可以在有限
关闭期限内等待明确结果。该期限允许用户配置并提供默认值；如果宿主给出
更早的 deadline，Runtime 不应超过它。

关闭期间完成的 Sink effect 正常更新 completion 和连续 safe position，安全位置
前进后应尽快提交。到期仍无法确认的操作不得标记为成功。Runtime 在释放
资源前最后尽力持久化当前安全进度，其余未确认记录由可重放 Source 在
后续执行中重新提供。

优雅停止不能替代故障恢复，因为进程仍可能被强制终止。

### 17.5 Kafka rebalance

```text
Kafka Connector receives revoke(split, generation)
   ↓
Runtime stops new records for that ownership
   ↓
drain or cancel split-scoped in-flight work
   ↓
compute safe continuous position
   ↓
commit when protocol lifecycle permits
   ↓
invalidate old generation
```

旧 generation 的迟到完成不能推进新 owner 的 position。

Revoke 开始后，Runtime 立即停止该 split 的新读取、新交接和尚未开始的 work。
在 ownership 失效前允许在有限期限内收敛已开始和已被 Sink 接受的操作，
并尽力提交连续 safe position。这一收尾不得无限阻塞 rebalance。

Ownership 失效后：

- Connector 丢弃该 split 尚未交接的本地缓存；
- Runtime 不再启动该 split 已接受但尚未执行的 work；
- 正在计算的 work 可被通知取消，其迟到完成的末端输出不得再转移给 Sink；
- 已被 Sink 接受的操作无法假定可以撤销，即使迟到成功也不得推进新 ownership 的 position；
- 已完成但未能在旧 ownership 有效期内提交的进度不得在失效后补交。

新 owner 从最后成功持久化的 safe position 恢复。未提交但已产生外部效果的记录
可能重复，这是当前 at-least-once 保证的已知边界，不得通过让旧 owner 跨
generation 提交来规避。

### 17.6 Checkpoint（远期）

```text
Coordinator starts checkpoint
   ↓
capture mutually consistent source positions and state
   ↓
prepare participating sinks
   ↓
persist complete checkpoint metadata
   ↓
notify completion / commit sinks
```

具体采用 stop-the-world、aligned barrier 或其他算法由 M6 设计决定。

## 18. Ownership 与生命周期规则

| 对象/概念 | 创建者 | 主要所有者 | 生命周期 |
|---|---|---|---|
| Job Definition | 用户 API | 调用方 | 构建到编译结束，可复用性待设计 |
| Logical Graph | DSL/Builder | Job Definition | 作业定义期 |
| Execution Graph | Planner | Runtime 启动流程 | 一次编译/运行版本 |
| Runtime | 调用方 | 调用方 | 一次 Job 运行 |
| Source Connector | Runtime/Factory | Runtime | Job 或 split ownership 生命周期 |
| Worker | Runtime | Runtime | Job 运行期 |
| Operator instance | Planner/Runtime | Execution Task | Task 生命周期 |
| Collector | Runtime/Execution Node | Runtime | 一次 Process 调用；物理复用属于实现细节 |
| Record | Source/Operator | 当前处理边界 | 随数据流转移 |
| Runtime Envelope | Source boundary | Runtime | 输入终结前 |
| Sink | Runtime/Factory | Runtime | Job/Task 生命周期 |
| Completion state | Runtime | Completion Tracker | position 可安全推进前 |
| Keyed State | State Backend | Runtime | checkpoint/retention 生命周期 |
| Checkpoint | Coordinator | Checkpoint Storage | retention policy 决定 |

所有权规则需要在实现阶段进一步精确化，尤其是 Record 是否允许复用底层字节、Collector 生命周期和 Sink batch 中的 ownership。

## 19. 推荐代码组织

当前只创建真实需要的 package。目标方向如下：

```text
yaspe/
├── record.go                 package yaspe：公共核心契约
├── operator.go               package yaspe：Operator/Collector 契约
├── job.go                    未来：公共 Job/DSL 入口
├── operator/                 内置类型安全 Operators
├── runtime/
│   └── local/                单进程 Runtime
├── graph/                    M4：逻辑/物理图
├── connector/
│   ├── memory/               测试和本地验证
│   ├── kafka/                M2
│   └── clickhouse/           M2
├── state/                    M5
├── checkpoint/               M6
├── streamtime/               M7，名称待定
├── cep/                      M10
└── internal/                 不承诺兼容的实现细节
```

### 19.1 依赖方向

推荐方向：

```text
public contracts (package yaspe)
       ↑           ↑
operators       connectors
       ↑           ↑
       └── runtime/planner ──┘
```

更准确地说，具体实现依赖稳定契约，核心契约不依赖具体 Connector 或 Runtime。

禁止的方向：

```text
package yaspe → Kafka client
Operator       → Runtime implementation
Operator       → ClickHouse client
Logical Graph  → channel/goroutine/session
State API      → specific backend implementation
```

如果出现循环依赖，不应优先通过新增大接口绕过，而应重新检查职责是否放错层。

### 19.2 不提前创建空目录

上面的目录是长期组织方向，不是立即创建清单。package 应在当前里程碑有第一个真实类型和测试时才创建。

## 20. 必须长期保持的架构不变量

1. Operator 描述计算，Runtime 控制执行。
2. 用户构建拓扑时不立即启动数据处理。
3. 所有队列、预取、在途记录和重试都有明确上限。
4. Source 已读取不等于输入已完成。
5. Collector 已接受不等于外部 Sink 已完成。
6. Sink 入队不等于外部副作用已完成。
7. 错误策略由 Runtime 统一应用，Operator 只报告错误。
8. work attempt 可以丢弃尚未转移给 Sink 的末端输出，但重试不隐含回滚已转移输出或外部副作用。
9. Source/Connector 专有类型不进入业务 Operator API。
10. 并行执行不默认提供全局顺序。
11. Source position 只能推进到连续终结的位置。
12. checkpoint 和 exactly-once 是不同层次的保证。
13. 任何一致性声明都说明边界、前提和故障模型。
14. Job 退出必须能回收 Runtime 管理的全部 goroutine 和资源。
15. 性能优化不能改变公开语义，除非通过新设计明确修改。
16. Runtime 与 Sink 的主动调度方向不是架构不变量，但完整 work 的责任交接、有界资源和 completion 事实不能因调度方式改变。

## 21. 当前开放问题

以下问题尚未定稿，应在阶段设计或原型中解决：

- `Record` 除 `Value` 外应包含哪些 metadata；
- 普通 Map 是否自动继承 event time、key 和 headers；
- Operator 是否长期保留为接口，还是以 function adapter 为主；
- Operator 实例是否由多个 Pipeline Worker 并发调用，还是每个 lane 独立实例；
- 第一版 Job Definition 是线性 Pipeline 还是最小 DAG；
- Skip 是否被视为允许推进 position 的终态；
- M2 的 Runtime Envelope 和 position 是否采用泛型、opaque token 或内部 adapter；
- Kafka rebalance 时允许多长时间 drain；
- 稳定 Operator identity 从何时开始强制要求。

这些问题出现在本文中不代表应当现在一次性解决。当前阶段只解决会影响当前代码的部分。

## 22. 文档更新规则

当实现或讨论改变架构时：

1. 判断变化属于架构、阶段设计还是局部实现；
2. 跨阶段职责变化更新本文；
3. 重要且长期有效的取舍新增 ADR；
4. 当前阶段具体方案更新对应 Design；
5. 代码行为变化必须有测试；
6. 更新 `status.md` 的当前进展、开放问题和下一步；
7. 被替代的 ADR/Design 标记 `Superseded`，不要抹除历史。

本文中的候选概念被实现后，应把其状态从 `Planned` 更新为 `Current`，并链接对应代码或 Design。被真实需求否定的概念应删除或标明替代方案。

## 23. 给新会话的最短上下文

如果只阅读一段，请使用以下摘要：

> yaspe 是一个 Go 1.27 的类型安全、可嵌入流处理引擎。当前处于 M0，已实现最小 `Record[T]`、`Operator[I,O]`、`Collector[T]` 以及 `Map`、`Filter`、`FlatMap`。近期目标是实现单进程、有界、record 级并行的 stateless Runtime，并用 `lightning-log-filter` 验证。Operator 只描述计算，Runtime 拥有并发、背压、错误、完成和生命周期。Source 面向 Runtime 采用非阻塞 Reader 和受控有界交接，外部阻塞 I/O 由 Connector 内部适配。成功 work 的输出通过有界边界整组向 Sink 转移责任；Sink Connector 不感知内部 pull/push，容量判断与整组责任接管原子完成，completion 仍由 Runtime 统一跟踪。多个 Kubernetes Pod 的 Kafka partition 分配早期交给 Kafka Consumer Group；yaspe Runtime 决定 safe position，Kafka Connector 执行 commit。不要提前实现 checkpoint、状态、完整 DAG 或分布式控制平面。所有设计仍可根据实现证据调整。
