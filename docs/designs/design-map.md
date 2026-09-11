# yaspe Design Map

最后更新：2026-09-11

本文是近期 Design 的导航和依赖地图。它维护每个 Design 的权威范围、设计状态与阅读顺序，
不复制完整契约，也不维护实现、验证、工作区或唯一下一步；这些动态事实由
[Current Status](../status.md) 统一维护。

## Design 目录

| Design | 权威范围 | 状态 | 主要依赖 |
|---|---|---|---|
| [0001 核心执行模型总览](0001-core-execution-model.md) | 共同术语、总体执行形态、资源不变量、阶段保证 | Accepted | — |
| [0002 Job Definition 与 Runtime 实例化](0002-job-definition-and-runtime-instantiation.md) | type-state API、Factory、Build、生命周期、复用、类型擦除 | Accepted | 0001 |
| [0003 Source Reader、Admission 与 Memory Source](0003-source-reader-and-admission.md) | Source lifecycle、最终失败报告、非阻塞读取、split control、position commit、reservation、Memory Source | Accepted | 0001、ADR-0006、ADR-0007 |
| [0004 Operator Attempt 与 Collector](0004-operator-attempt-and-collector.md) | Collector scope、Emit ownership、Chain、attempt 暂存 | Accepted | 0001、0002 |
| [0005 Sink Handoff 与 Completion](0005-sink-handoff-and-completion.md) | 整组交接、Memory Sink、异步 completion、capacity、有限关闭 | Accepted | 0001、0004 |
| [0006 Failure、Panic 与 Shutdown](0006-failure-panic-and-shutdown.md) | Operator Work Failure Policy、FailJob、RunError、panic、错误因果、shutdown | Accepted | 0001、0005 |
| [0007 Position、Ownership 与 Kafka Rebalance](0007-position-and-kafka-rebalance.md) | split/position、私有 Completion Tracker、generation fence、Kafka 分层预取/提交/rebalance/会话恢复/v1.21.6 基线 | Accepted（完整适配验证未完成，见 Status） | 0001、0003、0005、ADR-0005、ADR-0006、ADR-0007 |
| [0008 Runtime 验证与可观测性](0008-runtime-verification-and-observability.md) | 三类指标、确定性测试、故障矩阵、输出核对、分层证据、benchmark、审核清单 | Accepted（完整实现验证未完成，见 Status） | 全部近期执行契约 |
| [0009 ClickHouse Connector](0009-clickhouse-connector.md) | 一 item 一行的业务映射、多目标组批、初始配置、v2.48.0 Native batch、确认/重试/关闭 | Accepted（完整实现验证未完成，见 Status） | 0005、0006、ADR-0004、ADR-0008 |
| [0010 stdio 与优雅停止](0010-stdio-and-graceful-stop.md) | 自定义切分、EOF 尾部、stdio 失败策略、输入停止与输出 drain、信号边界 | Accepted（基本行为；API / I/O 细节待细化） | 0003、0005、0006、ADR-0009 |

`Accepted / details discussing` 或 `pending` 表示已接受的行为继续有效，但文件明确列出的
接口/策略问题或版本适配核验尚未完成。实现与验证是否完成不得从该标签推断。

## 依赖关系

```text
0001 Core Execution Model
├── 0002 Job Definition
│   └── 0004 Operator Attempt & Collector
│       └── 0005 Sink Handoff & Completion
│           └── 0006 Failure, Panic & Shutdown
├── 0003 Source Reader & Admission
│   └── 0007 Position & Kafka Rebalance
└──────────────┬───────────────────────
               └── 0008 Verification & Observability
```

0007 同时依赖 0003 的 Source ownership 和 0005 的 completion 事实；图中只画主路径，
不表示省略这些交叉依赖。
0009 是 0005/0006 的具体 Connector 消费者，验证要求回到 0008，局部驱动证据由附件保存。
0010 在通用 Source/Sink 与关闭契约上补充 stdio 和显式优雅停止，基本行为与待定 API 分开记录。

## 推荐阅读顺序

1. 先读 0001，建立 work、attempt、permit、completion、position 和 execution lane 的共同语言；
2. M1 Runtime 实现依次读 0002、0003、0004、0005、0006；
3. 测试和指标设计读 0008，并回到被验证行为的权威 Design；
4. M2 position、Kafka 和异步 Sink 工作再读 0007 及 0005 的 M2 部分，ClickHouse 读 0009；
5. 长期取舍背景从 [Decision Index](../decisions/README.md) 进入 ADR，而不是从 Design 猜测。

## 维护规则

- 一条完整行为规则只能有一个权威 Design；其他文档只摘要并链接；
- 跨多个能力的共同不变量留在 0001，能力局部契约进入对应 Design；
- 为什么选择某个长期方向由 ADR 记录，怎样执行由 Design 记录；
- Design 状态在本 Map 中汇总，Implementation、Verification 和当前断点只在 Status 中维护；
- 拆分或改名时必须同步 Architecture、Status、Roadmap、Decision Index 和相对链接；
- Design 内的参考代码若未明确声明为定稿 API，只用于说明契约，不锁定私有实现。
