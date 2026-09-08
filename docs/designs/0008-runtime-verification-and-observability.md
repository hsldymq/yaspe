# 0008：Runtime 验证与可观测性

状态：Accepted（M1/M2 验收设计已接受；实现与完整验证状态见 Status）
最后更新：2026-09-08
适用阶段：M1–M2
依赖：全部近期执行契约；见 [Design Map](design-map.md)

本文集中维护指标、确定性测试、race/leak、fault injection、benchmark、开放问题和实现前审核。各行为规则仍以对应能力 Design 为权威来源。

## 1. 验证要求

### 1.1 Source 与背压

- 慢 Sink 最终阻止 Source 继续扩大读取或预取；
- 队列和 Sink 饱和时，work/item 数量和 goroutine 保持有界；第一版不保证字节数有界；
- Source 各层按声明的单位验证容量：Kafka 客户端的在途/缓冲 fetch 数、Connector 已取出
  记录数、Runtime work 数分别受限；不据此声称整个 Source 的记录总数或字节上限；
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
- SourceContext.ReportFailure 测试覆盖没有 permit、队列已满、不再调用 TryRead 时仍能
  通知 Runtime，报告返回不等待 Job/客户端关闭；
- 覆盖 nil 参数拒绝、首个最终失败保留、并发重复报告、TryRead 与独立报告观察同一原因，
  以及已有其他 primary 时 Source failure 不覆盖它；报告接收错误不得替换 Source 根因；
- Open 阶段报告、Close/最终失效后的迟到报告及报告与取消竞争均有确定性交错；入口
  失效快速返回关闭错误，不重新启动流程、不泄漏 goroutine、不改变冻结的 RunError。

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

