# 0004：Operator Attempt 与 Collector

状态：Accepted
最后更新：2026-08-26
适用阶段：M0+
依赖：[核心执行模型](0001-core-execution-model.md) · [Job Definition](0002-job-definition-and-runtime-instantiation.md)

本文是 Collector scope、Emit ownership、Operator Chain 和 work-attempt 暂存边界的权威契约。Operator 可选生命周期与 lane 实例化见 Job Definition Design。

## 1. Collector 生命周期与并发

### 1.1 每次 Process 逻辑独立

Runtime 为每次 `Operator.Process` 调用提供一个逻辑上独立的 Collector：

```text
create logical Collector
    ↓
Process begins
    ↓
Emit 0..N times
    ↓
Process returns
    ↓
Collector becomes invalid
```

具体约束：

- Collector 仅在对应 `Process` 调用期间有效；
- Operator 不得保存 Collector；
- Operator 不得在 `Process` 返回后继续调用 Collector；
- Collector 可以关联当前 work attempt/execution scope；
- `Emit` 成功后，Runtime 取得输出的后续处理责任；
- 逻辑独立不等于必须为每次调用执行独立堆分配。

物理实现可以是栈上小对象、内联 edge、长期复用的 output 加当前 execution scope，或者经验证安全的池化对象。优化不能改变公开生命周期。

### 1.2 Collector 仅串行使用

- 单个 Collector 只能在对应 `Process` 的调用 goroutine 中串行使用；
- Collector 不保证线程安全；
- Operator 不得启动 goroutine 并发调用 Collector；
- Operator 不得异步保存 Collector；
- `Emit` 按调用发生顺序处理；
- 不同输入可以由不同 Pipeline Worker 并行处理。

普通 Operator 内部若任意创建 goroutine，会使真实并行度脱离 Runtime 控制，并引入输出顺序、错误竞争和生命周期问题。需要异步能力时，应由未来 Runtime 托管的 Async Operator 或显式 Stage 提供。

### 1.3 Emit 契约

- `Collector.Emit` 的公开形式为 `Emit(record)`，不接收调用方传入的 context；
- Runtime 创建 Collector 时将当前 work attempt 的 `Process` context 绑定到 Collector；
- `Emit` 在等待下游容量、传播背压和解除阻塞时只使用该绑定 context。用户
  Operator 不能用无关 context 脱离当前 attempt 的取消边界；
- `Process` 仍显式接收 context，供用户计算、I/O 和派生操作使用；
- `Emit(nil)` 表示当前 Collector 已接受输出并取得后续责任；
- Collector 接受不等于最终 Sink 已经完成；
- `Emit` 失败表示本次输出未被接受；
- FlatMap 首次 Emit 失败后停止后续输出；
- context 取消不撤回此前已经成功接受的输出；
- `Emit` 必须能够传播下游同步处理错误、Runtime 取消和容量边界错误；
- 具体实现可以在同步 Chain 中直接调用下一个 Operator，也可以在明确边界处等待容量；
- yaspe 不承诺所有端到端背压都必须表现为 `Emit` 长期阻塞。

### 1.4 Emit 值的 ownership 与复制

`Record[T]` 按值传递不表示其引用数据被复制。`T` 可以包含 slice、map、pointer、interface，
以及由这些值间接引用的任意对象；Runtime 无法对任意 `T` 实施通用、安全且语义正确的深拷贝。
第一版采用成功交接即转移 ownership 的约定：

- `Emit` 成功时，当前 `Record[T]` 及其可达引用数据的 ownership 立即转给 Runtime，不延迟到
  `Process` 返回；
- 调用方从成功的 `Emit` 返回起不得再修改、复用或释放相关引用数据和 backing storage，也
  不得将其放回对象池；普通变量随后改为指向其他值不属于复用；
- `Emit` 失败表示本次输出未被接受，ownership 仍属于调用方，调用方可以修改、复用或释放
  相关数据；此前成功 Emit 的其他输出不受这次失败影响；
- Runtime 默认不复制 `T`，也不提供通用 copier/serializer。需要复用原存储的用户代码必须
  在 Emit 前自行创建独立值；
- Runtime 和后续接收方取得生命周期管理责任，但把业务值视为逻辑不可变，不依赖 ownership
  对其原地修改。

该规则是 Go 类型系统无法完全强制的实现约定。成功 Emit 后仍通过别名修改或复用数据属于
用户实现错误；yaspe 不保证检测或阻止，也不保证此时的输出内容、确定性或并发安全。可能结果
包括已暂存输出被覆盖、多次输出意外共享最终内容、data race，以及对象池提前复用造成的数据
污染。race detector 只能发现其中一部分并发违规。

## 2. Operator Chain 与 work-attempt 边界

### 2.1 两个观察层级

单个 Operator/Collector 层级：

- FlatMap 第三次 Emit 失败，不会从这个 Collector 的测试观察中撤销前两次 Emit；
- 此前接受的输出仍然对本次 Collector 可见。

完整 work-attempt 层级：

- 中间 Collector 同步驱动下一个 Operator；
- 中间层不建立持久恢复队列；
- Chain 的最终输出在 attempt 成功前保存在 Runtime 可撤销的有界末端边界；
- 任一 Operator 失败时，Runtime 丢弃该 attempt 尚未转移给 Sink 的全部末端输出。

这两个层级并不矛盾：前者定义 Operator 契约，后者定义 Runtime 是否已经产生不可撤销的外部责任。

### 2.2 末端暂存

```text
Runtime-owned input
    ↓ work attempt
Synchronous Operator Chain
    ↓ attempt success
Bounded terminal output
    ↓ eligible handoff
Sink-owned output
```

末端暂存的作用是：

- Chain 失败时，避免把部分最终输出提前交给 Sink；
- 策略允许重试时，可以使用保留的原始输入重新执行整条 Chain；
- 暂停、position gap 或 generation 变化时，可以阻止尚未产生外部 effect 的 work 继续交接。

它不是事务日志，也不提供外部原子性。第一版终端暂存按 work 数量保持有界；字节预算属于后续增强。

Operator 内自行产生的外部副作用不受末端暂存保护。重新执行 Chain 可能重复这些副作用，责任由 Operator 作者承担。

近期实现使用有界 terminal queue 保存已经成功完成 Chain、尚待 Sink 接管的 work。Worker 以可取消的 `Put(ctx, completedWork)` 整组提交；队列已满时，Worker 阻塞并继续持有当前 completed work，不处理下一条输入。队列腾出或 context 取消后 Put 才返回。这样 Sink 回压会依次填满 terminal queue、阻塞固定数量的 Worker、耗尽 input queue/permit，最终传回 Source。

blocked Worker 当前持有的 work、terminal queue、Sink Coordinator 当前 work 和 Sink 已接管的 in-flight effect 都必须计入资源预算。Sink Coordinator 和 completion 协调路径使用独立执行路径，不得依赖可能全部阻塞在 terminal queue 的 Pipeline Worker，否则会形成循环等待。

### 2.3 零、一和多输出

- Map：形成一个派生输出；
- Filter 保留：继续传递原记录；
- Filter 丢弃：成功 work 零输出，可直接完成；
- FlatMap：形成有限个派生输出；
- 任一 Operator 返回错误：attempt 失败，尚未交给 Sink 的末端输出可撤销。

