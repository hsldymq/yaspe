# 0002：Job Definition 与 Runtime 实例化

状态：Accepted
最后更新：2026-09-08
适用阶段：M1+
依赖：[核心执行模型](0001-core-execution-model.md)

本文是 Job type-state API、Factory、Build、运行实例、生命周期启动顺序、不可变性与内部
类型擦除的权威契约。本文固定公开阶段能力和行为边界；私有 adapter、slice 和图存储布局
仍由受约束实现原型细化。

## 1. Type-state Job API

第一版使用 Go 1.27 泛型方法和不同公开类型表达构建阶段。M1 的定义期 API 已实现于
[job.go](../../job.go) 与 [stream.go](../../stream.go)；Retry 仍属于尚未实现的 M2 能力：

```text
NewJobDraft(name) → JobDraft
From / FromFunc → Stream[T]
Map / Filter / FlatMap → Stream[O]
Transform / TransformFunc → Stream[O]
SinkTo / SinkToFunc → JobBuilder
Retry（可选）→ JobBuilder
Build → Job
```

构建调用形态为：

```go
job, err := yaspe.NewJobDraft("lightning-log-filter").
    From(sourceFactory).
    Map(parse).
    Filter(validate).
    FlatMap(extract).
    Transform(customOperatorFactory).
    SinkTo(sinkFactory).
    Build()
```

`JobDraft` 只保存尚未绑定 Source 的 Job 级定义，只提供 `From`、`FromFunc` 和将来确有需求的
Job 级不可变配置；它不提供 Build、Operator 或 Sink 方法。`Stream[T]` 是当前
Transformation 的类型安全句柄，提供内置 Transformation、自定义 `Transform` 以及
`SinkTo`。`SinkTo` 返回 `JobBuilder`，其方法集合不再包含任何流转换，只允许 Retry 等完整
Job 配置与 `Build`。M2 Retry 只在该阶段设置；`JobDraft` 和 `Stream[T]` 不重复提供，避免
同一语义在不同 type-state 阶段产生覆盖或合并歧义。

因此以下结构错误由编译器排除：

```go
yaspe.NewJobDraft("x").Build()  // JobDraft 没有 Build
stream.SinkTo(sink).Map(f) // JobBuilder 没有 Map
```

每次 fluent 调用不可变地派生新定义。保留旧 `Stream[T]` 并继续派生会形成另一个独立 Job
路径，不会修改已经 SinkTo 或 Build 的定义：

```go
base := yaspe.NewJobDraft("base").From(sourceFactory)
jobA, err := base.Map(parseA).SinkTo(sinkA).Build()
jobB, err := base.Map(parseB).SinkTo(sinkB).Build()
```

M1 的每个最终 Job 仍只允许一个 Source、零个或多个 Operator 和一个 Sink；旧 Stream 可派生
独立 Job 不等于当前支持分支 DAG 或多 Sink Job。

## 2. Factory 与 Connector Builder

Job Definition 保存 Factory，不保存某次运行的活动 Source、Operator 或 Sink 实例。
具体接口与函数适配见 [factory.go](../../factory.go)；工厂返回的 Source、Operator、Sink
接口分别见 [source.go](../../source.go)、[operator.go](../../operator.go)、[sink.go](../../sink.go)。
接口定义已存在，不代表 Runtime 生命周期、Source control 或 Sink completion 行为已实现。

`FromFunc`、`TransformFunc`、`SinkToFunc` 接收对应的无参数 factory function，在定义期
适配为同一 Factory 协议；真正的 Create 调用仍留到 Runtime 启动时。

Connector 可以提供自己的 Builder 收集 brokers、topic、路径或批量参数；Builder 的
`Build` 应复制并冻结配置，产出可保存到 Job 的 Factory。两者不能混称：Builder 负责形成
配置，Factory 负责每次 Run 创建运行实例。