Kafka 适配的验证须覆盖 [Kafka Design §3](0007-position-and-kafka-rebalance.md#3-kafka-客户端适配)
与 [Source revoke 预算契约](0003-source-reader-and-admission.md#131-split-control-边界)：

- 用 barrier 在 poll 返回、缓存登记、AllowRebalance 和 revoke/reassign 之间交错，证明旧
  poll 数据不会登记进新 ownership；空结果、错误和取消都释放阻挡与未用 reservation；
- 窗口内不执行用户反序列化/I/O，不等待空位；raw、转换中及 decoded 记录共同占用预算，
  不因窗口外转换增加未受控缓存或改变 split 交接顺序；
- 缓存满时停止新 poll，callback 仍可推进；新 assignment 不能绕过背压，resume 同时检查
  capacity、ownership、revoke、shutdown，retained 缓存和 generation 不重置；
- 客户端在途、解析和剩余缓冲结果按 fetch 计数核验，部分 PollRecords 返回不得提前
  释放仍有数据的 fetch 预算；版本适配要求见 Kafka Design §4.2；
- 预取配置默认 MaxConcurrentFetches=2、Connector 缓冲=1,024 条，两者均可配置且必须
  为正整数；实际 Connector 默认组合及零/负值拒绝仍需测试，不能由小样本客户端测试替代；
- 单 fetch 记录数超过 Connector 剩余容量时分批取出，未取部分留在客户端；Connector
  缓冲大于一次 fetch 时可以经多次 fetch 填充，不能把并发数误当作 fetch 总次数限制；
- 小/大 Connector 缓冲均应在记录就绪后立即允许交接，不等待填满；验证容量差异影响
  背压而不改变结果、顺序或 ownership。满时停止新 poll/pause fetch，在途响应仍计数；
- 用大消息、多记录压缩批次、共享底层缓冲和慢 Sink 观察实际内存及分层计数，不将
  测得峰值提升为字节上限。保留客户端原有保护及真实错误处理，不测试尚未承诺的
  yaspe 字节拒收或额外受限解压；
- 空 assignment/revoke 不调用空集合 Runtime control，空 lost 仍观察错误；回调关闭路径
  不同步等待自身 group loop，最终失败在暂停数据读取时也能传到 Runtime；
- 普通提交与 revoke 最终提交始终 Source 级单请求，dirty advance 只合并最新位置；
  在旧请求开始前、请求在途、响应后及 handle Complete 前后分别注入 revoke/lost；
- 普通提交周期默认 3 秒并可配置；无 dirty 不发送，慢提交跨越多个周期时不积累任务，
  多 partition 批量提交且只使用各自安全位置。revoke 最终提交不等待下一个周期；
- 逻辑提交总超时 5 秒包含资格等待、请求、重试及退避；退避初始 100 毫秒、增长至
  最多 1 秒，更早的调用/revoke/shutdown deadline 优先，不能按请求或重试重新计时；
- 请求级成功但某 partition 失败不能返回整体 nil；部分外部成功、响应丢失、超时和迟到
  响应不被误报为原子全失败或全成功，最终提交失败按已接受 FailJob 契约处理；
- 旧请求不能在最终 revoke 提交后重新发送或绑定新 ownership，取消等待不被当作外部
  请求已撤销，真实客户端版本须验证 broker 请求身份与本地 generation 隔离；
- 旧普通提交必须确认成功结束才允许最终提交；超时、取消、最终失败后没有替代连接上
  的补救提交。提交 callback 不递归提交、不同步等待 Runtime 关闭；Lost 导致最终提交
  失败时不得借用会话恢复预算继续提交；
- 手动时钟覆盖缺少 deadline、负/零预留、总预算不足、提前 drain 完成、drain 到期但
  handle 有效、总 context 取消/到期、迟到 Complete，以及冻结后的 Sink completion；
- Kafka 默认预算、静态非法配置与运行时更早期限分别验证；已有普通提交等待、drain、
  最终提交与返回共同消耗一个总预算，不重置时间、不互相等待成环；
- 本地 revoke 计时从 callback 入口开始；异步 blocked 通知只用于诊断/提示，延迟到达
  不能重置计时或被当作精确起点。不声称本地预算覆盖 callback 前的窗口等待，缺少外部
  deadline 时不从 RebalanceTimeout 伪造剩余时间。最终提交还应给 Complete 与 callback
  返回留时间，本地超时/长调度停顿不被当作 ownership 仍有效的证据；
- 所选 franz-go 版本明确使用 classic Consumer Group，分别验证 eager/cooperative；
  客户端自动协议选择不能绕过范围，新的 group 协议不计入第一版通过的兼容性结果；
- Lost 立即 fence、不 drain、不提交；同 partition 再次 Assign 只能建立新 scope，旧
  buffers/work/completion 不得复活。
- 会话建立/恢复测试按 [Kafka Design §3.6](0007-position-and-kafka-rebalance.md#36-会话建立与恢复)
  覆盖默认一分钟、零/负配置拒绝、初次建立与运行时恢复起点；重复 lost/error/retry/backoff
  不重置同次期限，关闭立即终止等待；
- 空 assignment 成功结束计时但不调用 Runtime Assign([])，无数据/背压不被误判为会话
  恢复超时；非空 assignment 须成功交接 Runtime control 后才确认恢复；
- 恢复成功、到期、关闭三方竞争只形成一个结果，超时后迟到 assignment 不得复活 Source；
- 用表驱动验证 §3.6.2 的错误类别及阶段，同一底层错误来自会话维护与最终 offset 提交
  时分别遵守对应策略；已报告最终提交失败不能借恢复预算继续尝试；
- 真实客户端版本核验 Lost、HookGroupManageError、assignment、poll 错误的覆盖、顺序
  和同故障去重；暂停 poll 时最终失败仍能独立报告，见 Kafka Design §4.1。

上述均为待实现验证要求，不能当作客户端兼容性、race/fault 或性能已验证的证据。

#### v1.21.6 已运行的局部证据

[独立版本验证附件](../verification/franz-go-v1.21.6/README.md) 保存七项测试、固定依赖与
实际输出；已启用 race detector 并通过。它覆盖部分 poll/pause、classic 与窗口阻挡、
无竞争提交取消、callback/关闭等待，以及停止 poll 后的 lost/错误通知和初次权限失败。
这些是上述矩阵中的局部证据，其余项仍待验证。

附件使用 kfake 与 net.Pipe，客户端 RequestRetries(0)；不证明五秒提交/退避组合、真实
网络、完整多实例 rebalance 或 yaspe Runtime 的实现。默认 2/1,024、恢复一分钟的应用
行为仍需 Connector/Runtime 实现测试，不标记为已完成。

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
execution group 或等价的结构化追踪机制，`Run` 返回前等待这些 goroutine 退出；用户代码或
Connector 违反取消契约时，按 [Failure Design §2.3](0006-failure-panic-and-shutdown.md#23-context-与阻塞点)
的 shutdown timeout 例外报告并隔离，不得视为正常、完整回收。实现不能只靠
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

### 1.10 ClickHouse Connector 验证

[ClickHouse Design](0009-clickhouse-connector.md) 的已接受契约须验证：

- 业务目标表、列映射及设置准确传递，不因表引擎自动改写路由或写入策略；成功报告
  不早于完整 INSERT 的配置相关确认，实际交付声明须记录服务端前提；
- 组批阶段不创建驱动 batch；五秒尝试预算在 Prepare 前开始并覆盖连接获取、准备、
  填充、Send 与结果等待，不在 Send 时重新计时；
- 每次尝试使用新 batch/context，稳定的定义、行内容及顺序不变，不重新执行 Operator
  或业务转换，已通过其他请求确认成功的 item 不被再次发送；
- 十秒总预算、最多三次尝试、退避 200 毫秒至最多一秒及更早 Close deadline 共同生效；
  多次尝试与 lifecycle 取消/Close 竞争不得重置预算、突破并发或遗漏最终结果；
- 历史未知效果不能被最后一次连接前失败覆盖为 NotApplied；无逐行证据时不猜测成功
  子集，不把清理成功或 IsSent=true 当作写入成功；
- Sink 关闭须发送的缓冲通过 Send 处理；驱动 Close/Abort 不替代发送、不作为 rollback，
  释放错误不覆盖写入根因，所有已接管 item 最终结果与 reporter fence 一致；
- 验证已接受的一 item 一行、接管后转换一次、固定 Sink settings、按目标表和有序列
  集合分组；列数与值数不匹配不得发送，不得仅按表名混批或在重试时重新执行业务转换；
- 映射错误形成 NotApplied，不能将已接管 group 改判为 Accept 拒收；其他已接管 item
  仍须有限收尾。覆盖上游 Filter/FlatMap 的零/多输出、跨 work 合批与跨 batch completion；
- 验证 5,000 行、1 秒组批等待、10,000 item 总容量和并发 2 的默认值及正值配置校验；
  新行不重置最早行计时，时间到期是发送资格而非外部成功，空批不发送；
- 覆盖低流量、多目标、容量小于期望 batch、关闭发送未满批；待转换、buffer、发送、
  in-flight 和 retry 共用 item 预算，移动到后台不提前释放，不能靠等待新 Accept 才触发 flush；
- 热点目标不能长期挤占已就绪分组，空分组及时回收，持续变化的表/列组合不无限累积
  元数据、timer 或 goroutine；实现矩阵见
  [ClickHouse Design §7.1](0009-clickhouse-connector.md#71-输入映射与组批实现验证)。

[v2.48.0 白盒验证附件](../verification/clickhouse-go-v2.48.0/README.md) 中七项 TestProbe
开启 race detector 并通过，原始输出可复现。测试使用可控 net.Conn、无压缩 UInt64 和
测试用 block revision，未经过真实服务器、握手、公共连接池或 socket deadline。
这些证据只覆盖部分 driver batch 生命周期，不代表上述 Connector/服务端验证已完成。

### 1.11 M2 最小指标范围

M2 必需指标限定为吞吐量、消费与 commit 的差、分层缓存数量三类。失败次数、失败突增
及其他扩展指标延后审核；这不改变失败处理、错误报告、RunError、日志诊断或既有故障
测试要求，也不改变这些路径必须有界的约束。

#### 1.11.1 吞吐量

沿用 §1.6 的成功 work 累计数和记录边界，外部根据相邻采样的增量与时间差计算速率。
正常零输出计入成功，同一 work 的多个 retry attempts 不重复增加成功计数。item 数、
batch 数、请求数不能代替该 work 吞吐量；这一指标也不用于判定外部交付是否重复。

#### 1.11.2 消费与 commit 的差

Kafka Connector 按 partition 暴露消费位置与最后确认提交位置，统一采用“下一条 offset”
口径。消费位置指 Connector 已从客户端 poll 取得的最后一条记录的 offset + 1，可能
包含尚未交给 Runtime 的 Connector 缓存；不是只统计已完成 work 的 safe position。

```text
消费与 commit 的差 = consumed_next_offset − confirmed_committed_next_offset
```

例如消费位置 1200、已确认提交位置 1100，差值为 100。差值包含缓存中、处理中和已经
成功但还未确认提交的进度，是 offset 跨度，不一定恰好等于 100 条业务记录；也不是
以 broker 最新位置计算的 consumer lag。提交发出或 safe position 前进都不能冒充 commit
确认成功。

两端位置须属于当前有效的 Source/partition ownership，迟到旧结果不更新当前观察值。
缺少有效已确认位置时应表达未知，不把零伪装成已确认提交。数值由 Kafka Connector
解释，通用 Runtime 继续保存不透明 position，不为指标增加位置解析职责。

#### 1.11.3 分层缓存数量

分别观察以下积压，保留原始计数单位与责任边界：

| 层次 | 数量含义 |
|---|---|
| Kafka 客户端 | 已在客户端缓冲的记录数量；不能据此推断还未返回的 fetch 含多少记录 |
| Source Connector | 已从客户端取出、尚未交给 Runtime 的记录，包含转换中的记录 |
| Runtime | 排队及在途 work 数；in-flight 不等于仅队列中的 work |
| Sink Connector | 已接管但未结束的 item 数，覆盖待转换、组批、发送、写入及 retry |

分层数量不是互斥集合：Runtime in-flight work 可以正在等待 Sink item 完成，一个 work
也可能有多个 item。不能把这些数值简单相加成总记录数或总内存，fetch/record/work/item
单位也不能混用。各层容量保证仍以对应 Design 为准，不因增加指标而扩大记录数或字节
上限的承诺。跨组件采样不作为 completion、ownership 或容量接管的权威判断。

#### 1.11.4 范围、取舍与验证

三类指标优先回答处理速率、消费进度距持久提交多远、积压位于何处。相比同时引入大量
失败、时延及生命周期指标，先限制必需范围可以减少首版 instrumentation 与接口负担。
故障原因仍由已有错误路径提供，未来根据诊断需要再评估失败指标。

指标记录继续遵循 §1.6 的快速、并发安全、非阻塞和不影响 Job 结果要求。这里固定语义，
不提前冻结完整 Metrics API、导出器、标签命名或 Prometheus 适配；这些在实现中审核。

待实现验证须覆盖：

- 零输出成功、多个 retry attempts、重复 completion 和非成功终态的吞吐计数；
- poll 后尚未 admission、已成功尚未 commit、提交仅发出、确认成功和旧 ownership 迟到
  结果对位置差的影响，验证 next-offset 单位与未知位置，不把 offset gap 当成行数；
- 不同缓冲/责任阶段的数量变化，work 与 item 的重叠关系，移动到 Sink 或 retry 时不
  被误报成已完成。指标不用于替代真实状态机的正确性判定。

### 1.12 M2 故障注入验收矩阵

故障验收使用已有 Source、Operator、Sink、position 与关闭契约，不引入另一套执行规则。
失败指标延后不影响以下测试；错误结果、状态断言和输出核对不依赖新增失败计数指标。

| 故障位置或场景 | 必须验证的结果 | 详细契约 |
|---|---|---|
| Source 已读取、尚未交接时进程崩溃 | 未处理数据未被提前提交，符合恢复前提时可重放 | [Source §1.4](0003-source-reader-and-admission.md#14-source-admission-与所有权) |
| ready 交接与取消竞争 | 已交出的记录先绑定并追踪，不作为空 reservation 释放 | [Source §1.4](0003-source-reader-and-admission.md#14-source-admission-与所有权) |
| Operator 部分 Emit 后失败 | 本次失败 attempt 的部分 terminal outputs 不交给 Sink | [Operator Design](0004-operator-attempt-and-collector.md) |
| Sink 已接管、尚未发送时停止或崩溃 | 不提前完成输入；正常收尾逐项报告，崩溃后依赖 Source 重放 | [Sink Design](0005-sink-handoff-and-completion.md) |
| 写入已发出，响应丢失或超时 | 保留 Unknown，不误报未生效；有限重试可能重复 | [ClickHouse §5](0009-clickhouse-connector.md#5-clickhouse-有限重试) |
| Sink 按约定确认成功后、position 提交前后崩溃 | 从实际已提交位置恢复，允许重放，不越过未完成进度 | [Position §1.7](0007-position-and-kafka-rebalance.md#17-safein-flight-与-committed-position) |
| revoke/lost、提交与迟到结果竞争 | 旧结果不推进新 ownership；旧提交最终失败后不补交 | [Kafka §2–3](0007-position-and-kafka-rebalance.md#2-kafka-rebalance) |
| 慢 Sink、低流量、多目标、取消与关闭 | 各层计数约束有效，组批不永久等待，受控资源收敛，未知结果不算成功 | [ClickHouse §4](0009-clickhouse-connector.md#4-组批与容量) · [Failure §2](0006-failure-panic-and-shutdown.md#2-failjob取消与关闭) |

每一行是测试入口，具体交错沿用 §1.1–1.10 的详细矩阵，包括部分 commit、重复报告、
全局预算、eager/cooperative、lost 后重入及关闭期限，不以此摘要替代或减少已有要求。
进程崩溃、请求返回 error、context 取消和正常 Close 是不同注入方式，不能只测试其中
一种就声称其余路径也通过。

### 1.13 结果核对与分层证据

#### 1.13.1 独立结果核对

恢复测试使用有限、可重复的数据集，给输入分配稳定的测试 ID，预先独立计算预期输出。
用“输入 ID + 输出序号”标识一个预期输出，以覆盖 Filter 零输出、FlatMap 多输出及值
恰好相同的不同输出。测试标识由测试数据提供，不把 Runtime 私有 WorkID 变成公共 API。

在指定位置注入故障，按声明的恢复前提重启；故障消除并处理完测试集后，核对：

- 预期输出缺失数为零；
- 重复允许存在，须记录数量并关联到具体 retry/replay 场景；
- 正常零输出在预期结果中显式表达，不能被误判为丢失；
- Failed、Cancelled、Unknown 不得误报为 Success；可恢复测试不能越过未解决的输入
  提交位置，不可恢复配置错误仍须按契约明确失败，不能假定不修复也会完成全部输入。

没有 panic、日志正常、总计数相同或指标曲线正常，都不能替代稳定身份的结果核对。
核对边界必须与 Sink 业务配置和故障模型一致；条件性交付声明见
[Position Design §1.10](0007-position-and-kafka-rebalance.md#110-at-least-once)。

#### 1.13.2 三层验证

| 层次 | 证据范围 | 不能外推的结论 |
|---|---|---|
| 确定性测试 | 可控 fake/barrier/clock 验证状态机、取消和并发交错 | 不代表实际驱动或服务器行为 |
| 固定客户端测试 | 使用锁定版本验证驱动、请求与 callback 行为，已有两组七项属于局部证据 | 不代表完整 Runtime/Connector、真实网络或持久化 |
| 隔离环境中的真实 Kafka/ClickHouse 测试 | 固定版本与业务配置，验证进程重启、多实例交接和实际确认结果 | 不代表未声明的磁盘、集群或其他故障模型 |

每项证据记录输入集、版本/配置前提、注入位置与方式、预期断言、实际结果和未覆盖范围。
本次验收设计的接受不等于任何尚未运行的测试已经通过；通过一层不自动替代另一层。

#### 1.13.3 设计门槛与实现门槛

编码前需要明确行为契约与验收方法，不要求尚未实现的 Runtime/Connector 先通过全部
完整故障测试。完成设计落盘与 M0 收尾检查后，可按 Roadmap 进入 M1 最小链路实现；
测试随对应实现补齐，作为 M1/M2 能力完成和交付保证声明的依据。

M0 收尾检查仍须核对 Roadmap 完成标准、设计一致性、代码/测试事实和工作区，不因为
讨论清单收敛就自动标记 M0 Completed。真实适配若暴露影响公开契约的事实，仍须报告并
修正设计或实现，不以“测试留到后面”为由绕过正确性问题。

## 2. 设计收敛与验证断点

当前 M0 的退出目标是先收敛所有影响 M1/M2 公共 API、所有权、并发和恢复正确性的设计，
再开始 Runtime 与生产 Connector 编码。局部命名、私有类型组织和可由受约束原型验证的实现
选择不需要在文档中预先固定。

当前主要行为、M2 三类指标及本节之前的验收方案已接受；下一步由 Status 指向 M0 收尾
检查。以下适配与完整实现验证继续单独跟踪，不混同为必须重新讨论的设计结论。

### 2.1 M1 实现前必须收敛

- 无剩余设计问题；M1 指标记录、确定性测试、race/leak 和 benchmark 审核已经接受。具体私有
  类型与测试 package 组织可在实现中按 §2.3 细化。

### 2.2 M2 已接受设计与待验证项

- Kafka 主要设计、v1.21.6 版本、本地 callback 入口计时和 classic 范围已接受；完整
  协议配置、内部锁等待、retry 和取消/串行化仍需验证，见
  [Kafka Design §4.3](0007-position-and-kafka-rebalance.md#43-本地期限与提交的客户端适配核验)；
- Kafka fetch/记录分层预算、2/1,024 初始默认值及字节非保证已接受；实际默认组合、
  部分 poll、pause 和 assignment 变化下的完整计数仍需验证，见
  [Kafka Design §4.2](0007-position-and-kafka-rebalance.md#42-分层预取预算的客户端适配核验)。
  不再要求证明整个 Source 的固定记录数或内存字节上限；
- Kafka 会话恢复预算、错误分类与独立最终失败报告已接受，见
  [Kafka Design §3.6](0007-position-and-kafka-rebalance.md#36-会话建立与恢复)。v1.21.6 已观察
  Lost → GroupManageError → Assigned 及不 poll 时的权限错误；其余错误覆盖、恢复 timer
  和竞争仍待验证，完成条件见
  [Kafka Design §4.1](0007-position-and-kafka-rebalance.md#41-会话恢复信号与错误类型的版本适配核验)；
- ClickHouse 主要设计、输入映射与所有初始组批参数已接受；实际映射/组批、容量/调度
  验证见 [ClickHouse Design §7.1](0009-clickhouse-connector.md#71-输入映射与组批实现验证)。
  真实服务端错误、连接池竞争、完整 timeout/retry/Close 与交付保证仍需按 §7.2 验证，
  不能把七项驱动白盒测试扩展成完整 Connector 验证；
- M2 最小指标范围已由 §1.11 接受；失败指标延后，指标实现仍待验证。
- M2 故障矩阵、输出核对和分层证据方案已由 §1.12–1.13 接受；条件性交付声明见
  [Position Design §1.10](0007-position-and-kafka-rebalance.md#110-at-least-once)，完整故障测试尚未完成。

### 2.3 可由原型细化但不得改变语义的事项

- Transformation 私有接口、adapter 布局和异构存储方式；
- Source/Sink definition、运行实例和私有 adapter 的最终 Go 类型名；
- 有界内部 queue 的具体数据结构和不改变公开保证的容量微调；
- 测试工具、fake clock 和 fault injection hook 的 package 组织。

## 3. 实现前审核点

实现或评审 M1/M2 时必须能回答：

- 一条记录从何时开始由 Runtime 承担 completion responsibility；
- 每层预取、队列、暂存、请求和 retry 的计数单位及上限，哪些层不能合并为统一记录数
  或字节上限，以及背压时已在途数据如何核算；
- attempt 失败时哪些输出可丢弃，哪些已转给 Sink；
- Sink 入队、外部完成、输入终结和 position 提交是否严格区分；
- 暂停、终止和 revoke 是否仍允许安全进度继续提交；
- 旧 generation 的所有迟到路径是否被 fence；
- Kafka session 是否独立于业务回压继续维持；
- 宿主取消是否能有界结束所有 Runtime 管理的 goroutine。
