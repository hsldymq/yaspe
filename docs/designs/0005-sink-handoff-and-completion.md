# 0005：Sink Handoff 与 Completion

状态：Accepted（M1 Memory Sink 条款已定；M2 异步接口细节仍待收敛）
最后更新：2026-08-26
适用阶段：M1–M2
依赖：[核心执行模型](0001-core-execution-model.md) · [Operator Attempt](0004-operator-attempt-and-collector.md)

本文是 terminal output 整组交接、Memory Sink、异步 completion、有限关闭与迟到隔离的权威契约。

## 1. Sink 交接与 Completion

### 1.1 整组责任转移

Chain 成功后，Runtime 才允许 terminal output 进入 Sink 边界。一个 work 的最终输出必须整组交接：

```text
Sink 接受该 work 的全部输出
或
Sink 一个也不接受
```

这是责任转移原子性，不是外部写入事务原子性，也不要求一个 work 独占一个物理 batch。Sink 接管后可以把多个 work 的输出自由组批。

成功交接前，输出由 Runtime 持有，可以暂缓或撤销；成功交接后，输出责任转给 Sink，Runtime 不再假设可以撤回。

Sink 接管后失败时，优先保留已经形成的 terminal output，并在 Sink 边界恢复，而不是重新执行 Operator Chain。

交接 API 不向 Sink Connector 暴露 Runtime 内部的 `Work`。Runtime 把每个 terminal output 包装为 `SinkItem[T]`，其中包含业务 `Record[T]` 和仅供 Runtime 关联 completion 的不透明身份。Connector 接收该 work 产生的完整 `[]SinkItem[T]`；`T` 必须与 Sink 声明的输入类型一致，并由 Go 泛型在组装 Pipeline 时约束。

Connector 使用 `item.Record` 转换目标系统需要的请求，并让原 `SinkItem[T]` 跟随该请求直到 callback，再通过 reporter 原样报告对应 item 的结果。Connector 不解释不透明身份、不维护 records index，也不依赖业务值相等性；因此内容相同的多条 Record 仍可被 Runtime 准确区分。目标系统自己的结构应使用 `KafkaRequest`、`PostgresRow` 等具体名称，避免与 `SinkItem` 混淆。

attempt、generation、Source position、completion 状态和调度信息仍由 Runtime 保存。原子接管方法采用每次交接传入绑定式结果报告器的方案，当前拟定 API 为：

```go
type SinkContext interface {
    LifecycleContext() context.Context
    CapacityNotifier() SinkCapacityNotifier
}

type SinkCapacityNotifier interface {
    NotifyAvailable()
}

type Sink[T any] interface {
    Open(runtime SinkContext) error

    Accept(
        ctx context.Context,
        items []SinkItem[T],
        reporter SinkResultReporter[T],
    ) (SinkAcceptStatus, error)

    Close(ctx context.Context) error
}
```

Runtime 创建 `SinkContext` 并对每个 Sink 实例调用一次 `Open`；只有 Open 成功后才启动 Sink Coordinator、Pipeline Worker 和 Source admission。运行期仅 Sink Coordinator 调用 `Accept`。Runtime 最多调用一次 `Close`，Open 失败时不开放数据入口，并按逆序清理已经打开的其他组件。

`LifecycleContext()` 返回由 Runtime 控制的 Sink 生命周期 context，Sink 可以保存并用于后台任务和已接管的异步操作，但不能取得其 cancel function。它不同于 `Accept` 的单次接管 context；Accept 返回成功后，异步操作不得因该调用 context 生命周期结束而被取消。`Close(ctx)` 的 context 独立控制有限 drain/资源释放，因此即使生命周期 context 已取消，关闭仍可在单独 deadline 内收尾。`SinkContext` 第一版只提供生命周期 context 和 capacity notifier，未来能力按实际需求扩展。

`SinkResultReporter` 已经绑定当前内部 work，异步 Sink 可以保存它并在外部 callback 中报告完成事实，不需要取得或保存 work ID。它与 Collector 的调用期生命周期不同：可以在 `Accept` 返回后使用、可以跨 goroutine 调用，并且必须并发安全。

