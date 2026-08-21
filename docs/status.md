# yaspe Current Status

最后更新：2026-08-21
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
- Collector 的 Emit 当前显式接收 context；
- Map 同时提供简单 transform 和 context-aware transform 构造入口；
- Filter 同时提供简单 predicate 和 context-aware predicate 构造入口；
- FlatMap 同时提供简单 transform 和 context-aware transform 构造入口；
- FlatMap 的多个输出按顺序 Emit，首次 Emit 失败后不再发送剩余输出；
- M1/M2 近期采用多条并行的完整 Pipeline，单条输入在 Operator Chain 内同步执行；
- 每次 Process 获得逻辑独立的 Collector，Process 返回后 Collector 失效；
- Collector 仅允许在 Process 调用 goroutine 中串行使用，不保证线程安全；
- Worker 将输出交给异步 Sink 后可以处理下一条输入，但输入 completion 持续到所有必需 Sink effect 完成；
- 端到端 in-flight permit 持续到输入终结，Sink 变慢通过容量耗尽将回压传回 Source；
- Kafka position 只推进 partition 内连续完成位置；
- 第一版生产链路以 at-least-once、避免静默丢失为目标，长期演进到 checkpoint epoch；
- 多 Pod Kafka Source 早期使用 Kafka Consumer Group 协调 partition；
- yaspe Runtime 决定 safe position，Kafka Connector 执行 offset commit；
- 第一版目标是 record 级并行、单 record 内同步执行；
- 不通过无限队列、无限 goroutine 或提前提交 position 换取吞吐。

## 当前开放问题

- Collector context 最终采用显式传递还是调用级绑定；
- FailJob 时其他在途记录是 cancel、drain 还是按阶段处理；
- Record metadata 边界；
- 第一版 Source API；
- 第一版 Sink completion API；
- 全局 in-flight budget 与各局部队列容量的关系；
- Operator 实例是否允许被多个 Pipeline Worker 并发调用；
- 第一版线性 Job Definition。

## 下一步

1. 评审 `docs/designs/0001-core-execution-model.md` 已记录的近期决定和长期方向；
2. 从 Design 的问题四开始讨论 FailJob 与其他在途记录；
3. 继续讨论 Record metadata、Source/Sink API 和端到端容量预算；
4. 尚不要实现 Worker Pool、Kafka 或完整 DAG。

## 工具链

```text
Go language version: 1.27
Minimum toolchain: Go 1.27 stable
Current local toolchain: go1.27.0-X:nodwarf5 linux/amd64
```

`go.mod` 不固定具体 patch toolchain，由开发环境和 CI 使用 Go 1.27
或更新的兼容工具链。