Factory `Create()` 不接收 context，只允许快速创建尚未打开的实例。它不得执行阻塞 I/O、
启动 goroutine，或取得必须通过 `Close` 释放的外部资源；真正可能阻塞、失败和需要取消的
初始化属于实例的 `Open(...)`。Source 与 Sink 分别接收窄的 `SourceContext` 和 `SinkContext`，
Operator lifecycle 接收 `context.Context`；Factory panic 在 Runtime 启动边界转为带 stack 的启动错误，
不穿透宿主，也不进入 record Failure Policy。

Factory 必须支持多次且可能并发的 `Create`，每次返回独立运行实例。Func 形式捕获的闭包也
遵守相同契约；若捕获可变状态，用户负责并发安全。用户在 Build 后修改 Factory 依赖的可变
配置，结果不受保证。

## 3. Transformation 与实例数量

Transformation 是定义期对象，记录逻辑计算、拓扑身份、上游引用、用户函数和 Factory；
它不处理 Record，不创建 goroutine，也不持有运行期队列或 session。内部必须保留节点身份
和引用关系，不把定义退化成丢失关系的纯 Factory 切片，以保留未来 DAG 演进空间。

一次 Run 创建：

```text
SourceFactory   → 1 个 Source 实例
OperatorFactory → 每条 execution lane 1 个 Operator 实例
SinkFactory     → 1 个由所有 lane 共享的 Sink 实例
```

共享 Sink 统一协调 M1 结果收集以及未来容量和 batch；Runtime 不为每条 lane 创建互不协调的
Sink。内置 Map、Filter、FlatMap 为每条 lane 创建独立包装 Operator，但可以共享用户函数值。
多个 lane 可以并发调用同一个函数值，闭包捕获和外部依赖的并发安全、幂等性与副作用由用户
负责。需要 lane-local 状态时使用 `Transform` 接入 OperatorFactory。

## 4. Operator 可选生命周期

基础 `Operator[I, O]` 仍只要求 `Process`。需要一次初始化和清理的 Operator 可以额外实现
[OperatorLifecycle](../../operator.go)。接口已定义，以下运行期生命周期规则待 Runtime 实现。

Runtime 不并发调用同一实例的 Open、Process 和 Close。每个实例最多成功 Open 一次；Close
开始后不再调用 Process。Runtime 只保证 Close 已成功 Open 的实例；Open 在部分初始化后
返回 error 时，该实例必须先自行清理半成品资源。Chain 内按正向顺序 Open、逆向顺序 Close。

Open 失败属于 startup error，不进入 record Failure Policy；Close error 属于启动回滚或停止
过程的 secondary error。Open/Close panic 作为用户代码 panic 捕获并保留 stack。Close 使用
统一 shutdown deadline，不能建立独立的无限等待。

## 5. 启动和部分失败回滚

完整顺序为：

```text
Create Sink
Create every lane's Operator Chain
Create Source
Open Sink
Open every Operator Chain
Open Source
enable Source admission
```

所有创建和 Open 成功前不得开始 Source admission。任一步失败后，Runtime 只对已经成功 Open
的组件按逆序 Close；失败 Open 的实例负责自身部分初始化清理，未 Open 的实例不得假定会收到
Close。原启动错误保持 primary，回滚 Close error 或 panic 作为 secondary errors。

