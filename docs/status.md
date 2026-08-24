# yaspe Current Status

最后更新：2026-08-24

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

完整索引与权威链接见 [Decision Index](decisions/README.md)。

## 当前开放问题与顺序

完整清单见 [Core Design §16](designs/0001-core-execution-model.md#16-当前开放问题)。当前顺序：

1. Emit 成功/失败后的引用数据 ownership 与复制规则；
2. 用户函数、lane-local Operator、Collector 和 callback 的并发契约；
3. Collector context 与 panic/FailJob；
4. M1 Source Reader、admission、Memory Sink 和 Job API 定稿；
5. M2 Retry、position/Envelope、异步 Sink/completion；
6. Kafka/ClickHouse Connector、指标、故障注入和交付保证审核。

### 当前问题：Emit ownership

问题：`Collector.Emit` 成功或失败后，`Record[T]` 及其 slice、map、pointer 等引用数据归谁
所有，Runtime 是否复制，调用方何时可以修改或复用？

影响阶段：M1–M2。

已知约束：

- `T` 是任意 Go 类型，Runtime 无法通用、安全地深拷贝；
- 同步 Chain 的 terminal output 会在 attempt 成功前由 Runtime 暂存；
- Sink 整组接管成功前后需要明确责任转移；
- ownership 规则必须覆盖 Emit 成功、Emit 失败、Process 返回和异步 Sink callback。

候选方向：

- Emit 成功即转移 ownership，调用方不得继续修改或复用；
- 借用到 Process 返回，由 Runtime 在边界复制必要数据；
- 通过可选 copier/serializer 显式选择复制。

当前倾向：尚未接受。需要同时比较正确性、API 可理解性和复制成本。

完成条件：Core Design 明确每个边界的 ownership、允许操作、失败行为和测试要求，并从本节移除。

## 当前唯一下一步

讨论并接受 `Collector.Emit` 成功和失败后的引用数据 ownership 与复制规则。

在该问题收敛前不开始受其影响的 Runtime、queue 或 Sink 实现。

## 最近验证

- `go test ./...`：通过；
- `git diff --check`：通过；
- Markdown 相对链接目标检查：通过；
- Runtime/fault/race benchmark：尚不适用或尚未运行。

## 工作区交接说明

- 当前存在未提交的文档治理和设计更新；
- 尚未开始 M1/M2 Runtime 或 Connector 编码；
- 新会话必须先检查实际 `git status` 和 diff，不能仅依赖本节；
- 当前本地工具链：`go1.27.0-X:nodwarf5 linux/amd64`；`go.mod` 要求 Go 1.27。