只有 `SinkAccepted, nil` 才会使 reporter 对该次交接有效；返回 `SinkBackpressured` 或 error 时，Sink 不得保存或调用它。Runtime 还必须正确处理外部客户端在 `Accept` 返回前同步触发 callback 的竞态：提前到达的报告只能暂存，确认接管成功后才能应用；若最终没有接管，则不得据此终结 work。

结果报告采用一个可增量、可批量调用的方法：

```go
type SinkOutcome uint8

const (
    SinkSucceeded SinkOutcome = iota
    SinkNotApplied
    SinkUnknown
)

type SinkItemResult[T any] struct {
    Item    SinkItem[T]
    Outcome SinkOutcome
    Err     error
}

type SinkResultReporter[T any] interface {
    Report(results []SinkItemResult[T])
}
```

`Report` 可以调用一次或多次，每次报告任意数量的 item，不要求按原顺序或一次覆盖整个 work。`SinkSucceeded` 必须携带 nil `Err`；`SinkNotApplied` 和 `SinkUnknown` 必须携带非 nil `Err`。如果外部协议只提供结果状态而没有底层异常，Connector 使用 yaspe 提供的标准哨兵错误；因此 Runtime、日志和失败策略始终能得到具体错误值，但 outcome 仍是外部事实的权威分类。

调用 `Report` 时，`results` slice 的所有权转移给 Runtime。调用返回后 Connector 不得读取、修改、缩短、扩展、复用其 backing array，也不得放回对象池。该约定允许 Runtime 将结果直接投递到协调路径而不复制；`SinkItem` 和 `SinkItemResult` 对 Connector 都是逻辑不可变值。违反约定属于 Connector 实现错误，可使用 race detector 和 Connector 契约测试发现。

`Report` 不返回 error，可以由任意 goroutine 调用，并由 Runtime 幂等处理重复、迟到、越界、外来或互相矛盾的 item 结果；这些违规不得在 callback goroutine 中 panic，应被忽略并记录诊断。容量恢复通知属于整个 Sink 的容量状态，不属于某个 work，因此不放入 `SinkResultReporter`。

#### 1.1.1 M1 同步 Memory Sink

Memory Sink 的定位是 Runtime 同步参考 Sink、结果断言工具、benchmark 终点和本地示例
输出。它用来验证 terminal output、整组责任交接、并发调用、Sink 失败与 FailJob，
不提供外部 I/O、异步 completion、batch flush、partial success、position、Retry、持久化或
exactly-once。下列 `Accept`、`Groups`、`Records` 和 Close 等名称只表达已接受语义，
不锁定最终公开 Go API。

Runtime 只在整条 Operator Chain 成功后，把一个 work 的全部 terminal outputs 通过一次
同步调用交给 Memory Sink：

```text
Accept(group) returns nil
    → Sink atomically owns and has synchronously completed the whole group

Accept(group) returns error
    → Sink owns and retains none of the group
```

成功返回是整组 `Record` 及其可达引用数据的 ownership 转移点；Runtime 之后不得
修改或复用。返回 error 时 Memory Sink 不得保存组内任何 Record 或引用，ownership 仍属于
Runtime，错误进入 Failure Policy。不存在先保存部分记录再返回 error 的合法路径。

零输出 work 没有 Sink effect，Runtime 不调用 Memory Sink，直接将该 work 标记为 Success 并
释放 permit。这是 Filter 不保留、FlatMap 返回空结果或自定义 Operator 正常不 Emit 的
成功语义，不是 Skip/Discard。

Memory Sink 可以配置固定、确定性失败计划，例如第 N 次接管失败、所有接管失败或
按预先给定的 error 序列失败。M1 不接受任意用户失败回调，以避免多出一套用户代码
生命周期、panic 和 records 逃逸契约。每次接管在同一线性化区内先检查生命周期和
失败计划；若失败则不保存任何值，若成功则一次追加整组。

Memory Sink 必须线程安全并保留 work grouping。单组内记录顺序严格保持；并发 work 的
group 按实际接管线性化顺序保存。该顺序是可观察实现事实，不是 `Parallelism > 1`
时的业务顺序保证；`Parallelism = 1` 时应保持线性 work 顺序。M1 不承诺并发等待者公平性。

