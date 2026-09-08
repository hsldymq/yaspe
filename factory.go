package yaspe

// SourceFactory 每次 Create 返回独立, 尚未打开的 Source. Create 必须快速, 无阻塞 I/O,
// 不启动 goroutine, 不取得需 Close 的资源, 并支持多次及并发调用.
type SourceFactory[T any] interface {
	Create() (Source[T], error)
}

// OperatorFactory 为每条 execution lane 创建独立 Operator; Create 遵循 SourceFactory 的约束.
type OperatorFactory[I, O any] interface {
	Create() (Operator[I, O], error)
}

// SinkFactory 为每次 Run 创建一个由所有 lane 共享的 Sink; Create 遵循 SourceFactory 的约束.
type SinkFactory[T any] interface {
	Create() (Sink[T], error)
}

type sourceFactoryFunc[T any] func() (Source[T], error)

func (f sourceFactoryFunc[T]) Create() (Source[T], error) {
	return f()
}

type operatorFactoryFunc[I, O any] func() (Operator[I, O], error)

func (f operatorFactoryFunc[I, O]) Create() (Operator[I, O], error) {
	return f()
}

type sinkFactoryFunc[T any] func() (Sink[T], error)

func (f sinkFactoryFunc[T]) Create() (Sink[T], error) {
	return f()
}
