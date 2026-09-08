# 0008：ClickHouse 目标与写入策略归业务，Connector 按实际配置确认结果

状态：Accepted
日期：2026-09-08
影响阶段：M2+
关联：[ADR-0004](0004-keep-sink-retry-inside-connector.md)

## 背景

ClickHouse INSERT 的确认边界取决于目标表、转发、副本及异步插入设置。客户端收到
成功响应不自动意味着所有 shard/副本已经达到同一持久化状态。yaspe 需要定义正确的
completion，同时保持业务部署策略与通用 Runtime 分离。

## 决定

- 目标表、列映射及写入设置由业务提供；Connector 不自动识别表引擎来选择路由，也不
  强制本地表/Distributed 或某种前台转发模式，不擅自覆盖业务 async_insert 设置。
- Connector 在本地组批，通过后台完整 INSERT 等待实际配置下的服务端结果，再报告
  对应 item 的最终 outcome；Runtime 只消费 completion，不理解表引擎或 ClickHouse 错误码。
- 成功声明限于实际写入配置的确认边界，不能将弱接收确认描述成所有 shard/副本已
  持久化。端到端交付保证必须列出业务配置前提与观察边界。
- Sink 内部有限重试和 Unknown 的重复风险沿用 ADR-0004；ClickHouse 的具体初始预算、
  稳定 batch 重建及错误映射在专项 Design 中维护，不增加 Runtime Sink Retry。

完整契约、驱动基线与未决接口见
[ClickHouse Design](../designs/0009-clickhouse-connector.md)。

## 候选方案与取舍

- 框架自动探测表引擎并强制 Distributed 前台写入：把部署策略固化进 Connector，仍无法
  自动覆盖副本、持久化和任意表引擎的全部语义；当前不采用。
- 框架统一覆盖服务端异步插入配置：会改变业务明确选择的写入策略，当前不采用。
- 任意成功响应统一宣称端到端持久化成功：隐藏了实际确认边界，不采用。
- 业务配置决定语义、Connector 准确报告事实：采用；需要业务明确配置前提，并据此
  验证交付保证，不能只靠 Connector 名称推断可靠性。

## 后果与验证

Connector 可用于不同业务表与部署形态，Runtime 保持系统无关。代价是使用方必须理解
并声明写入设置对确认的影响；不具备持久确认的配置不能获得无条件 at-least-once 声明。
超时或取消无法撤销外部效果，有限重试可能重复；不依赖任意表默认具有去重能力。

验证完整 INSERT 成功后才报告、NotApplied 与 Unknown 的阶段证据、同 batch 稳定重试、
关闭时不把驱动 Close 误作发送，以及实际服务端配置对应的观察结果。驱动级探针不能
替代服务端、连接池竞争、重试预算和业务交付保证验证。

## 重新评估条件

- 业务需要经过认证的固定写入模式或 capability 配置，明确约束可用设置；
- checkpoint/Sink commit protocol 改变确认与重放单位；
- 实际驱动或服务器行为不能支撑专项 Design 的有限关闭与结果分类；
- 多个 Connector 证明需要共享的组批/重试设施，且不改变当前责任边界。
