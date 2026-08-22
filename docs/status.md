# yaspe Current Status

最后更新：2026-08-23
当前里程碑：M0 — 核心语义与项目基线

## 新会话阅读顺序

1. 本文件；
2. [Living Architecture](architecture.md)；
3. [Roadmap](roadmap.md) 当前里程碑；
4. [核心执行模型 Design](designs/0001-core-execution-model.md)；
5. 相关 [ADR](decisions/)；
6. 当前代码和测试。

## 项目定位

yaspe 是一个使用 Go 1.27 编写的、类型安全、可嵌入的流处理引擎。首个真实使用方是 `lightning-log-filter`。

## 当前代码

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

当前已有 Map 测试覆盖：

- 正常转换；
- transform 失败时零输出；
- Emit 失败向上传播；
- context 传递给 transform。

当前已有 Filter 测试覆盖：

- predicate 匹配时输出原记录；
- predicate 不匹配时零输出；
- context 传递给 predicate；
- predicate 失败时零输出；
- Emit 失败向上传播。

当前已有 FlatMap 测试覆盖：

- 零输出和多输出；
- 多个结果按切片顺序输出；
- context 传递给 transform；
- transform 失败时零输出；
- Emit 中途失败时保留此前输出并停止后续发送。

## 已接受方向

- Operator 描述计算，Runtime 控制执行；
- Source 数据进入受 Runtime 有界容量和背压控制；
- Connector 适配外部系统的物理 pull/push 模型；
- 第一版保留 `Collector.Emit(ctx, record)`；内置 Operator 默认透传 `Process` context，普通构造 API 的使用方无需直接处理 context；
- Map 同时提供简单 transform 和 context-aware transform 构造入口；
- Filter 同时提供简单 predicate 和 context-aware predicate 构造入口；
- FlatMap 同时提供简单 transform 和 context-aware transform 构造入口；
- FlatMap 的多个输出按顺序 Emit，首次 Emit 失败后不再发送剩余输出；
- FlatMap 调用不是事务边界，Sink batch 不保留 FlatMap 输出分组；
- 同一输入的派生输出可共同参与完成跟踪，但这不等于事务原子性；
- M1/M2 近期采用多条并行的完整 Pipeline，单条输入在 Operator Chain 内同步执行；
- 第一版以一条 Runtime 已接受的输入执行完整同步 Chain 作为一次 work attempt；
- 中间 Operator 不保留持久恢复缓存，最终输出在 attempt 成功前留在 Runtime 可撤销的有界末端边界；
- Chain 失败时丢弃未转移的末端输出，并在策略允许时使用原始输入重新执行整条 Chain；
- Chain 成功后输出才转移给 Sink，之后的失败优先在 Sink 边界恢复，不重新执行 Operator；
- 一个 work 的最终输出向 Sink 整组交接，成功前归 Runtime、成功后归 Sink；整组交接不要求同一物理 batch，Sink 可跨 work 组批；
- Runtime 将 terminal output 包装为带不透明 completion 身份的 `SinkItem[T]`；Sink Connector 接收一个 work 的完整 `[]SinkItem[T]`，不接收内部 `Work`；
- Connector 使用 `item.Record` 生成目标系统请求，并在 callback 时原样报告对应 item，无需维护 index 或依赖业务值相等性；`T` 必须匹配 Sink 输入类型；
- Runtime 保留 attempt、generation、position 和 completion 状态；`Accept` 每次接收绑定当前 work 的 `SinkResultReporter`，reporter 可在返回后保存、跨 goroutine 使用且必须并发安全；
- 只有 `SinkAccepted, nil` 才使 reporter 有效；Runtime 必须安全处理 callback 早于 `Accept` 返回的竞态，容量恢复通知与 work reporter 分离；
- reporter 通过 `Report([]SinkItemResult[T])` 增量或批量报告，outcome 为 `SinkSucceeded`、`SinkNotApplied` 或 `SinkUnknown`；成功的 `Err` 必须为 nil，后两者必须为非 nil，没有底层异常时使用标准哨兵错误；
- `Report` 不返回 error，并把 results slice 所有权转给 Runtime；Connector 调用后不得读取、修改或复用该 slice 及其 backing array，从而允许 Runtime 避免复制；
- 未来通用异步 Sink 可以内部增加物理请求级 result handler，聚合跨 work 组批或拆批结果，但不改变稳定的 `Accept(items, reporter)` 边界；
- 每个 Sink 由独立 Sink Coordinator 调用 `Accept`；Pipeline Worker 只向有界 terminal queue 整组提交 completed work，不直接调用 Sink 或维护 Sink 容量；
- Runtime 为每个 Sink 实例创建 `SinkContext` 并调用一次 `Open`，成功后才开放业务数据，结束时最多调用一次 `Close`；第一版 `SinkContext` 只提供 `LifecycleContext()` 和 `CapacityNotifier()`；
- `SinkCapacityNotifier.NotifyAvailable()` 报告调用瞬间观察到可用容量，但不预留容量或保证下一次 `Accept` 成功；它可并发调用、不返回 error，Runtime 容忍重复、合并和过时通知；
- Sink 生命周期 context 可供后台任务和已接管异步操作保存且仅由 Runtime 取消；`Accept` context 只控制单次接管，`Close` 使用独立 context 控制有限收尾；
- terminal queue 满时 Worker 可取消地阻塞并持有当前 work；固定 Worker、队列、Coordinator current 和 Sink in-flight 都计入资源预算，Coordinator/completion 路径不得依赖被阻塞的 Worker；
- Runtime 决定 work 的 Sink 交付资格与内部调度，Sink Connector 不感知 pull、push、mailbox 或 event loop；
- Sink 原子接管方法返回 `(SinkAcceptStatus, error)`：`SinkAccepted` 表示整组接管，`SinkBackpressured` 表示一个也未接管；任何非 `nil` error 同样表示一个也未接管并忽略 status；
- 回压时责任仍在 Runtime；容量通知采用单调 version，Coordinator 在 `Accept` 前观察版本，返回回压后若版本已变化就立即重试，否则原子等待后续版本，从而避免丢失唤醒；
- Sink completion 区分确认成功、可证明未生效和结果未知；重试决策是独立的用户策略维度；
- 能可靠获得逐项结果时保留成功部分并只重试未完成部分，整批重试是无法细分时的特殊情况；
- Sink 异步回调统一交给 Runtime 协调路径串行、幂等处理，迟到、乱序和重复通知不得重复终结 work 或推进旧 generation；
- 外部客户端 callback 只能通过 reporter/notifier 提交事实；capacity 使用同步推进 version 的独立 signal，每个 reporter 使用有界 result inbox；
- 同一 reporter 的多次增量 Report 在 Coordinator drain 前最多产生一个 pending wakeup，重复或非法结果在进入 pending 存储前丢弃，活跃 reporter 总数受 in-flight 上限约束；
- Sink Coordinator 将 item outcome 聚合成 work-level completion 后交给 Completion Tracker，后者负责输入终态、permit 和 position；capacity 与 completion 不强制共用普通 FIFO channel；
- Sink 的等待 buffer、并发请求、重试项和 timer 都必须有界；
- 每次 Process 获得逻辑独立的 Collector，Process 返回后 Collector 失效；
- Collector 仅允许在 Process 调用 goroutine 中串行使用，不保证线程安全；
- Worker 将输出交给异步 Sink 后可以处理下一条输入，但输入 completion 持续到所有必需 Sink effect 完成；
- 端到端 in-flight permit 持续到输入终结，Sink 变慢通过容量耗尽将回压传回 Source；
- Kafka position 只推进 partition 内连续完成位置；
- 第一版生产链路以 at-least-once、避免静默丢失为目标，长期演进到 checkpoint epoch；
- 重试优先暂停引入新数据，把问题限制在当前失败和有界在途数据；
- 重试可以在时间上无限等待，是否退出由用户策略决定，但空间和执行资源必须有界且始终响应宿主取消；
- 暂停后不再启动已接受但尚未执行的记录，已开始的 Chain 可继续收敛到末端边界；
- 同一 split 中位于未解决失败之后的新 Sink effect 暂缓，失败之前可填补连续进度的记录允许继续；
- 已被 Sink 接受的操作不撤回，暂停期间仍处理 completion、推进并尽快持久化 safe position；
- 同一暂停期间的多个失败由一次 Job 级恢复过程协调，每条失败仍保留独立诊断和恢复状态；
- 所有阻塞项解决后才恢复新数据，重试并发、依赖探测、日志和报警由统一恢复过程限制和聚合；
- 最终 FailJob 或宿主取消时，已被 Sink 接受的未定操作在可配置且受宿主 deadline 限制的关闭期限内等待；
- 关闭期间仍更新 completion 和 safe position 并尽快提交，到期未确认操作不得标记为成功；
- Connector 已读取但 Runtime 尚未接受的数据由 Connector 有界持有，不进入 record completion tracking；
- 临时暂停时可保留有界未交接数据并在恢复后优先按 split 内原顺序交接，暂停期间不得扩大预取；
- 未交接数据从属当前 split ownership，revoke、ownership 连续性无法确认或 Job 终止时丢弃，不产生 completion 或 position 推进；
- 同一 split 重新分配给同一实例也属于新 ownership，不复用旧 ownership 的未交接缓存；
- revoke 后对该 split 进行有限收尾并尽力提交 safe position，ownership 失效后通过 generation fence 拒绝旧任务推进或提交 position；
- 暂停业务数据交接不等于停止 Kafka session/heartbeat 维护；
- 第一版面向 Runtime 采用非阻塞 Reader，通过可等待的可用性通知避免忙轮询；
- Connector 内部适配外部阻塞 I/O、批量读取和 session 维护，Runtime 取得 permit 后才取走记录并完成责任交接；
- 业务记录与 Source 控制事件使用独立路径，ownership 失效、revoke、取消和 fatal error 优先于新的记录交接；
- 多 Pod Kafka Source 早期使用 Kafka Consumer Group 协调 partition；
- yaspe Runtime 决定 safe position，Kafka Connector 执行 offset commit；
- 第一版目标是 record 级并行、单 record 内同步执行；
- 不通过无限队列、无限 goroutine 或提前提交 position 换取吞吐。

## 当前开放问题

- Record metadata 边界；
- 全局 in-flight budget 与各局部队列容量的关系；
- Operator 实例是否允许被多个 Pipeline Worker 并发调用；
- 第一版线性 Job Definition。

## 下一步

1. 以 `designs/0001-core-execution-model.md` 为唯一正式核心执行模型；
2. 从正式 Design 第 16 节继续收敛开放问题，优先讨论 Sink API、容量预算和 Record metadata；
3. 明确 Operator 实例并发、Skip 终态和 Kafka revoke 默认期限；
4. 在核心开放问题收敛前，尚不要实现 Worker Pool、Kafka 或完整 DAG。

## 工具链

```text
Go language version: 1.27
Minimum toolchain: Go 1.27 stable
Current local toolchain: go1.27.0-X:nodwarf5 linux/amd64
```

`go.mod` 不固定具体 patch toolchain，由开发环境和 CI 使用 Go 1.27
或更新的兼容工具链。
