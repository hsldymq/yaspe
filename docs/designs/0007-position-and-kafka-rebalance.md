# 0007：Position、Ownership 与 Kafka Rebalance

状态：Accepted（Position、Completion Tracker 与 ownership 契约已定；M2 客户端适配仍待收敛）
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

Reader、position capability 与 control event 的公开边界已经在
[Source Design](0003-source-reader-and-admission.md) 接受；Envelope、WorkID、generation 引用和
私有存储布局仍由受约束实现原型细化，不得改变上述可见性、scope 和 identity 契约。

### 1.4 Work 终态、Success 与 permit

Completion Tracker 是 Runtime 私有的正确性组件。它不向用户或 Connector 暴露 `Ack`、
`Complete`、`CompletionID` 或 `WorkHandle.Done()`；Source 只提供 split、position、交接顺序和
安全位置提交能力，Sink reporter 只报告 item 的最终事实。Runtime 自行持有 `WorkID`、attempt、
终态、gap 和 generation 关联。实现可以使用单协调器或按 split 分片，但对外只表现为同一套
逻辑串行、幂等的状态机。

Work 只有三种终态：`Success`、`Failed` 和 `Cancelled`。Operator Retry 是保留同一 work 的
非终态，Retry 期间继续持有原 permit；`SinkUnknown` 是导致 `Failed` 的原因，不是第四种 work
终态。分类规则为：

- Operator Chain 成功且产生零输出时，不调用 Sink，attempt seal 后直接 Success；
- 有输出时，只有 sealed 的完整 group 已被 Sink 接管且每个 item 最终都是 `SinkSucceeded`，
  work 才 Success；
- Operator Retry 预算耗尽、Sink `NotApplied`/`Unknown` 或关闭时缺失 item 结果均为 Failed；
- FailJob、正常停止或 ownership fence 前尚未交给 Sink 的 queued work 为 Cancelled；已经
  started 的 work 只有在 `Process` 返回并 seal 前或尚未转移 terminal outputs 时才可取消；
- 已由 Sink 接管的 effect 不能假定可撤销，必须等待统一 deadline；fence 时仍未明确的结果按
  uncertain failure 终结为 Failed，而不是 Cancelled。

Success 的线性化顺序是：校验当前 Runtime execution、`WorkID`、attempt、reporter 与 ownership
generation 仍有资格；原子写入 Success；更新 positioned tracker 并推进可能形成的连续 safe
frontier；记录 work-completed metric；最后确保 permit 恰好释放一次。具体指令顺序可以由私有
实现优化，但外部可观察结果必须等价，任何迟到或重复事件都不能再次终结 work、记录成功或
释放 permit。

Attempt seal 是 Success 的必要前提。Worker 空闲、进入 terminal queue、Collector 接受输出或
Sink `Accept` 返回 Accepted 都不释放 work permit。Failed/Cancelled 在终态线性化时同样只释放
一次；position commit 在 permit 释放后由独立有界状态继续，不占用 work permit。

### 1.5 多输出聚合与失败收敛

一个有输出 work 在 Sink `Accept` 时建立固定、不可扩张的 item table；pending-accept 期间的同步
callback 先暂存，只有接管成立后才应用。全部 item Success 才形成 work Success。

首个 `SinkNotApplied`、`SinkUnknown` 或缺失结果使 work 立即 Failed、触发 FailJob 并释放 permit，
不等待同组其余 item 才停止 admission。Reporter 随后只保留有界的轻量 drain/fence 状态：后来
的失败进入 secondary diagnostics，后来成功只完成 drain，均不能改变 work 终态或 safe
position。Reporter 只有在 admission 已停止的失败收尾阶段才可短暂晚于 work permit 存活，且其
item table、pending results 和 wakeup 仍受 Sink item/global in-flight 容量约束。零输出 work 不
创建 reporter。

### 1.6 Position gap tracker

Position gap tracker 属于 Runtime，而不是 Source。它按完整 ownership scope
`(Source instance, SplitID, generation)` 管理，并为每个 split 按 Reader admission 顺序保存
positioned work entry。Runtime 不比较不透明 `SourcePosition`；顺序完全来自 Connector 在
admission 前保证的 split-local recovery order。

只有连续的 Success 前缀能够推进 safe frontier。Failed、Cancelled 或非终态 work 都形成 gap，
后面的 Success 不得越过它；零输出 Success 与普通 Success 一样填补 entry。已经弹出的成功前缀
压缩为最新 safe position，不保留逐 work 历史。每个 admitted work 最多一个 entry，Retry 不创建
新 entry，未压缩 entry 总数由全局 `MaxInFlightWorks` 等 admission 容量约束；新 generation 不
继承旧 generation 的 gap。