结果读取在语义上提供保留 work 边界的 groups 视图和按实际接管顺序扁平化的 records
视图。两者均返回新的外层与组内 slice 结构，调用方修改 slice 不得破坏 Sink 内部结构；
yaspe 不对任意 `T` 做深拷贝，因此快照中 Record 及可达引用数据仍必须视为只读，读取快照
不转移 ownership。运行期快照只包含该调用线性化前已完成的整组接管；`Run` 返回后
结果集合稳定。

Close 由 Runtime 调用，幂等、同步且快速，不清空已接管结果，Close 后仍可读取快照。
Memory Sink 没有后台 goroutine、外部 in-flight、flush 或可注入 Close failure。正常关闭顺序为：

```text
Runtime stops new Sink handoff
    ↓
already-entered synchronous Accept calls finish
    ↓
Memory Sink closes
    ↓
results remain readable and stable
```

接管与 Close 使用同一线性化边界。接管先生效时，其整组成功或失败结果完整形成后
Close 才继续；Close 先生效时，后续接管返回可识别的 closed error、不保存数据也不转移
ownership。正常 Runtime 不应在 Close 后调用接管；该错误不得被静默丢弃。

某个并发接管返回 error 并触发 FailJob 时，其他已在线性化点成功的 groups 仍然有效，
不回滚；Runtime 得知 FailJob 后阻止尚未开始的新 Sink handoff，等待已进入的同步调用返回，
再 Close。Memory Sink 不提供跨 work 事务或回滚。

### 1.2 有界通知驱动交接

Runtime 与 Sink 之间使用有界、通知驱动的交接边界：

- Runtime 为每个 Sink 实例提供独立的 Sink Coordinator；只有 Coordinator 调用 `Accept`，Pipeline Worker 不直接调用 Sink，也不维护 Sink 容量状态；
- Worker 只向有界 terminal queue 提交 completed work；交接成功后可以处理下一条，但端到端 permit 仍持续到 Sink effect 完成；
- Sink Coordinator 判断哪些 work 当前有资格产生 Sink effect，并负责选择、等待、公平性和重试调度；
- Sink Connector 只被动接收一个 work 的完整 `[]SinkItem[T]`，不感知 `Work` 内部状态，也不感知 Runtime 采用 pull、push、mailbox 还是 event loop；
- Sink 对容量的判断和整组责任接管必须是一个原子操作，方法返回 `(SinkAcceptStatus, error)`；
- `SinkAccepted, nil` 表示 Sink 已取得该 work 全部输出的后续责任；
- `SinkBackpressured, nil` 是流量控制结果，不是处理失败，此时责任仍在 Runtime，Sink 不得接管部分输出；
- 任何非 `nil` error 都表示 Sink 一个输出也没有接管，status 被忽略，错误交给 Runtime 的失败策略处理；
- Connector 不得在接管部分输出后返回 `SinkBackpressured` 或 error；
- 不提供先调用 `IsBackpressured`、再提交 work 的分离式协议，避免两步之间容量状态发生变化；
- Sink 在观察到接管容量恢复或增加时调用 `SinkCapacityNotifier.NotifyAvailable()`，报告调用瞬间存在可用容量；它不负责拉取或选择 work；
- 通知不预留容量，也不保证 Coordinator 稍后调用 `Accept` 时仍能成功；Runtime 必须把它视为重新检查触发器，并以新的原子 `Accept` 结果为准；
- Connector 不需要判断 Runtime 是否正在等待；notifier 可从任意 goroutine 并发调用、不返回 error，并应快速完成；Runtime 容忍重复、合并以及到达时已不再可用的通知；
- 永远无法被当前 Sink 接受的 item group 必须返回真实 error，不得以 `SinkBackpressured` 等待一个永远不会发生的容量通知；
- 容量通知使用单调递增的 generation/version，而不是可被旧 `Accept` 结果覆盖的 available 布尔值；
- Coordinator 在调用 `Accept` 前取得 observed version；若返回 `SinkBackpressured` 时版本已经变化，则立即重试，否则原子等待 version 大于 observed；
- 版本检查与等待必须避免 check-then-wait 窗口，可使用“版本号 + 每代关闭并替换的 channel”等内部机制；通知允许合并和伪唤醒，但不得丢失；
- Runtime 在容量变化后重新尝试交接；没有容量或状态变化时不得忙轮询；
- 未来可以在 Runtime 内部改用其他调度方式、DAG edge 或跨进程 exchange，只要不改变责任转移、有界背压和完成语义。

