# 0001：核心执行模型

状态：Draft（文中“已接受”条款已经讨论确认，开放问题仍待收敛）  
最后更新：2026-08-21  
适用阶段：M0–M2

## 1. 目的与范围

本文是 yaspe 近期执行语义的阶段设计，用于让后续会话和实现者在不依赖
原始讨论记录的情况下，准确理解一条输入如何进入 Runtime、执行 Operator Chain、
交给 Sink、进入终态以及在失败、暂停、终止和 Kafka rebalance 中如何处置。

本文固定语义和责任边界，不固定 Go 方法名、channel 布局、状态枚举或具体类型。
当前只适用于单进程、线性 Pipeline、record 级并行和单条输入内同步 Operator Chain。
完整 DAG、shuffle、有状态 Operator 和 checkpoint 恢复会在后续阶段重新评估这些边界。

### 1.1 阶段落地边界

本文同时描述 M1 执行基础和 M2 生产恢复语义，不表示所有内容都应在 M1 实现。

- M1 建立非阻塞 Memory Reader、有界 permit 和队列、完整 work-attempt 边界、末端暂存、同步 Memory Sink、FailJob/Skip 以及取消回收；
- M2 引入 Kafka split/position/ownership、异步批量 Sink、completion tracker、生产级重试恢复和 rebalance 收尾；
- M1 的边界不得阻断 M2 已接受的暂停和恢复语义，但不应提前实现 Kafka、生产 Sink 或 checkpoint。

## 2. 关键术语

- **split**：可独立读取和推进 position 的 Source 分片；Kafka 中对应 topic-partition。
- **ownership**：当前 Runtime 对一个 split 的处理和提交权责。
- **generation**：同一 split 的一次 ownership 任期。同一实例重新获得同一 split 也是新任期。
- **generation fence**：拒绝旧任期的迟到完成或提交影响当前 ownership。
- **in-flight permit**：Runtime 接受一条尚未终结输入所占用的端到端容量名额。
- **work attempt**：以一条 Runtime 已接受的原始输入执行完整同步 Operator Chain 的一次尝试。
- **completion**：一条输入的所有必要下游效果是否进入明确结果。
- **safe position**：一个 split 中连续完成、恢复时可从其后继续的位置。
- **committed position**：外部 Source 已确认持久化的 safe position。

## 3. 责任交接与整体流程

```text
External Source
    ↓ Connector 读取，可能批量预取
Connector-owned bounded data
    ↓ Runtime 先取得 in-flight permit，再成功取走一条
Runtime-owned input
    ↓ 有界等待或 Worker 执行
Synchronous Operator Chain / work attempt
    ↓ attempt 成功
Bounded terminal output
    ↓ 输出所有权转移
Sink accepted
    ↓ 必要外部效果明确完成
Input terminal
    ↓ 按 split 填补连续完成空洞
Safe position advances and is persisted
```

Source 已读取不等于 Runtime 已接受。只有 Runtime 取得 permit 并完成受控交接后，
才对该记录承担必须跟踪到明确终态的责任。permit 持续到输入终结，不在 Worker
完成计算或 Sink 仅入队时提前释放。

## 4. Source Reader

面向 Runtime 的 Reader 是非阻塞的。它只返回已可用记录、暂时无数据或读取结束等结果，
不在 Runtime 调用中等待外部 I/O。阻塞 I/O、批量 poll 和 session 维护由 Connector 内部
适配，必要时使用受 Runtime 管理的专用 I/O goroutine。

Reader 使用可等待的可用性通知避免忙轮询。该通知只是可用性提示；Runtime 取得 permit
后若未取到记录，必须归还名额。Connector 内部预取在数量和字节上有界，暂停时
不得继续扩大。

业务数据与可用性、结束、读取失败、assignment/revoke、ownership 变化和宿主取消等
控制事件分离。会使读取资格失效的控制事件优先于新数据交接。Runtime 一旦已知
ownership 失效，就不得再接受该任期的记录。

## 5. Operator Chain 与 Collector 的两个观察层级

Operator 调用 `Emit` 成功，表示当前 Process 的 Collector 已接受该输出。FlatMap 中首次
Emit 失败后停止剩余输出，且在单个 Operator 的调用和测试观察层级，此前成功 Emit
的输出仍对该 Collector 可见。

M1 Runtime 的线性同步 Pipeline 还有更外层的 work-attempt 边界。中间 Collector 同步驱动
下一个 Operator，不保留逐级持久恢复缓存。Chain 的最终输出在 attempt 成功前保留在
Runtime 可撤销的有界末端边界，尚未转移给 Sink。

