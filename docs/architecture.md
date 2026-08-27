# yaspe Living Architecture

文档状态：Living Document  
最后更新：2026-08-27
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

上述架构成熟度与项目统一的 Design、Implementation、Verification 三维能力状态不同；三维
状态和文档职责见 [Documentation Governance](governance.md)，当前事实见
[Current Status](status.md)。

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
- 由 type-state Job Definition API 持有 Source、Transformation、Sink 和作业级配置；
- 作为编译与运行入口；
- 保持声明式，不在构建过程中启动计算。

不负责：

- 不创建 Worker；
- 不直接读取外部数据；
- 不提交 Source position；
- 不持有运行期队列和客户端 session。

关系：

```text
JobDraft
└── From / FromFunc
    └── Stream[T]
        ├── Map / Filter / FlatMap
        ├── Transform / TransformFunc
        └── SinkTo / SinkToFunc
            └── JobBuilder.Build
                └── immutable Job
```

不同阶段类型只暴露合法方法；`SinkTo` 后不能继续转换。每次调用不可变地派生定义，旧
Stream 可用于形成另一个独立 Job，而不修改已有定义。`Build` 为当前逻辑拓扑创建不可变
Job 快照，不调用 Factory 或打开运行资源。第一版
Build 只接受恰好一个 Source、零个或多个 Operator Transformation、恰好一个 Sink 组成的
无分支线性 Pipeline。
内部保留 Transformation 身份和上游引用，未来可以放宽结构校验并增加图编译阶段，但不应
为了远期 DAG 在 M1 预建完整图优化器。公开泛型保证节点类型衔接，私有 typed adapter
完成 Runtime 所需的异构存储；类型断言失败属于引擎不变量缺陷。

Job 保存可重复、可并发创建运行实例的 Source/Operator/Sink Factory；Connector Builder
负责先冻结配置，Factory `Create()` 只构造未打开实例，外部初始化放在 `Open(ctx)`。一次
Run 创建一个 Source、每条 lane 一套 Operator Chain 和一个共享 Sink。Operator 可选择实现
独立的 `OperatorLifecycle`；启动按 Sink、Operator、Source 顺序 Open，失败或停止时逆序
清理。Job 可被不同 Runtime 重复并发执行，Runtime 本身是一次性执行容器。
完整契约见 [Job Definition Design](designs/0002-job-definition-and-runtime-instantiation.md)。

### 5.2 Typed Stream / DSL

状态：`Planned`。

职责：

- 使用 Go 泛型在编译期约束相邻 Operator 的输入输出类型；
- 使用 Go 1.27 泛型方法提供 `From`、Map、Filter、FlatMap、`Transform`、`SinkTo` 等 fluent
  构建入口；
- 以 `Stream[T]` 作为指向当前 Transformation 的类型安全句柄；
- 生成 Transformation 及其引用关系，而不是传输运行期数据。

不负责：

- 不启动 goroutine；
- 不保存实时 Record；
- 不在定义阶段创建一个由所有 lane 共享的运行时 Operator；
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
- 第一版只承载 `Value T`。

不负责：

- 不直接保存 Kafka consumer session；
- 不直接保存 retry attempt；
- 不直接保存 checkpoint ID；
- 不承担 Runtime acknowledgment；
- 不把 Kafka offset 当作所有 Source 的通用业务字段。

当前 `Record` 只有 `Value` 是刻意的最小设计。Source 特有且业务需要观察的信息由
deserializer 放入 `T`，而不是进入一个无类型的通用 metadata 容器。例如 Kafka Source
可以按配置产出纯消息值，也可以产出包含 key、headers、topic 和 timestamp 的业务类型。
Map 改变值类型时，只有 transform 明确保留在输出类型中的这些信息才继续向下游传播。

