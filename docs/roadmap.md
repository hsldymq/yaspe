# yaspe Roadmap

文档状态：Living Document  
最后更新：2026-08-01  
关联文档：[vision.md](vision.md) · [architecture.md](architecture.md) · [status.md](status.md)

## 1. Roadmap 的目的

本文描述 yaspe 从高吞吐无状态 ETL 引擎逐步演进到有状态流处理引擎的能力路径。

Roadmap 主要回答：

- 当前应该解决什么问题；
- 为什么这些问题应当按此顺序解决；
- 每个阶段依赖哪些已经验证的能力；
- 达到什么条件后才能进入下一阶段；
- 哪些远期能力目前只是方向，不应提前进入实现。

本文不提供日历排期。yaspe 是长期、间歇式开发项目，里程碑以能力依赖和验收证据为推进依据，而不是以日期或代码量为依据。

## 2. 推进原则

### 2.1 用垂直切片验证设计

每个阶段优先完成一条可以实际运行和测试的端到端链路，而不是先建立大量尚无使用者的抽象。

### 2.2 一个阶段只引入一种主要复杂度

并发、Source position、状态、checkpoint、事件时间和分布式执行分别包含不同的正确性问题。除非存在无法分离的依赖，不应在同一个阶段同时引入多种尚未验证的复杂度。

### 2.3 以真实工作负载校验抽象

`lightning-log-filter` 是 yaspe 的第一个真实使用者。早期设计必须能够承载其 Kafka ETL、业务提取和 ClickHouse 写入场景，并通过 shadow run、故障注入和 benchmark 验证。

### 2.4 正确性证据先于能力声明

一个能力只有在语义文档、自动化测试、故障测试和必要的性能数据齐备后才视为完成。特别是 acknowledgment、恢复和 exactly-once 不能仅凭接口存在就宣称实现。

### 2.5 允许修订 Roadmap

后续阶段代表当前理解下的合理路径，不是不可修改的合同。真实实现暴露的新事实可以改变里程碑内容或顺序，但应记录改变原因。

## 3. 能力演进总览

```text
M0  核心语义与项目基线
 |
 v
M1  有界并发的 Stateless Runtime
 |
 v
M2  Source Position、完成跟踪与生产级 Sink
 |
 v
M3  lightning-log-filter 迁移验证
 |
 v
M4  KeyBy、分区执行与逻辑/物理执行图
 |
 v
M5  Keyed State 与 State Backend
 |
 v
M6  Checkpoint 与故障恢复
 |
 v
M7  Event Time、Watermark 与 Timer
 |
 v
M8  Window、Aggregation 与 Join
 |
 v
M9  端到端一致性与 Sink Commit Protocol
 |
 v
M10 CEP
 |
 v
M11 分布式执行（探索）
```

这条路径表达主要依赖，不表示所有辅助工作必须严格串行。例如可观测性、测试工具、性能分析和文档治理会贯穿所有里程碑。

## 4. M0：核心语义与项目基线

状态：Current

### 目标

建立 yaspe 第一版共同语言和执行契约，使后续并发实现建立在明确语义上。

### 为什么先做

如果 Record、Operator、Emit、错误、完成和取消的含义不清楚，并发代码会通过偶然实现替项目做出难以撤销的决定。

### 范围

- 定义项目定位、能力边界和演进路线；
- 定义 Record、Source、Operator、Collector、Sink、Job 和 Runtime 的职责；
- 定义一个输入产生零个、一个或多个输出的语义；
- 定义部分 Emit 后失败的行为；
- 定义 Fail、Skip、Retry 和 Dead Letter 的概念边界；
- 定义正常结束、失败、取消和资源释放语义；
- 确定 Go 1.27 最低版本和工具链策略；
- 建立测试、benchmark、race test 和文档目录基线。

### 非范围

- Kafka 和 ClickHouse 实现；
- checkpoint 和 exactly-once；
- DAG 优化；
- 有状态 Operator；
- 分布式执行。

### 完成标准