### 1.3 异步 completion

Sink 的等待 buffer、并发请求、待重试项和 timer 都有上限。Worker 在 terminal output 被 Sink 整组接管后可以处理下一条输入，但父输入仍保持 in-flight，直到所有必要 Sink effect 明确完成。

外部异步 callback 不直接并发修改 work、completion、safe position 或 generation。callback 只报告事件，由 Runtime 协调路径串行且幂等地应用。

这里的 callback 是 Kafka、HTTP、数据库等外部异步客户端在请求完成、失败或超时时执行的回调，包括可能在注册时同步触发、早于 `Accept` 返回的回调。它只能通过 `SinkResultReporter.Report` 和 `SinkCapacityNotifier.NotifyAvailable` 提交事实，不能直接操作 Runtime 状态。

每个 Sink 使用一个 Sink Coordinator 串行处理 Accept、capacity 和 item completion，但两类事件采用不同入口：

- capacity 使用独立的版本化 signal，version 在 `NotifyAvailable` 调用期间同步推进，不通过普通 FIFO 事件累计；
- 每个 work reporter 使用独立、线程安全且有界的 result inbox；`Report` 先验证 item 身份，并只保留每个 item 的第一个有效结果；
- reporter 可以接收多次增量 Report，但通过 `wakePending` 或等价机制合并唤醒：Coordinator drain 前，同一 reporter 在 wakeup queue 中最多占一个位置；
- Coordinator 取得 reporter wakeup 后一次 drain 当前 pending results，再允许后续结果安排新的 wakeup；实现必须处理 drain 与新 Report 并发发生的竞态，不得丢失结果；
- 重复、外来、越界、迟到或互相矛盾的结果在进入 pending 存储前丢弃并记录诊断，不能借此无限扩大队列；
- 活跃 reporter 数量受全局 in-flight work 上限约束，因此每 reporter 一个 pending wakeup 的总空间也有界；
- capacity signal 和 result inbox 不强制共享一个普通 channel 或统一事件结构。

reporter 在 `Accept` 返回前处于 pending-accept 状态。提前到达的有效结果可以暂存，但只有 `SinkAccepted, nil` 才激活并允许 Coordinator 应用；若最终返回 Backpressured 或 error，则丢弃提前结果并记录 Connector 违反契约。Sink Coordinator 将同一 work 的 item outcome 聚合成 work-level completion，再交给 Runtime Completion Tracker；后者负责输入终态、permit 释放、position gap 和 safe position，不理解物理 Sink batch。

当前稳定边界先采用 work 级 reporter。未来提供跨 work 组批的通用异步 Sink 基础设施时，可以在其内部增加类似 Flink `ResultHandler` 的物理请求级 handler，由基础设施负责物理 batch 与多个 work reporter 之间的结果聚合；这不改变 Runtime 与 Sink 的 `Accept` 边界。yaspe 只借鉴绑定式回调和 mailbox 式串行应用，不把“是否重试”混入结果事实。

迟到、乱序或重复通知不得导致：

- work 重复终结；
- permit 重复释放；
- safe position 错误跨越空洞；
- 旧 generation 推进当前 position。

### 1.4 completion 结果

Sink completion 至少区分三种事实：

1. `SinkSucceeded`：确认成功，`Err` 必须为 nil；
2. `SinkNotApplied`：外部协议能够证明未生效，`Err` 必须非 nil；
3. `SinkUnknown`：结果未知、可能已经生效，`Err` 必须非 nil。

