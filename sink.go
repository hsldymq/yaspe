package yaspe

import "context"

// Sink 接管一个 work 的完整输出组. 只有 SinkAccepted, nil 转移整组 ownership.
// SinkBackpressured 或 error 表示全拒, 不能保存 items 或 reporter.
type Sink[T any] interface {
	Open(SinkContext) error
	Accept(context.Context, []SinkItem[T], SinkResultReporter[T]) (SinkAcceptStatus, error)
	Close(context.Context) error
}

// SinkContext 由 Runtime 提供; 生命周期与单次 Accept 的 context 分离.
type SinkContext interface {
	LifecycleContext() context.Context
	CapacityNotifier() SinkCapacityNotifier
}

// SinkCapacityNotifier 提示 Runtime 重新检查容量, 不预留容量.
type SinkCapacityNotifier interface {
	NotifyAvailable()
}

// SinkAcceptStatus 描述整组责任交接; 零值无效.
type SinkAcceptStatus uint8

const (
	SinkAcceptStatusInvalid SinkAcceptStatus = iota
	SinkAccepted
	SinkBackpressured
)

// SinkItem 携带业务 Record 和 Runtime 私有身份. Connector 应原样报告该 item,
// 不可用业务值相等性代替身份, 也不可构造新的 item 冒充已接管输出.
type SinkItem[T any] struct {
	Record   Record[T]
	identity *sinkItemIdentity
}

// 使用非零大小的对象, 确保不同身份分配具有独立地址.
type sinkItemIdentity struct {
	// 一次输出组内的索引, 还必须与报告器保存的身份指针匹配, 不能单独作为全局身份.
	index int
}

// SinkOutcome 是外部效果的最终分类; 零值无效.
type SinkOutcome uint8

const (
	SinkOutcomeInvalid SinkOutcome = iota
	SinkSucceeded
	SinkNotApplied
	SinkUnknown
)

// SinkItemResult 中 Succeeded 必须配 nil Err, 其他合法 outcome 必须配非 nil Err.
type SinkItemResult[T any] struct {
	Item    SinkItem[T]
	Outcome SinkOutcome
	Err     error
}

// SinkResultReporter 绑定一次完整组交接, 支持同步或异步报告.
// Report 转移 results slice 的 ownership, 调用后不得复用. Runtime 实现必须并发安全.
type SinkResultReporter[T any] interface {
	Report([]SinkItemResult[T])
}
