# yaspe Decision Index

本文索引重要 ADR 和正式 Design 中的局部决定。完整背景、取舍和验证要求以链接的权威文档
为准。

## ADR

| ID | 决策 | 状态 | 影响阶段 | 权威文档 |
|---|---|---|---|---|
| ADR-0001 | Runtime 控制 Source 数据进入（原资源保证） | Superseded by ADR-0007 | M1+ | [0001](0001-runtime-controlled-source-ingestion.md) |
| ADR-0002 | 早期多实例 Kafka Source 使用 Consumer Group 协调（原预算） | Superseded by ADR-0005 | M2+ | [0002](0002-use-kafka-consumer-group-for-external-coordination.md) |
| ADR-0003 | 使用仓库文档保存项目记忆与三维状态 | Accepted | 全阶段 | [0003](0003-use-repository-docs-as-project-memory.md) |
| ADR-0004 | Sink Retry 留在 Connector 内部，最终失败由 Runtime FailJob | Accepted | M2–M6 | [0004](0004-keep-sink-retry-inside-connector.md) |
| ADR-0005 | 保留 Consumer Group 协调，由 Connector 提供 revoke 时间预算 | Accepted | M2+ | [0005](0005-connector-owned-revoke-budget.md) |
| ADR-0006 | Source 最终失败独立报告，Kafka 会话恢复由 Connector 限时管理 | Accepted | M1–M2 | [0006](0006-source-failure-reporting-and-session-recovery.md) |
| ADR-0007 | Source 按层计数，Kafka 不新增字节限制 | Accepted | M1–M2 | [0007](0007-layered-source-prefetch-budgets.md) |
| ADR-0008 | ClickHouse 目标与写入策略归业务，按实际配置确认结果 | Accepted | M2+ | [0008](0008-clickhouse-business-owned-write-semantics.md) |
| ADR-0009 | 区分优雅停止与强制取消，先停止输入再 drain 处理和输出 | Accepted | M1 扩展及后续 | [0009](0009-separate-graceful-stop-from-cancellation.md) |

## 核心执行模型局部决定

