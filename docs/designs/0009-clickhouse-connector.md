# 0009：ClickHouse Connector

状态：Accepted（主要设计、输入映射与初始参数已收敛；§7 保留未完成的适配验证）
最后更新：2026-09-08
适用阶段：M2
依赖：[Sink Design](0005-sink-handoff-and-completion.md) · [Failure Design](0006-failure-panic-and-shutdown.md) · [ADR-0004](../decisions/0004-keep-sink-retry-inside-connector.md) · [ADR-0008](../decisions/0008-clickhouse-business-owned-write-semantics.md)

## 1. 目的与已有前提

ClickHouse Connector 接管 Runtime 的 terminal outputs，在本地组批，由后台写入路径执行
完整 INSERT 并等待结果，再向绑定 reporter 报告每个 item 的最终事实。这里的异步是
相对于 Runtime Accept：不要求 Runtime 在 Accept 内等待数据库写入。

整组责任转移、逐 item completion、容量通知、有限关闭和最终失败 FailJob 均沿用
[Sink Design](0005-sink-handoff-and-completion.md)。Connector 接管后自行执行有限 Retry，
Runtime 不重新执行 Operator、不重新接管已转移输出；这些通用决定不在本 Design 重开。
整组 Accept 是责任原子性，不保证整个 work 或物理 batch 的外部事务原子性。

## 2. 业务配置与成功边界

### 2.1 职责

业务指定目标表、列映射与 ClickHouse 写入设置，包括本地表或 Distributed 表的选择、
分片转发及副本策略。Connector 不自动探测表引擎来决定路由，不强制将 Distributed
改成前台写入，也不擅自覆盖业务的 async_insert 等写入设置。

Runtime 只理解 Sink completion，不解析 ClickHouse 表引擎、错误码或部署拓扑。Connector
负责配置传递、组批、请求执行、错误映射与重试。跨层理由见
[ADR-0008](../decisions/0008-clickhouse-business-owned-write-semantics.md)。

### 2.2 成功确认

`SinkSucceeded` 表示本次完整 INSERT 按业务提供的设置得到服务端成功确认。Accept 入队、
Append 成功、字节发出或驱动 IsSent 均不能单独形成成功。

此确认不自动推出所有 shard、所有副本或指定故障模型下的持久化完成。业务选择的设置
可能给出较弱的接收确认，Connector 不把这种结果改写成更强的落库保证。端到端交付声明
必须列明实际配置和观察边界；任意可接受配置不等于都满足持久化 at-least-once。

