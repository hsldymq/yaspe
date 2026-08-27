# 0008：Runtime 验证与可观测性

状态：M1 Accepted / M2 Discussing
最后更新：2026-08-27
适用阶段：M1–M2
依赖：全部近期执行契约；见 [Design Map](design-map.md)

本文集中维护指标、确定性测试、race/leak、fault injection、benchmark、开放问题和实现前审核。各行为规则仍以对应能力 Design 为权威来源。

## 1. 验证要求

### 1.1 Source 与背压

- 慢 Sink 最终阻止 Source 继续扩大读取或预取；
- 队列和 Sink 饱和时，work/item 数量和 goroutine 保持有界；第一版不保证字节数有界；
- Connector 内部预取数量可配置或有明确上限；字节上限属于后续增强；
- callback/push Source 也能通过有界适配层响应 admission；
- Kafka Connector 在业务回压期间仍满足 heartbeat/session 生命周期。
- 确定性竞态测试必须把数据发布分别注入到 `TryRead` 返回 `unavailable` 之前、之后以及
  Runtime 实际开始等待通知的两侧，证明不会丢失唤醒；
- 测试覆盖旧通知已占满 channel、Runtime 并发消费旧通知、通知合并、伪唤醒，以及
  end/failure/close 在等待窗口内发生的情形；
- 测试不得依赖调度器时序或 `time.Sleep` 碰撞竞态，必须用可控 hook/barrier 精确重现每个
  check-then-wait 交错。
- Memory Source 测试覆盖满缓冲 Submit 的背压和取消、Submit/Finish 线性化、finishing drain、
  Fail 优先级、首根因保留、Close 唤醒等待者与 Source-owned 缓存丢弃；
- Reader API 测试覆盖稳定且永不关闭的容量 1 Available channel、先发布状态后通知、终态唤醒、
  stale/coalesced notification，以及 Open 内同步 assignment 与 Close 后迟到 control call；
- split control 测试覆盖 Assign/BeginRevoke/Lost 的合法状态机、批量全验证、空/重复/未知 split、
  generation 只由 Runtime 创建、revoke handle 恰好一次 Complete、deadline 自动 fence 和迟到
  Complete 不提交 position；
- 用 barrier 交错 Connector 本地 readable 线性化、TryRead ready 返回与 assign/revoke/lost，证明
  ready 后必先绑定、control 后不再交付旧 ownership，并且 lost 丢弃未交接缓存；
- positioned Source capability 测试覆盖 positioned/unpositioned 混用、空 Split/nil Position、
  未实现 PositionCommitter 却返回 positioned ready，以及 CommitPositions 必须同步确认持久化；
- 有限 Memory Source 的端到端测试必须证明 `Finish` 返回不会提前终止 Runtime，`Run` 只在
  已缓存和已接纳 work 及 Sink 都按契约收敛后返回 `nil`。
- admission 竞态测试必须把 context 取消分别注入到 reservation 之前、预留之后/读取之前、
  ready 返回与绑定之间、绑定之后/调度之前，证明只有空 reservation 可直接释放，
  ready 记录必须先登记再取消；
- 队列与 Worker 饱和测试必须证明 ready 后的绑定不再等待或失败，work 可在有界 pending
  状态中受追踪等待调度；
- 不可重放 Memory Source 的取消测试必须把未处理记录显式归类为 cancelled/未完成，不得
  把它误报为 success 或声称可恢复。

### 1.2 取消与资源回收

- context 取消可以解除所有 Runtime 管理的等待路径；
- Connector 阻塞 I/O 能被取消或通过受控关闭结束；
- Runtime 退出后不存在 Source、Worker、Sink 或 retry goroutine 泄漏；
- deadline 到期的未知 Sink operation 不被错误标记成功。
- `Close` 返回后 Connector 自身可控的 goroutine、timer、队列和 callback 注册已经有界收敛；
  外部客户端迟到调用只命中失效的轻量 reporter/notifier，不保留完整 Runtime 协调状态；

### 1.3 attempt、Sink 与 completion

- attempt 失败不会把 terminal output 部分交给 Sink；
- Emit ownership 契约测试覆盖成功后发送方不得复用、失败后仍可复用，以及内置 Operator 不在
  成功 Emit 后修改输出；测试不得宣称能够检测所有违反契约的用户代码；
- Sink 整组交接不会发生部分责任转移；
- Accept table test 覆盖 Accepted/Backpressured/error/invalid status、context 取消竞态、items slice
  ownership 转移，以及零输出不调用 Accept；
- pending-accept 测试覆盖同步 callback 后 Accepted、Backpressured 和 error；未接管却报告结果
  必须 FailJob且不得重试 Accept；
