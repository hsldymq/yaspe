# yaspe Current Status

最后更新：2026-08-09  
当前里程碑：M0 — 核心语义与项目基线

## 新会话阅读顺序

1. 本文件；
2. [Living Architecture](architecture.md)；
3. [Roadmap](roadmap.md) 当前里程碑；
4. 当前阶段 Design（尚未创建）；
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
└── Map[I, O]
```

当前已有 Map 测试覆盖：

- 正常转换；
- transform 失败时零输出；
- Emit 失败向上传播；
- context 传递给 transform。

## 已接受方向

- Operator 描述计算，Runtime 控制执行；
- Source 数据进入受 Runtime 有界容量和背压控制；
- Connector 适配外部系统的物理 pull/push 模型；
- Collector 的 Emit 当前显式接收 context；
- 多 Pod Kafka Source 早期使用 Kafka Consumer Group 协调 partition；
- yaspe Runtime 决定 safe position，Kafka Connector 执行 offset commit；
- 第一版目标是 record 级并行、单 record 内同步执行；
- 不通过无限队列、无限 goroutine 或提前提交 position 换取吞吐。

## 当前开放问题

- Collector context 最终采用显式传递还是调用级绑定；
- Collector 的准确生命周期和并发约束；
- Record metadata 边界；
- 第一版 Source API；
- 第一版线性 Job Definition；
- Filter 和 FlatMap 如何验证现有 Operator/Collector 契约；
- FailJob 时其他在途任务如何处理。

## 下一步

1. 重新暂存并检查 `git diff --cached`；
2. 起草 `docs/designs/0001-core-execution-model.md`；
3. 评审 Filter、FlatMap 与当前契约的适配性；
4. 尚不要实现 Worker Pool、Kafka 或完整 DAG。

## 工具链

```text
Go language version: 1.27
Current toolchain: go1.27rc2
```

Go 1.27 正式版发布后，应移除 RC toolchain 固定并更新本文件。

## 工作区提醒

当前 Git 状态曾显示核心 Go 文件为 `AM`，表示暂存区版本落后于工作区版本。提交前必须重新 `git add` 并检查 `git diff --cached`，避免提交空壳文件。
