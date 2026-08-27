# 0004：Sink Retry 留在 Connector 内部

状态：Accepted  
日期：2026-08-27  
影响阶段：M2–M6

## 背景

异步 Sink 接管 terminal outputs 后，外部请求可能部分成功、明确未生效或结果未知。yaspe 需要
决定由 Runtime 重新提交失败 item，还是由 Sink 在报告最终 completion 前自行恢复。

## 约束

- Sink 接管后 Runtime 不能回滚已经产生的外部效果；
- 部分成功必须保留，Operator Chain 不能因为 Sink 失败而重新执行；
- schema、业务数据、外部协议和错误是否值得重试由使用方及具体 Connector 理解；
- M2 尚无 checkpoint 驱动的自动 Runtime 恢复；
- 所有 buffer、请求、timer、goroutine 和关闭等待必须有界。

## 候选方案

1. Runtime 根据 item outcome 管理独立的 Sink effect Retry、预算和 backoff；
2. Sink Connector 在内部决定并执行 Retry，只向 Runtime 报告最终 outcome；
3. M2 完全不做运行期 Sink Retry，首次外部失败立即 FailJob。

## 决定

采用方案 2。M2 Runtime 不提供 Sink effect Retry，不解析 Sink error，也不重新提交失败 item。
Sink 接管 items 后自行决定是否 Retry，并在内部恢复完成、放弃或预算耗尽后报告最终的
`SinkSucceeded`、`SinkNotApplied` 或 `SinkUnknown`。

Runtime 收到首个最终 `SinkNotApplied` 或 `SinkUnknown` 后立即 FailJob，同时在统一 shutdown
deadline 内有限 drain 其他已经接管的 item。Job 的 FailJob/Retry 配置只适用于 Operator work
attempt，不适用于 Sink、Source 或 Runtime 内部错误。

## 原因

具体 Connector 最接近外部协议，也最有能力判断怎样重试物理请求、batch 或 item。把该判断放入
Runtime 会迫使通用 API 理解业务数据、schema 和外部错误分类，并引入第二套 effect attempt、
预算、backoff 与重新接管协议。当前阶段没有 checkpoint，Runtime 即使接住失败也缺少安全自动
恢复整个 Run 的状态基础。

## 后果

- Runtime completion 只消费最终事实，核心 API 不增加 retryable/permanent 分类；
- Connector 内部 Retry 策略可以不同，但必须有界并响应关闭 deadline；
- Sink 最终失败会终止当前 Run，由宿主决定是否重新创建 Runtime；
- `SinkUnknown` 或尚未持久化 position 的成功 effect 在后续重放时可能重复，M2 只声明边界明确的
  at-least-once；
- 未来可以提供 Connector 层的通用异步 Sink 辅助设施，但不能把其内部 Retry 冒充 Runtime
  work Retry。

## 验证要求

- 证明 Sink 内部 Retry 的 buffer、timer、请求和 goroutine 有界；
- 证明中间失败不会被 Runtime 误当成最终 completion；
- 证明第一个最终失败立即触发 FailJob，后续已接管 item 仍有限收敛；
- 证明部分成功不会回滚、失败 work 不推进 position；
- 证明 Operator Retry 配置不会捕获或重试 Sink 最终失败。

## 重新评估条件

M6 引入 checkpoint 自动恢复、M9 引入 Sink commit protocol，或多个 Connector 证明需要共享的
异步 Sink 基础设施时，重新评估恢复单位和框架/Connector 职责；改变本决定时新增替代 ADR。