超时、断连等不能自动解释为“未写入”。错误临时或永久、是否重试是失败策略的另一维度，不能由这三种事实直接推导。

如果 Sink 能可靠报告每个输出的结果：

- 已确认成功的部分保持成功；
- 只重试确认未生效或仍需处理的部分；
- 原始 work 等所有必要输出都完成后才终结。

整批重试是外部协议无法提供可靠细粒度结果时的退化方案。

### 1.5 completion 关联

概念上可以用“封口 + 未完成计数”理解多个派生输出：

```text
initial: sealed=false, pending=0
accepted child: pending++
attempt sealed: sealed=true
child complete: pending--

sealed && pending == 0
→ input complete
```

最终实现不必采用同名字段，但必须避免 pending 暂时为零、attempt 尚可能继续产生输出时提前完成。

### 1.6 有限关闭与迟到事件隔离

Sink 一旦以 `SinkAccepted, nil` 接管一个 work 的输出，就承担把所有 item 推进到明确结果的
责任。关闭不能把“已接管但仍在内部 buffer”解释为可以丢弃；buffer、已组 batch 和外部
in-flight 请求都属于有限 drain 的范围。

Runtime 关闭 Sink 时采用以下顺序：

```text
停止新的 Accept
    ↓
Sink 在 Close deadline 内 flush 已接管 buffer
并等待外部 in-flight 请求
    ↓
逐 item 形成 Succeeded / NotApplied / Unknown
    ↓
deadline 到期或 drain 完成后，使 reporter/notifier 失效隔离
    ↓
释放 Connector 与 Runtime 协调资源
```

结果必须表达外部事实，而不是为了结束关闭而选择方便的状态：

- 明确完成外部效果的 item 报告 `SinkSucceeded`；
- Connector 能证明尚未提交或取消发生在外部效果之前的 item 报告 `SinkNotApplied`；内部
  buffer 在释放前必须先以该事实完成报告；
- 已发出请求但在 deadline 内无法确认效果的 item 报告 `SinkUnknown`；超时本身不能证明
  `NotApplied`；
- `Close` 返回的整体 error 只表达关闭过程或资源释放结果，不能替代每个已接管 item 的
  completion 事实，也不能授权静默丢弃 buffer。

`Close` 是有期限的尽力完成，不是无限等待。Connector 必须停止自身能够控制的 admission、
goroutine、timer、内部队列和 callback 注册，并在 deadline 内尽量收敛全部已接管 item；但
yaspe 不要求 Connector 证明外部客户端在 `Close` 返回后绝不触发迟到 callback。

Runtime 在关闭边界将相关 reporter 和 notifier 标记为失效（fence）。这里的 fence 是结果资格
隔离，不是终止 callback 或撤销外部效果：对象仍可被迟到 callback 安全调用，但 fence 生效后
尚未应用的事件只产生有界诊断，不得阻塞、panic、重新终结 work、释放 permit 或推进 safe
position。fence 与结果应用必须在同一串行协调路径中建立明确顺序，避免 callback 先观察 active、
再越过并发 fence 提交结果的 check-then-act 竞态。迟到 callback 自身持有的轻量 reporter 可以
自然存活，但 Runtime 不为它保留完整 work、Sink Coordinator 或 position tracker。

callback/push Source 采用同一责任划分：Connector 有界停止自身可控的接收与缓存，Runtime 在
admission/ownership 失效后拒绝迟到数据和 availability notification。外部回调线程始终不能
绕过 Connector 边界直接修改 Runtime 状态。

关闭时的本地完成进度与外部 Source commit 仍然分离。只有 fence 前已经应用、并在 split 内
形成连续前缀的成功结果才能推进 safe position；还必须成功持久化该位置，重启后才能从其后
恢复。例如 Kafka offset 120–140 已连续完成，而 141–150 在关闭期限内仍为 `NotApplied` 或
`Unknown`，最多提交 next offset 141；若该 commit 也未成功，恢复位置会早于 141，但绝不能
因为 Sink 曾接管 141–150 而跳到 151。`Unknown` 或尚未提交的成功效果可能在恢复后重复，这是
当前 at-least-once 边界。

