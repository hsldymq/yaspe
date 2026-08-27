# yaspe Decision Index

本文索引重要 ADR 和正式 Design 中的局部决定。完整背景、取舍和验证要求以链接的权威文档
为准。

## ADR

| ID | 决策 | 状态 | 影响阶段 | 权威文档 |
|---|---|---|---|---|
| ADR-0001 | Source 数据进入由 Runtime admission 控制 | Accepted | M1+ | [0001](0001-runtime-controlled-source-ingestion.md) |
| ADR-0002 | 早期多实例 Kafka Source 使用 Consumer Group 协调 | Accepted | M2+ | [0002](0002-use-kafka-consumer-group-for-external-coordination.md) |
| ADR-0003 | 使用仓库文档保存项目记忆与三维状态 | Accepted | 全阶段 | [0003](0003-use-repository-docs-as-project-memory.md) |
| ADR-0004 | Sink Retry 留在 Connector 内部，最终失败由 Runtime FailJob | Accepted | M2–M6 | [0004](0004-keep-sink-retry-inside-connector.md) |

## 核心执行模型局部决定

| ID | 决策 | 状态 | 影响阶段 | 权威位置 |
|---|---|---|---|---|
| D-EXEC-001 | M1/M2 使用并行完整 Pipeline，单 work 内同步执行 Chain | Accepted | M1–M2 | [Core Design §3](../designs/0001-core-execution-model.md#3-近期执行形态) |
| D-EXEC-002 | 每条 lane 创建独立 Operator 包装实例，用户函数值可以共享 | Accepted | M1+ | [Job Design](../designs/0002-job-definition-and-runtime-instantiation.md) |
| D-JOB-001 | type-state Job Definition 模型，Build 产生不可变快照 | Accepted | M1+ | [Job Design](../designs/0002-job-definition-and-runtime-instantiation.md) |
| D-SOURCE-001 | Source 使用非阻塞 Reader、独立 split control 与两阶段 revoke handle | Accepted | M1–M2 | [Source Design](../designs/0003-source-reader-and-admission.md) |
| D-FAIL-001 | Runtime 不提供 Skip/DiscardRecord；业务错误由用户函数收敛为正常零输出 | Accepted | M1+ | [Failure Design §1.1](../designs/0006-failure-panic-and-shutdown.md#11-错误不直接等于退出) |
| D-FAIL-002 | 所有非正常 Run 统一返回保留 primary、active 与 secondary 因果的 RunError | Accepted | M1+ | [Failure Design §1.7](../designs/0006-failure-panic-and-shutdown.md#17-公开-runerror) |
| D-SINK-001 | 一个 work 的 terminal output 向 Sink 整组原子交接 | Accepted | M2+ | [Sink Design §1.1](../designs/0005-sink-handoff-and-completion.md#11-整组责任转移) |
| D-BUDGET-001 | Runtime 第一版公开 Parallelism 和 MaxInFlightWorks，内部队列保持有界 | Accepted | M1+ | [Core Design §5.3](../designs/0001-core-execution-model.md#53-第一版数量预算) |
| D-REBALANCE-001 | revoke 暂停 Source 全部 admission，started/Sink-owned work有限收敛 | Accepted | M2 | [Kafka Design §2](../designs/0007-position-and-kafka-rebalance.md#2-kafka-rebalance) |
| D-REBALANCE-002 | RevokeDrainTimeout 默认 30 秒且受 Connector 更早 deadline 限制 | Accepted | M2 | [Kafka Design §2](../designs/0007-position-and-kafka-rebalance.md#2-kafka-rebalance) |

## 维护规则

- 新 ADR 接受或被替代时更新本索引；
- 影响公共 API、ownership、并发、恢复或多个阶段的局部决定应加入索引；
- 索引只保存摘要，不复制完整理由；
- 决定改变时保留旧条目并标记 `Superseded`，链接替代决定。
