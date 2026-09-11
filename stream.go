package yaspe

import "context"

// Stream 是线性路径末端的类型安全句柄. 每次派生保留旧路径, 零值无效.
// 从同一个 Stream 派生多个路径会形成独立 Job, 不是单个 Job 内的分支.
type Stream[T any] struct {
	name  string
	tail  *definitionNode
	valid bool
}

// Map 将每个输入值转换为一个输出值. 用户函数可被不同 lane 并发调用.
func (s Stream[T]) Map[O any](transform func(T) O) Stream[O] {
	if transform == nil {
		return s.TransformFunc[O](nil)
	}
	return s.MapWithContext(func(_ context.Context, value T) (O, error) {
		return transform(value), nil
	})
}

// MapWithContext 的函数可以感知取消或返回错误. 构建期不会执行该函数.
func (s Stream[T]) MapWithContext[O any](transform func(context.Context, T) (O, error)) Stream[O] {
	if transform == nil {
		return s.TransformFunc[O](nil)
	}
	return s.TransformFunc(func() (Operator[T, O], error) {
		return &functionOperator[T, O]{
			process: func(ctx context.Context, in Record[T], out Collector[O]) error {
				value, err := transform(ctx, in.Value)
				if err != nil {
					return err
				}
				return out.Emit(Record[O]{
					Value: value,
				})
			},
		}, nil
	})
}

// Filter 只保留 predicate 返回 true 的输入; false 是正常零输出.
func (s Stream[T]) Filter(predicate func(T) bool) Stream[T] {
	if predicate == nil {
		return s.TransformFunc[T](nil)
	}
	return s.FilterWithContext(func(_ context.Context, value T) (bool, error) {
		return predicate(value), nil
	})
}

// FilterWithContext 是可以取消或失败的 Filter.
func (s Stream[T]) FilterWithContext(predicate func(context.Context, T) (bool, error)) Stream[T] {
	if predicate == nil {
		return s.TransformFunc[T](nil)
	}
	return s.TransformFunc(func() (Operator[T, T], error) {
		return &functionOperator[T, T]{
			process: func(ctx context.Context, in Record[T], out Collector[T]) error {
				keep, err := predicate(ctx, in.Value)
				if err != nil || !keep {
					return err
				}
				return out.Emit(in)
			},
		}, nil
	})
}

// FlatMap 按返回 slice 的顺序产生有限个输出; 空 slice 是正常零输出.
func (s Stream[T]) FlatMap[O any](transform func(T) []O) Stream[O] {
	if transform == nil {
		return s.TransformFunc[O](nil)
	}
	return s.FlatMapWithContext(func(_ context.Context, value T) ([]O, error) {
		return transform(value), nil
	})
}

// FlatMapWithContext 在首次 Emit 失败时停止. 函数可被不同 lane 并发调用.
func (s Stream[T]) FlatMapWithContext[O any](transform func(context.Context, T) ([]O, error)) Stream[O] {
	if transform == nil {
		return s.TransformFunc[O](nil)
	}
	return s.TransformFunc(func() (Operator[T, O], error) {
		return &functionOperator[T, O]{
			process: func(ctx context.Context, in Record[T], out Collector[O]) error {
				values, err := transform(ctx, in.Value)
				if err != nil {
					return err
				}
				for _, value := range values {
					if err := out.Emit(Record[O]{
						Value: value,
					}); err != nil {
						return err
					}
				}
				return nil
			},
		}, nil
	})
}

// Transform 保存为每条 lane 创建独立 Operator 的 Factory.
func (s Stream[T]) Transform[O any](factory OperatorFactory[T, O]) Stream[O] {
	return Stream[O]{
		name:  s.name,
		valid: s.valid,
		tail: newDefinitionNode(s.tail, operatorAdapter[T, O]{
			factory,
		}),
	}
}

// TransformFunc 是 Transform 的函数形式.
func (s Stream[T]) TransformFunc[O any](factory func() (Operator[T, O], error)) Stream[O] {
	return s.Transform[O](operatorFactoryFunc[T, O](factory))
}

// SinkTo 结束流转换阶段, 保存每次 Run 的共享 Sink Factory.
func (s Stream[T]) SinkTo(factory SinkFactory[T]) JobBuilder {
	return JobBuilder{
		name:  s.name,
		valid: s.valid,
		tail: newDefinitionNode(s.tail, sinkAdapter[T]{
			factory,
		}),
	}
}

// SinkToFunc 是 SinkTo 的函数形式.
func (s Stream[T]) SinkToFunc(factory func() (Sink[T], error)) JobBuilder {
	return s.SinkTo(sinkFactoryFunc[T](factory))
}

// 每次 Factory 调用创建独立包装实例, 函数值可共享.
type functionOperator[I, O any] struct {
	process func(context.Context, Record[I], Collector[O]) error
}

func (o *functionOperator[I, O]) Process(ctx context.Context, in Record[I], out Collector[O]) error {
	return o.process(ctx, in, out)
}
