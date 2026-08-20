package operator

import (
	"context"

	"github.com/hsldymq/yaspe"
)

// Predicate 判断一条记录是否应当保留。
type Predicate[T any] func(T) bool

// PredicateWithContext 判断一条记录是否应当保留，并允许判断过程被取消或失败。
type PredicateWithContext[T any] func(context.Context, T) (bool, error)

// Filter 根据 predicate 保留或丢弃输入记录。
type Filter[T any] struct {
	predicate PredicateWithContext[T]
}

// NewFilter 使用不返回错误的 predicate 创建 Filter。
func NewFilter[T any](predicate Predicate[T]) *Filter[T] {
	return NewFilterWithContext(func(_ context.Context, value T) (bool, error) {
		return predicate(value), nil
	})
}

// NewFilterWithContext 使用可感知 context 且可返回错误的 predicate 创建 Filter。
func NewFilterWithContext[T any](predicate PredicateWithContext[T]) *Filter[T] {
	return &Filter[T]{predicate: predicate}
}

// Process 在 predicate 匹配时将原输入记录发送给 Collector。
func (f *Filter[T]) Process(
	ctx context.Context,
	input yaspe.Record[T],
	output yaspe.Collector[T],
) error {
	keep, err := f.predicate(ctx, input.Value)
	if err != nil {
		return err
	}

	if !keep {
		return nil
	}

	return output.Emit(ctx, input)
}
