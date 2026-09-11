package yaspe

import (
	"context"
	"fmt"
	"runtime/debug"
)

// 异构值只在内部类型适配边界装箱, 调度器不解析业务值.
type runtimeValue = any

// runtimeRead 在泛型 Source 与调度器之间传递读取状态, 调度器不解析业务值.
type runtimeRead struct {
	state ReadState
	// 仅 ReadReady 有效, 内容是装箱后的 Record[T].
	value runtimeValue
	// 记录是否附带恢复位置, 用于拒绝不支持的位置输入.
	positioned bool
}

// runtimeSource 是调度器使用的非泛型读取边界, 由 typed Source adapter 实现.
type runtimeSource interface {
	open(SourceContext) error
	read() (runtimeRead, error)
	available() <-chan struct{}
	close(context.Context) error
}

// runtimeOperator 是单条 execution lane 持有的非泛型 Operator 边界.
type runtimeOperator interface {
	open(context.Context) error
	process(context.Context, runtimeValue, func(runtimeValue) error) error
	close(context.Context) error
}

// runtimeSink 将整组末端输出交给 Sink, 把接管和同步完成校验收敛成执行结果.
type runtimeSink interface {
	open(SinkContext) error
	accept(context.Context, []runtimeValue) error
	close(context.Context) error
}

// sourceInstantiator 让定义期 Source adapter 在运行启动时创建实例.
type sourceInstantiator interface {
	createSource() (runtimeSource, error)
}

// operatorInstantiator 为每条 lane 延迟创建独立 Operator.
type operatorInstantiator interface {
	createOperator() (runtimeOperator, error)
}

// sinkInstantiator 为一次运行创建供各 lane 共享的 Sink.
type sinkInstantiator interface {
	createSink() (runtimeSink, error)
}

func (a sourceAdapter[T]) createSource() (result runtimeSource, err error) {
	err = invokeUser("source factory", func() error {
		source, createErr := a.factory.Create()
		if createErr != nil {
			return createErr
		}
		if isNil(source) {
			return fmt.Errorf("yaspe: source factory returned nil")
		}
		if _, positioned := source.(PositionCommitter); positioned {
			return ErrUnsupportedSource
		}
		result = &sourceInstance[T]{
			source: source,
		}
		return nil
	})
	return
}

func (a operatorAdapter[I, O]) createOperator() (result runtimeOperator, err error) {
	err = invokeUser("operator factory", func() error {
		op, createErr := a.factory.Create()
		if createErr != nil {
			return createErr
		}
		if isNil(op) {
			return fmt.Errorf("yaspe: operator factory returned nil")
		}
		result = &operatorInstance[I, O]{
			operator: op,
		}
		return nil
	})
	return
}

func (a sinkAdapter[T]) createSink() (result runtimeSink, err error) {
	err = invokeUser("sink factory", func() error {
		sink, createErr := a.factory.Create()
		if createErr != nil {
			return createErr
		}
		if isNil(sink) {
			return fmt.Errorf("yaspe: sink factory returned nil")
		}
		result = &sinkInstance[T]{
			sink: sink,
		}
		return nil
	})
	return
}

// sourceInstance 保留 Source 的实际泛型类型, 负责值装箱和组件调用的 panic 边界.
type sourceInstance[T any] struct {
	source Source[T]
}

func (s *sourceInstance[T]) open(ctx SourceContext) error {
	return invokeUser("source Open", func() error {
		return s.source.Open(ctx)
	})
}
func (s *sourceInstance[T]) close(ctx context.Context) error {
	return invokeUser("source Close", func() error {
		return s.source.Close(ctx)
	})
}
func (s *sourceInstance[T]) available() <-chan struct{} {
	return s.source.Available()
}
func (s *sourceInstance[T]) read() (result runtimeRead, err error) {
	err = invokeUser("source TryRead", func() error {
		read, readErr := s.source.TryRead()
		if readErr != nil {
			return readErr
		}
		result = runtimeRead{
			state:      read.State,
			positioned: read.Positioned != nil,
		}
		if read.State == ReadReady {
			result.value = Record[T]{
				Value: read.Value,
			}
		}
		return nil
	})
	return
}

// operatorInstance 在实际泛型 Operator 与非泛型调度之间转换, 并管理每次 Process 的 Collector scope.
type operatorInstance[I, O any] struct {
	operator Operator[I, O]
}

func (o *operatorInstance[I, O]) open(ctx context.Context) error {
	if lifecycle, ok := o.operator.(OperatorLifecycle); ok {
		return invokeUser("operator Open", func() error {
			return lifecycle.Open(ctx)
		})
	}
	return nil
}
func (o *operatorInstance[I, O]) close(ctx context.Context) error {
	if lifecycle, ok := o.operator.(OperatorLifecycle); ok {
		return invokeUser("operator Close", func() error {
			return lifecycle.Close(ctx)
		})
	}
	return nil
}
func (o *operatorInstance[I, O]) process(ctx context.Context, value runtimeValue, next func(runtimeValue) error) error {
	input, ok := value.(Record[I])
	if !ok {
		panic(internalFault("operator input type mismatch"))
	}
	collector := &scopeCollector[O]{
		ctx:    ctx,
		next:   next,
		active: true,
	}
	defer func() {
		collector.active = false
		collector.next = nil
		collector.ctx = nil
	}()
	err := invokeUser("operator Process", func() error {
		return o.operator.Process(ctx, input, collector)
	})
	if err == nil {
		err = collector.err
	}
	return err
}

// scopeCollector 只在一次 Process 调用期间有效, 同步调用下游且不支持并发 Emit.
type scopeCollector[T any] struct {
	// 绑定当前 Process 的取消边界, 失效时清空.
	ctx context.Context
	// 下一 Operator 或末端暂存入口, Process 返回后清空以断开执行状态引用.
	next func(runtimeValue) error
	// Process 尚未返回, 返回后任何 Emit 都必须被拒绝.
	active bool
	// 锁存首次 Emit 失败, 防止 Operator 忽略返回错误后把 attempt 误报为成功.
	err error
}

func (c *scopeCollector[T]) Emit(record Record[T]) error {
	if !c.active {
		return ErrCollectorClosed
	}
	if c.err != nil {
		return c.err
	}
	if err := c.ctx.Err(); err != nil {
		c.err = err
		return err
	}
	c.err = c.next(record)
	return c.err
}

// internalFault 标记内部类型适配不变量错误, 避免被组件调用边界误分类为用户 panic.
type internalFault string

func invokeUser(component string, fn func() error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			p := PanicError{
				component: component,
				value:     value,
				stack:     debug.Stack(),
			}
			if _, internal := value.(internalFault); internal {
				err = &InternalPanicError{
					PanicError: p,
				}
			} else {
				err = &p
			}
		}
	}()
	return fn()
}
