# 0008：Runtime 验证与可观测性

状态：Discussing
最后更新：2026-08-26
适用阶段：M1–M2
依赖：全部近期执行契约；见 [Design Map](design-map.md)

本文集中维护指标、确定性测试、race/leak、fault injection、benchmark、开放问题和实现前审核。各行为规则仍以对应能力 Design 为权威来源。

## 1. 验证要求

### 1.1 Source 与背压

- 慢 Sink 最终阻止 Source 继续扩大读取或预取；
- 队列和 Sink 饱和时，work/item 数量和 goroutine 保持有界；第一版不保证字节数有界；
- Connector 内部预取数量可配置或有明确上限；字节上限属于后续增强；
- callback/push Source 也能通过有界适配层响应 admission；
- Kafka Connector 在业务回压期间仍满足 heartbeat/session 生命周期。
- 确定性竞态测试必须把数据发布分别注入到 `TryRead` 返回 `unavailable` 之前、之后以及
  Runtime 实际开始等待通知的两侧，证明不会丢失唤醒；
- 测试覆盖旧通知已占满 channel、Runtime 并发消费旧通知、通知合并、伪唤醒，以及
  end/failure/close 在等待窗口内发生的情形；
- 测试不得依赖调度器时序或 `time.Sleep` 碰撞竞态，必须用可控 hook/barrier 精确重现每个
  check-then-wait 交错。
- Memory Source 测试覆盖满缓冲 Submit 的背压和取消、Submit/Finish 线性化、finishing drain、
  Fail 优先级、首根因保留、Close 唤醒等待者与 Source-owned 缓存丢弃；
- 有限 Memory Source 的端到端测试必须证明 `Finish` 返回不会提前终止 Runtime，`Run` 只在
  已缓存和已接纳 work 及 Sink 都按契约收敛后返回 `nil`。
- admission 竞态测试必须把 context 取消分别注入到 reservation 之前、预留之后/读取之前、
  ready 返回与绑定之间、绑定之后/调度之前，证明只有空 reservation 可直接释放，
  ready 记录必须先登记再取消；
- 队列与 Worker 饱和测试必须证明 ready 后的绑定不再等待或失败，work 可在有界 pending
  状态中受追踪等待调度；
- 不可重放 Memory Source 的取消测试必须把未处理记录显式归类为 cancelled/未完成，不得
  把它误报为 success 或声称可恢复。

### 1.2 取消与资源回收

- context 取消可以解除所有 Runtime 管理的等待路径；
- Connector 阻塞 I/O 能被取消或通过受控关闭结束；
- Runtime 退出后不存在 Source、Worker、Sink 或 retry goroutine 泄漏；
- deadline 到期的未知 Sink operation 不被错误标记成功。
- `Close` 返回后 Connector 自身可控的 goroutine、timer、队列和 callback 注册已经有界收敛；
  外部客户端迟到调用只命中失效的轻量 reporter/notifier，不保留完整 Runtime 协调状态；

### 1.3 attempt、Sink 与 completion

- attempt 失败不会把 terminal output 部分交给 Sink；
- Emit ownership 契约测试覆盖成功后发送方不得复用、失败后仍可复用，以及内置 Operator 不在
  成功 Emit 后修改输出；测试不得宣称能够检测所有违反契约的用户代码；
- Sink 整组交接不会发生部分责任转移；
- 乱序、迟到和重复 callback 不会重复终结或释放 permit；
- 零输出 work 能直接完成；
- 多输出 work 只在全部必要 effect 完成后终结；
- Sink 入队、外部完成、输入终结和 position 提交可以分别观测和测试。
- 关闭测试覆盖已接管 buffer、外部 in-flight、deadline 前部分成功、可证明未生效、结果未知、
  fence 前后并发 callback，以及 `Close` error 不能替代逐 item completion；
- Memory Sink 单元测试覆盖整组成功、固定失败计划下的全组拒绝、零输出不调用 Sink、
  group 顺序与扁平视图，以及快照 slice 与内部 slice 结构隔离；
- Memory Sink 并发测试使用内部 hook/barrier 精确控制接管线性化前后、接管与 Close 竞争、
  失败触发 FailJob 时已进入与尚未进入的调用；不使用 `time.Sleep` 碰撞时序；
- `go test -race` 必须覆盖 Accept、运行期快照与 Close 并发。Close 返回后快照必须稳定，已成功
  groups 不得因其他 work 失败而回滚。测试 barrier 只属于内部测试设施，不进入公开 Sink API。

### 1.4 position 与 ownership

- 较大 position 先完成时不会越过前序空洞；
- generation 失效后任何迟到路径都被 fence；
- revoke 收尾有期限，不会无限阻塞；
- 旧 owner 不会为新 owner 提交迟到位置。

### 1.5 可测试边界

- Operator 可不依赖真实 Source/Sink 独立测试；
- Connector 可用假的 Runtime admission boundary 测试；
- Runtime 可用 Memory Reader/Sink 和可控时钟测试；
- 慢 Sink、结果未知、重复 callback、取消和 rebalance 可以确定性注入。

## 2. 当前开放问题

当前 M0 的退出目标是先收敛所有影响 M1/M2 公共 API、所有权、并发和恢复正确性的设计，
再开始 Runtime 与生产 Connector 编码。局部命名、私有类型组织和可由受约束原型验证的实现
选择不需要在文档中预先固定。

### 2.1 M1 实现前必须收敛

- M1 指标、确定性测试、race/leak 测试和 benchmark 的实现前审核。

### 2.2 M2 实现前必须收敛

- Retry 的适用错误、backoff/jitter、次数或持续时间、耗尽动作和 Job 级恢复范围；
- Source split/position 的公共或内部表示，以及 Kafka committed offset 转换边界；
- Runtime Envelope 中 split、position、generation、work、attempt 和 completion identity 的组织；
- 非阻塞 Reader、availability notification、Source control event 和 Connector Open/Close 的最终接口；
- Sink `Open/Accept/Close`、原子接管、reporter、capacity notification 和 callback slice ownership 的最终接口；
- `SinkSucceeded`、`SinkNotApplied`、`SinkUnknown`、部分成功和迟到/重复 callback 的精确动作；
- Completion Tracker 的零/多输出、permit 释放、position gap 和 generation fence；
- Kafka 客户端适配、poll/pause/commit、assignment/revoke/lost 和 commit 失败规则；
- ClickHouse batch、flush、部分失败、unknown effect 和关闭 deadline；
- M2 指标、故障注入矩阵和 at-least-once 声明审核。

### 2.3 可由原型细化但不得改变语义的事项

- Transformation 私有接口、adapter 布局和异构存储方式；
- Source/Sink definition、运行实例和私有 adapter 的最终 Go 类型名；
- 有界内部 queue 的具体数据结构和不改变公开保证的容量微调；
- 测试工具、fake clock 和 fault injection hook 的 package 组织。

## 3. 实现前审核点

实现或评审 M1/M2 时必须能回答：

- 一条记录从何时开始由 Runtime 承担 completion responsibility；
- 每层预取、队列、暂存、请求和 retry 的数量上限，以及哪些部分暂不承诺字节上限；
- attempt 失败时哪些输出可丢弃，哪些已转给 Sink；
- Sink 入队、外部完成、输入终结和 position 提交是否严格区分；
- 暂停、终止和 revoke 是否仍允许安全进度继续提交；
- 旧 generation 的所有迟到路径是否被 fence；
- Kafka session 是否独立于业务回压继续维持；
- 宿主取消是否能有界结束所有 Runtime 管理的 goroutine。

