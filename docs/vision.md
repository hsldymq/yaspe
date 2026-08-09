# yaspe Vision

> Yet Another Streaming Process Engine

文档状态：Living Document  
最后更新：2026-08-01

## 1. 项目定位

yaspe 是一个使用 Go 编写的、类型安全、可嵌入的流处理引擎。

它负责将用户定义的流式计算拓扑转换为可执行计划，并由 Runtime 统一管理数据流动、并发调度、背压、生命周期、失败处理以及后续的状态和恢复语义。

yaspe 从单进程内的高吞吐无状态 ETL 开始，逐步发展为能够支持有状态计算、事件时间、故障恢复、端到端一致性和复杂事件处理的通用流处理引擎。

yaspe 首个真实使用场景是替代和承载 `lightning-log-filter` 的行为数据采集链路：从 Kafka 持续读取日志，完成解析、过滤、转换和业务数据提取，最终写入 ClickHouse 等下游存储。

## 2. 为什么创建 yaspe

`lightning-log-filter` 已经包含 Source、Flow、Extractor 和 Sink 的雏形，但其执行模型主要围绕具体业务需求逐步形成。并发、错误处理、消息确认、重试和数据写入等执行职责分散在 Kafka handler、全局任务池和业务 Flow 中，使系统难以清楚回答以下问题：

- 一条记录在什么时候处理完成；
- 下游变慢时如何限制上游读取；
- 失败发生后哪些数据已经产生副作用；
- 哪些错误可以安全重试；
- Source position 应当在什么时候提交；
- 如何在进程失败后恢复且不丢失数据；
- 如何在不破坏一致性语义的情况下提高并行度。

yaspe 的目标不是简单地将现有代码抽成公共包，而是建立一套明确、可测试、可以长期演进的执行语义，并将业务计算与 Runtime 职责分离。

## 3. 核心目标

### 3.1 明确且可验证的执行语义

yaspe 必须明确描述记录的创建、流转、完成、失败、跳过、重试和取消语义。系统行为应当可以通过测试验证，而不是依赖实现细节或使用者猜测。

### 3.2 高吞吐且资源有界

yaspe 应支持可配置的并行执行和批量写入，以满足日志 ETL 的吞吐需求。同时，队列、在途记录、重试数量和状态大小必须受到明确约束。

高吞吐不等于无限创建 goroutine、无限缓冲或提前确认输入。性能优化不能牺牲正确性，也不能模糊记录的完成时刻。

### 3.3 类型安全的流计算 API

yaspe 使用 Go 泛型表达 Operator 的输入和输出类型关系，尽可能在编译期发现不合法的拓扑连接。

泛型和链式 API 是用户表达计算的工具，不应侵入或限制 Runtime 的执行模型。API 的美观程度不能优先于执行语义的清晰度。

### 3.4 计算与执行分离

Operator 描述计算，Runtime 控制执行。

Operator 不自行决定并发、队列、重试、Source position 提交和作业生命周期。Runtime 负责构建执行计划，驱动 Operator，传播背压，处理失败并管理资源。

### 3.5 可演进的一致性能力

yaspe 将从明确的无状态处理语义开始，逐步支持 Source position 管理、幂等写入、状态快照和 checkpoint 协议。

yaspe 不把 exactly-once 当作不附带条件的功能开关。端到端一致性取决于 Source、Runtime、State Backend 和 Sink 能否共同参与一致性协议。每一种保证都必须说明适用范围、前提条件和失败边界。

### 3.6 可测试的故障恢复

失败、取消、重试、乱序、延迟和恢复必须能够在确定性的测试环境中重现。yaspe 应提供测试工具，以便在指定处理位置注入故障、控制时间并验证恢复结果。

### 3.7 可观测和可诊断

Runtime 应能够暴露吞吐、延迟、队列长度、在途记录、失败次数、重试次数、Source lag、checkpoint 状态和状态大小等信息。

错误应保留作业、节点、Operator、输入位置和根因等诊断上下文，同时避免要求业务 Operator 自行实现一套监控和日志机制。

## 4. 主要使用场景

yaspe 主要面向以下场景：

