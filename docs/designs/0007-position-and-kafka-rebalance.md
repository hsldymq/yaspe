# 0007：Position、Ownership 与 Kafka Rebalance

状态：Accepted（总体契约已定；M2 公开表示与客户端适配仍待收敛）
最后更新：2026-08-26
适用阶段：M2
依赖：[核心执行模型](0001-core-execution-model.md) · [Source Reader](0003-source-reader-and-admission.md) · [Sink Handoff](0005-sink-handoff-and-completion.md) · [ADR-0002](../decisions/0002-use-kafka-consumer-group-for-external-coordination.md)

本文是 safe position、generation fence、一致性边界和 Kafka rebalance 收尾的权威契约。

## 1. Position 与第一版一致性保证

### 1.1 连续 safe position

记录可以乱序完成，但 Source position 只能推进到 split 内连续完成位置：

```text
100 complete
101 pending
102 complete
103 complete

safe frontier: 100
resume offset: 101
```

101 完成后可以一次推进到 103。Kafka Connector 将 Runtime safe position 转换为 Kafka offset commit。

### 1.2 generation fence

每次 partition ownership 带 generation。旧 ownership 的迟到 completion、Sink callback 或 commit 请求不能推进新 owner 的 position。

### 1.3 at-least-once

Sink 未明确成功时不能推进 position。结果未知时，为避免丢失，第一版选择重试或失败恢复，而不是提前确认。这可能产生重复。

第一版目标是边界明确的 at-least-once：

- 不因 Source 已读取、Collector 已接受或 Sink 已入队而提前确认；
- 崩溃后从未安全提交位置重放；
- 外部结果未知时优先避免丢失；
- 没有事务或幂等 Sink 时不承诺 exactly-once；
- 不可重放 Source 不保证无丢失恢复。


## 2. Kafka Rebalance

Revoke 开始后，第一版暂停该 Kafka Source 所有 split 的新业务 admission，让现有资源优先
用于收尾；Kafka heartbeat、session 和必要的 poll/control 处理必须继续运行。暂停全局
admission 不等于撤销所有 ownership：只有 Kafka 报告的 revoked split 进入 drain、commit、
缓存清理和 generation fence，retained split 保留当前 ownership generation 和有界未交接
缓存，rebalance 收尾后恢复 admission 并优先交接已有缓存。

对于 revoked split，Runtime 冻结收尾集合：尚未开始 Operator Chain 的 work 不再启动；
已经开始的 work 允许在期限内完成 Chain、把 terminal output 原子交给 Sink，并等待 Sink
completion；已经被 Sink 接受的操作继续等待明确结果。正常零输出且已经完成的 work 保留
Success。这样可以尽量填补并发处理形成的 position gap，减少新 owner 重放已经产生 Sink
effect 的较大 position。在 ownership 失效前，Runtime 允许：

- 收敛已开始的 Chain；
- 将这些 Chain 成功形成的 terminal output 交给 Sink；
- 等待已被 Sink 接受的操作；
- 推进并提交连续 safe position。

默认 `RevokeDrainTimeout` 为 30 秒。实际收尾 deadline 取用户配置和 Kafka Connector 当前
可用 rebalance deadline 中较早者，并为最终 safe position commit、控制回调返回和协议推进
预留安全时间；收尾不得无限阻塞 rebalance。期限到期时取消仍未交给 Sink 的 work，Sink-owned
unknown 不得标记成功，只提交当时连续的 safe position。

Ownership 失效后：

- Connector 丢弃该 split 尚未交接的缓存；
- Runtime 不启动该 split 已接受但未执行的 work；
- 正在计算的 work 可以收到取消；
- 迟到 terminal output 不得转移给 Sink；
- 已被 Sink 接受的操作无法假定可撤销，但不得再推进当前 position；
- 已完成但未在旧 ownership 内提交的进度不得失效后补交。

新 owner 从最后成功持久化的 safe position 恢复。旧 ownership 已产生外部效果但未提交的记录可能重复，这是 at-least-once 边界。

Kafka offset 按 consumer group、topic 和 partition 独立保存，committed offset 表示下一条
应读取的 offset；Connector 负责把 Runtime 的最后连续完成 position 转换成该语义。被
revoked 的 split 即使重新分配给同一实例也创建新 generation、丢弃旧预取并从 committed
offset 恢复；retained split 不重置读取位置或 generation。

同一机制同时支持 eager 和 cooperative rebalance：eager 通常把全部当前 assignment 作为
revoked 集合处理，cooperative 只处理实际移动的子集。正确性不得依赖 cooperative 一定启用，
但生产默认可以优先使用 cooperative 以减少暂停和重放。Kafka 报告 split 已 lost 时，旧
ownership 可能已被其他 Consumer 接管，Connector 必须立即 fence generation、清理本地缓存
且不再提交旧 position，不执行正常 revoke drain。

