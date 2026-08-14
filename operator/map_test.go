package operator_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/hsldymq/yaspe"
	"github.com/hsldymq/yaspe/operator"
)

type collectingCollector[T any] struct {
	records []yaspe.Record[T]
	err     error
}

func (c *collectingCollector[T]) Emit(
	_ context.Context,
	record yaspe.Record[T],
) error {
	if c.err != nil {
		return c.err
	}

	c.records = append(c.records, record)
	return nil
}

// TestMapTransformsRecord 验证 Map 会转换输入值，并向 Collector 输出一条记录。
func TestMapTransformsRecord(t *testing.T) {
	op := operator.NewMap(func(_ context.Context, value int) (string, error) {
		return strconv.Itoa(value), nil
	})

	output := &collectingCollector[string]{}
	err := op.Process(context.Background(), yaspe.Record[int]{Value: 42}, output)

	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	if len(output.records) != 1 {
		t.Fatalf("got %d records, want 1", len(output.records))
	}

	if output.records[0].Value != "42" {
		t.Fatalf(
			"got value %q, want %q",
			output.records[0].Value,
			"42",
		)
	}
}

// TestMapDoesNotEmitWhenTransformFails 验证转换失败时 Map 会返回错误且不产生输出。
func TestMapDoesNotEmitWhenTransformFails(t *testing.T) {
	transformErr := errors.New("transform failed")

	op := operator.NewMap(func(_ context.Context, _ int) (string, error) {
		return "", transformErr
	})

	output := &collectingCollector[string]{}

	err := op.Process(context.Background(), yaspe.Record[int]{Value: 42}, output)

	if !errors.Is(err, transformErr) {
		t.Fatalf("got error %v, want %v", err, transformErr)
	}

	if len(output.records) != 0 {
		t.Fatalf(
			"got %d emitted records, want 0",
			len(output.records),
		)
	}
}

// TestMapReturnsEmitFailure 验证 Collector 拒绝输出时 Map 会向调用方传播该错误。
func TestMapReturnsEmitFailure(t *testing.T) {
	emitErr := errors.New("emit failed")

	op := operator.NewMap(func(_ context.Context, value int) (string, error) {
		return strconv.Itoa(value), nil
	})

	output := &collectingCollector[string]{
		err: emitErr,
	}

	err := op.Process(context.Background(), yaspe.Record[int]{Value: 42}, output)

	if !errors.Is(err, emitErr) {
		t.Fatalf("got error %v, want %v", err, emitErr)
	}
}

// TestMapPassesContextToTransform 验证 Map 会将 Process 收到的 context 传给转换函数。
func TestMapPassesContextToTransform(t *testing.T) {
	type contextKey struct{}

	ctx := context.WithValue(context.Background(), contextKey{}, "expected")

	op := operator.NewMap(func(got context.Context, value int) (int, error) {
		if got.Value(contextKey{}) != "expected" {
			return 0, errors.New("context not propagated")
		}

		return value * 2, nil
	})

	output := &collectingCollector[int]{}

	if err := op.Process(ctx, yaspe.Record[int]{Value: 21}, output); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
}