- 持续日志和行为数据的解析、过滤、转换与写入；
- Kafka 等消息系统之间的数据清洗与路由；
- 高吞吐、顺序无关或按 Key 局部有序的 ETL；
- 基于 Key 的持续聚合和有状态计算；
- 基于事件时间的窗口、Join 和延迟数据处理；
- 在明确时间范围内进行复杂事件模式匹配；
- 需要嵌入 Go 服务、由应用代码定义计算拓扑的场景。

## 5. 长期能力视图

以下内容描述能力方向，不承诺具体 API、算法或交付时间。

### 5.1 已承诺方向（Committed）

- 类型安全的 Source、Operator 和 Sink 组合；
- Map、Filter、FlatMap 等无状态 Operator；
- 单进程内的有界并行执行；
- 背压和明确的在途记录上限；
- 统一的错误传播和失败策略；
- 作业启动、取消、停止和资源回收；
- 不要求输出有序时的高吞吐执行；
- 确定性测试与性能基准；
- 用 `lightning-log-filter` 的真实工作负载验证设计。

### 5.2 计划方向（Planned）

- Source position、acknowledgment 和连续完成位置跟踪；
- KeyBy、分区执行和 Key 内顺序保证；
- ValueState、MapState 等 keyed state 抽象；
- 可替换的 State Backend；
- checkpoint、恢复和 savepoint；
- Event Time、Watermark 和 Timer；
- Window、Aggregation 和流式 Join；
- 支持事务或幂等协议的 Sink；
- Kafka 和 ClickHouse 等生产级 Connector；
- 作业拓扑、执行指标和失败原因的诊断能力。

### 5.3 探索方向（Exploratory）

- 复杂事件处理（CEP）；
- Operator chain、数据交换和调度优化；
- 动态调整并行度和状态重分布；
- 分布式执行；
- SQL 或声明式查询层；
- 独立的作业提交和管理服务；
- 多租户、资源隔离和集群级调度。

探索方向只有在其依赖的基础语义稳定并经过真实工作负载验证后，才进入具体设计。

## 6. 概念架构

yaspe 的长期概念架构如下：

```text
User API / DSL
       |
       v
Logical Graph
       |
       v
Planner / Compiler
       |
       v
Physical Execution Graph
       |
       v
Runtime
  |         |          |
Source   Operator     Sink
  |         |          |
  +---- State / Time --+
              |
              v
     Checkpoint / Recovery
```

各层职责如下：

- User API / DSL：让用户以类型安全的方式描述计算；
- Logical Graph：记录用户定义的逻辑拓扑，不直接执行计算；
- Planner / Compiler：验证拓扑并将其转换为物理执行计划；
- Runtime：管理任务、并发、队列、背压、错误和生命周期；
- Source / Sink：连接外部系统，但不将外部客户端类型泄漏到核心语义；
- State / Time：提供有状态计算、事件时间、Watermark 和 Timer；
- Checkpoint / Recovery：协调 Source position、Operator state 和 Sink commit。

这是用于保持模块边界的概念视图，不要求项目初期创建所有目录或抽象。

## 7. 长期设计原则

### 7.1 正确性优先，性能必须测量

任何优化都必须先定义其语义影响，并通过 benchmark 和真实 workload 证明收益。不能通过提前提交 Source position、无限缓冲或忽略错误来制造高吞吐。

### 7.2 所有权必须明确

Record、输出、Buffer、State 和外部资源在任意时刻都应有明确所有者。并发执行不能依赖隐含的数据共享或调用方猜测对象是否仍可修改。

### 7.3 所有资源都应有界

队列容量、并发度、在途记录、批次大小、重试次数、迟到范围和状态生命周期必须能够配置或由协议约束。

### 7.4 顺序保证必须显式

yaspe 不默认所有记录全局有序。作业应明确选择无序执行、Source partition 内有序或 Key 内有序。增强顺序保证通常会降低并行度，不能隐藏其成本。

### 7.5 背压属于 Runtime

下游无法继续接收数据时，压力必须通过 Runtime 传播到上游。业务 Operator 不负责自行实现队列、限流或 goroutine 管理。