第一版不增加通用 event time。文件逐行读取等 Source 可能根本没有事件时间，而外部系统
提供的时间戳也未必等于业务事件时间。只有在 window、watermark 和 timer 等需求出现并
定义完整传播语义后，才重新评估可选 event-time 字段。

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
├── unpositioned | positioned(SplitID, Ownership, SourcePosition)
├── WorkID / current attempt
└── Completion state / Runtime metadata
```

Envelope 是 Runtime 私有的 work scope，不进入用户 API，也不沿 Pipeline 复制。Completion
直接属于 Work，不建立一对一的 Completion identity；是否真的实现为一个 `Envelope[T]`
struct、metadata 如何分层保存仍属于私有实现选择。完整契约见
[Position Design §1.3](designs/0007-position-and-kafka-rebalance.md#13-runtime-envelope-与-identity)。

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
- 不决定 Job 级 Retry 或 FailJob；可忽略业务错误由用户函数显式收敛为正常零输出；
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

状态：`Current`，具体 Runtime 实现尚未出现。生命周期与并发决定见
[Operator Attempt Design](designs/0004-operator-attempt-and-collector.md)。

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

- `Collector.Emit(record)` 不显式接收 context；Runtime 将当前 `Process` scope 绑定到
  Collector，`Emit` 使用该 context 解除背压和容量等待；
- 普通 `Map`、`Filter` 和 `FlatMap` 构造 API 的使用方无需直接处理 context；
- `Process` 仍显式接收 context，供用户计算和 I/O 使用，但用户不能替换 Collector 所属
  attempt 的取消边界；
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

这里 work 是一条已接管输入的端到端状态与责任载体，不是 goroutine；同一 work 重试时可以
经历多个 attempt。Runtime 预先创建固定 Pipeline Worker goroutine，第一版每个 Worker 对应
一个 execution slot，并顺序复用执行不同 work。completed work 成功进入 terminal queue 后，
Worker/slot 可以处理下一条，而该 work 的 in-flight permit 仍持续到最终终结。

Chain 成功后，最终输出才转移给 Sink；之后的失败优先在 Sink 边界恢复，
不重新执行 Operator Chain。末端暂存的数量和大小必须纳入端到端容量预算。
这一边界只用于减少 Runtime 能够明确避免的重复，不使 FlatMap 成为事务，
也不要求 Sink 保留 work attempt 的物理 batch 分组。

近期 Runtime 使用有界 terminal queue。队列满时，固定数量的 Pipeline Worker 可取消地阻塞
在整组 Put 上并继续持有当前 completed work；它们不直接调用 Sink。每个 Sink 的独立
Coordinator 消费 terminal queue 并调用 `Accept`，completion 路径也独立于这些 Worker，确保
即使所有 Worker 都被回压阻塞，Sink 容量仍能继续释放并向上游传播进度。
第一版内部默认容量为 `min(MaxInFlightWorks, 2 * Parallelism)`；这个倍数只用于吸收短暂
完成突发，是可以根据 benchmark 调整的实现参数，不属于稳定公开语义。

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
但 Connector 内部的未交接数据在第一版至少必须按数量有界；按字节限制属于后续增强。

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
通知是不可丢失的重新检查触发器，Connector 必须先发布 Reader 状态再通知，并消除
`TryRead -> unavailable -> wait` 的 check-then-wait 窗口；通知可合并、重复和过期，下一次
Reader 结果才是状态权威。完整不变量和竞态测试要求见
[Source Design §1.3](designs/0003-source-reader-and-admission.md#13-可用性通知与控制事件)。

外部系统的阻塞读取、批量 poll 和 session 维护由 Connector 内部适配，必要时可使用
专用 I/O goroutine。Runtime 只在获得 in-flight permit 后才从 Reader 取走记录；
成功取走即完成 Source 到 Runtime 的记录级责任交接。可用性通知只表示
“可能有数据”，如果 Runtime 取得 permit 后未能取到记录，应归还该容量。

Runtime 在读取前预留包含 permit、work identity、追踪与调度容量的完整 admission
reservation。Reader 返回 ready 是 ownership 与 completion responsibility 转移点；Runtime
必须立即以不阻塞、无普通失败且不调用用户代码的操作把记录绑定到 reservation，
之后才可观察取消或调度 Worker。详细顺序与故障边界见
[Source Design §1.4](designs/0003-source-reader-and-admission.md#14-source-admission-与所有权)。

业务记录与 Source 控制事件使用独立路径。可用性、正常结束、读取失败、
split assignment/revoke、ownership 变化和宿主取消不伪装成业务 Record，
在没有业务数据时也能及时唤醒 Runtime。会使读取资格失效的控制事件优先于
新的数据交接；一旦 Runtime 已知当前 ownership 失效，就不得再接受该 ownership 的记录。

这些条款固定 Source 驱动语义，不预先固定 Go 方法名、通知载体或内部缓冲实现。

M1 已固定非阻塞读取在语义上返回 `ReadResult[T], error`；Design 中的 `TryRead` 和
`Available` 只是参考名称，容量为 1 的 channel 也只是参考通知实现，不是对最终公开
Go API 或通知载体的定稿。任何最终实现都必须保持不丢失唤醒的语义。`ReadResult` 的
正常状态为 ready、unavailable 和 finished，零值/unknown state 通过
`InvalidReadResultError` 进入 Job 级 FailJob 路径；读取 error 与正常状态分开返回。正常结束先
drain Connector 缓存再呈现永久 finished，读取失败则优先于尚未交接的缓存数据。
完整结果和错误契约见 [Source Design §1.2](designs/0003-source-reader-and-admission.md#12-非阻塞-reader)。

M1 Memory Source 是 Runtime 参考实现、确定性测试设施、benchmark 输入和本地示例数据源，
不是生产级队列。它使用动态有界缓冲并分离 Runtime-facing Source 与 producer-facing
Controller；Controller 提供可取消的背压提交、正常结束和失败注入语义，Close 仍由
Runtime 管理。正常结束 drain 已缓存记录，失败优先于尚未交接缓存；所有并发操作
在同一生命周期状态机上线性化。完整定位、ownership、终态竞争和测试契约见
[Source Design §1.7](designs/0003-source-reader-and-admission.md#17-m1-memory-source)。方法名和具体公开
Go API 仍留待实现阶段商议。

Source 数据进入业务类型的边界固定为：Connector 读取外部原始数据，配置的
deserializer/parser 产生业务值 `T`，并在正式交接前由 Connector 有界持有。Runtime 取得
in-flight permit 后通过非阻塞 Reader 取走 `T`，再统一创建 `Record[T]`、内部 Envelope 和
Work；Source Connector 与 deserializer 都不创建或解释 Runtime Envelope。split、position、
ownership generation、work identity、attempt、completion 和 permit 等正确性 metadata
永远留在 Runtime 内部。具体 Reader 接口与方法名留待 Source API 设计确定。

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

M1 Memory Sink 是同步 Runtime 参考 Sink、结果断言工具、benchmark 终点和本地示例输出。
它每次全收或全拒一个 work 的全部 terminal outputs，成功返回即同步完成并转移整组
ownership；零输出不调用 Sink。Memory Sink 保留 work groups，提供组视图和扁平只读快照，
支持固定确定性失败计划，并在同一线性化边界上协调并发接管、快照与 Close。
Close 同步、幂等且不清空结果；没有异步 completion、外部 in-flight、flush 或跨 work 回滚。
详细契约与确定性并发测试要求见
[Sink Design §1.1.1](designs/0005-sink-handoff-and-completion.md#111-m1-同步-memory-sink)。参考方法名
不锁定最终公开 Go API。

Chain 成功后的最终输出先保留在 Runtime 的有界末端边界。一个 work 的输出在责任上
整组交接给 Sink：全部接受或全部不接受；这不要求它们在同一个物理 batch 中写入，
Sink 可以跨 work 组批。交接成功前由 Runtime 持有，成功后由 Sink 负责直到每个必要
外部效果得到明确结果。

稳定的 Connector 边界不暴露 Runtime 内部 `Work`。Runtime 将每个 terminal output 包装为
`SinkItem[T]`，其中包含业务 `Record[T]` 和 completion 使用的不透明身份；Sink 接收一个 work
产生的完整 `[]SinkItem[T]`，其中 `T` 与 Sink 输入类型在编译期匹配。Connector 使用
`item.Record` 生成目标系统请求，并在 callback 时通过 reporter 原样返回对应 item，无需维护
index 或比较业务值。attempt、generation、position 和 completion 等内部状态留在 Runtime，
异步结果通过 `Accept` 每次交接传入的绑定式 `SinkResultReporter` 返回。该 reporter 不暴露
work ID，可以在 `Accept` 返回后保存并跨 goroutine 使用；只有 `SinkAccepted, nil` 才使其
有效。Runtime 必须把 callback 事件投递到协调路径串行应用，并处理 callback 早于 `Accept`
返回的竞态。容量恢复是 Sink 级通知，不属于某个 work reporter。

Runtime 为每个 Sink 实例创建 `SinkContext` 并调用一次 `Open(SinkContext)`，只有成功后才开放
业务数据；运行期仅 Sink Coordinator 调用 `Accept`，结束时 Runtime 最多调用一次
`Close(context.Context)`。`SinkContext` 第一版提供 `LifecycleContext() context.Context` 和
`CapacityNotifier()`：前者覆盖 Sink 运行生命周期，可由 Sink 后台任务保存，但 cancel function
只归 Runtime；后者返回 `SinkCapacityNotifier`。Sink 在观察到容量恢复或增加时调用其
`NotifyAvailable()`，报告调用瞬间存在可用容量。该通知不预留容量，也不保证稍后的 Accept
成功；Runtime 把它作为重新检查触发器并容忍重复、合并和到达时已过时的通知。Accept 的
context 只控制单次接管调用，成功接管的
异步操作不从属于它；Close 使用独立 context 控制有限收尾。

`SinkResultReporter[T]` 通过 `Report([]SinkItemResult[T])` 增量或批量报告 item 结果，结果状态为
`SinkSucceeded`、`SinkNotApplied` 或 `SinkUnknown`。成功结果必须具有 nil `Err`，后两者必须
具有非 nil `Err`；没有底层异常时使用 yaspe 的标准哨兵错误。`Report` 不返回 error，调用时
results slice 的所有权转给 Runtime，Connector 此后不得读取、修改或复用该 slice 及其
backing array。Runtime 因此可以不复制地把事件投递到协调路径。

Runtime 负责判断 work 是否具有交付资格，包括暂停、position gap 和 generation fence，
并负责选择、等待、公平性和重试调度。Sink Connector 只被动接收完整 items group，不感知
Runtime 内部采用 pull、push、mailbox 还是 event loop。容量判断与整组责任接管必须原子完成：
交接方法返回 `(SinkAcceptStatus, error)`；`SinkAccepted, nil` 后责任转给 Sink，
`SinkBackpressured, nil` 时责任仍在 Runtime，任何非 `nil` error 也表示一个输出都未接管且
status 被忽略。回压不等于处理失败。容量恢复由 Sink 通过 Runtime 提供的通用通知入口报告，
Runtime 再次尝试交接，不使用忙轮询。未来可以改变内部调度方式，只要不改变责任转移、
有界背压和完成语义。

容量恢复通知采用单调 version，而不是 available 布尔状态。Coordinator 在 `Accept` 前观察
version；收到 `SinkBackpressured` 后，若 version 已变化则立即重试，否则原子等待后续版本。
内部的版本检查与等待必须消除 check-then-wait 窗口，使通知即使早于 `Accept` 返回也不会
丢失。通知允许合并和伪唤醒，`Accept` 的下一次原子结果仍是容量事实的最终依据。

异步回调只向 Runtime 报告结果事件，由 Runtime 协调路径串行、幂等地更新 completion、
permit、safe position 和 generation 状态。结果区分确认成功、可证明未生效和可能已生效的
未知状态。Sink 接管后自行决定是否以及怎样 Retry，只向 Runtime 报告最终 outcome；Runtime
不解析 error、不重新提交 item，也不重新执行 Operator Chain。首个最终失败立即触发 FailJob，
其余已接管 item 在统一 shutdown deadline 内有限收敛。

外部客户端 callback 只能调用 reporter/notifier 提交事实。Capacity 通过调用期间同步推进的
版本化 signal 唤醒 Coordinator；completion 进入每个 reporter 独立且有界的 result inbox。
同一 reporter 的多次增量 Report 合并为最多一个 pending wakeup，Coordinator drain 后才允许
安排下一次；重复或非法结果在进入 pending 存储前丢弃。活跃 reporter 数量受全局 in-flight
上限约束。Sink Coordinator 聚合 item outcome 为 work-level completion 后再交给 Completion
Tracker，由后者释放 permit 并维护 position；两类入口无需共用普通 FIFO event channel。

未来的 Connector 层通用异步 Sink 基础设施可以参考 Flink，在提交物理 batch 时提供请求级
result handler，并负责内部 Retry 以及把一个物理请求的最终结果聚合回一个或多个 work
reporter；该层级不改变稳定的 `Accept(items, reporter)` 边界，也不把中间失败冒充最终事实。

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
- 应用 Operator Work Failure Policy；
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

一个 Pipeline Worker、一个 execution slot 和该通道独占的 Operator Chain 实例共同组成一条
`execution lane`。每条 lane 独立创建 Operator Chain，不与其他 lane 共享 Operator 实例；
同一实例只由所属 Worker 串行调用。这样普通 Operator 不需要为 `Process` 并发调用加锁，
并为未来的实例局部状态保留清晰边界。Job Definition 中的 Transformation 描述如何为每条
lane 创建 Chain。内置 Transformation 可以共享用户函数值，但每条 lane 的 Operator 包装
实例独立；函数捕获和外部依赖的并发安全、幂等性及副作用仍由用户负责。

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

### 7.10 Operator Work Failure Policy

状态：`Planned`，M1 支持 FailJob，M2 扩展受控 Retry。

职责：

- 根据未被用户函数吸收的错误、失败阶段和 attempt 决定 Runtime 行为；
- 将 Operator 的“发生错误”与 Runtime 的“如何处置”分离。

该策略只处理 Operator work attempt，不覆盖 Source read error、Sink 最终失败或 Runtime 内部
错误；这些没有安全 work Retry 单位的错误固定进入 FailJob。

终态：

```text
Success
Failed
Cancelled
```

动作：

```text
FailJob
RetryWork
```

Runtime 不提供 Skip/Discard record 动作，也不在 Transformation 上提供 `OnError`。Filter
不保留、FlatMap 返回零输出或自定义 Operator 不 Emit 并返回 nil 都是正常 Success；用户以
这些方式在业务逻辑附近吸收可忽略错误。未被吸收的 Operator error 才进入 Job 级 Operator
Work Failure Policy。
未来 Dead Letter 应建模为显式业务输出、Side Output、分支或专用 Sink，而不是失败终态。

重试不意味着回滚。只要此前已有 Emit 或外部副作用，就可能产生重复。策略必须了解失败阶段和下游能力。

当 Job 策略选择 Retry 时，Runtime 先原子暂停整个 Source 的新业务 admission，把恢复范围
限制在当前失败和已经受控进入的在途数据，避免持续扩大问题范围。暂停不按 split、partition
或 position 选择，因为 work Retry 不能假设 Source 提供这些概念。Retry 可以配置有限次数、
有限持续时间、两者组合或显式无限；无论等待多久，保留的数据、goroutine、timer、队列和其他
运行资源都必须有界，并且 Runtime 必须始终响应宿主取消。

完整错误、panic 与停止契约见
[Failure Design](designs/0006-failure-panic-and-shutdown.md)。

暂停后，已经被 Runtime 接受的记录继续竞争 execution lane、完成 Chain 并尽量进入 Sink；
Runtime 只是不再用新 Source record 填补空闲 lane。retry work 在 backoff 中保留输入、permit 和
completion responsibility，但不占 lane；不同 retry work 可在 Parallelism 限制内并发执行。
所有 retry blocker 成功消失后才恢复 Source admission，有限预算耗尽则 FailJob。

暂停不冻结已完成进度。Runtime 继续处理 Sink completion、计算每个 split 的
连续 safe position，并在 ownership 仍有效时尽快持久化前进的安全位置。

同一暂停期间继续出现的失败进入统一的 active failure set，而不由每条记录创建不受协调的
后台重试循环。每个 active failed work 保留第一次错误和有界 attempt 摘要；恢复成功后移除，
任一 work 耗尽时以触发项为 primary，并快照当时仍活跃的 failure collection。公开错误集合
形态仍属于后续阶段问题，不在本文预先固定为具体类型或接口。

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
      │ Emit(Record[O])
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
├── Operator Work Failure Policy
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
- 未处理 error 由 Runtime 以 FailJob 处置；业务可忽略错误由用户函数收敛为正常零输出；
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
└── Operator Work Failure Policy
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
Operator Work Failure Policy
   ├── RetryWork (when safe/allowed)
   └── FailJob
```

