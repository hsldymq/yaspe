# yaspe Agent Instructions

本仓库把文档作为跨会话、跨人员和跨模型的项目记忆。不要只依赖聊天历史判断项目状态。

## 开始工作前

1. 完整阅读 [docs/README.md](docs/README.md)；
2. 完整阅读 [docs/status.md](docs/status.md)；
3. 阅读当前 Roadmap 里程碑、Status 指向的正式 Design 和相关 ADR；
4. 检查 `git status`、最近提交、当前代码和测试；
5. 确认当前里程碑、设计状态、实现状态、验证状态、唯一下一步和未提交修改。

如果文档、代码、测试或工作区状态互相冲突，先报告并修正事实来源，不要静默选择一个版本继续。

## 工作规则

- Vision 管长期目标，Roadmap 管能力顺序，Architecture 管跨阶段结构，ADR 管长期取舍，Design 管具体契约，Status 管当前断点，代码和测试证明已实现行为；
- `Accepted` Design 不等于 `Implemented`，`Implemented` 不等于 `Verified`；
- 不跳过 Status 中尚未收敛的前置决策直接实现受其影响的能力；
- 讨论结论只有在用户明确确认并明确要求更新文档后才能写入项目记忆；局部同意、倾向或追问
  不得视为定稿，完整门槛见 [Documentation Governance §4.1](docs/governance.md#41-用户确认与文档更新门槛)；
- 文档记录长期有效的架构、契约、考量、取舍、实现规划和验证证据，不记录会话过程或修改
  流水；避免流水账不等于省略必要理由，详见 [Documentation Governance §2.8](docs/governance.md#28-持久知识而非讨论流水)；
- 不在多个文档中维护同一规则的完整副本，摘要必须链接权威位置；
- 不悄悄改写已接受 ADR 的历史；改变决定时新增替代记录并标记 `Superseded`；
- 不把未来规划、概念 API 或候选结构描述成当前实现；
- 保留用户已有的未提交修改，并先理解与当前任务重叠的 diff。

## 经用户确认并要求落盘后

满足 [Documentation Governance §4.1](docs/governance.md#41-用户确认与文档更新门槛) 后，
结束当前工作前：

1. 更新权威 Design；
2. 按影响更新 ADR、Architecture 或 Roadmap；
3. 更新 `docs/status.md` 的三维状态、开放问题、唯一下一步和验证记录；
4. 检查旧术语、重复规则和文档冲突；
5. 运行 `git diff --check` 和与改动相称的测试；
6. 报告验证结果与未提交文件。

完整规范见 [docs/governance.md](docs/governance.md)。