- `vision.md` 和 `roadmap.md` 已接受；
- 核心执行模型设计文档已接受；
- 关键决策已通过 ADR 记录；
- 各概念的所有权和生命周期没有已知矛盾；
- 设计明确指出第一版提供和不提供的保证；
- `status.md` 能准确指向下一项具体工作。

## 5. M1：有界并发的 Stateless Runtime

状态：Planned

### 目标

实现一个单进程、资源有界、record 级并行的无状态流处理 Runtime。

### 为什么现在做

这是 `lightning-log-filter` 所需的最小计算能力，也是后续 acknowledgment、状态和 checkpoint 的执行基础。先在无外部系统参与的环境中验证并发、背压和停止语义，可以缩小问题范围。

### 范围

- Memory Source 和线程安全的 Memory Sink；
- Map、Filter 和有限输出的 FlatMap；
- 用户定义拓扑与可执行对象的基本分离；
- 有界输入队列和可配置 Worker Pool；
- 不同 record 并行、单个 record 内同步执行的第一版模型；
- Runtime 负责并发，Operator 不自行创建 goroutine；
- FailJob 和 SkipRecord 错误策略；
- context 取消、优雅停止和 goroutine 回收；
- 基础执行指标；
- deterministic test runner 或等价测试设施；
- CPU 型和 I/O 模拟型 benchmark。

### 非范围

- 自动重试外部副作用；
- Kafka offset 提交；
- 异步批量 Sink；
- Key 内顺序；
- 状态和 checkpoint；
- 全局输出顺序。

### 完成标准

- `Parallelism=1` 时结果和失败位置可确定复现；
- `Parallelism>1` 时不承诺输出顺序，并在 API 中明确表达；
- 队列满时 Source 停止读取，内存不会随输入无限增长；
- FailJob 能停止新输入并回收所有 Runtime goroutine；
- SkipRecord 不终止其他独立记录；
- Map、Filter、FlatMap 的错误和部分输出语义有完整测试；
- `go test -race ./...` 通过；
- benchmark 记录吞吐、延迟、分配次数和并发度，不只记录单一 ops/s；
- 不通过无限缓冲或丢弃错误提高基准结果。

## 6. M2：Source Position、完成跟踪与生产级 Sink

状态：Planned

### 目标

让 Runtime 能够跟踪并发记录的完成状态，并安全推进可恢复的 Source position；同时建立异步批量 Sink 的完成语义。

### 为什么现在做

业务记录可以顺序无关地并行执行，但 Kafka partition position 不能越过尚未完成的记录提交。异步 Sink 入队也不代表外部写入已经完成。这两个问题必须在接入生产数据源之前由 Runtime 统一解决。

### 范围

- 与具体 Connector 解耦的 Source position 抽象；
- record acknowledgment 和终态模型；
- 并发完成、连续 position 推进；
- 成功、Skip、Dead Letter 和未解决失败对 position 的不同影响；
- Kafka Source Connector；
- 使用 Kafka Consumer Group 协调多 Pod 的 partition ownership；
- partition assignment、revocation 和 ownership generation；
- rebalance 时停止读取、在途任务 drain/cancel 和安全 position 提交；
- 旧 ownership 完成的任务不得推进当前 position；
- Kafka poll、heartbeat/session 与 Runtime 背压的协作；
- 禁用或约束不理解 Runtime 完成语义的自动 offset 提交；
- graceful shutdown 时停止读取、flush、提交和释放 partition 的顺序；
- 有界异步批量 Sink；
- batch flush、成功、部分失败和关闭语义；
- ClickHouse Sink Connector 的第一版；
- 有上限的 Retry 策略及 backoff；
- Dead Letter Sink；
- Source lag、in-flight、batch 和 commit 指标；
- 在指定 position 和 batch 阶段进行故障注入。

### 非范围

- checkpoint；
- 任意 Sink 的原子批量写入；
- 端到端 exactly-once；
- Kafka partition 动态重分配期间的完整状态迁移；
- 有状态 Operator。

### 完成标准

