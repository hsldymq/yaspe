# 0006：Source 最终失败独立报告，Kafka 会话恢复由 Connector 限时管理

状态：Accepted

日期：2026-09-07

影响阶段：M1–M2（通用 Source 报告入口用于 M1+，Kafka 会话恢复用于 M2）

关联：[ADR-0005](0005-connector-owned-revoke-budget.md)。本决定补充会话恢复与最终失败
通知，不修改 revoke 预算或最终 offset 提交的失败策略。

## 背景

Runtime 因背压或暂停可能不再调用 Source TryRead。此时客户端仍可能发生权限错误、
会话恢复超时等最终失败，仅依赖读取返回 error 无法保证这些故障及时到达 Runtime。
另一方面，Lost 只说明旧 ownership 已失效，不足以决定客户端能否重新建立会话；立即
结束所有 lost Job 会失去恢复机会，无限跟随客户端重试又会掩盖持续故障。

## 决定

- Runtime 通过 SourceContext 向 Source 提供独立的最终失败报告能力，不受数据 admission
  阻塞；输入 error 表达 Source 根因，返回 error 表达报告是否被接收。
- Connector 区分暂时会话错误与最终失败；会话建立/恢复使用有限总预算，重复 retry 和
  backoff 不重置同次期限，成功处理 assignment 后结束计时，空 assignment 也可成功。
- Lost 立即隔离旧 ownership；恢复不复活旧 work/caches，不使用 Operator Retry，不恢复
  已经报告最终失败的 Source。最终 offset 提交仍遵守独立的有限重试和 FailJob 契约。
- 首个最终 Source failure 被保留，Reader 与独立报告入口不重复触发失败流程；全局
  RunError primary/secondary 继续按 Runtime 已接受的因果顺序确定。

完整接口与生命周期见 [Source Design §1.1.2](../designs/0003-source-reader-and-admission.md#112-独立的最终失败报告)；
Kafka 默认值、错误表、计时与信号边界见
[Kafka Design §3.6](../designs/0007-position-and-kafka-rebalance.md#36-会话建立与恢复)。

## 候选方案与取舍

- 仅从 TryRead 返回最终错误：依赖业务侧再次读取，无法覆盖持续背压或暂停。
- callback 内同步关闭 Runtime/客户端：生命周期可能互相等待，报告只登记事实，关闭
  由独立路径执行。
- 所有 lost 立即 FailJob：未区分会话失效与永久配置/权限错误，拒绝了可恢复情况。
- 无限重试或每次重试重新计时：没有一次故障过程的有限边界，宿主无法明确观察持续失败。
- 只看 Kafka Retriable 标记：请求重试与重新建立会话不同，必须同时识别类型与阶段。

## 后果

通用 Runtime 无需理解 Kafka 错误码或重入过程，Connector 无需等待数据容量来报告最终
失败。代价是独立失败入口与 Reader 错误的去重，以及客户端回调、超时、关闭之间的协调。
有限预算会结束超出窗口但可能稍后恢复的故障，默认值需要依据实际故障数据校准。

## 验证要求

验证报告与背压/取消/Close 的竞争、重复报告、原始根因保留，以及会话初次建立、重复
重试不刷新预算、空 assignment 成功、超时后的迟到成功。真实客户端版本的错误包装及
hook 顺序必须核验，不能用概念状态机替代实现证据。测试要求见
[Verification Design](../designs/0008-runtime-verification-and-observability.md)。

## 重新评估条件

- 客户端 API 不能可靠观察已接受的恢复成功或最终失败边界；
- 真实工作负载表明恢复预算或错误分类造成不可接受的误停或等待；
- 未来引入通用 Source/Job 恢复或 checkpoint 协议，需要重新划分恢复单位；
- 增加其他异步 Source 后，独立失败入口不能满足有界性或生命周期要求。
