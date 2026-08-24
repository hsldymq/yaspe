# yaspe Current Status

最后更新：2026-08-25

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
| Record / Collector / Operator | Accepted | Implemented | Unit Tested | [Core Design §5](designs/0001-core-execution-model.md#5-collector-生命周期与并发) |
| Map / Filter / FlatMap | Accepted | Implemented | Unit Tested | [Core Design §6](designs/0001-core-execution-model.md#6-operator-chain-与-work-attempt-边界) |
| 线性 Job Definition | Accepted | Not Started | Not Applicable | [Core Design §3.1](designs/0001-core-execution-model.md#31-第一版线性-job-definition) |
| M1 Stateless Runtime | Discussing | Not Started | Not Applicable | [Core Design §16.1](designs/0001-core-execution-model.md#161-m1-实现前必须收敛) |
| M2 Position / Completion | Discussing | Not Started | Not Applicable | [Core Design §16.2](designs/0001-core-execution-model.md#162-m2-实现前必须收敛) |
| 异步 Sink 协议 | Discussing | Not Started | Not Applicable | [Core Design §7](designs/0001-core-execution-model.md#7-sink-交接与-completion) |
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
- JobBuilder 持有 Transformation 定义，`Stream[T]` 提供 Go 1.27 泛型 fluent API，`Build`
  产生不可变 Job 快照；第一版只接受单 Source、线性 Chain 和单 Sink；
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

完整索引与权威链接见 [Decision Index](decisions/README.md)。

## 当前开放问题与顺序

完整清单见 [Core Design §16](designs/0001-core-execution-model.md#16-当前开放问题)。当前顺序：

1. Collector context 与 panic/FailJob；
2. M1 Source Reader、admission、Memory Sink 和 Job API 定稿；
3. M2 Retry、position/Envelope、异步 Sink/completion；
4. Kafka/ClickHouse Connector、指标、故障注入和交付保证审核。

### 当前问题：Collector context 与 panic/FailJob

问题：`Collector.Emit` 是否继续显式接收 context，还是绑定到当前 `Process` scope；用户函数
或 Operator panic 时 Runtime 是否 recover、保留哪些诊断信息，并如何触发 FailJob？

影响阶段：M1。

已知约束：

- `Process` 已显式接收当前 attempt context，现有 `Collector.Emit` 也接收 context；
- Collector 只在对应 `Process` goroutine 中串行使用，`Process` 返回后失效，因此可以绑定当前
  execution scope；
- Runtime 管理的所有阻塞点必须响应取消，但调用方传入不同或脱离当前 attempt 的 context
  可能破坏统一取消和责任边界；
- panic 不能导致 Runtime 静默丢失 work 或让 Worker goroutine 无诊断退出，也不能被当作正常
  业务 error 自动忽略。

候选方向：

- 保留 `Emit(ctx, record)`，并定义传入 context 必须与 `Process` context 相同或由其派生；
- 改为 `Emit(record)`，由 Collector 内部绑定当前 attempt context，消除错误 context 的可能；
- panic 统一在 Runtime 调用用户代码的边界 recover，记录 panic value 与 stack，并直接进入
  FailJob；或把 panic 包装为普通 attempt error 再交给 Retry 策略。

当前倾向：尚未接受。需要比较 API 显式性与 execution scope 一致性，并决定 panic 是否允许
进入可能重试用户代码的普通错误路径。

完成条件：Core Design 明确 Collector context 来源、取消传播、panic recovery 边界、stack
诊断、错误分类和 FailJob 动作，并从本节移除。

## 当前唯一下一步

讨论并接受 Collector context 与用户函数 panic/FailJob 契约。

在该问题收敛前不开始受其影响的 Runtime、Connector 或 Sink 实现。

## 最近验证

- `go test ./...`：通过；
- `git diff --check`：通过；
- Markdown 相对链接目标检查：通过；
- Runtime/fault/race benchmark：尚不适用或尚未运行。

## 工作区交接说明

- 当前存在未提交的文档治理与 Emit ownership 设计更新；
- 尚未开始 M1/M2 Runtime 或 Connector 编码；
- 新会话必须先检查实际 `git status` 和 diff，不能仅依赖本节；
- 当前本地工具链：`go1.27.0-X:nodwarf5 linux/amd64`；`go.mod` 要求 Go 1.27。