因此下列两句话同时成立：

- 单个 FlatMap 调用中，第三次 Emit 失败不会从该 Collector 中撤销前两次 Emit；
- 整个 work attempt 因任一 Operator 失败时，Runtime 会丢弃该 attempt 所有尚未转移给 Sink 的末端输出。

在策略允许重试时，Runtime 使用保留的原始输入重新执行整条 Chain。这会重复用户在
Operator 内自行产生的副作用，该风险由 Operator 作者承担。终端暂存的数量和字节必须纳入
端到端容量预算。

FlatMap 不是事务边界，Sink 也不必保留某次 FlatMap 或 work attempt 的物理 batch 分组。
末端暂存只用于减少 Runtime 可明确避免的重复，不构成外部事务原子性。

## 6. Sink 边界与 Completion

Chain 成功后，Runtime 才允许其终端输出进入 Sink 交接边界。一个 work 的最终输出
必须整组交接：Sink 要么接受该 work 的全部输出，要么一个也不接受。这里保证的是
责任转移的原子性，不是外部写入的事务原子性，也不要求同一 work 的输出独占一个
物理 batch。Sink 接管后仍可把多个 work 的输出自由组批。

成功交接前，输出仍由 Runtime 持有，可以因暂停、position gap 或 generation 失效而
暂缓或撤销；成功交接后，输出责任转移给 Sink，Runtime 不能再假定操作可撤回。
Sink 之后失败时，优先保留已形成的最终输出并在 Sink 边界恢复，不重新执行
Operator Chain。

Runtime 与 Sink 之间使用有界、通知驱动的交接边界。Runtime 决定哪些 work 当前有资格
产生 Sink effect，Sink 决定自身 buffer 和异步请求是否有接收容量。M2 首版调度由 Sink
在有容量时非阻塞取得一个完整的 eligible work；暂时没有可交接 work 时等待状态变化通知，
不得忙轮询。pull 只是当前调度策略，不是稳定的公开语义。未来引入 DAG、shuffle、
checkpoint 或跨进程执行时，可以改用 push 或其他调度方式，只要保持整组责任转移、
有界资源、交付资格和背压语义。

Sink 的等待 buffer、并发外部请求、待重试项和 timer 都必须有上限。Sink 停止接管后，
终端边界和端到端 permit 会逐渐耗尽，从而把背压传回 Source。异步 I/O 可以继续并行，
但外部回调不直接并发修改 work、completion、safe position 或 generation 状态；回调只报告
事件，由 Runtime 协调路径串行且幂等地应用。迟到、乱序或重复通知不能使 work 重复终结、
重复释放 permit 或跨 generation 推进位置，相互矛盾的结果应作为诊断异常处理。

同一输入的所有必要 Sink effect 完成后，输入才能成功终结。该 completion 关联不是事务；
部分外部效果成功、请求结果未知或进程在 position 提交前终止仍可能导致重复。
第一版目标是 at-least-once 且不静默丢失，不宣称 exactly-once。

Sink completion 至少区分三种事实：确认成功、协议能够证明未生效，以及结果未知但可能
已经生效。只有外部协议能够证明时才能报告“未生效”；超时、断连等无法确认的结果必须
保留为未知。错误是临时还是永久、是否继续重试属于用户失败策略的另一判断维度，不能由
这三种结果直接推导。

若 Sink 协议能可靠报告每个输出的结果，已经确认成功的部分保持成功，只重试确认未生效
或仍需处理的部分；整批重试是无法取得更细粒度可靠结果时的特殊情况。原始 work 仍要等
所有必要输出都明确完成后才成功终结。零输出的成功 work 不经过 Sink，直接完成。

以上固定的是行为和责任边界，不固定 Go 方法、回调类型、队列布局或 pull 接口形态。

## 7. 失败、暂停与恢复

发生错误不直接等于进程退出。用户失败策略可以根据错误、阶段、历史尝试、
当前时段和业务信息决定等待、重试或最终 FailJob。重试可以在时间上无限，
但数据、goroutine、队列、timer 和其他资源始终有界，且 Runtime 始终响应宿主取消。

一条记录引起暂停后：