- position 较大的记录先完成时，不会越过前序未完成记录提交；
- Sink 仅入队但尚未落库时，输入不会被标记完成；
- batch 写入失败不会被报告为成功；
- Retry 是否可能产生重复输出有清楚说明和测试；
- Dead Letter 写入成功后才能按配置终结原记录；
- 多个实例使用同一 Consumer Group 时，同一 partition 不会被 yaspe 主动重复分配；
- partition 被 revoke 后，旧 ownership 不会继续推进其 committed position；
- 队列饱和和 Sink 变慢时，Kafka session 不会因错误的阻塞模型持续发生非预期 rebalance；
- graceful shutdown 会停止新读取，并在期限内处理或明确放弃未完成的在途记录；
- Kafka rebalance、取消和关闭时不会静默丢弃已确认但未提交的状态；
- 故障测试覆盖读取后、处理时、Sink 入队后、batch 写入时和 position 提交前后的进程失败；
- 当前交付保证被准确描述为 at-most-once、at-least-once 或其他限定语义。

## 7. M3：lightning-log-filter 迁移验证

状态：Planned

### 目标

使用 yaspe 承载至少一条真实的 `lightning-log-filter` 行为采集链路，并通过生产形态 workload 验证架构。

### 为什么单独成为里程碑

Memory benchmark 和合成测试无法暴露真实 JSON 解析、业务分派、ClickHouse batching、Kafka lag、分配压力及异常数据等问题。迁移验证是后续抽象继续扩张前的现实检查点。

### 范围

- 选择一条代表性的 BusinessEvent 或标准日志链路；
- 将业务 Extractor 适配为 yaspe Operator；
- 新旧链路 shadow run；
- 使用稳定业务键或来源 position 对输出进行 diff；
- 对比吞吐、端到端延迟、Kafka lag、CPU、内存和 GC；
- 验证慢 Sink、异常数据、进程重启和 Kafka rebalance；
- 在 Kubernetes 中使用同一 Consumer Group 运行多个 Pod；
- 验证扩容、缩容、rolling update 和强制终止单个 Pod；
- 验证 rebalance 发生时仍有在途记录和待 flush batch 的场景；
- 验证 Kafka partition 数少于、等于和大于 Pod 数的资源利用情况；
- 统计上述场景中的缺失、重复、lag、batch 和资源变化；
- 补齐真实迁移中暴露的 Runtime 诊断能力；
- 形成迁移指南和兼容边界。

### 非范围

- 一次性迁移所有现有 Flow；
- 为兼容旧实现而绕过 yaspe 的完成语义；
- 在缺少恢复测试时替换全部生产链路；
- 引入有状态计算。

### 完成标准

- 代表性链路可由 yaspe API 清晰表达；
- shadow run 的输出差异可以解释并收敛；
- 在同等资源和可靠性约束下达到可接受的吞吐与延迟；
- 进程在任意允许的故障点退出后，结果满足已声明的交付保证；
- 没有无限队列、无限重试或 goroutine 泄漏；
- Runtime 与业务代码的职责边界经迁移验证仍然成立；
- 是否扩大迁移范围由数据而不是主观感觉决定。

## 8. M4：KeyBy、分区执行与逻辑/物理执行图

状态：Planned

### 目标

支持按 Key 路由记录并保证 Key 内执行顺序，同时将拓扑定义、逻辑图和物理执行计划明确分层。

### 为什么此时做

Keyed State、窗口和 CEP 都要求同一个 Key 的事件由确定的执行单元处理。经过无状态生产链路验证后，再抽象 Logical Graph 和 Physical Graph，能够避免 Planner 围绕假设设计。

### 范围

- 稳定的 Operator/Node identity；
- Logical Graph 验证；
- Physical Execution Graph；
- Key selector 和稳定分区算法；
- Key 内有序、不同 Key 并行；
- Operator 并行度和 partition ownership；
- shuffle edge 与 forward edge；
- 简单 Operator chaining；
- 拓扑可视化和执行计划诊断。

### 非范围

- 跨机器 shuffle；
- 动态 rescale；
- keyed state 持久化；
- checkpoint barrier；
- 复杂成本优化器。

### 完成标准

