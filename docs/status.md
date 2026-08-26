# yaspe Current Status

最后更新：2026-08-26

本文是动态交接快照，不是完整设计记录。完整契约见正式 Design，决定背景和取舍见
[决策索引](decisions/README.md)，维护规则见 [Documentation Governance](governance.md)。

## 当前里程碑

M0 — 核心语义与项目基线。

## 当前目标

先收敛所有影响 M1/M2 公共 API、ownership、并发和恢复正确性的设计，再开始 Runtime、
Kafka 和 ClickHouse 编码。局部私有类型、package 组织和不改变公开保证的数据结构可以由
受约束原型细化。

## 三维能力状态

| 能力 | Design | Implementation | Verification | 权威位置 |
|---|---|---|---|---|
| Record / Operator | Accepted | Implemented | Unit Tested | [Operator Design §1](designs/0004-operator-attempt-and-collector.md#1-collector-生命周期与并发) |
| Collector scope-bound context API | Accepted | Implemented | Unit Tested | [Operator Design §1.3](designs/0004-operator-attempt-and-collector.md#13-emit-契约) |
| Map / Filter / FlatMap | Accepted | Implemented | Unit Tested | [Operator Design §2](designs/0004-operator-attempt-and-collector.md#2-operator-chain-与-work-attempt-边界) |
| M1 Reader / Memory Source 语义 | Accepted | Not Started | Not Applicable | [Source Design](designs/0003-source-reader-and-admission.md) |
| M1 同步 Memory Sink 语义 | Accepted | Not Started | Not Applicable | [Sink Design §1.1.1](designs/0005-sink-handoff-and-completion.md#111-m1-同步-memory-sink) |
| 线性 Job Definition | Accepted | Not Started | Not Applicable | [Job Design](designs/0002-job-definition-and-runtime-instantiation.md) |
| M1 Stateless Runtime | Accepted | Not Started | Not Applicable | [Verification Design §2.1](designs/0008-runtime-verification-and-observability.md#21-m1-实现前必须收敛) |
| M2 Operator work Retry | Accepted | Not Started | Not Applicable | [Failure Design §1](designs/0006-failure-panic-and-shutdown.md#1-失败暂停与恢复) |
| M2 Position / Completion | Discussing | Not Started | Not Applicable | [Verification Design §2.2](designs/0008-runtime-verification-and-observability.md#22-m2-实现前必须收敛) |
| 异步 Sink 协议 | Discussing | Not Started | Not Applicable | [Sink Design](designs/0005-sink-handoff-and-completion.md) |
| Kafka Consumer Group / Rebalance | Accepted | Not Started | Not Applicable | [ADR-0002](decisions/0002-use-kafka-consumer-group-for-external-coordination.md) |
| Kafka / ClickHouse Connector | Discussing | Not Started | Not Applicable | [Roadmap M2](roadmap.md#6-m2source-position完成跟踪与生产级-sink) |
| Dead Letter / Side Output | Planned for later | Not Started | Not Applicable | [Roadmap M4](roadmap.md#8-m4keyby分区执行与逻辑物理执行图) |

## 当前代码事实

```text
package yaspe
├── Record[T]
├── Collector[T]
└── Operator[I, O]

package operator
├── Map[I, O]
├── Filter[T]
└── FlatMap[I, O]
```

已有单元测试覆盖：

- Map 的正常转换、transform error、Emit error 和 context 传递；
- Filter 的匹配/不匹配、predicate error、Emit error 和 context 传递；
- FlatMap 的零/多输出、输出顺序、transform error 和中途 Emit error。

尚不存在 JobBuilder、Transformation、Runtime、Source/Sink Connector、position、completion
tracker、Kafka 或 ClickHouse 实现。

## 最近接受的决定

- 仓库文档、代码和测试作为跨会话项目记忆；Design、Implementation、Verification 分开跟踪，
  新会话按统一入口和 Status 接力，详见 [ADR-0003](decisions/0003-use-repository-docs-as-project-memory.md)；
- Job Definition 使用 `JobDraft → Stream[T] → JobBuilder → Job` 的 Go 1.27 type-state fluent
  API；`From/Transform/SinkTo` 及 Func 变体保存 Factory，Build 产生不可变 Job 快照且不创建
  运行资源；第一版只接受单 Source、线性 Chain 和单 Sink；
- 每条 execution lane 创建独立 Operator 包装实例，同一用户函数值可以跨 lane 共享并并发调用；
- Runtime 不提供 `SkipRecord`、`DiscardRecord` 或 Transformation `OnError`；可忽略业务错误
  由用户函数收敛为正常零输出，未处理 error 进入 Job 级 Retry/FailJob；
- Dead Letter 延后为显式业务输出、Side Output、分支和专用 Sink，不是 Runtime 失败终态；
- Kafka revoke 暂停该 Source 全部新 admission，started/Sink-owned work有限收敛，默认期限
  30 秒且受 Connector 更早 deadline 限制；eager/cooperative/lost 共用 generation 机制。
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
- M1 Reader 的非阻塞读取在语义上返回 `ReadResult[T], error`，`TryRead` 等只是参考名称；
  result 只表达 ready/unavailable/finished，error 表达读取失败，invalid state 作为契约错误
  进入 Job 级 failure 路径且不借用 Operator work Retry。正常结束先交付
  已缓存记录，读取失败则优先于尚未交接的缓存，详见
  [Source Design §1.2](designs/0003-source-reader-and-admission.md#12-非阻塞-reader)。
- Memory Source 定位为动态有界的 Runtime 参考 Source、确定性测试设施、benchmark 输入和
  本地示例数据源；Runtime-facing Source 与 producer Controller 分离，并已接受 Submit 背压/
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
  group 追踪并在 `Run` 返回前回收，再以 race detector 和 leak detector 兜底，详见
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

能力契约与依赖见 [Design Map](designs/design-map.md)，长期取舍索引见
[Decision Index](decisions/README.md)。

## 当前开放问题与顺序

完整清单见 [Verification Design §2](designs/0008-runtime-verification-and-observability.md#2-当前开放问题)。当前顺序：

1. M2 Source split/position 与 Runtime Envelope 的表示和 identity 组织；
2. M2 异步 Sink/completion 及 Sink effect Retry；
3. Kafka/ClickHouse Connector、M2 指标、故障注入和交付保证审核。

## 当前唯一下一步

讨论并接受 M2 Source split/position 与 Runtime Envelope 的表示和 identity 组织。

## 最近验证

- `go test ./...`：通过；
- `git diff --check`：通过；
- Markdown 相对链接目标检查：通过；
- Runtime/fault/race benchmark：尚不适用或尚未运行。

## 工作区交接说明

- 当前存在未提交的 Design Map、按能力拆分的权威 Design、Architecture/Status/Roadmap/ADR/
  Decision Index 链接迁移，以及 Job Definition 契约更新；
- `Emit(record)` 代码与 Operator 测试已在当前 HEAD，不属于本次未提交 diff；
- 尚未开始 M1/M2 Runtime 或 Connector 编码；
- 新会话必须先检查实际 `git status` 和 diff，不能仅依赖本节；
- 当前本地工具链：`go1.27.0-X:nodwarf5 linux/amd64`；`go.mod` 要求 Go 1.27。