Sink 最先 Open、最后 Close；Source 最后 Open，避免下游尚未准备好时产生业务输入。Source、
Sink 不需要额外初始化时可以立即 Open，其既有 Runtime-owned Close 契约不因此取消。
Source 的最终生命周期、Reader 与 control reporter 契约见
[Source Design §1.1.1](0003-source-reader-and-admission.md#111-source-生命周期)。

## 6. Build 契约

`Build` 创建并校验 yaspe 自己拥有的不可变定义快照。它不调用 Factory 或 Open，不创建
goroutine，也不接触外部系统。Factory 无法创建实例属于 `Runtime.Run` startup error，而不是
Build error。

M1 Build 接受 Source 直接连接 Sink，也就是零个 Operator。它必须拒绝：

- 空或全空白 Job 名称；
- `JobDraft`、`Stream` 或 `JobBuilder` 的非法零值；
- nil Source、Operator、Sink Factory 或 nil Func；
- M2 增加 Retry 配置时，还须拒绝 finite/unlimited 模式冲突、无有效有限预算或非法 backoff/jitter 参数；
- 缺失、重复、悬空或顺序损坏的节点；
- 不能形成唯一 Source、线性 Chain 和唯一 Sink 的结构；
- yaspe 私有 adapter 的类型元信息自相矛盾。

Build error 必须确定；失败后可再次调用。对同一 JobBuilder 重复 Build 产生语义等价、相互
独立的 Job。Build 复制内部 slice、map、metadata 和节点关系，但不能深拷贝任意用户 Factory、
函数及其可达对象。

节点按最终线性结构顺序取得 Job 内统一结构序号。相同 Builder 重复 Build 得到相同序号；
序号不依赖进程全局计数器、指针地址或函数名，只在该 Job Definition 内唯一，不承诺跨代码
修改、重启或版本升级稳定。M1 不把 checkpoint/savepoint 的用户稳定 UID 混入该身份。

## 7. 私有类型擦除

公开泛型 API 在编译期保证相邻 Source、Operator 和 Sink 的类型衔接。不同泛型实参的节点
无法直接放入一个 Go slice，因此 yaspe 在类型安全的构建调用内创建私有 typed adapter，
捕获真实 `I`/`O` 后适配成 Runtime 可统一调度的非泛型接口。

`any` 和类型断言只允许存在于 yaspe 私有 adapter，不泄漏到用户 API。用户不能伪造 adapter；
正常构图下断言必然成功，失败表示 yaspe 自身破坏不变量并按内部缺陷处理，而不是进入用户
Failure Policy。M1 不引入公开 TypeInformation 或 TypeHint。

逐节点 adapter 的装箱、断言和分配成本必须由 benchmark 验证。如果成本显著，可以将类型
擦除收缩到 Source 输入和 Sink 输出边界或组合整条 typed Chain，但不得改变公开 API 和错误
语义。

## 8. Job 与 Runtime 复用

`Job` 是不可变定义，可以被不同 Runtime 重复且并发执行；每次 Run 的 Source、Operator、
Sink、队列、错误和 completion 状态相互独立。

`Runtime` 是一次性执行容器：

```text
Created → Running → Terminated
```

同一个 Runtime 不允许并发 Run，也不允许 Run 返回后再次使用；Running 或 Terminated 状态
再次调用 Run 返回可识别的 `RuntimeAlreadyUsedError`。需要重新运行时创建新 Runtime。
一次性约束避免 cancellation tree、metrics、错误聚合以及 shutdown timeout 后仍需 fence 的
迟到路径污染下一次执行。

Job 本身不提供 Run。规范入口为：

```go
runtime := yaspe.NewRuntime(options)
err := runtime.Run(ctx, job)
```

Runtime options 持有 Parallelism、shutdown timeout、metrics、clock 等运行策略；这些状态不
写回 Job Definition。Retry/FailJob 属于 Job 的失败与副作用语义，由不可变 Job Definition
持有，不是 Runtime 资源 option；同一 Job 被不同 Runtime 执行时默认保持相同失败语义。

## 9. 实现与验证证据

M1 定义期能力已实现：type-state fluent API、Factory 保存、内置转换工厂、不可变派生、
Build 校验和独立拓扑快照。实现以 [job.go](../../job.go)、[stream.go](../../stream.go) 和
[私有 adapter](../../job_adapter.go) 为准，不在本文复制内部类型和存储布局。

- [Job 测试](../../job_test.go)：构建惰性、非法零值/nil、损坏拓扑、独立快照、局部结构序号和并发派生/Build；
- [编译契约测试](../../job_compile_test.go)：有效跨类型链路可编译，非法阶段调用及不匹配类型被编译器拒绝；
- [转换测试](../../stream_test.go)：零/多输出、context 与错误传播、首次 Emit 失败停止，以及每次工厂创建独立包装实例。

上述测试已通过 race detector；`go vet ./...` 通过。运行实例创建与启动回滚、Runtime 调度、
M2 Retry、位置和异步 completion 尚未实现或验证，不能由定义期测试推断已满足。