- 相同 Key 始终进入同一逻辑分区；
- Key 内顺序在并发压力和失败情况下符合声明；
- Logical Graph 不包含 Runtime goroutine、channel 或 Connector 客户端对象；
- Planner 生成的执行计划可检查且可稳定测试；
- chaining 优化不会改变公开执行语义；
- 分区算法和序列化变化对未来状态兼容性的影响已有记录。

## 9. M5：Keyed State 与 State Backend

状态：Planned

### 目标

为按 Key 执行的 Operator 提供受 Runtime 管理的状态能力。

### 为什么现在做

状态必须建立在稳定的 Key 分区和 Operator identity 上，否则状态归属、隔离和迁移都无法定义。

### 范围

- ValueState、MapState 等最小状态原语；
- Operator State 与 Keyed State 的边界；
- 状态 namespace、schema 和 serializer；
- In-memory State Backend；
- 状态 TTL 和清理语义；
- 状态访问的线程安全与 ownership；
- 状态大小和访问延迟指标；
- 状态 schema 演进的初步约束。

### 非范围

- durable checkpoint；
- 任意语言或任意格式的状态兼容；
- 在线 rescale；
- 分布式 State Backend；
- Window 和 CEP 上层 API。

### 完成标准

- 不同 Job、Operator 和 Key 的状态严格隔离；
- 状态只能在合法执行上下文中访问；
- TTL 在 processing time 或 event time 中采用哪一种语义已有明确说明；
- serializer 和 schema 变化不会无声破坏已有状态；
- 大量 Key 下的内存占用可观测且有界策略明确；
- In-memory Backend 的正确性测试为后续持久化 Backend 提供共同契约。

## 10. M6：Checkpoint 与故障恢复

状态：Planned

### 目标

一致地保存 Source position、Operator state 和必要的 Runtime 元数据，使 Job 可以从已完成 checkpoint 恢复。

### 为什么现在做

只有当 Source position、执行图、Key 分区和 State Backend 都稳定后，checkpoint 才有清楚的快照对象和恢复位置。

### 范围

- Checkpoint Coordinator；
- checkpoint identity、触发、完成和失败；
- Source position 快照；
- Operator/Keyed State 快照；
- 第一版 stop-the-world 或其他最简单的正确算法；
- durable checkpoint storage；
- Job 重启和状态恢复；
- checkpoint retention 和清理；
- checkpoint 超时、失败和重试策略；
- 恢复兼容性检查；
- crash-at-any-point 故障测试。

### 非范围

- 未验证的高性能 barrier 优化；
- unaligned checkpoint；
- 在线 rescale；
- 任意拓扑修改后的状态自动映射；
- 对所有外部 Sink 承诺 exactly-once。

### 完成标准

- 任意已声明故障点退出后可从最近一次成功 checkpoint 恢复；
- 未完成 checkpoint 不会被当作可恢复快照；
- Source position 与状态快照不会来自互相矛盾的处理时刻；
- checkpoint storage 损坏或版本不兼容会明确失败；
- checkpoint 期间的背压、暂停或延迟影响可观测；
- 恢复测试覆盖重复处理和不丢失的边界；
- 文档准确说明 checkpoint 提供的是状态一致性，而非自动提供任意 Sink 的 exactly-once。

## 11. M7：Event Time、Watermark 与 Timer

状态：Planned

### 目标

使计算能够基于事件发生时间而不是机器处理时间推进，并支持可恢复的定时行为。

### 为什么现在做

事件时间和 Timer 会成为 Operator state 的一部分，也必须参与 checkpoint 和恢复。先建立恢复能力可以避免时间语义成为不可持久化的旁路机制。

### 范围

- Record event time；
- Processing Time 与 Event Time 的明确区分；
- Watermark 生成、传播和多输入合并；
- idle source/partition 检测；
- 迟到数据定义；
- Processing Time Timer 和 Event Time Timer；
- Timer state 的 checkpoint 与恢复；
- 可手动推进时间的确定性测试工具。

### 非范围

- 通用 Window API；
- CEP pattern；
- 自动猜测 watermark 策略；
- 对不可控外部时钟提供绝对确定性。

### 完成标准

