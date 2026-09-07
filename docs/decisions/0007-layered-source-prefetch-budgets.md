# 0007：Source 预取按层计数，Kafka 第一版不新增字节限制

状态：Accepted

日期：2026-09-07

影响阶段：M1–M2（通用 Source admission 方向保留，Kafka 分层预取用于 M2）

替代：[ADR-0001](0001-runtime-controlled-source-ingestion.md)。保留其职责边界，修改统一
预取记录数及进程内存有界的表述；旧正文保留为历史。

## 背景

Kafka 客户端内部 fetch 与 Connector 已取出记录是两层不同的缓冲。一份 fetch 可包含
多个 partition、多个批次和很多记录；PollRecords 的返回数量不等于客户端已持有的
记录数量。压缩批次展开也使网络响应大小无法直接表示解压后的内存占用。

第一版目标是让背压停止积压继续扩大，并接受大小可变的消息。为了获得更强的统一
记录数或字节上限而新增响应大小拒收和受限解压，会引入额外配置与客户端适配复杂度。

## 决定

- 外部物理 pull/push 仍由 Connector 适配；Runtime 控制 admission 和 work permit，
  Operator 不感知客户端，读取不等于处理完成，安全 position 仍由 Runtime 决定。
- Source 各层声明有限容量及计数单位。Kafka 客户端内部以 fetch 数量限制在途和缓冲
  结果，Connector 已取出的数据以记录数限制，Runtime 继续以 work 数限制。
- Connector 按预留容量分批 poll；满时停止新 poll 并暂停 fetch，已有在途或缓冲结果
  继续受客户端计数约束。session/control 及最终失败报告仍须独立推进。
- Kafka 第一版不新增 yaspe 消息、响应或解压字节限制，保留客户端原有配置和保护机制。
  客户端返回的真实错误继续按既定失败契约处理，不静默丢弃或无限放宽保护。
- 不承诺整个 Kafka Source 的固定记录数或内存字节上限；不同层的计数不能直接相加。
  Memory Source 的严格记录容量及 Runtime MaxInFlightWorks 不因此放宽。

完整 Kafka 行为与保证范围见 [Kafka Design §3.2](../designs/0007-position-and-kafka-rebalance.md#32-分层缓存与背压)，
通用 Source 边界见 [Source Design §1.4](../designs/0003-source-reader-and-admission.md#14-source-admission-与所有权)。

## 候选方案与取舍

- 只限制 PollRecords 数量：只能约束进入 Connector 的部分，未覆盖客户端内部积压。
- 对整个 Source 强制统一记录上限：客户端先取整份结果再分批交付，难以直接用相同
  记录 permit 约束其内部解析；第一版选择显式区分 fetch 和记录单位。
- 新增响应及批次解压字节上限：可提供更强输入约束，但需处理额外配置、压缩格式、
  边界拒收及大批次失败，当前暂不采用。
- 降低 fetch 并发来替代 Connector 容量：客户端可连续完成多次 fetch 并填充较大
  Connector 缓冲，降低并发不能消除后者允许积累的记录，因此两层均需独立约束。

## 后果

缓冲小会更早产生背压，缓冲大可以吸收更多突发；在合法配置和内存足够时，两者不改变
处理正确性，记录就绪即可交接，不等待缓存填满。Connector 无需装下完整一次 fetch。

大消息、多记录压缩批次、共享底层缓冲及业务转换仍可能造成较高内存峰值；小缓冲或
有限 fetch 数也不能保证进程不会内存不足。不新增 yaspe 大小限制，不等于绕过客户端
保护或承诺任意大小消息可正常消费。实际内存表现须由 workload 测量解释。

## 验证要求

覆盖单 fetch 大于 Connector 剩余容量、跨多次 fetch 填满大缓存、满时 pause 与在途结果
竞争、恢复后继续消费、assignment 变化及 session/control 推进。使用大消息、压缩批次
和慢 Sink 记录资源表现，验证每层计数而非声称固定内存上限。客户端版本仍需核验，测试
矩阵见 [Verification Design](../designs/0008-runtime-verification-and-observability.md)。

## 重新评估条件

- 实际 workload 的大消息/压缩批次造成无法接受的内存峰值；
- 部署需要硬内存预算或明确的大消息拒收策略；
- 客户端无法满足 fetch 计数、部分 poll 或背压控制契约；
- 未来客户端支持直接的记录 credit，或 checkpoint/分布式协调要求改变 Source 交接方式。
