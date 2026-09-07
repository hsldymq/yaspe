# 0007：Position、Ownership 与 Kafka Rebalance

状态：Accepted（主要设计已收敛，固定 v1.21.6 与初始默认值；§4 保留未完成的适配验证）
最后更新：2026-09-07
适用阶段：M2
依赖：[核心执行模型](0001-core-execution-model.md) · [Source Reader](0003-source-reader-and-admission.md) · [Sink Handoff](0005-sink-handoff-and-completion.md) · [ADR-0005](../decisions/0005-connector-owned-revoke-budget.md)

本文是 safe position、generation fence、一致性边界、Kafka 客户端适配与 rebalance 收尾的权威契约。

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

Connector 提供总 context deadline 和提交预留时长，Runtime 推导更早的 drain 截止时间；
通用接口、预算计算与到期行为见 [Source Design §1.3.1](0003-source-reader-and-admission.md#131-split-control-边界)，
Kafka 默认配置见 §3.4。`BeginRevoke` 返回时冻结目标 split 的最终 safe positions，Connector
在自己的 rebalance callback 中提交这些 position，再通过 handle 的 `Complete` 报告结果并
使 Runtime fence generation。这样不要求 Runtime 在 Connector 阻塞于 revoke call 时从另一
goroutine 反向调用 Kafka client 完成最终提交。

drain 到期后，未完成 work 不得被算作 Success，handle 仍可用于提交冻结的安全前缀；总期限
到期则 fence 尚未完成的 handle，迟到 Complete 不更新 committed frontier。提交超时只表示
未确认成功，不证明 broker 没有持久化；恢复时以 broker 实际保存的位置为准。

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

## 3. Kafka 客户端适配

### 3.1 客户端候选与 poll 登记窗口

第一版固定纯 Go 的 franz-go（`kgo`）**v1.21.6** 为适配基线，源码核验、原型与后续实现
针对该版本。其模块最低 Go 版本为 1.25，符合 yaspe 的 Go 1.27 基线。版本选择不等于
完整兼容性或 Connector 实现验证完成；七项已运行的定向验证及限制见 §5。
旧业务使用 Sarama 不约束 yaspe 的客户端选择；客户端对象保持在 Connector 内，业务
Operator 不感知 kgo 类型。升级依赖必须复核适配契约，不随 master 变化自动更新基线。

第一版限定 classic Consumer Group 协议，支持 eager/cooperative 分配；新的 consumer group
协议不在第一版兼容声明内。协议范围与未来扩展条件见 §3.7。

职责分为客户端的网络/session 管理、Connector 的取数与有界缓存、串行 control callback，
以及 Runtime admission/completion。数据获取可以因容量不足等待，control 路径必须仍可
推进，不等待 Runtime 消费一整批记录或完成 Sink 写入。

使用 `BlockRebalanceOnPoll` 保护一次 poll 结果到 Connector 缓存登记完成的短窗口：

```text
预留 Connector 记录容量
    → PollRecords（正数上限，不超过预留容量）
    → 将记录登记到当前 ownership 的缓存
    → AllowRebalance
    → 后续反序列化、Runtime admission 与业务处理
```

只有一个取数循环负责该顺序；取数后登记不得再等待空位，不在受保护窗口内执行用户
反序列化、业务函数或外部 I/O。poll 后的所有退出路径，包括空结果、错误与取消，都必须
保证释放 rebalance 阻挡及未使用的预留容量，之后才可等待容量。缓存中的 raw/decoded
记录及正在转换的记录必须纳入同一 Connector 数量预算；只有转换完成且 ownership 仍
有效的值才能经非阻塞 Reader 交接，split 内顺序仍受 §1.1 约束。

选择短窗口是为了消除“旧 ownership poll 结果在 revoke/reassign 后才登记”的竞态。
不把阻挡延长到 Operator/Sink 完成，以免业务背压拖住 rebalance；完全不阻挡则需要另行
证明 poll 结果与 assignment epoch 的关联。短窗口仍有调度停顿风险，必须验证最坏交错。

### 3.2 分层缓存与背压

资源核算覆盖完整路径：

```text
franz-go 内部：fetch 数量 → Connector 缓存：记录数量 → Runtime：work 数量
```

Connector 所有 partitions 共享可配置的记录数预算，包含 poll reservation、已取出但尚未
交给 Runtime 的记录与转换中的记录，内部保留 partition 顺序。总预算避免 partition 数
增加时容量按每 partition 固定配额成倍增长；代价是热点 partition 可能占据较多容量，
第一版不承诺严格公平。初始默认值为：

| 配置 | 默认值 | 有效范围 |
|---|---|---|
| `MaxConcurrentFetches` | 2 | 可配置，必须为正整数 |
| Connector 缓冲容量 | 1,024 条记录 | 可配置，必须为正整数 |

两项独立配置，不要求相互匹配，也不要求缓存装得下完整一次 fetch。第一版不开放
MaxConcurrentFetches 的零值特殊模式或负值无独立上限模式。默认少量并发 fetch 与有限
记录缓冲用于吸收速度差异，尚未进行性能调优；后续按 workload 校准，不据此承诺吞吐或
固定内存占用。

容量耗尽时暂停当前 assignment 的 fetch 并停止新 poll；等待容量前已经 AllowRebalance。
恢复取数须同时满足容量、ownership、revoke 和关闭状态；不能仅因有空位就 resume。
背压期间新增 assignment 也受同一暂停状态约束，retained 缓存保留，恢复后优先交接已有
记录。数据暂停不得阻塞 session/control 处理，也不得以 session 维护为由持续扩大预取。

第一版对客户端内部按 fetch 数量约束，对 Connector 已取出的数据按记录数量约束，不承诺
整个 Source 统一的固定记录数或内存字节上限。客户端内部预取仍属 Connector 适配责任，
但不能将不同计数单位直接相加或把 fetch 个数解释为记录个数。

- 显式使用有限的 `MaxConcurrentFetches`，约束同时在途或在客户端缓冲、尚未被完全
  poll 取走的 fetch 数量；不采用随 broker 数量无独立配置上限的模式；
- `PollRecords(n)` 的正数上限不超过当前预留的 Connector 记录容量。一份 fetch 可包含
  多个 partition、多个批次与多条记录，未取出的部分继续留在客户端，不要求 Connector
  一次装下完整 fetch；
- 一份结果被完全取走后，客户端可以继续发起下一次 fetch；fetch 并发上限不是运行期间
  fetch 总次数上限。较大的 Connector 缓冲可以由多次 fetch 逐渐填充；
- 记录就绪后即可交给 Runtime，不等待缓存填满。缓冲大小影响背压时机、吞吐和内存
  占用，不改变 ownership、顺序或完成语义；较小缓冲也不能消除单次大批次的内存峰值；
- 缓冲满时停止新 poll 并暂停 fetch，已经在途或已缓冲的数据仍按客户端 fetch 数量
  限制核算，不要求 pause 瞬间撤销已有响应。稳定背压下不得持续新增 fetch 或私有队列
  来扩大积压，session/control 必须仍可推进。

第一版不新增 yaspe 自己的单条消息、fetch 响应或解压批次字节上限，不实现额外的受限
解压器，也不以新增 yaspe 大小阈值拒收数据。保留客户端原有 fetch 大小配置、响应读取
及解压保护机制；“不新增限制”不表示绕过这些保护或支持任意大的消息。客户端确实返回
读取/解压错误时，仍按已有 Source failure 契约处理，不静默丢弃或无限放宽限制。

`FetchMaxBytes` 等参数是客户端正常 fetch 大小目标，不能作为硬性的整体内存保证；
`BrokerMaxReadBytes` 等现有保护也不等于解压后的内存上限。压缩展开、记录对象、共享
底层缓冲及用户转换后的值都会影响驻留内存。内存不足时不能承诺正常工作，降低 fetch
并发也不能抵消 Connector 过大缓存的全部占用。

选择这一分层计数保证，是为了防止持续背压下无限积累，同时接受大小可变的输入。
较大的消息/批次可能带来较高内存峰值；对响应和解压字节再加硬上限能够提供更强的输入
约束，但会引入额外配置、解析路径和大批次拒收语义，第一版暂不采用。原“所有未交接
预取必须按记录数给出上限”的要求由此替代，理由与旧决定见
[ADR-0007](../decisions/0007-layered-source-prefetch-budgets.md)。

初始默认值已接受，实际 Connector 实现与其组合仍须通过 §4.2 验证；该验证检查分层计数
和背压，不再要求证明整个 Source 的固定记录数或字节上限。

### 3.3 Offset 提交与 revoke 交接

禁用 Kafka 自动提交，只提交 Runtime 确认的连续 safe position。正常运行由 Runtime 按
可配置周期合并 dirty frontier，没有进度变化不提交；不按每次完成积累提交任务。Kafka
路径每个 Source 同时最多一个提交请求，可包含多个 partitions；这是对 §1.7 通用的每
split 单 in-flight 约束的进一步限制。初始提交参数如下，名称作为设计表达，代码尚未实现：

| 参数 | 初始值 | 含义 |
|---|---|---|
| 普通提交周期 | 默认 3 秒，可配置 | 正常运行中周期检查、合并并提交有变化的 safe position |
| 单次逻辑提交总超时 | 5 秒 | 包含等待提交资格、请求和本次提交内的重试 |
| 重试退避 | 初始 100 毫秒，增长至最多 1 秒 | 只在本次剩余总预算内等待和重试 |

普通提交周期不控制读取或业务执行频率，也不保证位置在一个周期内持久化。上次提交
未结束时只合并最新 safe position，不叠加请求、不积累周期触发任务；一次请求可批量
提交多个 partitions。周期较短增加提交频率并通常减少故障后已完成但未提交记录的
重放，周期较长则相反。初始参数是设计选择，尚无 workload 验证。

一次逻辑提交的绝对期限从开始本次提交流程时计算，不在请求重试时重置；已有调用
context、revoke 或 shutdown 的更早期限优先。退避等待也必须可取消且包含在总预算内，
不能在 Connector 与客户端之间叠加不受总期限约束的重试。具体客户端 retry 配置映射
须在 §4.3 核验；超时与退避端点不表示已固定全部私有算法或客户端选项名。

Revoke 最终提交在 drain 后直接进入，不等待普通提交周期。它同时受单次逻辑提交超时
及 revoke 剩余时间限制，并给 Complete 与 callback 返回留下时间，不能在 drain 后重新
获得完整的一份独立预算。RevokeCommitReserve 是整个提交及收尾的预留，不保证最终
网络请求本身总有五秒可用。

Connector 以有期限的同步提交实现 `CommitPositions`；独立等待路径不能阻塞 Runtime
处理 completion/control。检查请求级与逐 partition 结果，全部确认成功才返回 nil；任何
partition 失败则整批返回 error，Runtime 按 §1.7 保守地不推进整批 committed frontier。
这不声称 broker 原子处理整批，也不撤销实际已经成功的部分。

可重试错误仅在本次有限预算内重试；最终失败或超时报告 Source failure 并 FailJob，不进入
Operator Retry。超时是未确认成功，不能描述成确定未生效。第一版不增加长期 commit 失败
但继续消费的降级状态；代价是持续提交故障会停止 Job。

非空 revoke 与普通提交的交接顺序为：

1. 先禁止目标 partition 交接，并暂停整个 Source 的新普通提交；未发出的进度只保持 dirty，
   不排队形成可在新 ownership 中发出的旧请求；
2. 已发出的普通提交有限收敛，`BeginRevoke` 同时推进 drain；两条等待路径不持有阻止
   completion/control 的锁，也不相互依赖才能完成；
3. 只有旧普通提交已确认成功结束且 handle 已返回，才在回调中提交 handle 内被 revoked partitions
   的最终位置，不能在最终提交之后再发出旧普通提交；
4. 用 `Complete` 报告结果；Job 正常且其他暂停原因消失后，retained partitions 恢复普通
   提交，其尚未提交的最新 safe position 继续保留。

所有等待受同一总 deadline 约束，已有普通提交不得在 revoke 到来后独占原先更长的预算。
旧提交超时、取消或最终失败按既有 FailJob/关闭因果规则处理，不再发起后续最终提交或
补救提交；不能先取消旧请求，再立即换连接提交较新的位置。预算耗尽则取消等待并执行
fence，不再发起已无时间完成的最终提交。具体取消/串行化实现必须通过 §4.3 的交错审核。
取消已发出的请求不能撤销 broker 可能已处理的请求，迟到结果只能按旧 scope 隔离。

Lost 取消并隔离旧提交；如果产生最终提交失败，仍按提交失败契约结束 Job，不能因会话
重入规则允许恢复而忽略这个失败。提交 callback 只记录结果，不同步等待 Runtime 关闭，
不在 callback 内递归发起下一次提交。下一次提交只能在前次流程结束后由协调路径发起。

选择 Source 级单请求简化普通/最终提交排序，代价是不同 partitions 的提交不能完全独立，
retained progress 在 revoke 期间可能暂缓。若测量证明提交吞吐成为瓶颈，再评估更细粒度
串行化，不能削弱旧请求隔离或有限收尾。

### 3.4 Kafka revoke 配置

Kafka Connector 提供以下配置；名称作为当前设计表达，代码尚未实现：

| 配置 | 初始默认值 | 含义 |
|---|---|---|
| `RevokeTimeout` | 30 秒 | 本地允许的整个 revoke 收尾时长 |
| `RevokeCommitReserve` | 5 秒 | 总预算内为最终提交、Complete 和回调返回预留的时长 |

静态校验要求 `RevokeTimeout > 0` 且 `0 < RevokeCommitReserve < RevokeTimeout`。运行时总
期限取本次 revoke 回调进入时刻对应的本地预算、已有 shutdown deadline 和能够可靠确定的
更早外部期限中的最早者；向
`BeginRevoke` 传入总 deadline context 及 `RevokeOptions{CommitReserve: ...}`。运行时预算
缩短到不足预留时长是正常收尾情形，不作为静态配置错误。

在没有更早期限时，默认最多 drain 25 秒；提前完成就提前提交。30/5 秒是初始设计默认，
未经 workload 验证，不是协议常量。它替代旧 Runtime `RevokeDrainTimeout` 的含义，不能
描述为仍有完整 30 秒 drain。理由与被替代决定见 [ADR-0005](../decisions/0005-connector-owned-revoke-budget.md)。

#### 3.4.1 本地期限与外部期限的不确定性

franz-go 的 revoke callback context 是客户端 context，不直接给出 broker 实际截止时间。
本地受控等待按有限预算结束或放弃收尾；在 Kafka 外部期限内成功交接是需要争取并验证
的目标，不承诺网络异常或进程长时间停顿后 ownership 仍然有效。

```text
总 deadline = min(
    本次 revoke 回调进入时刻 + RevokeTimeout,
    已有 shutdown deadline（若有）,
    能够可靠确定的更早外部 deadline（若有）,
)
```

没有可靠的外部 deadline 时不补造一个。Kafka RebalanceTimeout 用于协调配置和验证，
不能在 callback 开始时重新完整计时并声称剩余时间由 broker 保证。发现 rebalance、
短窗口阻挡、调度和协议推进都可能已经消耗时间。

v1.21.6 的 OnPartitionsCallbackBlocked 由独立 goroutine 异步调用，覆盖多类 control
callback，其实际执行时刻不能可靠代表本次 revoke 开始。因此以 revoke 回调进入时刻
作为本地计时起点；blocked 通知仅作诊断和尽快释放 poll 短窗口的提示，不重置或决定
revoke deadline。

本地预算不声称覆盖回调进入之前已经发生的短窗口等待、调度停顿或协议耗时，也不能
声称已准确扣除这些不可可靠测量的消耗。先前希望从 blocked 通知确定更早起点的方向
由此具体化为回调入口计时，避免用可能迟到的异步通知构造错误时间关系。

这一选择将本地有限收尾与不可精确观察的外部时限分开，避免用配置时长制造外部期限
保证；代价是在异常情况下可能失去最终提交机会并增加重放。Lost、提交错误和 generation
fence 仍必须执行，本地 deadline 不授予或延长 Kafka ownership。

### 3.5 控制回调与 Source failure

Connector 把客户端通知转换成具体、合法的 split ownership 变化：

| 通知 | Runtime 边界 |
|---|---|
| 实际新增 assignment | Assign 成功后才允许新 split 交接 |
| 非空 revoke | 先本地禁止目标 split 交接，再执行 §3.3 的两阶段收尾 |
| 非空 lost | 先禁止交接、清理未交接缓存，再调用 Lost；不 drain、不最终提交 |
| 空 assignment/revoke | 不调用对应的空集合 control 方法，不因此启动 drain |
| 空 lost | 不调用 Lost([])，仍处理导致 lost 的客户端错误 |

franz-go 的 revoke 回调也可能表示 group session 结束，即使当次没有 partition 要交出。
空 revoke 不表示整个 group 没有 rebalance，也不保证后面不会收到非空 revoke。

Lost 表示旧 ownership 已失效，不是消息丢失。Runtime 立即 fence 旧 scope；尚未开始的工作
不启动，未交给 Sink 的迟到输出不再接管，Sink-owned 操作即使迟到成功也不推进旧或新
ownership 的位置。同一实例再次获得同一 partition 时创建新 generation，从外部 committed
position 重新开始，不能复活旧工作或复用旧缓存。

Lost 本身不直接决定 FailJob；会话恢复、错误分类及预算按 §3.6 执行。客户端会话维护不使用
Operator Work Retry，也不改变 Source 已报告最终 error 后当前 Run 进入 FailJob 的规则。

控制回调处理失败时报告失败并结束回调，由独立关闭路径清理客户端；不得在 callback 内
同步 Close 或等待 LeaveGroup，以免关闭等待 group loop、group loop 又等待 callback 返回。
最终失败使用 [SourceContext.ReportFailure](0003-source-reader-and-admission.md#112-独立的最终失败报告)
独立通知 Runtime，不能仅把错误留在暂停后不再读取的数据通道中。

### 3.6 会话建立与恢复

Kafka Connector 在未报告最终失败之前，可以让客户端恢复会话；它不恢复一个已 FailJob
的 Source 或 Run。恢复期间立即按 Lost 契约隔离旧 ownership，重入后的 assignment 建立
新 generation，从 Kafka 已提交位置重新开始。恢复成功不复活旧 buffers、work 或 completion。

#### 3.6.1 恢复预算与成功边界

`SessionRecoveryTimeout` 默认 **1 分钟**，必须大于零，第一版不提供无限等待模式。
该配置归 Kafka Connector，初次建立会话与运行中的会话恢复共用这一时长配置；它与
`RevokeTimeout` 的交还收尾预算相互独立，不为已经失败的 offset 提交延长重试预算。

```text
初次开始建立会话 / 观察到有效会话失效
    → 启动一次绝对期限
    → 客户端重试、backoff、重新加入
        ├─ 成功处理对应 assignment 回调：结束计时
        ├─ 不可恢复错误 / 到期：锁存最终失败并 ReportFailure
        └─ Job 取消或关闭：终止等待，按关闭流程处理
```

- 初次计时从 Connector 开始建立会话时开始，运行中从观察到会话失效时开始；不是从
  第一次读取记录失败、下一次 poll 或下一次 retry 才开始；
- 同次恢复中的重复失效通知、错误、retry 和 backoff 都消耗原预算，不重置 deadline；
- 成功边界是客户端确认已加入 group，且 Connector 成功处理对应的 assignment 回调。
  非空 assignment 必须完成 Runtime Assign；空 assignment 不调用 Assign([])，但同样
  可以确认会话恢复，因为有效 group member 可能没有分配到 partition；
- 成功不要求收到数据。正常无数据、没有 partition、下游背压或正常 rebalance 本身，
  不能单独被当作一次异常会话恢复。恢复中即使发生正常协议重试，也不刷新原预算；
- assignment 成功只说明会话建立/恢复，不证明后续 offset 获取或数据读取必定成功。
  这些阶段的错误按自身职责处理，不以收到第一条记录作为统一健康条件；
- 超时或不可恢复错误一旦锁存最终失败，该 Source 不再恢复。与超时竞争的迟到 assignment
  成功不能清除失败或恢复交接；成功与到期必须由同一协调状态确定唯一结果；
- Job 取消、Source 关闭立即结束恢复等待，不能把本地关闭造成的取消另包装成新的会话
  恢复故障。初次建立失败仍遵循 Source Open 的资源清理契约。

选择有限时间预算而非仅计重试次数，是因为每次请求、退避及重新加入耗时不同。默认值
是初始设计选择，不是已测得的最佳恢复时间；恢复窗口内允许暂时故障，超过窗口让宿主
明确观察失败。代价是持续但可能最终恢复的故障仍会结束 Job；真实故障数据可促使调整
默认值，但不能自动改为无限等待。

#### 3.6.2 错误分类与适用阶段

第一版采用明确的错误类型及阶段规则，保留原始原因，不仅依据 Kafka error 的 `Retriable`
字段。重新建立会话与在原会话中原样重试请求不是同一种恢复操作。

| 会话维护中的错误或事件 | 行为 |
|---|---|
| 正常 `RebalanceInProgress` | 客户端推进正常 rebalance，不作为不可恢复错误 |
| `IllegalGeneration`、`UnknownMemberID` 导致会话失效 | 隔离旧 ownership，允许有限恢复 |
| 暂时网络错误、coordinator 不可用 | 允许客户端处理；进入建立/恢复阶段后受同次总预算约束 |
| `GroupAuthorizationFailed`、`TopicAuthorizationFailed`、`SaslAuthenticationFailed` | 立即报告最终 Source failure |
| `InvalidGroupID`、`InvalidSessionTimeout`、`InconsistentGroupProtocol` | 立即报告最终 Source failure |
| 因 Job 关闭而产生的取消 | 按关闭流程处理，不另外产生会话恢复失败 |
| 未识别或未纳入恢复规则的错误 | 保留具体原因，保守报告最终 Source failure |

分类由 Connector 承担，Runtime 不解析 Kafka 错误码；包装错误应保留底层原因供识别与
诊断。暂时请求错误若没有导致会话失效，不据此启动会话恢复计时或直接结束 Job。
同一错误若来自最终 offset 提交，则仍执行 §3.3 的有限提交重试/FailJob 契约，不自动
转换成会话恢复或获得新预算。该表不是所有 Kafka 请求的通用重试策略。

#### 3.6.3 客户端信号与最终失败报告

franz-go 的 `OnPartitionsAssigned` 是会话成功信号的候选适配点，`OnPartitionsLost` 负责
ownership 失效，`HookGroupManageError` 提供导致会话管理失败的原因。`OnPartitionsLost`
自身不携带完整错误原因，不能仅凭其 partition 集合判断错误可恢复性。

Connector 必须把客户端回调、hook 和读取路径观察到的同一故障归入同一恢复/最终失败
状态。最终错误通过 Runtime 提供的 `SourceContext.ReportFailure(error) error` 报告，
参数、返回值、重复及关闭行为只在
[Source Design §1.1.2](0003-source-reader-and-admission.md#112-独立的最终失败报告) 维护。
暂时错误在 Connector 内处理；一旦报告最终失败，就按现有 FailJob 进行有限关闭。

这种分工让数据背压不遮蔽最终故障，同时保持 Operator Retry、Kafka 会话维护与 Source
最终失败三者的边界。代价是新增独立报告入口及多错误来源的去重协调；只靠下一次
TryRead、直接在 callback 中同步关闭、把所有 lost 立即 FailJob 或无限跟随客户端重试，
均不能满足这些要求。跨组件理由见
[ADR-0006](../decisions/0006-source-failure-reporting-and-session-recovery.md)。

v1.21.6 的模拟验证已观察到 Lost 返回后调用 GroupManageError，再重新 Assigned；初次
group 权限失败在完全不 poll 时也会先给出空 Lost，再通过 hook 提供原因。因此 Lost
callback 先完成旧 ownership 隔离并返回，不能等待后续错误 hook 来决定是否隔离，否则
会阻塞该 hook 的调用。具体证据见 §5，完整恢复 timer、错误覆盖及关闭竞争仍见 §4.1。

### 3.7 第一版 group 协议范围

第一版使用 classic Consumer Group 协议，分别验证 eager 和 cooperative 分配。分配策略
与 group 协议是不同维度：支持 cooperative 不表示支持新 consumer group 协议。

新的 group 协议涉及不同的成员 epoch、回调和提交重试行为，当前不把它并入已有正确性
证明。v1.21.6 默认使用 classic；源码中的新协议需要客户端 context 携带隐藏的显式启用
键。Connector 须确保客户端不携带该启用条件，不能仅依赖分配策略名称，也不能把源码
注释中的 DisableNextGenRebalancer 当作实际可调用配置。模拟验证已观察到 JoinGroup，
未观察到 next-gen heartbeat；仍需真实 broker 和多实例测试。存在实际需求时再独立设计、
验证后扩展协议范围。

选择有限协议范围，是为了让普通提交、revoke 最终提交、lost 和重入的请求身份与顺序
能在一套明确模型下接受检验。版本虽已固定，完整客户端兼容性仍未验证；升级时须重新
检查新协议启用条件和所依赖的回调、请求语义。

## 4. 当前开放问题

Kafka 主要设计、版本和初始默认值已接受；以下保留尚未完成的 M2 适配验证，不把局部
模拟测试扩展成完整实现保证。测试发现影响既定契约的事实时再显式重新评估设计。

### 4.1 会话恢复信号与错误类型的版本适配核验

- 已接受前提：§3.6 定义一分钟预算、恢复状态机、错误分类和独立最终失败报告，版本
  适配以这些契约为验证依据；
- 问题：v1.21.6/classic 下，Lost、GroupManageError、assignment 与 poll 错误
  的时序和覆盖是否足以落实契约；暂时网络/coordinator 错误的具体包装与类型如何映射；
- 完成条件：给出版本对应的适配表，核验初次建立、空 assignment、暂停 poll、超时与
  assignment 竞争、关闭与迟到 hook，证明不漏报、不误判成功、不重置同次恢复预算。
  若发现 API 不能支持既定行为，明确报告并重新评估，不静默扩大可恢复错误范围。

### 4.2 分层预取预算的客户端适配核验

- 已接受前提：§3.2 的 fetch/记录/work 分层计数及字节非保证；不新增 yaspe 响应或解压
  字节限制，保留客户端原有保护；
- 问题：所选客户端版本中，MaxConcurrentFetches 是否覆盖在途、正在解析及剩余缓冲
  的结果；部分 poll、pause/resume 与 assignment 变化是否产生未受控的额外预取；
- 完成条件：验证已接受的分层容量及默认值，用小/大 Connector 缓冲、跨多次 fetch 填充、
  单 fetch 大于剩余容量、大消息、多记录压缩批次及慢 Sink 验证不溢出 Connector 记录
  预算、不无限累计 fetch，并记录实际内存表现及客户端保护触发后的错误行为。
  不将有限 workload 的峰值报告为硬内存上限；若客户端不能满足分层计数约束，须显式
  重新评估适配或候选客户端。

### 4.3 本地期限与提交的客户端适配核验

- 已接受前提：§3.3 的提交参数与失败后不补交、§3.4 的本地期限及外部不确定性、§3.7
  的 classic group 范围；不要求客户端制造不存在的精确 broker deadline；
- 已接受起点：revoke callback 入口；blocked 异步通知不作为计时依据；
- 问题：v1.21.6 客户端提交的锁等待、内部重试及请求取消能否落实完整逻辑提交总预算，
  多实例 classic 协议下旧请求怎样隔离；
- 完成条件：基于固定版本及协议配置，给出参数映射和 callback/请求交错证据，覆盖
  部分成功、响应丢失、旧请求超时后无补交、迟到请求不得绑定新 ownership、下一次请求
  仅在旧提交成功结束后发出。局部锁、提交等待与 BeginRevoke 不得形成死锁；
- 本地取消不撤销外部 effect，Runtime generation fence 不等于 broker 必然拒绝旧请求。
  若现有 API 无法实现契约，明确报告并重新评估适配，不能以仅给 context 设置 timeout
  作为底层已按时退出或没有迟到请求的证明。

## 5. 验证与实现边界

实现与验证状态由 [Current Status](../status.md) 维护。v1.21.6 的七项模拟 Kafka 定向测试
已启用 race detector 并通过，程序、固定依赖、结果及复现方式保存在
[版本验证附件](../verification/franz-go-v1.21.6/README.md)。它验证客户端部分行为，不是
yaspe Runtime/Kafka Connector 实现，也不是完整故障或 workload 验证。测试矩阵见
[Verification Design §1.4](0008-runtime-verification-and-observability.md#14-position-与-ownership)，
包括 poll 登记窗口、背压时控制推进、提交排序、两种期限、empty callbacks、lost/reassign
和外部不确定结果。已通过项与剩余真实 broker、多实例、完整超时/重试、资源验证在附件
中分开记录，不将七项测试通过描述成所有 M2 验证已完成。

## 6. 客户端事实参考

以下源码固定为 v1.21.6，用于解释版本选择和适配约束；运行证据单独见 §5。升级必须
复核这些依赖，不能用移动的 master 页面代替版本事实：

- [franz-go 项目说明](https://github.com/twmb/franz-go/tree/v1.21.6)：纯 Go 客户端及 group 支持；
- [版本依赖](https://github.com/twmb/franz-go/blob/v1.21.6/go.mod)：Go 1.25 基线与 kmsg 依赖；
- [kgo API](https://pkg.go.dev/github.com/twmb/franz-go@v1.21.6/pkg/kgo)：PollRecords、BlockRebalanceOnPoll、
  AllowRebalance、fetch 限制、CommitOffsetsSync 与回调 context/关闭约束；
- [group 实现](https://github.com/twmb/franz-go/blob/v1.21.6/pkg/kgo/consumer_group.go)：空 revoke、
  lost 后的客户端重入及 commit 串行化路径；
- [配置与回调注释](https://github.com/twmb/franz-go/blob/v1.21.6/pkg/kgo/config.go)：RebalanceTimeout
  包含检测消耗，回调 context 不能直接作为实际 rebalance deadline。
- [poll 与 blocked 通知](https://github.com/twmb/franz-go/blob/v1.21.6/pkg/kgo/consumer.go)：窗口与异步通知；
- [新协议启用条件](https://github.com/twmb/franz-go/blob/v1.21.6/pkg/kgo/consumer_group_848.go)：classic 默认路径；
- [group 错误 hook](https://github.com/twmb/franz-go/blob/v1.21.6/pkg/kgo/hooks.go)：独立观察会话
  管理错误的候选入口；
- [Kafka 错误类型](https://github.com/twmb/franz-go/blob/v1.21.6/pkg/kerr/kerr.go)：错误码与
  Retriable 标记，用于阶段相关的 Connector 分类。