- reporter 契约测试覆盖 invalid outcome、错误 outcome/error 组合、相同重复、矛盾结果、外来
  item、并发 Report、result slice 转移、每 reporter 单 pending wakeup 和 active/fenced 差异；
- capacity 测试枚举 Notify 发生在 Accept 前、调用中、Backpressured 返回后和实际 wait 两侧，
  证明 versioned signal 不丢唤醒、不忙轮询且 fence 后 no-op；
- 乱序、迟到和重复 callback 不会重复终结或释放 permit；
- 零输出 work 能直接完成；
- 多输出 work 只在全部必要 effect 完成后终结；
- Sink 入队、外部完成、输入终结和 position 提交可以分别观测和测试。
- Sink 内部 Retry 的中间失败不会形成 Runtime completion；最终 `NotApplied/Unknown` 的第一个
  报告立即触发 FailJob，Operator Retry 配置不得捕获或重新提交该 effect；
- Sink 内部待重试项、timer、请求和 goroutine 保持有界，并响应 lifecycle cancel 与 Close
  deadline；
- 所有非正常 `Run` 都返回 `*RunError`；测试覆盖单错误、多 active failure、多个 secondary、
  context 取消先触发、正常处理后 Close 首次失败和并发终止事件的 primary 线性化；
- `RunError` 查询方法返回稳定副本，`errors.Is/As` 可识别 primary、active first/last 和每个
  secondary error，迟到 callback 不得改变已经返回的快照；
- 关闭测试覆盖已接管 buffer、外部 in-flight、deadline 前部分成功、可证明未生效、结果未知、
  missing result、fence 前后并发 callback，以及 `Close` nil/error 都不能替代逐 item completion；
- shutdown deadline 测试证明 Source、Operator、Sink 和 goroutine 回收共享同一绝对 deadline，
  前序步骤消耗的时间不会在 Sink Close 时重新补足；
- Memory Sink 单元测试覆盖整组成功、固定失败计划下的全组拒绝、零输出不调用 Sink、
  group 顺序与扁平视图，以及快照 slice 与内部 slice 结构隔离；
- Memory Sink 并发测试使用内部 hook/barrier 精确控制接管线性化前后、接管与 Close 竞争、
  失败触发 FailJob 时已进入与尚未进入的调用；不使用 `time.Sleep` 碰撞时序；
- `go test -race` 必须覆盖 Accept、运行期快照与 Close 并发。Close 返回后快照必须稳定，已成功
  groups 不得因其他 work 失败而回滚。测试 barrier 只属于内部测试设施，不进入公开 Sink API。

### 1.4 position 与 ownership

- 较大 position 先完成时不会越过前序空洞；
- generation 失效后任何迟到路径都被 fence；
- revoke 收尾有期限，不会无限阻塞；
- 旧 owner 不会为新 owner 提交迟到位置。

### 1.5 可测试边界

- Operator 可不依赖真实 Source/Sink 独立测试；
- Connector 可用假的 Runtime admission boundary 测试；
- Runtime 可用 Memory Reader/Sink 和可控时钟测试；
- 慢 Sink、结果未知、重复 callback、取消和 rebalance 可以确定性注入。

### 1.6 M1 指标记录能力

M1 先建立指标记录能力，但不在 Runtime 与 M2 尚未实现时预先冻结完整指标集合或公开 Metrics
API。完整指标名称、标签和 adapter 在 M1、M2 具备真实实现经验后统一审核；当前只固定以下语义：

- M1 唯一必需指标是成功完成的 work 累计数，外部观察者通过相邻采样点的增量和时间差计算
  单位时间吞吐量；Runtime 不维护 QPS、滑动窗口或采样周期；
- 计数发生在 work 成功终态完成线性化之后，并且同一 work 最多增加一次；正常零输出同样计数，
  失败、取消、未完成和 unknown 不计数；M2 引入 Retry 后，多个 attempt 仍只能形成一次 work
  完成计数；
- 记录点由 Runtime 根据 completion 事实触发，Source、Operator 和 Sink 不自行推断 work 是否
  完成；
- recorder/observer 由 Runtime option 注入，未配置时使用 no-op；记录调用必须并发安全、快速、
  不阻塞，不在执行路径中进行网络 I/O；
- 指标记录失败不得改变 Job 结果、触发 FailJob 或掩盖原始错误。M1 可以先使用私有 recorder
  隔离内部调用；公开类型名、instrument 形态和 Prometheus 等 adapter 不在本阶段定稿。

### 1.7 确定性并发测试与 goroutine 回收

并发正确性测试采用由外到内的可控边界，不把概率碰撞当作正确性证据：

