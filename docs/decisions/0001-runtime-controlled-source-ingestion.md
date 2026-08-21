# 0001：由 Runtime 控制 Source 数据进入

状态：Accepted  
日期：2026-08-01

## 背景

不同外部数据源具有不同的物理读取方式：Kafka consumer 主动 poll broker，文件 Source 主动读取文件，而 callback 或订阅型数据源可能天然向客户端推送数据。

如果 yaspe 强制所有 Connector 使用同一种物理 pull/push 模型，会把外部系统的实现差异泄漏到核心 Runtime。另一方面，如果 Source 可以不受控制地向 Runtime 推送记录，就可能通过无限 goroutine、无限队列或 Connector 内部预取耗尽内存，并使背压失效。

Source、Runtime 和 Operator 需要建立与外部读取方式无关的职责边界。

## 决定

yaspe 不要求所有外部系统采用统一的物理 pull 或 push 模型。

- Connector 负责适配外部系统的读取方式；
- Runtime 负责控制允许进入执行系统的记录数量；
- Source 只能通过 Runtime 提供的输出边界提交记录；
- Source 到 Runtime 的队列、预取和在途记录必须有界；
- 当 Runtime 没有容量时，Connector 必须阻塞、暂停读取或使用等价机制响应背压；
- Connector 的读取和 session 维护可以使用专用 I/O goroutine，但其生命周期由 Runtime 管理；
- Operator 不感知 Source 的物理读取模型；
- Operator 通过能够传播错误、取消和背压的 `Collector.Emit` 产生下游结果；
- Source 不以“已经读取”作为记录处理完成的依据；
- Runtime 负责判断记录何时完成，以及何时可以确认 Source position。

整体模型为：

```text
External System
      ^
      | Connector 适配 pull/push
      v
Bounded Source Boundary
      ^
      | Runtime 容量与背压控制
      v
Operator --Collector.Emit--> Downstream
```

本决定固定职责和语义，不固定第一版 Go 接口。`Run(output)`、`Poll(output)` 或其他接口形式应在核心执行模型设计和原型中验证后决定。

后续阶段设计已选择“外部阻塞 I/O 由 Connector 内部适配，Runtime 面向非阻塞 Reader”的分层模型，详见 [核心执行模型](../designs/0001-core-execution-model.md)。本 ADR 中“不固定接口”表示当时的职责决定不依赖具体调用形式，不表示该问题目前仍未决。

## 原因

- 允许 Kafka、文件和 callback Source 使用适合自身协议的读取方式；
- Runtime 能够统一约束资源并传播背压；
- 业务 Operator 不依赖 Connector 或外部客户端；
- 后续可以引入 pause/resume、credit 或异步可用通知，而不改变 Operator；
- 将“读取数据”和“完成处理”明确分离，为 acknowledgment 和 checkpoint 建立基础。

## 后果

### 正面影响

- yaspe 核心不绑定 Kafka 的 poll API；
- Source Connector 可以为阻塞 I/O 使用专用 goroutine；
- 慢下游可以限制上游进入量；
- Runtime 可以统一管理取消、停止和资源回收；
- Source 和 Operator 可以独立测试。

### 代价和风险

- Connector 必须正确实现背压和取消；
- Connector 内部缓冲也必须纳入资源预算；
- 阻塞 `Emit` 不能妨碍 Kafka 等客户端维持必要的 session/heartbeat；
- Source I/O goroutine 与 Runtime 调度之间需要明确 ownership；
- 不同 Connector 可能需要不同的 pause/resume 实现。

## 验证要求

- 慢 Sink 最终能够使 Source 停止继续读取或预取；
- 队列饱和时进程内存保持有界；
- context 取消能解除阻塞读取和阻塞 Emit；
- Runtime 退出后不存在 Source goroutine 泄漏；
- Connector 内部预取数量可配置或有明确上限；
- Kafka Connector 在背压期间仍能满足其 session 生命周期要求。

## 重新评估条件

- 原型证明当前边界无法支持某类重要 Source；
- 为支持 checkpoint barrier，需要调整 Source 与 Runtime 的控制协议；
- 分布式 Source split 管理要求引入新的 Coordinator/Reader 模型；
- 性能数据证明当前交接模型造成不可接受且无法优化的开销。