- Watermark 不会越过任何仍可能产生更早事件的活跃输入；
- idle partition 不会永久阻塞整个拓扑的 watermark；
- Timer 在 checkpoint 恢复后不会无声丢失；
- 重复触发边界有明确语义；
- 迟到事件策略可测试；
- 测试不依赖真实 sleep 推进 event time。

## 12. M8：Window、Aggregation 与 Join

状态：Planned

### 目标

基于 Keyed State、Event Time 和 Timer 提供常见有状态流计算能力。

### 为什么现在做

Window 和 Join 是基础时间与状态能力的消费者，不应反过来定义底层 state、watermark 和 timer 的偶然语义。

### 范围

- Tumbling、Sliding 和 Session Window；
- Incremental Aggregation；
- Trigger、Evictor 或经过收敛的更小抽象；
- allowed lateness 和 late-data side output；
- 有界时间范围内的 keyed stream join；
- Window state 清理；
- 状态大小和热点 Key 诊断。

### 非范围

- 无时间边界的无限 Join 状态；
- 完整 SQL 查询优化；
- CEP；
- 任意自定义 trigger 组合，除非真实需求证明必要。

### 完成标准

- Window 在乱序、迟到和恢复后产生符合定义的结果；
- Session merge 语义有属性测试或等价强度测试；
- Window 状态能够按规则清理；
- Join 不会在缺少显式保留边界时无限增长；
- 热点 Key 和大窗口对性能的影响可观测；
- API 没有泄漏 State Backend 或 Timer 实现细节。

## 13. M9：端到端一致性与 Sink Commit Protocol

状态：Planned

### 目标

让能够参与一致性协议的 Source 和 Sink 与 checkpoint 协作，提供边界明确的端到端处理保证。

### 为什么此时做

端到端 exactly-once 依赖稳定的 checkpoint 和恢复协议。过早实现 Sink 事务接口，容易得到一个名称正确但无法与 Runtime 状态原子协调的抽象。

### 范围

- Sink Writer、Committer 和 Global Committer 等职责评估；
- prepare、pre-commit、commit、abort 和 recovery commit；
- checkpoint 与 Sink transaction/batch identity 关联；
- Kafka-to-Kafka transactional path；
- ClickHouse 幂等 ID、去重或 staging 方案评估；
- at-least-once + idempotent effect 的明确表达；
- commit 不确定结果和恢复；
- 一致性能力声明与 Connector capability negotiation。

### 非范围

- 为不支持事务或幂等的系统制造虚假的 exactly-once 承诺；
- 通用分布式事务协调器；
- 跨任意数据库的两阶段提交；
- 忽略外部系统去重窗口或保留策略。

### 完成标准

- 每个 Connector 明确声明其交付保证、前提和观察边界；
- Kafka-to-Kafka 在故障测试中验证事务输出和 offset 原子提交；
- ClickHouse 路径准确区分 transport delivery 与 observable effect；
- commit 成功但响应丢失等不确定场景有恢复策略；
- 重启和 checkpoint 恢复不会无声产生无法解释的缺失或重复；
- exactly-once 声明能够由自动化故障测试支持。

## 14. M10：复杂事件处理（CEP）

状态：Exploratory

### 目标

在同一 Key 的事件流中，根据时间约束和事件关系识别复杂模式。

### 为什么较晚实现

CEP 依赖 Keyed State、Event Time、Watermark、Timer、恢复和状态清理。缺少这些基础能力时，CEP 只能成为无法正确处理乱序和故障的内存状态机。

### 初步范围

- Pattern 的类型安全表达；
- 基于 NFA 或经验证的等价模型；
- begin、next、followed-by、within 等有限模式；
- 匹配状态、Timer 和 checkpoint；
- skip strategy；
- 乱序、迟到与超时语义；
- 模式状态大小和清理策略。

### 暂不承诺

- 任意递归或无限模式；
- 动态修改运行中 Pattern 并自动迁移全部状态；
- 跨 Key 的无界全局匹配；
- 在基础能力未稳定前确定最终 CEP DSL。

### 进入具体设计的前置条件