1. 优先用 fake Source、Sink 和 Operator 在真实组件调用边界暂停、返回或完成操作；
2. 用 channel/future、barrier、可控 clock 或可手动推进的 executor 精确安排异步顺序；
3. 只有组件边界无法观察的 Runtime 内部线性化窗口才设置私有 test hook；hook 只负责观测和
   暂停，不修改 Runtime 状态，也不进入公开 API；
4. 关键竞态测试不使用 `time.Sleep` 猜测调度时序；所有等待必须有测试超时，并在失败时暴露
   所处阶段；随机或重复压力测试只作补充，不能替代已枚举交错的确定性测试。

`go test -race ./...` 必须覆盖正常执行、失败、取消、Source/Sink 并发与关闭路径，确定性竞态
测试本身也必须能在 race detector 下运行。Runtime 创建的每个 goroutine 都必须进入内部
execution group 或等价的结构化追踪机制，`Run` 返回前等待这些 goroutine 退出；实现不能只靠
测试前后比较整个进程的 goroutine 总数证明无泄漏。测试同时使用 goroutine leak detector 或
等价检查兜底，覆盖正常结束、Operator 失败、Sink 失败、外部取消、shutdown deadline 和
Source 等待通知时取消。外部客户端无法阻止的迟到 callback 按 Failure Design 的 fence 与状态
解耦规则处理，不计作仍由 Runtime 持有的 goroutine。

### 1.8 M1 benchmark

M1 benchmark 使用 Go 标准 `testing.B` 格式作为原始事实来源，通过 `ReportMetric` 补充 yaspe
的 work 级测量，并使用 `benchstat` 进行多轮统计比较；不建设独立报告格式或自制统计系统。

#### 1.8.1 Workload

第一版固定三类最小 workload：

1. Runtime overhead：`Memory Source -> identity Operator -> consuming Memory Sink`，每个小 Record
   产生一个输出，测量 admission、队列、类型擦除、Operator 调用、Sink 交接、completion 和
   必需 work-completed 记录路径的综合成本；Sink 必须实际消费结果，但 benchmark 不因永久保存
   全部输出而退化为内存增长测试；
2. CPU-bound：每个 work 执行固定、可重复且无外部依赖的纯计算，校验最终结果以防编译器消除，
   用于观察不同 Parallelism 的吞吐和扩展效率；
3. blocking-I/O simulation：每个 work 使用真实墙钟 `time.Sleep` 模拟明确标注的固定等待，
   用于观察并发隐藏等待、timer/scheduler 成本和 `MaxInFlightWorks` 的影响。它不代表具体生产
   Connector 的绝对性能。

`testing/synctest` 用于 timeout、deadline、backoff 和异步稳定状态等确定性正确性测试，不用于
声称真实 I/O 等待下的性能；虚拟时间测试可验证容量和行为，但其结果不得作为墙钟吞吐数据。

默认配置矩阵只覆盖 `Parallelism = 1 / GOMAXPROCS / 2*GOMAXPROCS` 与
`MaxInFlightWorks = Parallelism / 2*Parallelism / 8*Parallelism`。Runtime overhead 另测零输出
和固定有限多输出；较大 Record 作为独立诊断项，不与所有配置形成完整笛卡尔积。真实实现或
profiler 发现问题后再增加有解释价值的针对性 benchmark。

#### 1.8.2 测量与正确性

- benchmark 使用 `testing.B.Loop`，将一次性输入构造和 Job setup 排除在被测区间外；完整 Run
  benchmark 必须包含 Open、执行和 Close，若另设 steady-state benchmark，名称与报告必须明确
  它排除了哪些生命周期成本；
- 每轮处理固定批次 work，并按成功完成总数报告 `works/s`、`ns/work`、`B/work` 和
  `allocs/work`；Go 默认的 `B/op`、`allocs/op` 若以 batch 为一次 op，不得冒充 work 级结果；
- 延迟使用独立 benchmark，在 Runtime 接管 work 与成功终态线性化之间采样，预分配固定比例的
  采样存储，并报告 p50、p95 和 p99；采样机制及其开销必须在报告中说明；
- 另行报告或验证 startup/close 成本、实际 `peak-inflight` 和 CPU workload 相对
  `Parallelism=1` 的扩展效率；
- 每个 benchmark 都校验 work 完成数、Sink 接收数、预期输出数和关键资源上限。必需指标记录、
  completion 与 ownership 路径不得为提高成绩而关闭；
- 失败、取消和 shutdown 只进入独立收尾 benchmark，测量从停止触发到 `Run` 返回的耗时与未完成
  work，不与正常路径的 `works/s` 横向比较。

#### 1.8.3 可重复性与结果解释

- benchmark 子名称编码 workload、Parallelism、MaxInFlightWorks、输出数量等关键配置；原始结果
  同时记录 commit、Go 版本、OS、架构、CPU 和 GOMAXPROCS；
