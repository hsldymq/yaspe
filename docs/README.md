# yaspe Documentation

本文是 yaspe 文档和新会话的统一入口。项目通过仓库中的文档、代码和测试保存连续上下文。

## 新会话阅读顺序

1. [Current Status](status.md)：恢复当前里程碑、实现状态、开放问题和唯一下一步；
2. [Roadmap](roadmap.md) 当前里程碑：确认范围、非范围和完成标准；
3. [Living Architecture](architecture.md) 相关章节：恢复跨阶段职责、边界和不变量；
4. Status 指向的 [Design](designs/)：恢复当前能力的正式执行契约；
5. 相关 [ADR](decisions/)：恢复重要决定的背景、取舍和重新评估条件；
6. 当前代码、测试、`git status` 和最近提交：核对仓库现实。

开始实际工作前，应能准确说明：

- 当前里程碑；
- 当前设计、实现和验证状态；
- 已接受但尚未实现的能力；
- 当前唯一下一步；
- 工作区是否有未提交修改或事实冲突。

## 文档地图

| 问题 | 权威载体 |
|---|---|
| 为什么创建 yaspe、长期成功是什么 | [Vision](vision.md) |
| 能力按什么顺序演进 | [Roadmap](roadmap.md) |
| 系统长期由什么组成、职责如何划分 | [Living Architecture](architecture.md) |
| 为什么作出一个长期取舍 | [ADR 与决策索引](decisions/README.md) |
| 某项能力具体如何工作 | [Design](designs/) |
| 当前做到哪里、下一步是什么 | [Current Status](status.md) |
| 实际已经实现并验证了什么 | 代码和自动化测试 |
| 文档如何维护和接力 | [Documentation Governance](governance.md) |

## 事实冲突处理

- 已实现行为以代码和测试为证据；若与 Accepted Design 冲突，必须报告并决定修实现还是修设计；
- 预期行为以 Accepted ADR 和 Design 为准；
- 当前工作顺序以 Status 为准；
- 长期方向与里程碑边界以 Vision、Roadmap 和 Architecture 为准；
- 未提交 diff 和最近提交可能比 Status 更新，新会话必须交叉验证。

不要通过删除历史来消除冲突，也不要把 `Accepted`、`Implemented` 和 `Verified` 混为一谈。