统一的 Source 恢复前提和条件性交付声明见
[Position Design §1.10](0007-position-and-kafka-rebalance.md#110-at-least-once)，端到端结果核对
与证据层次见 [Verification Design §1.13](0008-runtime-verification-and-observability.md#113-结果核对与分层证据)。

Connector 后台执行 Send 并等待结果，与服务端是否使用异步插入设置是两个不同维度。
初始“由框架强制关闭 async_insert、强制 Distributed 前台转发”的候选未被采用；选择
由业务决定写入策略，并准确描述确认语义。

## 3. 固定客户端与 batch 生命周期

### 3.1 适配基线

采用官方 `github.com/ClickHouse/clickhouse-go/v2` **v2.48.0** 的 Native API 为适配基线。
该版本要求 Go 1.25，满足 yaspe 的 Go 1.27 基线。既有业务使用 v2.40.3 可提供 schema 与
workload 参考，但不锁定新 Connector 版本。主模块尚未引入该生产依赖。

### 3.2 一次尝试的顺序

```text
Connector 组批，保存稳定的行数据与写入定义
    → 创建本次尝试 context、开始尝试预算
    → PrepareBatch → Append → Send → 最终结果
    → 清理本次驱动 batch
```

组批等待阶段不提前创建驱动 batch；准备发送时才 Prepare，避免组批等待长期占用
数据库连接。五秒尝试预算从 Prepare 前开始，包含获取连接、发送 INSERT/读取列样本、
填充和发送数据、等待结果，不从 Send 才重新计时。

| 驱动操作 | 适配含义 |
|---|---|
| PrepareBatch | 已经发送 INSERT 查询并等待服务端列信息，属于 I/O 尝试 |
| Append | 将稳定的行数据填入客户端 batch，不作为写入成功 |
| Send | 发送剩余行并结束 INSERT、等待结果；成功仍按 §2 的配置边界解释 |
| Flush | 可发送当前数据块而不结束 INSERT，不作为最终确认入口 |
| Close | 结束请求和释放资源，不发送尚未发送的行 |
| Abort | 放弃当前 batch 并释放连接，不是已发生外部效果的 rollback |
| IsSent | 表示 batch 已经结束/处理过，失败 Send、Close、Abort 也可能为 true |

第一版一批通过完整 Send 结束，不用驱动 Flush 实现跨多次发送的长期 batch。Connector 的
“flush”指形成并完成一次 INSERT，不必对应驱动同名方法。

### 3.3 重建与清理

每次重试创建新的驱动 batch 和本次 context，重新填入同一份稳定行数据；不复用上一次
驱动 batch，也不重新执行 Operator 或重新调用可能产生不同结果的业务转换函数。
表、列顺序、写入设置、行内容和行顺序在同一物理 batch 的尝试间保持不变。

源码存在重新取得连接再发送的路径，但不依赖该路径恢复旧 batch 状态；新 batch 让
每次尝试的 context、资源归属与失败边界清楚。重新填充的代价包括再次编码与 Prepare。

所有退出路径必须清理本次驱动资源。Close/Abort 的选择及错误处理不能覆盖写入根因，
清理成功也不能把未确认写入变成 Success 或 NotApplied。Send 的取消会尝试关闭底层
连接中断 I/O；准备、读响应、阻塞 Write 与连接池竞争的完整收敛仍须验证。

## 4. 组批与容量

### 4.1 已接受基础与行数默认值

每批行数阈值默认 **5,000 行**，可配置。该值是发送阈值，不要求每次必须凑满，也不是
内存大小上限；实际单行大小、编码和并发影响资源占用，默认值仍需 workload 校准。

既有 Sink 契约允许多个 work 的输出合并到同一物理 batch，也允许一个 work 的输出分到
多个 batch；该 work 的必要输出全部成功后才可能完成。等待组批、等待发送、写入中和
待重试 item 都属于 Sink 已接管责任，不因移到后台 goroutine 就提前释放容量。

同一物理 INSERT 必须具有一致的目标与列结构；业务输入映射和分组规则见 §4.3。

### 4.2 组批默认值与发送调度

以下初始默认值均可配置，且必须大于零；行数、item 容量和并发数为正整数：

| 配置 | 默认值 |
|---|---|
| 每批行数阈值 | 5,000 行 |
| 最长组批等待 | 1 秒，从最早待写记录计时，新记录不重置 |
| 已接管未结束总容量 | 10,000 条 item，覆盖 buffer/in-flight/retry |
| 并发 INSERT | 2，固定写入 goroutine、多个目标共享预算 |

行数达到阈值、最早待写行达到组批等待上限或进入关闭收尾，任一满足则 batch 具备发送
资格，空批不发。组批等待从该组最早待写行进入组批缓冲开始，新行不重置；等待上限
不是端到端写入完成期限，并发名额繁忙时仍可能等待发送。

时间触发须在低流量和背压时继续推进，避免 Runtime work 上限、较小 Sink 容量或多目标
分散输入导致永远等不满 batch。关闭时按既有 deadline 发送未满批数据，不重新等待满批。

所有目标共享已接管未结束的总 item 容量和固定写入并发。接管后待转换、等待组批、等待
发送、写入中和待重试的 item 均占用容量，形成最终结果后才释放；从缓冲移到后台请求
不释放容量。只为仍有待处理 item 的目标保留活跃分组状态，空分组及时回收；表名或列
组合持续变化不能无限增加缓存、timer、goroutine 或空分组元数据。已具备发送资格的分组
不得被热点目标长期挤占，具体有界 ready queue 与调度数据结构可由实现细化。

Sink 整组接管、永久超过容量返回真实 error、临时不足返回 Backpressured 的规则已在
通用 Sink Design 接受。多个 work 可共享 batch，一个 work 也可分到多个 batch，但
Accept 不能部分接管一组输出。这些初始默认值未经过生产调优，不承诺内存字节上限。

### 4.3 输入映射与多目标组批

业务提供从 `T` 到一行 ClickHouse 输入的转换函数。以下是已接受的结构语义，名称用于
设计表达，尚非已实现 API：

```go
type InsertRow struct {
    Table   string
    Columns []string
    Values  []any
}

type EncodeFunc[T any] func(T) (InsertRow, error)
```

一条 SinkItem 恰好映射为一行，业务决定目标表、有序列集合和对应值，Connector 构造并
执行 INSERT；Runtime 不解析 InsertRow。一条业务输入需要多行或多张表时由上游 FlatMap
产生多个输出，零输出由 Filter 等 Operator 表达，不在 Sink 编码器内隐式丢弃或 fan-out。
这样 Sink item 与行一一对应，容量与 completion 单位明确。

同一个 Sink 可以写多个表或同表不同列集合。第一版 ClickHouse 写入设置固定在 Sink
配置中，行映射不逐行改变 settings；物理组批键为“目标表 + 有序列集合”。例如：

```text
events       (user_id, time)        → 分组 A
events       (user_id, time, extra) → 分组 B
user_actives (user_id, time)        → 分组 C
```

列数必须与 Values 数量匹配，Values[i] 对应 Columns[i]；不能只因表名相同就混批，也
不能单独重排列名而不保持值的对应关系。同一 batch 的稳定性继续遵循 §3.3。

转换时机在成功 Accept 之后：Runtime 先按原子接管协议交出完整 item group，Sink 再在
内部执行转换、校验与组批。转换函数不执行数据库写入，每个 item 的业务转换只做一次，
成功结果由 Sink 保存并用于后续尝试，不在重试时重新调用转换函数。输入与返回结果的
可达引用须遵守既有 ownership 和稳定数据约束，不能被发送方复用修改。

转换或校验失败且该 item 尚无外部效果时，报告 NotApplied 并进入既有最终失败路径；
不能把已接管的一组重新描述成 Accept 拒收，也不能因此悄悄丢弃其他已接管 item，后者
仍按通用有界收尾协议形成最终结果。

此模型支持现有业务的多表及可变列集合，不要求预先把全部输入转成统一宽表。相比在
Sink 内允许零/多行映射，一行对应一个 item 避免额外的行级 completion 聚合；代价是
过滤和业务展开需要在上游显式表达。与逐行 settings 相比，固定 Sink settings 简化稳定
分组键及重试定义，不同写入策略由业务在配置层组织。

## 5. ClickHouse 有限重试

### 5.1 预算

已接受的初始参数：

| 参数 | 初始值 |
|---|---|
| 单次写入尝试最长时长 | 5 秒 |
| 一个物理 batch 的整个重试过程 | 最多 10 秒 |
| 尝试次数 | 最多 3 次，包含首次 |
| 退避 | 初始 200 毫秒，增长至最多 1 秒 |

重试总预算从该 batch 开始首次写入尝试计时，包含各次 Prepare/Append/Send 与退避，不在
重试、清理或进入 Close 时重新补足。单次尝试同时受五秒上限和剩余总预算约束；已有
更早的 lifecycle/Close 限制优先。开始新的尝试前必须确认尚有剩余预算，不能仅因为
没有用满三次就越过总期限。这些初始参数尚未经过真实 workload 调优。

### 5.2 错误映射与历史效果

| 情况 | 已接受策略 |
|---|---|
| 本地转换/编码失败，且能证明所有尝试均未发送行数据 | 不重试，最终 NotApplied |
| 可恢复连接错误或服务端暂时错误 | 在剩余预算内重试 |
| 请求发出后超时/断连，结果未知 | 允许有限重试，接受可能重复写入 |
| 权限、目标表不存在、字段类型不匹配等明确配置问题 | 不反复重试，报告与实际效果相符的最终失败 |
| 重试耗尽，历史上仍有未解决的可能写入 | 最终 Unknown |

错误是否值得重试与 outcome 是不同判断。错误表先固定语义类别，v2.48.0 的具体异常码、
包装与可证明阶段仍须核验；不能把任意 server error 或最后一次连接失败直接解释成
整个 batch 未生效。

```text
尝试 1：行已发送、响应丢失 → 可能生效
尝试 2：连接失败 → 仅能证明此次没有写入
最终放弃 → Unknown，不能抹掉尝试 1 的未知效果
```

若后续某次完整 INSERT 成功确认，可以报告对应 items 成功，但此前未知尝试可能产生
重复。内容和顺序稳定有助于业务使用外部去重机制，不构成通用 exactly-once 保证；不
默认假定任意表/设置都具有去重能力。

无法获得可靠逐行结果时，按整个物理 batch 重试和保守分类，不猜测成功行；已经通过
其他请求明确成功的 item 不回滚，也不能因重试另一个 batch 而重新发送这些成功 item。
中间失败不报告为最终 completion，最终失败仍沿用 ADR-0004 的 FailJob 契约。

## 6. 关闭适配与验证证据

通用 Sink Close 的停止接管、lifecycle 取消、有限 drain、逐 item 报告与 reporter fence
继续按 [Sink Design §1.6](0005-sink-handoff-and-completion.md#16-有限关闭与迟到事件隔离)
执行。需要发送的剩余数据必须经过完整 Send，不能只对驱动 batch 调用 Close。结果
已经未知时，关闭或 Abort 成功不能撤销这种不确定性。

Runtime lifecycle 取消与独立 Close context 的衔接、正在 Prepare/Send 的请求退出、
关闭期间是否还有本批剩余重试预算及资源释放错误，必须在具体实现中共同验证。
不能因为驱动提供 context 参数，就宣称整个池获取、写入和清理都已严格满足 deadline。

v2.48.0 七项驱动级白盒测试已开启 race detector 并通过，源码、固定模块校验和、复现
脚本、实际输出及覆盖限制见 [验证附件](../verification/clickhouse-go-v2.48.0/README.md)。
它使用可控连接而非真实服务端，直接验证 batch/connect 的局部行为，不是 Connector
实现、完整 Native 协议、服务端落库或业务重试保证的证明。

## 7. 尚未完成的适配验证

主要设计及默认参数已接受，不把下列未运行测试描述成尚未决定的通用规则。实际验证若
发现影响接口、ownership 或正确性的事实，再显式重新评估。

### 7.1 输入映射与组批实现验证

- 已接受前提：§4 的一 item 一行、接管后转换一次、固定 Sink settings、多目标按表和
  有序列集合分组，以及全部初始默认值；
- 尚需覆盖：非法列值数量、编码错误、引用数据稳定性、同表不同列、多表、上游零/多
  输出、跨 work 组批/跨 batch 完成、重试不重新转换；
- 完成条件：正值配置校验、5000 行/1 秒触发、10000 item 总容量/并发 2 的实际组合，
  低流量、多目标和容量相互限制下不永久等待；验证待转换/请求/重试计数、热点调度与
  空分组回收，确认编码失败及其他已接管 item 收尾的责任边界。

### 7.2 驱动与实际服务端验证

- 基线：v2.48.0 Native API；业务负责具体表/写入设置，不由框架强制部署策略；
- 尚需覆盖：公共 Conn/连接池竞争、Prepare/Send/清理的阻塞 I/O 与取消、真实服务器
  错误类型、部分成功、响应丢失、完整超时/退避组合、Close 与重试衔接、资源回收；
- 完成条件：将具体错误与阶段映射到 §5 策略，给出服务端配置及观察边界，说明何时可
  声明 at-least-once、何时只有较弱接收确认；验证默认 batch/容量组合及真实 workload。
  证据若暴露不满足既定契约的行为，显式重新评估，不绕过错误或提前报告成功。

## 8. 官方事实来源

- [v2.48.0 go.mod](https://github.com/ClickHouse/clickhouse-go/blob/v2.48.0/go.mod)：固定版本依赖；
- [Native batch](https://github.com/ClickHouse/clickhouse-go/blob/v2.48.0/conn_batch.go)：Prepare、Send、Flush、Close、Abort；
- [驱动接口](https://github.com/ClickHouse/clickhouse-go/blob/v2.48.0/lib/driver/driver.go)：Batch 生命周期；
- [响应与取消](https://github.com/ClickHouse/clickhouse-go/blob/v2.48.0/conn_process.go)：响应处理和取消；
- [ClickHouse 写入策略](https://clickhouse.com/docs/concepts/best-practices/selecting-an-insert-strategy)：客户端组批及确认模式背景。

移动的服务端文档不是特定部署的验证证据；实际版本与 settings 须在 §7.2 中固定核验。
