package yaspe

import "context"

// Collector 接收 Operator 产生的输出。
type Collector[T any] interface {
	Emit(context.Context, Record[T]) error
}

// Operator 将一条输入记录转换为零条或多条输出记录。
type Operator[I, O any] interface {
	Process(
		context.Context,
		Record[I],
		Collector[O],
	) error
}
