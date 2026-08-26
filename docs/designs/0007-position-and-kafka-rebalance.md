# 0007：Position、Ownership 与 Kafka Rebalance

状态：Accepted（总体契约与 Runtime 表示已定；M2 客户端适配仍待收敛）
最后更新：2026-08-27
适用阶段：M2
依赖：[核心执行模型](0001-core-execution-model.md) · [Source Reader](0003-source-reader-and-admission.md) · [Sink Handoff](0005-sink-handoff-and-completion.md) · [ADR-0002](../decisions/0002-use-kafka-consumer-group-for-external-coordination.md)

本文是 safe position、generation fence、一致性边界和 Kafka rebalance 收尾的权威契约。

## 1. Position 与第一版一致性保证

### 1.1 Split、position 与有序交接

Positioned split 是一条具有确定恢复顺序的逻辑输入流。`SplitID` 是 Source 定义、Runtime
只作相等比较和诊断的不透明标识；它只需在一个 Source 实例内区分 split，不承诺跨 Job、
进程或版本稳定。一个 split 的实际身份还受当前 Runtime execution、Source instance 和
ownership generation 约束，不能只凭裸 `SplitID` 跨 scope 关联。

`SourcePosition` 同样由 Connector 定义，Runtime 不解析、不比较，也不假定它是整数。
Connector 必须在同一个 split ownership 内按恢复顺序交接 Source element；即使 Connector
内部并行读取，也必须在 admission 前恢复该顺序。不同 split 可以任意交错，无法定义稳定
恢复顺序的 Source 只能作为 unpositioned Source，不能声明 position 恢复保证。

M2 第一版要求一个 positioned Source element 成功反序列化为恰好一个 `Record[T]`。业务过滤
与 fan-out 分别由 Operator 的正常零输出和多输出表达；Connector 不得静默跳过影响恢复位置的
Source element。未来若开放 Source 级零/多输出，必须增加 element-level completion，使零输出
也能形成明确进度、多输出全部完成后才允许推进 position。

### 1.2 连续 safe position

记录可以乱序完成，但 Source position 只能推进到 split 内连续完成位置：

```text
100 complete
101 pending
102 complete
103 complete

safe frontier: 100
resume offset: 101
```

101 完成后可以一次推进到 103。Runtime 按 admission 顺序追踪每个 Work 的 completion，并以
连续成功前缀最后一个 Work 的不透明 `SourcePosition` 形成 safe position。队列、环形缓冲或
内部递增序号只是私有数据结构选择，不构成公共 identity。Source 是否遗漏外部位置属于
Connector 契约，Runtime 不通过解析 position 二次验证。

Kafka Connector 将 Runtime safe position 转换为 Kafka offset commit：记录 offset 42 成功表示
safe position 是 42，而 Kafka committed offset 写入 43，表示下一条应读取的记录。该转换完全
属于 Kafka Connector。

### 1.3 Runtime Envelope 与 identity

Runtime 在 ready Source element 完成 admission 时统一创建 `Record[T]`、私有 Envelope 和
`WorkID`。Envelope 是一个 Source 输入的 Runtime-owned work scope，不是进入用户 API 或沿
Pipeline 复制的公开消息。它逻辑上保存：

- 当前 Source instance；
- unpositioned 状态，或作为整体存在的 `SplitID`、ownership reference 与
  `SourcePosition`；
- `WorkID`、当前 attempt 和 completion state；
- 原始输入 Record 及 permit 等私有执行状态。

Positioned/unpositioned 必须采用可区分的整体状态，不能用彼此独立的 nullable split、position
和 generation 组合出非法中间状态。Reader 提供业务值与 Connector position；ownership 由
Runtime 根据当前 assignment/generation 绑定，不能由 Reader 任意伪造。

`WorkID` 在一次 Runtime execution 内唯一，Retry 时保持不变，不由业务值、position、指针或
函数名推导，也不承诺跨重启稳定。每次完整 Operator Chain 执行使用新的 attempt identity；旧
attempt 的迟到 Emit、完成或错误不得影响新 attempt。Completion 是 Work 的状态，直接由
`WorkID` 定位，不增加一对一的 `CompletionID`。异步 Sink 的不同输出使用不透明 `SinkItemID`；
它逻辑绑定 Work、attempt 和该 attempt 内的输出 ordinal，重复或迟到 callback 不得重复终结
Work。

具体 Go 类型名、泛型签名和私有存储布局留到 Reader、control event 与 Connector 生命周期
接口联合审核时确定，但不得改变上述可见性、scope 和 identity 契约。

### 1.4 generation fence

每次 partition ownership 带 generation。Runtime 以 Source instance、`SplitID` 和 generation
形成当前 ownership scope，Work 保存对该 scope 的绑定。失效 ownership 的迟到 completion、
Sink callback 或 commit 请求不能推进新 owner 的 position；校验完整 scope，而不是只比较裸
generation 数值。

### 1.5 M2 恢复与未来 checkpoint

M2 的 safe position 表示 Sink effect 已明确成功的连续 Source 前缀，外部 committed position
是当前无 checkpoint 阶段的恢复依据。未来 checkpoint 会保存 Connector 定义并版本化序列化的
完整 split state，并与 Operator、in-flight/channel 和 Sink state 形成一致快照；届时 checkpoint
成为恢复权威，外部 offset commit 可以只承担进度暴露或兼容职责。

Source position、Runtime completion frontier 和 checkpoint cut 是不同概念，不能共用一个状态
或把 safe position 命名为 checkpoint position。M2 不提前冻结 split-state serializer、barrier、
in-flight snapshot 或 Sink transaction API。

### 1.6 at-least-once

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