用户函数可以把可忽略的业务错误转换为正常零输出；未被吸收的 error 不允许由 Runtime
静默 discard。M1 中 Operator Chain 失败会丢弃本次 work attempt
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

正常停止允许 started work 在期限内完成并交给 Sink；普通 FailJob 不启动
queued work，取消尚未交给 Sink 的 started work，丢弃尚未开始 Sink handoff 的 terminal
outputs；已经进入 Sink 的调用则在统一 shutdown deadline 内收敛。普通 FailJob 不由
position gap 驱动额外 Operator drain，position tracker 只消费最终 completion 事实。
第一个触发 FailJob 的 error 始终是主根因，停止期间的 Close、超时和 panic 等错误作为
可通过 `errors.Is/As` 识别的附加错误。若 yaspe 内部 panic 首先触发终止，Runtime 只做
有界资源收尾且不再推进或提交 position；若 panic 发生在已有 FailJob 的收尾期间，则完整
panic value 和 stack 作为高严重度附加错误保留，而不改写原始因果顺序。详细契约见
[Failure Design §2](designs/0006-failure-panic-and-shutdown.md#2-failjob取消与关闭)。

优雅停止不能替代故障恢复，因为进程仍可能被强制终止。

### 17.5 Kafka rebalance

```text
Kafka Connector receives revoke(splits, generation, deadline)
   ↓
Runtime pauses all new business admission for this Kafka Source
   ↓
for revoked splits: drain started/Sink-owned work; do not start queued work
   ↓
compute safe continuous position
   ↓
commit when protocol lifecycle permits
   ↓
invalidate old generation
   ↓
resume retained/newly assigned splits
```

旧 generation 的迟到完成不能推进新 owner 的 position。

Revoke 开始后，第一版暂停该 Kafka Source 所有 split 的新业务 admission，避免 retained split
继续占用 Worker、permit 和 Sink capacity；heartbeat、session 和必要的 poll/control 仍继续。
只有 revoked split 进入收尾，retained split 保留 ownership generation 和有界预取，收尾后
恢复。尚未开始的 revoked work 不再启动；已开始的 work 可以完成 Chain、进入 Sink 并等待
completion，已有 Sink-owned work 同样有限等待，以尽量填补 position gap、减少重放。

默认 `RevokeDrainTimeout` 为 30 秒，实际 deadline 不得超过 Connector 从 Kafka 协议和客户
端生命周期获得的更早期限，并需为最终 commit 和控制回调返回预留安全时间。到期未知的
Sink operation 不得标记成功，只提交连续 safe position。

Ownership 失效后：

- Connector 丢弃该 split 尚未交接的本地缓存；
- Runtime 不再启动该 split 已接受但尚未执行的 work；
- 正在计算的 work 可被通知取消，其迟到完成的末端输出不得再转移给 Sink；
- 已被 Sink 接受的操作无法假定可以撤销，即使迟到成功也不得推进新 ownership 的 position；
- 已完成但未能在旧 ownership 有效期内提交的进度不得在失效后补交。

Eager rebalance 把全部 revoked assignment 交给同一流程；cooperative rebalance 只处理实际
移动的子集。被 revoke 的 split 即使重新分配给同一实例也创建新 generation，并从 Kafka
committed offset 恢复；retained split 不重置。split lost 表示 ownership 可能已经转移，
此时立即 fence、清理且不再提交旧 position，不执行正常 drain。

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
| JobDraft / Stream / JobBuilder | 用户 API | 调用方 | 不可变派生的 type-state 定义期 |
| Transformation | DSL/Builder | Job Definition API | 作业定义期；持有逻辑身份和引用关系 |
| Job Definition | JobBuilder.Build | 调用方 | Build 时不可变快照，可独立于 Builder 使用 |
| Logical Graph | Builder/Compiler | Job Definition | 作业定义与编译期 |
| Execution Graph | Planner | Runtime 启动流程 | 一次编译/运行版本 |
| Runtime | 调用方 | 调用方 | 一次 Job 运行 |
| Source Connector | Runtime/Factory | Runtime | Job 或 split ownership 生命周期 |
| Worker | Runtime | Runtime | Job 运行期 |
| Operator instance | Planner/Runtime | Execution Task | Task 生命周期 |
| Collector | Runtime/Execution Node | Runtime | 一次 Process 调用；物理复用属于实现细节 |
| Record | Source/Operator | 当前处理边界 | 随数据流转移 |
| Runtime Envelope | Source boundary | Runtime | 输入终结前；私有 work scope，不沿 Pipeline 公开复制 |
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
7. Operator Work Failure Policy 由 Runtime 统一应用，Operator 只报告错误；Sink 内部恢复结束后
   报告最终 completion，Sink 最终失败固定触发 FailJob。
8. work attempt 可以丢弃尚未转移给 Sink 的末端输出，但重试不隐含回滚已转移输出或外部副作用。
9. Source/Connector 专有类型不进入业务 Operator API。
10. 并行执行不默认提供全局顺序。
11. Source position 只能推进到连续终结的位置。
12. checkpoint 和 exactly-once 是不同层次的保证。
13. 任何一致性声明都说明边界、前提和故障模型。
14. Job 退出必须能回收 Runtime 管理的全部 goroutine 和资源。
15. 性能优化不能改变公开语义，除非通过新设计明确修改。
16. Runtime 与 Sink 的主动调度方向不是架构不变量，但完整 work 的责任交接、有界资源和 completion 事实不能因调度方式改变。

Positioned split 内 Connector admission 顺序必须等于该 split 的恢复顺序；Runtime 按 admission
顺序追踪 completion，但不解析 Connector position。Source position、completion safe frontier
和未来 checkpoint cut 是不同状态；完整契约见
[Position Design §1](designs/0007-position-and-kafka-rebalance.md#1-position-与第一版一致性保证)。

## 21. 当前开放问题

以下问题尚未定稿，应在阶段设计或原型中解决：

- Operator 是否长期保留为接口，还是以 function adapter 为主；
- 稳定 Operator identity 从何时开始强制要求。

这些问题出现在本文中不代表应当现在一次性解决。当前阶段只解决会影响当前代码的部分。

## 22. 文档更新规则

项目统一遵循 [Documentation Governance](governance.md)。跨阶段职责、依赖方向、ownership
或长期不变量变化时更新本文；重要且长期有效的取舍同时形成 ADR。具体能力契约留在 Design，
当前断点留在 Status，代码和测试提供实现证据。

本文中的候选概念被实现后，应把架构成熟度从 `Planned` 更新为 `Current`，并链接对应代码
或 Design。被真实需求否定的概念应标明替代方案；不要抹除 ADR 和重要取舍历史。

## 23. 给新会话的最短上下文

新会话不得依赖本节的静态摘要恢复动态状态。请从 [文档入口](README.md) 和
[Current Status](status.md) 开始，按治理规范核对当前 Roadmap、Design、ADR、代码、测试、
Git 历史和未提交 diff。
