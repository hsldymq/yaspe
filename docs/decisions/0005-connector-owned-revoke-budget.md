# 0005：保留 Consumer Group 协调，由 Connector 提供 revoke 时间预算

状态：Accepted

日期：2026-09-07

影响阶段：M2+

替代：[ADR-0002](0002-use-kafka-consumer-group-for-external-coordination.md)。保留其外部协调
方向，修改 revoke 时间预算归属并收紧 Kafka offset 提交方式；旧正文作为历史保存。

## 背景

yaspe 早期每个 Pod 运行独立 Runtime，尚无跨 Pod 的 Source Coordinator、lease 或状态
迁移。Kafka Consumer Group 继续承担成员管理与 partition ownership，Runtime 则根据
Operator/Sink 完成事实决定 safe position，Connector 执行 Kafka offset 提交。

Revoke 需要先由 Runtime 收敛已开始的工作，再由 Connector 提交最终安全位置。原决定的
默认 30 秒 drain 没有显式分离业务等待与后续提交预算；若传给 BeginRevoke 的 context
全部用于 drain，返回时 handle 可能已失效。额外增加一个始终被 Source 更早期限覆盖的
Runtime 上限，也会增加难以解释的重复配置。

## 决定

- 保留 Kafka Consumer Group 的跨实例 partition 协调，不在 M2 自建分布式控制平面。
- Kafka 禁用自动提交，只提交 Runtime 连续成功前缀对应的安全位置；ownership 与完成
  跟踪仍按 scope/generation 隔离，Kafka commit 不替代 Sink completion。
- Connector 提供整个 revoke 的有限 deadline 和提交/收尾预留时长，Runtime 从总期限中
  扣除预留时长执行有限 drain，不再设置独立的 revoke drain 最长时长。
- drain 结束时冻结安全位置并返回 handle，Connector 提交后报告 Complete；handle 的
  有效期覆盖提交阶段，总期限到期则 fence。drain 到期不等于总协议到期。
- 非空 revoke 暂停该 Source 全部新 admission，只收尾被撤销 splits；retained ownership
  和有界缓存保留。Lost 立即 fence、不 drain、不提交，是否 FailJob 另按错误性质决定。
- Kafka 客户端采用 franz-go 为首选候选；版本、资源上界、实际可用期限与恢复错误分类
  尚需验证，不据此宣称完整 Connector 已设计或实现完成。

公开接口与时间边界只在 [Source Design §1.3.1](../designs/0003-source-reader-and-admission.md#131-split-control-边界)
维护；Kafka 配置、提交排序与客户端适配只在 [Kafka Design §3](../designs/0007-position-and-kafka-rebalance.md#3-kafka-客户端适配)
维护。本 ADR 不复制默认值和完整操作规则。

## 候选方案与取舍

- **自建 Source Coordinator**：需要跨实例协调、故障检测、fencing 和恢复能力，超出 M2；
  独立 Consumer Group 按 Pod 广播也不能替代同 group 的分区扩容。
- **自动提交客户端已读取位置**：不能反映 Runtime/Sink 完成，可能跳过未处理记录。旧 ADR
  允许的“受约束自动提交”也不作为第一版适配路径，明确手动提交以缩小排序和验证范围。
- **独立 Runtime drain 上限与 Connector 总预算并存**：只有应用需要比 Source 更早停止
  业务等待时才有独立价值；当前无此要求，选择单一收尾预算减少重复配置。
- **在 drain 时长之外追加提交时间**：可能越过外部总期限，采用总期限内预留。
- **用更早的 context deadline 同时控制 drain 和 handle**：会让 handle 在提交前失效。
  context value 可携带第二种期限，但隐式控制参数难以发现与验证，因此使用显式 options。

## 后果

Connector 负责外部期限与提交预留，Runtime 专注通用 drain/completion。Source 不需要
读取 Runtime 配置，不支持动态 ownership 的 Source 不承担 revoke 参数处理。

代价是支持 revoke 的 Connector 必须给出有限预算及合理预留；预留过大缩短业务收尾、
可能增加重放，过小可能使最终提交无法及时确认。franz-go 回调并不直接暴露 broker 的
实际截止时间，预算前提必须继续核验。未知写入或提交结果仍可能产生重复，不能承诺
任意 Sink 的 exactly-once，也不能把本地 fence 当作撤销外部请求。

## 验证要求

确定性验证两种截止时间、预算不足、提前完成、总取消、迟到 Complete，以及普通提交与
revoke/lost 的排序。验证 eager/cooperative、空回调、retained splits、同 partition 重新
分配和慢 Sink 下控制路径可推进。完整矩阵见
[Verification Design §1.4](../designs/0008-runtime-verification-and-observability.md#14-position-与-ownership)。

## 重新评估条件

- 真实场景需要独立于 Connector 总预算的 Runtime 业务等待上限；
- 客户端无法满足资源有界、有限收尾、旧请求隔离或控制推进的契约；
- 默认预算在故障/生产 workload 下造成不可接受的延迟或重复；
- yaspe 开始跨 Pod 有状态执行，拥有 Source Coordinator、checkpoint 或状态迁移能力，
  partition ownership 需要与状态归属统一。
