# 0002：早期多实例 Kafka Source 使用 Consumer Group 协调

状态：Accepted  
日期：2026-08-01

## 背景

`lightning-log-filter` 在 Kubernetes 中通过多个 Pod 运行。yaspe 引入后，每个 Pod 将运行一个独立的单进程 Runtime，多个实例需要协调 Kafka partition 的 ownership，并在扩缩容、rolling update、进程退出和故障时重新分配 partition。

yaspe 当前没有跨实例 Job Manager、Source Coordinator、成员管理、lease、fencing 或状态迁移能力。自行分配 Kafka partition 等同于过早建立一套分布式控制平面。

同时，Kafka Consumer Group 只能知道 consumer 读取和提交的位置，不能理解一条记录是否已经完成全部 Operator 和 Sink 副作用。因此不能把处理完成的判断交给 Kafka 自动提交机制。

## 决定

在 yaspe 尚未拥有分布式 Source Coordinator 时，多 Pod Kafka Source 使用 Kafka Consumer Group 管理成员和 partition ownership。

职责划分如下：

```text
Kafka Consumer Group
    - group membership
    - heartbeat/session
    - partition assignment/revocation
    - committed offset storage

yaspe Runtime
    - record 完成状态
    - partition 内连续完成位置
    - Fail/Skip/Retry/Dead Letter 终态
    - 背压、取消和在途任务管理

Kafka Connector
    - poll Kafka
    - 将 partition lifecycle 转换为 Runtime 事件
    - 将 safe position 转换为 Kafka offset commit
```

Kafka Connector 不得根据最新读取位置提前提交 offset。自动提交必须禁用，或者被配置为只提交由 Runtime 明确确认的安全位置。

每次 partition ownership 应具有可区分的 generation/epoch。partition 被 revoke 后，旧 ownership 下迟到完成的任务不得推进当前 committed position。

## 原因

- 复用 Kafka 已有的成员管理、故障检测和 partition rebalance；
- 避免早期 yaspe 重复实现分布式协调；
- 满足 `lightning-log-filter` 无状态、多 Pod ETL 的近期需求；
- 保留 Runtime 对记录完成和正确提交时机的控制；
- 允许通过增加 Pod 和 Kafka partition 扩展整体吞吐。

## 后果

### 正面影响

- 多个 Pod 不需要直接互相通信；
- Kafka 负责外部工作分配，yaspe 仍保持单进程引擎定位；
- Pod 故障和扩缩容可以利用成熟的 Consumer Group 协议；
- committed offset 可被 Kafka 生态监控。

### 代价和风险

- rebalance 可能导致短暂停顿和重复处理；
- revoke 时必须协调该 partition 的读取、在途任务和 Sink batch；
- context cancel 不能撤销已经发生的外部副作用；
- Pod 内 Worker 并行完成后，offset 仍必须按 partition 连续推进；
- Kafka partition 数量限制有效的 Pod 级消费并行度；
- Sink 必须承受多个 Pod 的并发写入，并避免产生过多小 batch；
- `FailJob` 在没有控制平面时仅能终止当前 Pod 内的 Runtime。

## 不采用的方案

### yaspe 自行分配 Kafka partition

当前不采用。该方案需要成员发现、lease、fencing、故障检测、脑裂处理、position 持久化和 ownership 转移，超出早期引擎范围。

### 完全依赖 Kafka 自动提交

不采用。自动提交可能根据已读取而非已完成位置推进 offset，导致进程失败时丢失仍在处理或尚未落库的数据。

### 每个 Pod 使用不同 Consumer Group

不作为扩容方式。这样每个 Pod 都会消费全部数据，除非业务明确要求广播语义，否则会产生重复处理。

## 验证要求

- 多 Pod 使用同一 Consumer Group 时覆盖全部 partition；
- 扩容、缩容和 rolling update 不产生超出已声明保证的数据缺失；
- partition revoke 后旧 generation 不得提交 position；
- rebalance 时存在在途记录和待 flush batch 的场景有故障测试；
- 队列饱和和慢 Sink 不会持续触发非预期 session 失效；
- 强制终止 Pod 后，新 owner 能从已提交位置继续；
- 重复处理的可能位置可以观测和解释；
- partition 数与 Pod 数的容量关系有部署文档。

## 与 Flink 的区别

现代 Flink 已拥有分布式 Source Coordinator、Source Split、checkpoint 和状态重分配能力，因此可以由 Flink Runtime 发现并向 SourceReader 分配 Kafka partitions；Kafka `group.id` 仍可用于读取或提交 committed offsets 等用途。

yaspe 当前缺少这些分布式基础设施，所以使用 Kafka Consumer Group 是符合当前阶段能力的选择，而不是永久规定。

## 重新评估条件

- yaspe 开始支持跨 Pod 的有状态执行；
- yaspe 拥有 Job Manager、Source Coordinator 或等价控制平面；
- Kafka partition ownership 必须与 keyed state ownership 统一；
- yaspe 需要主动控制 rescale 和 Source split reassignment；
- Consumer Group rebalance 无法满足 checkpoint 或恢复协议；
- 真实运行数据证明该模型无法满足可用性或性能目标。