### 7.6 错误策略属于 Runtime

Operator 返回错误和上下文，Runtime 根据明确策略决定 Fail、Skip、Retry 或 Dead Letter。重试不隐含回滚，涉及外部副作用时必须说明幂等或事务前提。

### 7.7 一致性声明必须附带边界

at-most-once、at-least-once 和 exactly-once 等术语必须同时说明观察对象与系统边界。只保证状态恢复，不代表任意外部 Sink 都拥有 exactly-once 效果。

### 7.8 Connector 与核心解耦

Kafka partition/offset、ClickHouse batch 等概念可以通过适配层参与 Runtime 协议，但特定客户端对象不应成为核心 Operator API 的组成部分。

### 7.9 API 与 Runtime 分离

用户 API 可以利用 Go 1.27 泛型方法提高表达能力，Runtime 不应为维持链式 DSL 而牺牲稳定的内部执行表示。

### 7.10 从真实场景演进抽象

优先使用 `lightning-log-filter` 的真实需求验证核心抽象。接口应从可观察的调用关系和执行约束中产生，而不是从遥远的功能列表中提前推演。

### 7.11 可恢复性必须通过故障测试证明

涉及 acknowledgment、checkpoint、state 和 exactly-once 的能力，必须包含进程终止、重复输入、部分写入和恢复等故障测试。

### 7.12 保留演进空间，但不提前实现

初期设计不应主动阻断有状态和分布式执行的可能性，但也不为尚未出现的需求建立复杂抽象。概念架构可以前瞻，代码必须服务当前里程碑。

## 8. 当前非目标

在基础执行语义稳定之前，yaspe 不以以下事项为目标：

- 成为 Flink、Kafka Streams 等成熟系统的完整替代品；
- 首先实现分布式集群、控制平面或资源调度；
- 首先实现 Web 控制台、权限或多租户；
- 首先实现 SQL、配置驱动 DSL 或动态插件系统；
- 为所有 Source/Sink 无条件承诺 exactly-once；
- 支持任意形式的无限 FlatMap 输出；
- 对所有作业提供全局顺序；
- 仅凭微基准追求理论上的最大吞吐量；
- 为保持 API 兼容而长期保留未经验证的早期抽象。

非目标可以随着项目发展调整，但调整应记录原因以及对 Roadmap 的影响。

## 9. 成功标准

yaspe 的成功不以功能数量或代码规模衡量。以下结果更重要：

- `lightning-log-filter` 的真实 ETL 链路可以由 yaspe 表达并稳定运行；
- 在相同资源和正确性约束下，吞吐和延迟达到可接受水平，并有可重复的基准数据；
- Sink 变慢或失败时，系统不会无限占用内存或静默丢失输入；
- 每条记录何时完成、何时可确认、失败后会发生什么都有明确答案；
- 并发、错误策略和 Connector 不污染业务 Operator；
- 新增 Operator 或 Connector 不需要复制一套执行和错误处理逻辑；
- 状态与 checkpoint 能够在故障注入测试中正确恢复；
- exactly-once 等一致性能力能够清楚说明边界并由测试验证；
- 项目在经历较长时间中断后，仍能根据文档恢复设计上下文并继续演进。

## 10. 文档与决策治理

本文件描述长期方向，是持续修订的 Living Document，不是不可变规范。

项目使用以下文档保存不同层次的信息：

```text
vision.md       最终希望成为什么，以及长期原则
architecture.md 核心概念、结构关系、职责边界和阶段演化
roadmap.md      能力演进顺序、依赖和里程碑
status.md       当前进度、开放问题和下一步行动
designs/        当前阶段或具体能力的完整设计
decisions/      具有长期影响的关键设计决策及其原因
```

当不同资料发生冲突时，应主动检查并修正，不应默认任何文档永远正确。一般参考顺序为：

```text
代码和测试所表达的当前行为
        ↓
已接受的决策记录
        ↓
当前阶段设计
        ↓
Roadmap
        ↓
Vision
```

代码行为与已接受设计不一致时，必须判断这是实现缺陷还是设计已经改变，并相应更新测试、设计或决策记录。