这一模型适用于 Kafka offset、文件位置、CDC LSN、队列 sequence 等具有 split-local 全恢复顺序
的 Source。没有稳定全序、多维 cursor，或在 Source 层把一个 element fan-out 为多个独立完成
单元的 Source，不能直接声明该 position 能力；后者需要未来的 element completion/checkpoint
协议。

### 1.7 Safe、in-flight 与 committed position

Work Success 只推进 Runtime 的 safe position，不等于外部提交成功。正常运行中 Runtime 可以
合并同一 split 的多个 safe advance；每个 split 至多有一个普通 commit in flight，新 safe 在
提交期间只把状态标记为 dirty 并保留最新值，当前提交成功后再发起必要的下一次提交。
`PositionCommitter` 可以批量提交多个 split，但批次返回非 nil error 时 Runtime 保守地认为该批
没有任何 split 成为 committed，除非未来接口明确提供逐 split 结果。

普通 commit 返回 nil 才推进 committed frontier。Revoke 使用 `BeginRevoke` 冻结的最新 safe
position，由 Connector 在 callback goroutine 提交；`RevokeHandle.Complete(nil)` 才更新
committed 并 fence generation，error 则不更新而直接 fence。Lost 不提交旧 generation。任何旧
commit result 在 fence 后都不能更新当前 generation；同一 split 后续被重新 Assign 时从外部
committed position 恢复，而不是从旧 Runtime safe state 继承。

### 1.8 generation fence

每次 partition ownership 带 generation。Runtime 以 Source instance、`SplitID` 和 generation
形成当前 ownership scope，Work 及所有 completion/reporter/commit token 都绑定完整 scope，
不能只比较裸 generation 数值。

Fence 必须原子地停止接收该 scope 的新 completion progress，按 §1.4 终结未完成 work 并恰好
释放一次 permit，使旧 attempt、reporter 和 commit token 失效，清除 gap、dirty 与 in-flight
commit 状态，只保留有界的迟到诊断。Fence 前已经成立的终态不变；fence 后任何事件不得反转
终态、重复释放 permit、推进 safe/committed position 或污染新 generation。

Cooperative rebalance 中 retained split 不 fence。Revoked split 在 handle Complete 或 deadline
到期时 fence；Lost 立即 fence；同一 split 再次 Assign 必须创建新 generation。新的 Runtime Run
创建新的 Source instance scope，因此上一次 execution 的迟到 token 同样无资格。

### 1.9 M2 恢复与未来 checkpoint

M2 的 safe position 表示 Sink effect 已明确成功的连续 Source 前缀，外部 committed position
是当前无 checkpoint 阶段的恢复依据。未来 checkpoint 会保存 Connector 定义并版本化序列化的
完整 split state，并与 Operator、in-flight/channel 和 Sink state 形成一致快照；届时 checkpoint
成为恢复权威，外部 offset commit 可以只承担进度暴露或兼容职责。

Source position、Runtime completion frontier 和 checkpoint cut 是不同概念，不能共用一个状态
或把 safe position 命名为 checkpoint position。M2 不提前冻结 split-state serializer、barrier、
in-flight snapshot 或 Sink transaction API。

### 1.10 at-least-once

Sink 未明确成功时不能推进 position。结果未知时，为避免丢失，第一版选择重试或失败恢复，而不是提前确认。这可能产生重复。

第一版目标是边界明确的 at-least-once：

- 不因 Source 已读取、Collector 已接受或 Sink 已入队而提前确认；
- 崩溃后从未安全提交位置重放；
- 外部结果未知时优先避免丢失；
- 没有事务或幂等 Sink 时不承诺 exactly-once；
- 不可重放 Source 不保证无丢失恢复。


## 2. Kafka Rebalance

Kafka Connector 通过 Source Design 接受的 `Assign`、`BeginRevoke`、`RevokeHandle` 和 `Lost`
把 Consumer Group ownership 变化同步交给 Runtime。它必须先使目标 split 在 Connector 本地
不可读，再开始 revoke/lost；Assign 返回后才可使新 split 数据 ready。控制调用不与业务
Record 或 availability notification 混用。

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
预留安全时间；收尾不得无限阻塞 rebalance。`BeginRevoke` 返回时冻结目标 split 的最终 safe
positions，Connector 在自己的 rebalance callback context 中提交这些 position，再通过 handle
的 `Complete` 报告 commit 结果并使 Runtime fence generation。这样不要求 Runtime 在 Connector
正阻塞于 revoke call 时从另一 goroutine 反向调用 Kafka client。

期限到期时取消仍未交给 Sink 的 work，Sink-owned unknown 不得标记成功；尚未完成 handle 的
generation 自动 fence，迟到 Complete 不得更新 committed frontier。只有 deadline 前确认成功的
commit 才成为外部恢复位置。

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
