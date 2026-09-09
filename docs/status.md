# yaspe Current Status

最后更新：2026-09-09

本文是动态交接快照，不是完整设计记录。完整契约见正式 Design，决定背景和取舍见
[决策索引](decisions/README.md)，维护规则见 [Documentation Governance](governance.md)。

## 当前里程碑

M1 — 有界并发的 Stateless Runtime。M0 核心语义与项目基线已完成。

## 当前目标

按已接受契约实现 Memory Source → Operator Chain → Memory Sink 最小链路，并验证有界
调度、ownership、FailJob、取消与回收。Job Definition、Memory Source 和 Memory Sink 已完成；
Runtime 最小链路尚未实现。Kafka、ClickHouse 与 Operator Retry 留在 M2。

## 三维能力状态

| 能力 | Design | Implementation | Verification | 权威位置 |
|---|---|---|---|---|
| Record / Operator | Accepted | Implemented | Unit Tested | [Operator Design §1](designs/0004-operator-attempt-and-collector.md#1-collector-生命周期与并发) |
| Collector scope-bound context API | Accepted | Implemented | Unit Tested | [Operator Design §1.3](designs/0004-operator-attempt-and-collector.md#13-emit-契约) |
| Map / Filter / FlatMap | Accepted | Implemented | Unit Tested | [Operator Design §2](designs/0004-operator-attempt-and-collector.md#2-operator-chain-与-work-attempt-边界) |
| M1 Reader / Memory Source 语义 | Accepted | Implemented | Race Tested | [Source Design](designs/0003-source-reader-and-admission.md) |
| M2 Source lifecycle / split control / position commit API | Accepted | Not Started | Not Applicable | [Source Design](designs/0003-source-reader-and-admission.md) |
| SourceContext 最终失败报告 | Accepted | Not Started | Not Applicable | [Source Design §1.1.2](designs/0003-source-reader-and-admission.md#112-独立的最终失败报告) |
| M1 同步 Memory Sink 语义 | Accepted | Implemented | Race Tested | [Sink Design §1.1.1](designs/0005-sink-handoff-and-completion.md#111-m1-同步-memory-sink) |
| M1 线性 Job Definition | Accepted | Implemented | Race Tested | [Job Design](designs/0002-job-definition-and-runtime-instantiation.md) |
| M1 Stateless Runtime | Accepted | Not Started | Not Applicable | [Verification Design §2.1](designs/0008-runtime-verification-and-observability.md#21-m1-实现前必须收敛) |
| M2 Operator work Retry | Accepted | Not Started | Not Applicable | [Failure Design §1](designs/0006-failure-panic-and-shutdown.md#1-失败暂停与恢复) |
| 统一 RunError 与多错误因果 | Accepted | Not Started | Not Applicable | [Failure Design §1.7](designs/0006-failure-panic-and-shutdown.md#17-公开-runerror) |
| M2 Position / Completion | Accepted | Not Started | Not Applicable | [Position Design](designs/0007-position-and-kafka-rebalance.md) · [Sink Design](designs/0005-sink-handoff-and-completion.md) |
| 异步 Sink 协议 | Accepted | Not Started | Not Applicable | [Sink Design](designs/0005-sink-handoff-and-completion.md) |
| Kafka Consumer Group / Revoke 时间预算 | Accepted | Not Started | Not Applicable | [ADR-0005](decisions/0005-connector-owned-revoke-budget.md) · [Source Design §1.3.1](designs/0003-source-reader-and-admission.md#131-split-control-边界) |
| Kafka poll / 背压 / 提交 / 控制回调 | Accepted | Not Started | Not Applicable | [Kafka Design §3](designs/0007-position-and-kafka-rebalance.md#3-kafka-客户端适配) |
| Kafka fetch/记录分层预算与字节非保证 | Accepted | Not Started | Not Applicable | [Kafka Design §3.2](designs/0007-position-and-kafka-rebalance.md#32-分层缓存与背压) · [ADR-0007](decisions/0007-layered-source-prefetch-budgets.md) |
| Kafka 会话建立/恢复预算与错误分类 | Accepted | Not Started | Not Applicable | [Kafka Design §3.6](designs/0007-position-and-kafka-rebalance.md#36-会话建立与恢复) |
| Kafka 提交参数 / 本地期限边界 / classic group 范围 | Accepted | Not Started | Not Applicable | [Kafka Design §3.3–3.7](designs/0007-position-and-kafka-rebalance.md#33-offset-提交与-revoke-交接) |
| Kafka v1.21.6 基线 / 预取默认值 / revoke 计时起点 | Accepted | Not Started | Not Applicable | [Kafka Design §3](designs/0007-position-and-kafka-rebalance.md#3-kafka-客户端适配) |
| Kafka Connector | Accepted（完整适配验证未完成） | Not Started | Not Applicable | [Kafka Design](designs/0007-position-and-kafka-rebalance.md) |
| ClickHouse 写入确认 / v2.48.0 Native 生命周期 / 有限重试 | Accepted | Not Started | Not Applicable | [ClickHouse Design §2–6](designs/0009-clickhouse-connector.md#2-业务配置与成功边界) |
| ClickHouse 输入映射 / 多目标组批 / 默认配置 | Accepted | Not Started | Not Applicable | [ClickHouse Design §4](designs/0009-clickhouse-connector.md#4-组批与容量) |
| M2 吞吐 / 消费与 commit 差 / 分层缓存指标 | Accepted | Not Started | Not Applicable | [Verification Design §1.11](designs/0008-runtime-verification-and-observability.md#111-m2-最小指标范围) |
| M2 故障矩阵 / 输出核对 / 条件性交付声明 | Accepted | Not Started | Not Applicable | [Verification Design §1.12–1.13](designs/0008-runtime-verification-and-observability.md#112-m2-故障注入验收矩阵) · [Position Design §1.10](designs/0007-position-and-kafka-rebalance.md#110-at-least-once) |
| Dead Letter / Side Output | Planned for later | Not Started | Not Applicable | [Roadmap M4](roadmap.md#8-m4keyby分区执行与逻辑物理执行图) |

## 当前代码事实

现有实现及验证入口：

- [Record](../record.go)、[Operator / Collector](../operator.go) 与 [operator 包](../operator/)：
  Map、Filter、FlatMap 及其正常输出、错误与 context 传播测试；
- [Job Definition](../job.go)、[fluent 转换](../stream.go)、[Factory](../factory.go) 与
  [私有 adapter](../job_adapter.go)：已实现不可变构建、类型衔接和拓扑校验；
  [Job 测试](../job_test.go)、[编译契约测试](../job_compile_test.go) 和 [转换测试](../stream_test.go)
  覆盖构建惰性、独立快照、并发派生、非法结构/类型和内置转换行为；
- [Memory Source](../connector/memory/source.go) 与
  [SourceProducer](../connector/memory/source_producer.go)：有界提交、非阻塞 FIFO 读取、
  通知、正常结束、独立失败报告和关闭已实现；基础、并发和使用示例的证据见
  [Source Design §1.8](designs/0003-source-reader-and-admission.md#18-memory-source-实现与验证证据)；
- [Memory Sink](../connector/memory/sink.go)：整组同步接管与报告、固定失败计划、分组与扁平
  快照、关闭及竞争测试已实现，证据见
  [Sink Design §1.1.2](designs/0005-sink-handoff-and-completion.md#112-memory-sink-实现与验证证据)；
- [Source](../source.go)、[Sink](../sink.go) 和 OperatorLifecycle 定义公共协议；Memory Connector
  使用可控环境验证了组件行为，Runtime 提供的控制、completion 与生命周期协调仍待实现。

尚不存在 Runtime、position/completion tracker、Kafka 或 ClickHouse 生产实现。
定义期与独立 Connector 测试不证明端到端运行、回收或交付保证。

[franz-go v1.21.6 验证附件](verification/franz-go-v1.21.6/README.md) 是独立 Go module，包含
七项客户端模拟测试及固定依赖；不属于上述生产实现，根模块测试也不包含它。已观察行为
与未覆盖场景由附件维护，不能据此把 Kafka Connector 标记为 Implemented 或完整 Verified。

[clickhouse-go v2.48.0 验证附件](verification/clickhouse-go-v2.48.0/README.md) 保存原驱动
batch/connect 的七项白盒探针和复现脚本，在临时驱动副本中执行，根模块测试不包含它。
可控连接结果不代表真实服务器、公共连接池、完整超时/重试或 Connector 实现已验证。

## 最近接受的决定

- Sink 最终使用 `Open(SinkContext)`、整组原子 `Accept` 和统一 deadline 下的有限 `Close`；
  Accepted 转移 items slice 与 Record ownership，Backpressured/error 全拒。绑定 reporter 支持
  pending-accept 同步 callback，零值 outcome invalid，active 期协议违规 FailJob；capacity 使用
  versioned notifier，Close 必须逐 item 收敛并在 drain inbox 后 fence，详见
  [Sink Design](designs/0005-sink-handoff-and-completion.md)；
- Source 最终组合非阻塞 `TryRead`、容量 1 且永不关闭的 `Available` channel、
  `Open(SourceContext)` 与有限 `Close`；positioned Source 通过可选 `PositionCommitter` 提交不透明
  split position，动态 ownership 使用 Assign、两阶段 BeginRevoke/RevokeHandle 和 Lost，并对
  ready/control 竞态与迟到 callback 建立 generation fence，详见
  [Source Design](designs/0003-source-reader-and-admission.md)；
- 所有非正常 `Runtime.Run` 统一返回冻结的 `*RunError`，明确区分首个因果 primary、停止时其他
  Operator Retry work 的 active failure snapshots 和停止期间的 secondary errors；通过
  `Unwrap() []error` 支持 `errors.Is/As`，但不公开内部 work identity，详见
  [Failure Design §1.7](designs/0006-failure-panic-and-shutdown.md#17-公开-runerror)；
- M2 Runtime 不提供 Sink effect Retry，也不解析 Sink error 或重新提交 item；Sink 接管后在内部
  决定是否 Retry，只报告最终 `Succeeded/NotApplied/Unknown`。首个最终失败立即 FailJob，其他
  已接管 item 在统一 deadline 内有限收敛；Operator Work Failure Policy 不适用于 Sink、Source
  或 Runtime 内部错误，详见 [ADR-0004](decisions/0004-keep-sink-retry-inside-connector.md)；
- 仓库文档、代码和测试作为跨会话项目记忆；Design、Implementation、Verification 分开跟踪，
  新会话按统一入口和 Status 接力，详见 [ADR-0003](decisions/0003-use-repository-docs-as-project-memory.md)；
- Job Definition 使用 `JobDraft → Stream[T] → JobBuilder → Job` 的 Go 1.27 type-state fluent
  API；`From/Transform/SinkTo` 及 Func 变体保存 Factory，Build 产生不可变 Job 快照且不创建
  运行资源；第一版只接受单 Source、线性 Chain 和单 Sink；
- 每条 execution lane 创建独立 Operator 包装实例，同一用户函数值可以跨 lane 共享并并发调用；
- Runtime 不提供 `SkipRecord`、`DiscardRecord` 或 Transformation `OnError`；可忽略业务错误
  由用户函数收敛为正常零输出，未处理 error 进入 Job 级 Retry/FailJob；
- Dead Letter 延后为显式业务输出、Side Output、分支和专用 Sink，不是 Runtime 失败终态；
- Kafka revoke 暂停该 Source 全部新 admission，仅对 revoked splits 有限收尾；Connector
  提供总 deadline 与提交预留，Runtime 推导 drain 截止，handle 覆盖后续提交阶段。取消
  独立 Runtime drain 上限，Kafka 总预算/预留初始默认分别为 30/5 秒；接口、边界与旧
  决定的替代关系见 [Source Design §1.3.1](designs/0003-source-reader-and-admission.md#131-split-control-边界)
  与 [ADR-0005](decisions/0005-connector-owned-revoke-budget.md)。
- `Collector.Emit` 成功即把 Record 及其可达引用数据 ownership 转给 Runtime，失败则不转移；
  转移在每次成功 Emit 时立即发生，Runtime 不复制也不提供通用 copier/serializer，违规复用
  属于用户实现错误且结果不受保证。
- Sink 关闭采用有限 drain 与迟到事件隔离：停止新 `Accept` 后，在 deadline 内处理所有已接管
  buffer 和外部 in-flight item，逐项形成 `SinkSucceeded`、`SinkNotApplied` 或
  `SinkUnknown`；随后使 reporter/notifier 失效，迟到调用可安全返回但不得推进 completion
  或 position；Connector 负责收敛自身可控资源，Runtime 负责隔离无法完全杜绝的外部迟到
  callback。
- Collector 绑定当前 `Process` scope，公开 API 采用 `Emit(record)`；用户代码 panic 被包装为
  带 stack 的 `PanicError` 并交给 Failure Policy，yaspe 内部 panic 则强制 FailJob、有界收尾且
  不推进 position，详见 [Operator Design §1.3](designs/0004-operator-attempt-and-collector.md#13-emit-契约)
  与 [Failure Design §2](designs/0006-failure-panic-and-shutdown.md#2-failjob取消与关闭)。
- Reader 可用性通知必须消除 `TryRead -> unavailable -> wait` 的丢失唤醒窗口；M1 采用
  先发布状态、后发送可合并通知的契约，并要求可控交错的确定性竞态测试，详见
  [Source Design §1.3](designs/0003-source-reader-and-admission.md#13-可用性通知与控制事件)。
- Reader 的最终非阻塞接口使用 `TryRead() (ReadResult[T], error)` 与容量 1、永不关闭的
  `Available()` channel；result 只表达 ready/unavailable/finished，error 表达读取失败，invalid state 作为契约错误
  进入 Job 级 failure 路径且不借用 Operator work Retry。正常结束先交付
  已缓存记录，读取失败则优先于尚未交接的缓存，详见
  [Source Design §1.2](designs/0003-source-reader-and-admission.md#12-非阻塞-reader)。
- Memory Source 定位为动态有界的 Runtime 参考 Source、确定性测试设施、benchmark 输入和
  本地示例数据源；Runtime-facing Source 与 SourceProducer 分离，并已接受 Submit 背压/
  ownership、Finish drain、Fail 根因、Runtime Close 和并发终态线性化语义，详见
  [Source Design §1.7](designs/0003-source-reader-and-admission.md#17-m1-memory-source)。
- Source admission 在读取前预留完整 reservation；ready 返回即转移 ownership 和 completion
  responsibility，Runtime 必须先无失败地绑定到预留 work slot，之后才观察取消或调度，
  详见 [Source Design §1.4](designs/0003-source-reader-and-admission.md#14-source-admission-与所有权)。
- M1 Memory Sink 对一个 work 的 terminal outputs 一次同步全收或全拒，零输出不调用 Sink；
  它保留 work groups 与只读快照，仅提供固定失败计划，并使 Accept/Snapshot/Close 线性化。
  Close 同步幂等且不清空结果，并发竞态必须用内部 barrier 确定性验证，详见
  [Sink Design §1.1.1](designs/0005-sink-handoff-and-completion.md#111-m1-同步-memory-sink)。
- 普通 FailJob 与 position 解耦：queued work 不启动，started Operator 被取消，尚未进入
  Sink 的 terminal outputs 被丢弃，已进入 Sink 的调用在统一可配置 deadline 内收敛；首个
  触发 error 始终是根因，停止期间错误作为可识别的 secondary errors，详见
  [Failure Design §2](designs/0006-failure-panic-and-shutdown.md#2-failjob取消与关闭)。
- Job 可被不同 Runtime 重复并发执行，Runtime 是一次性执行容器；每次 Run 创建一个 Source、
  每条 lane 一套 Operator Chain 和一个共享 Sink。Operator 可选实现独立生命周期接口，组件
  按 Sink、Operator、Source 顺序 Open 并逆序清理；私有 typed adapter 承担异构类型擦除，
  详见 [Job Design](designs/0002-job-definition-and-runtime-instantiation.md)。
- M1 先只记录成功完成的 work 累计数，由外部按采样增量计算吞吐量；记录发生在成功终态
  线性化之后且每个 work 至多一次，零输出计数，失败、取消和 unknown 不计数。Runtime option
  注入快速、并发安全、非阻塞的 recorder，默认 no-op，完整公开 Metrics API 延后到 M1/M2
  实现后审核，详见 [Verification Design §1.6](designs/0008-runtime-verification-and-observability.md#16-m1-指标记录能力)。
- M1 并发测试优先使用可控 fake、barrier、clock/executor，只为外部边界无法观察的 Runtime
  内部窗口保留私有 hook；关键测试不依赖 `time.Sleep`。Runtime goroutine 使用结构化 execution
  group 追踪并在 `Run` 返回前回收；违反取消契约的超时例外遵循
  [Failure Design §2.3](designs/0006-failure-panic-and-shutdown.md#23-context-与阻塞点)。测试以 race detector 和 leak detector 兜底，详见
  [Verification Design §1.7](designs/0008-runtime-verification-and-observability.md#17-确定性并发测试与-goroutine-回收)。
- M1 benchmark 使用 Runtime overhead、CPU-bound 和真实墙钟 blocking-I/O simulation 三类
  最小 workload；标准 `testing.B` 输出与 `ReportMetric` 是原始事实，`benchstat` 负责多轮比较。
  结果联合解释 work 级吞吐、延迟、分配和 peak in-flight，不允许通过扩大资源或削弱语义制造
  提升，详见 [Verification Design §1.8](designs/0008-runtime-verification-and-observability.md#18-m1-benchmark)。
- M2 Operator Retry 以整个 work attempt 为恢复单位，在 `JobBuilder` 配置 finite 或 explicit
  unlimited budget 及内置 fixed/exponential backoff。任意 retry blocker 先原子暂停整个 Source
  的新业务 admission，已接纳 work 继续收敛；所有 blocker 成功后恢复，有限预算耗尽只能
  FailJob。Runtime 为每个 active failed work 保留第一次 error，详见
  [Failure Design §1](designs/0006-failure-panic-and-shutdown.md#1-失败暂停与恢复)。
- Positioned split 内 Connector 必须按恢复顺序交接 Source element；Runtime 不解析不透明
  position，而按 admission 顺序追踪 Work completion，只把连续成功前缀末端交给 Connector
  转换和提交。Envelope 是私有 work scope，Completion 直接由 `WorkID` 定位，不增加
  `CompletionID`；Retry 更换 attempt identity，异步 Sink 输出使用 `SinkItemID`。M2 暂定一个
  Source element 恰好产生一个 Record，未来 checkpoint 保存完整 split state 并成为恢复权威，
  详见 [Position Design §1](designs/0007-position-and-kafka-rebalance.md#1-position-与第一版一致性保证)。
- Completion Tracker 保持 Runtime 私有，不公开逐记录 Ack、Completion identity 或 Done handle。
  Work 只以 Success/Failed/Cancelled 终结并恰好释放一次 permit；多输出必须全部 Sink Success 才
  成功，首个失败立即 FailJob 并进入有界 drain。Runtime 按完整 ownership scope 维护 admission
  顺序 gap，只让连续 Success 前缀推进 safe position，并把 safe、in-flight commit、committed
  position 和 generation fence 分离，详见
  [Position Design §1.4](designs/0007-position-and-kafka-rebalance.md#14-work-终态success-与-permit)。

- Kafka 固定 franz-go v1.21.6 为适配基线；BlockRebalanceOnPoll 只保护 poll 到缓存登记的短窗口，
  不等待业务完成。客户端内部限制在途及缓冲 fetch 数，Connector 已取出/转换中数据
  使用全局记录预算；按预留容量分批 poll，满时暂停。第一版不新增 yaspe 字节/解压
  限制，保留客户端原有配置和保护，不承诺整个 Source 的固定记录数或字节上限。
  MaxConcurrentFetches 默认 2，Connector 缓冲默认 1,024 条，两者均可配置且须为正整数；
  具体组合仍需实现验证，详见
  [Kafka Design §3.1–3.2](designs/0007-position-and-kafka-rebalance.md#31-客户端候选与-poll-登记窗口)
  与 [ADR-0007](decisions/0007-layered-source-prefetch-budgets.md)。
- Kafka 禁用自动提交，Runtime 周期合并 safe frontier；每 Source 一个提交请求，有限
  重试后最终失败 FailJob。普通周期默认 3 秒、可配置；逻辑提交总超时 5 秒，退避初始
  100 毫秒并增长至最多 1 秒。revoke 暂停新普通提交，旧请求确认成功后才提交冻结位置；
  旧请求超时/取消/最终失败后不再补交，本地期限不代表精确 Kafka 外部期限或延长 ownership。
  空控制回调不直接传给 Runtime；Lost 立即 fence，不等于必然 FailJob，详见
  [Kafka Design §3.3–3.5](designs/0007-position-and-kafka-rebalance.md#33-offset-提交与-revoke-交接)。
- Kafka 第一版限定 classic Consumer Group，支持 eager/cooperative 分配；具体客户端版本
  已固定，局部模拟已观察到 classic 路径，完整兼容性仍待验证，详见
  [Kafka Design §3.7](designs/0007-position-and-kafka-rebalance.md#37-第一版-group-协议范围)。
- Revoke 本地计时从对应 callback 进入时刻开始；OnPartitionsCallbackBlocked 是异步
  诊断/提示，不作为可靠起点，不声称本地预算覆盖 callback 前的窗口等待或外部耗时，
  详见 [Kafka Design §3.4.1](designs/0007-position-and-kafka-rebalance.md#341-本地期限与外部期限的不确定性)。
- Kafka `SessionRecoveryTimeout` 默认 1 分钟且必须大于零，覆盖初次建立和会话恢复；
  同次重试不刷新预算，成功处理 assignment 后结束计时，空 assignment 也可成功。
  错误按类型与阶段区分，最终 offset 提交不借用会话恢复预算；客户端版本信号仍待核验，
  详见 [Kafka Design §3.6](designs/0007-position-and-kafka-rebalance.md#36-会话建立与恢复)。
- Runtime 提供 `SourceContext.ReportFailure(error) error`，独立于业务背压接收最终 Source
  失败；输入是根因，返回值是报告接收状态。重复报告不覆盖首因，关闭后迟到报告快速
  返回关闭错误，统一 RunError 因果规则保持有效，详见
  [Source Design §1.1.2](designs/0003-source-reader-and-admission.md#112-独立的最终失败报告)
  与 [ADR-0006](decisions/0006-source-failure-reporting-and-session-recovery.md)。

能力契约与依赖见 [Design Map](designs/design-map.md)，长期取舍索引见
[Decision Index](decisions/README.md)。

ClickHouse 已接受的具体决定：

- 业务提供目标表、列映射和写入设置，Connector 不按表引擎自动路由或强制某种转发/
  服务端异步配置。完整 INSERT 的成功按实际配置确认边界解释，不能推断所有 shard/
  副本的持久化，详见 [ClickHouse Design §2](designs/0009-clickhouse-connector.md#2-业务配置与成功边界)
  与 [ADR-0008](decisions/0008-clickhouse-business-owned-write-semantics.md)。
- 采用 clickhouse-go/v2 v2.48.0 Native API，先在 Connector 组批，再开始尝试并 Prepare。
  每次尝试新建 batch/context，Send 才形成完整写入结果；IsSent 和驱动 Close/Abort
  不作为行成功依据，详见 [ClickHouse Design §3](designs/0009-clickhouse-connector.md#3-固定客户端与-batch-生命周期)。
- 每批默认 5,000 行、组批等待 1 秒、总容量 10,000 item、并发 INSERT 2，均可配置且
  必须为正值；最早行等待不因新行重置，时间到期是发送资格而非完成保证。全部目标
  共享待转换/buffer/in-flight/retry 容量与并发，空分组回收且不长期饿死已就绪目标，
  详见 [ClickHouse Design §4](designs/0009-clickhouse-connector.md#4-组批与容量)。
- 一条 SinkItem 映射为恰好一行，业务提供 Table、Columns、Values；接管后转换与校验
  一次，列值数量匹配，重试复用稳定结果。多目标按表与有序列集合组批，写入设置固定
  在 Sink 配置中；零/多行由上游 Filter/FlatMap 表达。转换失败时不能把已接管 group
  改判为拒收，详见 [ClickHouse Design §4.3](designs/0009-clickhouse-connector.md#43-输入映射与多目标组批)。
- 可恢复暂时故障含未知结果可有限重试，接受重复风险；单次最多 5 秒、总预算最多
  10 秒、最多 3 次含首次，退避 200 毫秒至最多 1 秒，更早 Close deadline 优先。
  重试保持稳定数据与定义，历史 Unknown 不被最后一次未发送覆盖，详见
  [ClickHouse Design §5](designs/0009-clickhouse-connector.md#5-clickhouse-有限重试)。

M2 最小指标已接受：成功 work 吞吐量、每个 Kafka partition 的消费/commit offset 差、
客户端/Source Connector/Runtime/Sink 的分层数量。消费位置以 poll 已取出的记录计，
两端统一为 next offset；不同层的记录、work、item 数不能简单相加。失败指标延后，
错误报告和故障测试继续有效，详见
[Verification Design §1.11](designs/0008-runtime-verification-and-observability.md#111-m2-最小指标范围)。

## 当前开放问题与顺序

M2 验收设计已接受：按既有契约在 Source、Operator、Sink、commit、ownership 和关闭
边界注入故障，恢复后用稳定测试 ID/输出序号核对预期结果，缺失为零、重复可解释。
at-least-once 带 Source 重放与数据保留、Sink 确认配置、故障消除及模型范围前提；三层
验证证据不互相冒充，失败指标仍延后。设计接受不表示这些测试已完成。

完整清单见 [Verification Design §2](designs/0008-runtime-verification-and-observability.md#2-设计收敛与验证断点)。当前顺序：

1. M1 实现：Job Definition、Memory Source 和 Memory Sink 已通过相应测试；接下来接入
   Runtime 生命周期、调度和 FailJob，完整 Runtime 验收仍待完成；
2. Kafka 主要设计已收敛，完整适配验证仍待完成：真实 broker、多实例 eager/cooperative、
   提交超时/重试及旧请求、恢复 timer/迟到事件、默认预取组合和大消息/长期背压。
   已通过的七项与证据限制见 [验证附件](verification/franz-go-v1.21.6/README.md)，详细矩阵
   见 [Kafka Design §4](designs/0007-position-and-kafka-rebalance.md#4-当前开放问题)。
3. ClickHouse 主要设计已收敛，实际输入映射/组批、容量/热点调度、真实客户端/服务端、
   连接池竞争、完整 timeout/retry/Close、错误分类及交付前提仍待验证，见
   [ClickHouse Design §7](designs/0009-clickhouse-connector.md#7-尚未完成的适配验证)。

## 当前唯一下一步

实现 Runtime 的 Memory Source → Operator Chain → Memory Sink 最小链路，落实每次运行
的组件实例化与生命周期、有界 admission 和调度、attempt 输出暂存、同步 Sink 交接、
FailJob、取消与统一关闭，以及成功 work 计数。按
[Job Design](designs/0002-job-definition-and-runtime-instantiation.md)、
[核心执行模型](designs/0001-core-execution-model.md) 和
[Verification Design](designs/0008-runtime-verification-and-observability.md) 补齐确定性测试及 benchmark。

## 最近验证

验证日期：2026-09-08。Job Definition 与独立 Memory Source/Sink 已通过相应验证，Runtime 和生产 Connector 行为仍待实现。

- `go test -count=1 ./...` 与 `go test -race -count=1 ./...`：Job 构建、公开 API 编译契约、
  并发派生/Build、fluent 内置转换与已有 Operator 测试通过；
- `go test -race -count=1 -timeout 30s ./...`：Memory Source 基础、通知、背压、终态竞争与多
  生产者交付，以及 Memory Sink 整组交接、失败计划、快照、取消、Close 竞争和使用示例均通过；
  并发等待由 `testing/synctest` 与 barrier 控制；
- `go vet ./...`：通过；
- 2026-09-07 在 Kafka 版本验证附件运行 `go test -race -v -count=1 -timeout 60s ./...`：七项通过，
  原始输出和限定范围见 [验证附件](verification/franz-go-v1.21.6/README.md)；
- 2026-09-08 在 ClickHouse 附件运行 `python3 run_probe.py`，原始 v2.48.0 驱动副本上七项
  TestProbe 带 race detector 通过，输出与限制见 [验证附件](verification/clickhouse-go-v2.48.0/README.md)；
- `git diff --check`：通过；
- Markdown 相对链接目标与章节锚点检查：通过；
- Runtime race/fault、真实 Kafka/ClickHouse 故障测试及 benchmark：尚未运行；不能与局部驱动测试混同。