- Source 不再向 Runtime 交接新记录，Connector 不扩大预取；
- 已被 Runtime 接受但尚未开始的 work 保留原记录和 permit，不分配给 Worker；
- 已开始的 Chain 可继续收敛到末端边界；
- 同一 split 中位于未解决失败之后的新 Sink effect 暂缓；
- 位于失败之前并能填补连续完成缺口的 work 可继续进入 Sink；
- 不同 split 按各自连续完成进度判断；
- 已被 Sink 接受的操作不撤回，继续等待明确结果；
- 暂停不冻结 completion、safe position 计算和安全位置持久化。

同一暂停期间的多个失败纳入一次 Job 级恢复过程。每条失败保留独立错误、
记录上下文和恢复状态；统一协调限制重试并发、对共享故障依赖的探测、日志和报警。
所有阻塞项进入允许继续的状态后，才恢复新数据。

## 8. FailJob 和宿主取消

FailJob 是用户策略认为当前 Job 不再应恢复的最终动作。它终止当前 Runtime 执行并使
Run 返回根因错误，但不直接杀死嵌入 yaspe 的宿主进程。宿主决定是否退出进程、
报警或重新启动 Job。

最终终止时，Runtime 停止新读取、新交接、新重试和尚未开始的 work。已被 Sink 接受但
结果未定的操作在有限关闭期限内等待。该期限可配置且有默认值，不得超过宿主更早的
deadline。期限内的成功继续更新 completion 和 safe position 并尽快提交；到期仍未确认的操作
不得标记成功。未提交数据由可重放 Source 在后续执行中重新提供。

## 9. Kafka Rebalance

Revoke 开始后，Runtime 立即停止该 split 的新读取、新交接和尚未开始的 work。
在 ownership 失效前，允许在有限期限内收敛已开始和已被 Sink 接受的操作，并尽力提交
连续 safe position；该收尾不得无限阻塞 rebalance。

Ownership 失效后：

- Connector 丢弃该 split 尚未交接的缓存；
- Runtime 不再启动该 split 已接受但未执行的 work；
- 正在计算的 work 可被通知取消，迟到完成的末端输出不得再转移给 Sink；
- 已被 Sink 接受的操作无法假定可撤销，但不得再推进当前 position；
- 已完成但未在旧 ownership 有效期内提交的进度不得失效后补交。

新 owner 从最后成功持久化的 safe position 恢复。旧 ownership 中已产生外部效果但未提交的
记录可能重复，这是 at-least-once 的已知边界，不得让旧 owner 跨 generation 提交来规避。

## 10. 已接受的保证与非保证

第一版保证：

- Source、Connector 预取、Runtime 队列、work attempt 输出和 Sink 等待都受有界资源约束；
- Sink 变慢最终使 permit 耗尽并把背压传回 Source；
- Source 已读取、Collector 已接受和 Sink 已入队都不等于输入完成；
- position 只推进到 split 内连续允许终结的位置；
- 旧 ownership 的迟到结果不污染新 ownership 的进度；
- Runtime 管理的 goroutine 和资源最终响应取消并被回收。

第一版不保证：

- 并行度大于一时的全局输出顺序；
- 用户 Operator 内部副作用的撤销、幂等或事务；
- Sink 已接受后的通用回滚；
- 在没有事务或幂等 Sink 时的 exactly-once；
- rebalance、请求结果未知和 position 提交前崩溃时完全无重复；
- 对不可重放 Source 的无丢失恢复。

## 11. 开放问题

- Sink 交接与 completion 语义对应的具体 Go API；
- 全局 in-flight budget 与 Connector 预取、队列、attempt 暂存和 Sink 缓冲的关系；
- `Record` metadata 与 Runtime envelope 的边界；
- Operator 实例是否允许被多个 Worker 并发调用；
- 第一版线性 Job Definition；
- Skip 是否为允许推进 position 的终态；
- Kafka revoke 的有限收尾期限；
- 失败策略的具体动作集合、等待节奏和更细恢复范围。

## 12. 实现前必须保持的审核点

实现或评审 M1/M2 时，必须能在设计和测试中回答：

- 一条记录从哪一刻开始由 Runtime 承担 completion responsibility；
- 每一层缓冲和预取的数量及字节上限是什么；
- 一次 work attempt 失败时哪些输出仍可丢弃，哪些已经转移给 Sink；
- Sink 入队、外部完成、输入终结和 position 提交是否被正确区分；
- 暂停、终止和 revoke 是否仍允许安全进度持续提交；
- 旧 generation 的任何迟到路径是否都被 fence；
- 宿主取消是否能有界结束所有 Runtime 管理的 goroutine。
