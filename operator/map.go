package operator

import (
	"context"

	"github.com/hsldymq/yaspe"
)

// MapFunc 将一个输入值转换为一个输出值。
type MapFunc[I, O any] func(I) O

// MapFuncWithContext 将一个输入值转换为一个输出值，并允许转换过程被取消或失败。
type MapFuncWithContext[I, O any] func(context.Context, I) (O, error)

type Map[I, O any] struct {
	transform MapFuncWithContext[I, O]
}

// NewMap 使用不返回错误的转换函数创建 Map。
func NewMap[I, O any](transform MapFunc[I, O]) *Map[I, O] {
	return NewMapWithContext(func(_ context.Context, value I) (O, error) {
		return transform(value), nil
	})
}

// NewMapWithContext 使用可感知 context 且可返回错误的转换函数创建 Map。
func NewMapWithContext[I, O any](transform MapFuncWithContext[I, O]) *Map[I, O] {
	return &Map[I, O]{transform: transform}
}

func (m *Map[I, O]) Process(
	ctx context.Context,
	input yaspe.Record[I],
	output yaspe.Collector[O],
) error {
	value, err := m.transform(ctx, input.Value)
	if err != nil {
		return err
	}

	return output.Emit(ctx, yaspe.Record[O]{
		Value: value,
	})
}