- M7 的 Event Time、Watermark 和 Timer 已稳定；
- M6 的 checkpoint 能恢复 Timer 与 keyed state；
- 至少存在一个真实 CEP 使用场景；
- 已评估状态爆炸和匹配复杂度边界。

## 15. M11：分布式执行

状态：Exploratory

### 目标

评估并在必要时支持跨进程或跨机器执行物理任务。

### 为什么不是早期目标

分布式执行会引入网络交换、任务部署、成员管理、故障检测、资源调度和协调一致性。单机 Runtime 的语义如果尚未稳定，分布式只会放大歧义。

### 可能涉及

- Job Manager 与 Task Worker；
- 任务部署和生命周期；
- 跨节点 shuffle；
- 心跳、失败检测和重启；
- checkpoint 协调；
- 状态放置和迁移；
- 资源模型；
- rolling upgrade 和 savepoint；
- 控制平面与数据平面分离。

### 进入具体设计的前置条件

- 单进程 Runtime 已被真实生产链路长期验证；
- 存在单机扩展无法满足的真实需求；
- Logical/Physical Graph、partition ownership 和 checkpoint 已稳定；
- 已明确选择自建调度能力还是复用外部平台；
- 分布式带来的复杂度能够被持续维护。

## 16. 贯穿所有里程碑的工作

以下事项不是一次性阶段，应在每个里程碑持续推进。

### 16.1 测试

- 单元测试；
- 并发与 race test；
- 属性测试或 fuzz test；
- 故障注入；
- goroutine 和资源泄漏检查；
- 兼容性与恢复测试。

### 16.2 性能

- 建立稳定、可重复的 benchmark workload；
- 记录硬件、Go 版本和配置；
- 同时观察吞吐、延迟、CPU、内存、GC 和分配；
- 使用真实数据分布校验微基准结论；
- 性能回归应有可定位的执行指标。

### 16.3 可观测性

- Job、Node、Operator 和 Connector 维度的指标；
- 队列、在途记录和背压状态；
- 错误分类和根因链；
- Source lag 和 Sink batch 状态；
- state、timer 和 checkpoint 指标。

### 16.4 文档与决策记录

- 当前里程碑应有 design 文档；
- 跨模块或长期有效的选择应形成 ADR；
- 完成开发会话后更新 `status.md`；
- 实现改变公开语义时同步修改设计和测试；
- 过期设计应标记 superseded，而不是抹去历史原因。

### 16.5 API 稳定性

项目早期允许破坏性修改，但必须有明确理由。API 只有经过真实使用和语义验证后才承诺稳定，不通过过早兼容冻结错误抽象。

## 17. 里程碑状态和变更规则

里程碑使用以下状态：

```text
Exploratory  方向存在，但尚未满足设计前提
Planned      依赖关系明确，但尚未开始
Current      当前主要工作
Validating   已实现，正在收集正确性和生产证据
Completed    完成标准已经满足
Paused       主动暂停，原因已记录
Superseded   被新的里程碑或方案替代
```

只有一个里程碑应当作为主要的 `Current` 阶段。可以进行辅助研究，但不能借此绕过当前阶段的完成标准。

修改 Roadmap 时应说明：

- 哪项新事实促成了变化；
- 哪些依赖关系发生改变；
- 已完成实现是否仍然有效；
- 是否需要新增或废弃 design/ADR；
- `status.md` 的下一步行动是否需要更新。

## 18. 当前下一步

当前处于 M0。下一项工作是起草并评审：

```text
docs/designs/0001-core-execution-model.md
```

该设计至少需要明确：

- Record 及其 metadata 的边界；
- Source、Operator、Collector、Sink 和 Runtime 的职责；
- Map、Filter 和 FlatMap 的输出模型；
- 多次 Emit 和部分 Emit 后失败的语义；
- record 级并发、队列和背压；
- FailJob、SkipRecord、Retry 和 Dead Letter；
- 正常结束、取消和优雅停止；
- 一条记录的完成条件；
- 第一版明确不提供的事务与一致性保证；
- 测试矩阵和 benchmark 基线。