| ID | 决策 | 状态 | 影响阶段 | 权威位置 |
|---|---|---|---|---|
| D-EXEC-001 | M1/M2 使用并行完整 Pipeline，单 work 内同步执行 Chain | Accepted | M1–M2 | [Core Design §3](../designs/0001-core-execution-model.md#3-近期执行形态) |
| D-EXEC-002 | 每条 lane 创建独立 Operator 包装实例，用户函数值可以共享 | Accepted | M1+ | [Job Design](../designs/0002-job-definition-and-runtime-instantiation.md) |
| D-JOB-001 | type-state Job Definition 模型，Build 产生不可变快照 | Accepted | M1+ | [Job Design](../designs/0002-job-definition-and-runtime-instantiation.md) |
| D-SOURCE-001 | Source 使用非阻塞 Reader、独立 split control 与两阶段 revoke handle | Accepted | M1–M2 | [Source Design](../designs/0003-source-reader-and-admission.md) |
| D-SOURCE-002 | Runtime 提供 SourceContext.ReportFailure，独立于数据 admission 报告最终失败 | Accepted | M1+ | [Source Design §1.1.2](../designs/0003-source-reader-and-admission.md#112-独立的最终失败报告) |
| D-FAIL-001 | Runtime 不提供 Skip/DiscardRecord；业务错误由用户函数收敛为正常零输出 | Accepted | M1+ | [Failure Design §1.1](../designs/0006-failure-panic-and-shutdown.md#11-错误不直接等于退出) |
| D-FAIL-002 | 所有非正常 Run 统一返回保留 primary、active 与 secondary 因果的 RunError | Accepted | M1+ | [Failure Design §1.7](../designs/0006-failure-panic-and-shutdown.md#17-公开-runerror) |
| D-SINK-001 | 一个 work 的 terminal output 向 Sink 整组原子交接 | Accepted | M2+ | [Sink Design §1.1](../designs/0005-sink-handoff-and-completion.md#11-整组责任转移) |
| D-SINK-002 | Sink 使用绑定 reporter、versioned capacity 与统一 deadline 下的逐 item Close | Accepted | M2+ | [Sink Design](../designs/0005-sink-handoff-and-completion.md) |
| D-BUDGET-001 | Runtime 第一版公开 Parallelism 和 MaxInFlightWorks，内部队列保持有界 | Accepted | M1+ | [Core Design §5.3](../designs/0001-core-execution-model.md#53-第一版数量预算) |
| D-REBALANCE-001 | revoke 暂停 Source 全部 admission，started/Sink-owned work有限收敛 | Accepted | M2 | [Kafka Design §2](../designs/0007-position-and-kafka-rebalance.md#2-kafka-rebalance) |
| D-REBALANCE-002 | 独立 RevokeDrainTimeout 默认 30 秒（历史） | Superseded by D-REBALANCE-003 | M2 | [ADR-0002](0002-use-kafka-consumer-group-for-external-coordination.md) |
| D-REBALANCE-003 | Connector 总 deadline 与提交预留，Runtime 推导 drain 截止 | Accepted | M2 | [Source Design §1.3.1](../designs/0003-source-reader-and-admission.md#131-split-control-边界) · [ADR-0005](0005-connector-owned-revoke-budget.md) |
| D-KAFKA-001 | 固定 franz-go v1.21.6，poll 登记短窗口与分层背压 | Accepted | M2 | [Kafka Design §3](../designs/0007-position-and-kafka-rebalance.md#3-kafka-客户端适配) |
| D-KAFKA-002 | 显式安全位置提交及参数，Source 级串行，旧提交最终失败后不补交 | Accepted / version adaptation pending | M2 | [Kafka Design §3.3](../designs/0007-position-and-kafka-rebalance.md#33-offset-提交与-revoke-交接) |
| D-KAFKA-003 | 有限会话建立/恢复、阶段相关错误分类与 assignment 成功边界 | Accepted / version adaptation pending | M2 | [Kafka Design §3.6](../designs/0007-position-and-kafka-rebalance.md#36-会话建立与恢复) |
| D-KAFKA-004 | 默认 2 个 fetch / 1,024 条 Connector 缓冲，可配置且为正整数，不承诺整体记录数或字节上限 | Accepted | M2 | [Kafka Design §3.2](../designs/0007-position-and-kafka-rebalance.md#32-分层缓存与背压) |
| D-KAFKA-005 | Revoke callback 入口计时，blocked 只作诊断；本地期限不等于外部保证，限定 classic group | Accepted | M2 | [Kafka Design §3.4.1](../designs/0007-position-and-kafka-rebalance.md#341-本地期限与外部期限的不确定性) · [§3.7](../designs/0007-position-and-kafka-rebalance.md#37-第一版-group-协议范围) |

## stdio 局部决定

| ID | 决策 | 状态 | 权威位置 |
|---|---|---|---|
| D-STDIO-001 | 输入格式中立、自定义切分，结束时由切分函数处理尾部，默认保留末尾无换行记录 | Accepted | [stdio Design §2](../designs/0010-stdio-and-graceful-stop.md#2-输入切分与尾部) |
| D-STDIO-002 | 写出失败不自动重试，不回滚已输出字节，也不误报成功 | Accepted | [stdio Design §3](../designs/0010-stdio-and-graceful-stop.md#3-输出与失败) |

## 可观测性局部决定

| ID | 决策 | 状态 | 权威位置 |
|---|---|---|---|
| D-OBS-001 | M2 必需指标为成功 work 吞吐、消费/commit offset 差及分层缓存数量；失败指标延后 | Accepted | [Verification Design §1.11](../designs/0008-runtime-verification-and-observability.md#111-m2-最小指标范围) |

## 验收局部决定

| ID | 决策 | 状态 | 权威位置 |
|---|---|---|---|
| D-VERIFY-001 | 故障矩阵、稳定测试身份的输出核对、三层证据；预期输出零缺失且重复可解释 | Accepted | [Verification Design §1.12–1.13](../designs/0008-runtime-verification-and-observability.md#112-m2-故障注入验收矩阵) |
| D-DELIVERY-001 | at-least-once 带 Source 重放/保留、Sink 配置与故障模型前提，不承诺任意配置的持久化 | Accepted | [Position Design §1.10](../designs/0007-position-and-kafka-rebalance.md#110-at-least-once) |

## ClickHouse 局部决定

| ID | 决策 | 状态 | 权威位置 |
|---|---|---|---|
| D-CH-001 | v2.48.0 Native batch；组批后 Prepare，每次尝试重建，Send 确认 | Accepted | [ClickHouse Design §3](../designs/0009-clickhouse-connector.md#3-固定客户端与-batch-生命周期) |
| D-CH-002 | 默认 5,000 行/1 秒/10,000 item 容量/并发 2，均可配置且为正值 | Accepted | [ClickHouse Design §4](../designs/0009-clickhouse-connector.md#4-组批与容量) |
| D-CH-003 | 有限重试保留历史 Unknown，初始五秒尝试/十秒总预算/最多三次 | Accepted | [ClickHouse Design §5](../designs/0009-clickhouse-connector.md#5-clickhouse-有限重试) |
| D-CH-004 | 一 item 一行，接管后转换一次，固定 Sink settings，按表与有序列集合组批 | Accepted | [ClickHouse Design §4.3](../designs/0009-clickhouse-connector.md#43-输入映射与多目标组批) |

## 维护规则

- 新 ADR 接受或被替代时更新本索引；
- 影响公共 API、ownership、并发、恢复或多个阶段的局部决定应加入索引；
- 索引只保存摘要，不复制完整理由；
- 决定改变时保留旧条目并标记 `Superseded`，链接替代决定。
