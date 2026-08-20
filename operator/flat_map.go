package operator

import (
	"context"

	"github.com/hsldymq/yaspe"
)

// FlatMapFunc 将一个输入值转换为零个或多个输出值。
type FlatMapFunc[I, O any] func(I) []O

// FlatMapFuncWithContext 将一个输入值转换为零个或多个输出值，并允许转换过程被取消或失败。
type FlatMapFuncWithContext[I, O any] func(context.Context, I) ([]O, error)

// FlatMap 将一条输入记录展开为零条或多条输出记录。
type FlatMap[I, O any] struct {
	transform FlatMapFuncWithContext[I, O]
}

// NewFlatMap 使用不返回错误的转换函数创建 FlatMap。
func NewFlatMap[I, O any](transform FlatMapFunc[I, O]) *FlatMap[I, O] {
	return NewFlatMapWithContext(func(_ context.Context, value I) ([]O, error) {
		return transform(value), nil
	})
}

// NewFlatMapWithContext 使用可感知 context 且可返回错误的转换函数创建 FlatMap。
func NewFlatMapWithContext[I, O any](
	transform FlatMapFuncWithContext[I, O],
) *FlatMap[I, O] {
	return &FlatMap[I, O]{transform: transform}
}

// Process 按转换结果的顺序发送输出，并在首次 Emit 失败时停止。
func (f *FlatMap[I, O]) Process(
	ctx context.Context,
	input yaspe.Record[I],
	output yaspe.Collector[O],
) error {
	values, err := f.transform(ctx, input.Value)
	if err != nil {
		return err
	}

	for _, value := range values {
		if err := output.Emit(ctx, yaspe.Record[O]{Value: value}); err != nil {
			return err
		}
	}

	return nil
}
