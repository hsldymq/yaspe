package yaspe

import "context"

// Collector 接收 Operator 产生的输出.
type Collector[T any] interface {
	Emit(Record[T]) error
}

// Operator 将一条输入记录转换为零条或多条输出记录.
type Operator[I, O any] interface {
	Process(context.Context, Record[I], Collector[O]) error
}

// OperatorLifecycle 是 Operator 可选的初始化和清理能力.
// Runtime 仅 Close 成功 Open 的实例, 不并发调用同一实例的 Open, Process 和 Close.
// Open 失败时实例自行清理半成品; Close 使用 Runtime 统一的 shutdown deadline.
type OperatorLifecycle interface {
	Open(context.Context) error
	Close(context.Context) error
}