- 正式 old/new 比较在同一机器和相同软件、资源配置下至少运行多轮，并以 `benchstat` 的统计结果
  为准；单次运行差异不构成性能结论；
- 共享 CI 只检查 benchmark 可运行和断言成立，不因小幅噪声直接判定回归。自动性能门槛仅在稳定
  专用环境和积累足够基线后设置，M1 不预设统一百分比；
- 结果必须联合解释吞吐、延迟、分配和 peak in-flight。吞吐提升若依赖更多内存、更高资源上限、
  丢弃输出、跳过 completion 或削弱语义，不得称为有效优化；
- benchmark 不能替代确定性正确性、race 或 leak 测试。仓库长期保存 benchmark 代码；只有里程碑
  基线、重要优化对比或影响架构决定的结果才形成独立性能报告并由 Status 链接，不记录每次运行
  流水。

### 1.9 M2 Operator Retry 验证

- attempt 测试证明任一 Operator error 或 `PanicError` 都丢弃该 attempt 的全部暂存输出，新
  attempt 从原始输入重跑完整 Chain，work identity 不变、attempt identity 更新，同一 work 不
  出现重叠 attempt；
- 输入 ownership 测试覆盖 Runtime 跨 backoff 保留输入以及内置 Operator 不原地修改；测试只
  能验证内置行为和公开契约，不宣称能检测任意用户代码对可达引用数据的违规修改；
- 用 barrier 把 Source admission 分别暂停在 Operator Work Failure Policy 判定前、active failure 登记前后、
  lane 释放前后和并发 Read 线性化两侧，证明 fence 安装后没有新业务 admission，已越过边界的
  work 仍继续收敛；
- 多 blocker 测试覆盖同时成功、再次失败、最后移除与新登记竞争，证明 gate 只在 active set
  稳定为空时恢复且通知不丢失；
- finite 次数、时间、组合预算和 explicit unlimited 都使用可控 clock 验证；时间 deadline 在
  running attempt 中到达时必须取消 attempt context、禁止新 attempt，并等待协作返回；
- fixed/exponential backoff 覆盖首次 delay、倍增、Max clamp、jitter 边界、确定随机序列、budget
  deadline 截断、宿主取消和 shutdown；正确性测试优先使用 `testing/synctest`，不等待真实墙钟；
- backoff 不占 execution lane 但持续占 permit；多个 retry work 可以并发但受 Parallelism 限制，
  timer/goroutine/ready state 不随 attempt 次数无界增长；
- first-failure set 测试覆盖单 work 多 attempt 不替换根因、多 work 有界聚合、恢复后移除以及
  耗尽时 primary trigger 与 active failure snapshot 分离；
- Build table test 覆盖 finite/unlimited 冲突、零或负预算和非法 backoff/jitter，默认未配置
  Retry 时仍直接 FailJob。

## 2. 当前开放问题

当前 M0 的退出目标是先收敛所有影响 M1/M2 公共 API、所有权、并发和恢复正确性的设计，
再开始 Runtime 与生产 Connector 编码。局部命名、私有类型组织和可由受约束原型验证的实现
选择不需要在文档中预先固定。

### 2.1 M1 实现前必须收敛

- 无剩余设计问题；M1 指标记录、确定性测试、race/leak 和 benchmark 审核已经接受。具体私有
  类型与测试 package 组织可在实现中按 §2.3 细化。

### 2.2 M2 实现前必须收敛

- Kafka 客户端适配、poll/pause/commit、assignment/revoke/lost 和 commit 失败规则；
- ClickHouse batch、flush、部分失败、unknown effect 和关闭 deadline；
- M2 指标、故障注入矩阵和 at-least-once 声明审核。

### 2.3 可由原型细化但不得改变语义的事项

- Transformation 私有接口、adapter 布局和异构存储方式；
- Source/Sink definition、运行实例和私有 adapter 的最终 Go 类型名；
- 有界内部 queue 的具体数据结构和不改变公开保证的容量微调；
- 测试工具、fake clock 和 fault injection hook 的 package 组织。

## 3. 实现前审核点

实现或评审 M1/M2 时必须能回答：

- 一条记录从何时开始由 Runtime 承担 completion responsibility；
- 每层预取、队列、暂存、请求和 retry 的数量上限，以及哪些部分暂不承诺字节上限；
- attempt 失败时哪些输出可丢弃，哪些已转给 Sink；
- Sink 入队、外部完成、输入终结和 position 提交是否严格区分；
- 暂停、终止和 revoke 是否仍允许安全进度继续提交；
- 旧 generation 的所有迟到路径是否被 fence；
- Kafka session 是否独立于业务回压继续维持；
- 宿主取消是否能有界结束所有 Runtime 管理的 goroutine。
