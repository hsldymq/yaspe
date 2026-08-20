package operator_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hsldymq/yaspe"
	"github.com/hsldymq/yaspe/operator"
)

type failingAtCollector[T any] struct {
	records []yaspe.Record[T]
	failAt  int
	err     error
	calls   int
}

func (c *failingAtCollector[T]) Emit(
	_ context.Context,
	record yaspe.Record[T],
) error {
	c.calls++
	if c.calls == c.failAt {
		return c.err
	}

	c.records = append(c.records, record)
	return nil
}

// TestFlatMapEmitsAllValuesInOrder 验证简单 transform 的多个结果会按切片顺序输出。
func TestFlatMapEmitsAllValuesInOrder(t *testing.T) {
	op := operator.NewFlatMap(func(value string) []string {
		return strings.Fields(value)
	})

	output := &collectingCollector[string]{}
	if err := op.Process(
		context.Background(),
		yaspe.Record[string]{Value: "one two three"},
		output,
	); err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	got := make([]string, 0, len(output.records))
	for _, record := range output.records {
		got = append(got, record.Value)
	}

	want := []string{"one", "two", "three"}
	if !slices.Equal(got, want) {
		t.Fatalf("got values %v, want %v", got, want)
	}
}

// TestFlatMapEmitsNothingForEmptyResult 验证 transform 返回空切片时 FlatMap 会正常结束且不输出记录。
func TestFlatMapEmitsNothingForEmptyResult(t *testing.T) {
	op := operator.NewFlatMap(func(int) []string {
		return nil
	})

	output := &collectingCollector[string]{}
	if err := op.Process(
		context.Background(),
		yaspe.Record[int]{Value: 42},
		output,
	); err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	if len(output.records) != 0 {
		t.Fatalf("got %d emitted records, want 0", len(output.records))
	}
}

// TestFlatMapWithContextPassesContextToTransform 验证完整版 FlatMap 会把 Process 的 context 传给 transform。
func TestFlatMapWithContextPassesContextToTransform(t *testing.T) {
	type contextKey struct{}

	ctx := context.WithValue(context.Background(), contextKey{}, "expected")
	op := operator.NewFlatMapWithContext(func(
		got context.Context,
		value int,
	) ([]int, error) {
		if got.Value(contextKey{}) != "expected" {
			return nil, errors.New("context not propagated")
		}

		return []int{value}, nil
	})

	output := &collectingCollector[int]{}
	if err := op.Process(ctx, yaspe.Record[int]{Value: 42}, output); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
}

// TestFlatMapWithContextDoesNotEmitWhenTransformFails 验证 transform 失败时 FlatMap 会返回错误且不输出记录。
func TestFlatMapWithContextDoesNotEmitWhenTransformFails(t *testing.T) {
	transformErr := errors.New("transform failed")
	op := operator.NewFlatMapWithContext(func(
		_ context.Context,
		_ int,
	) ([]string, error) {
		return nil, transformErr
	})

	output := &collectingCollector[string]{}
	err := op.Process(context.Background(), yaspe.Record[int]{Value: 42}, output)

	if !errors.Is(err, transformErr) {
		t.Fatalf("got error %v, want %v", err, transformErr)
	}

	if len(output.records) != 0 {
		t.Fatalf("got %d emitted records, want 0", len(output.records))
	}
}

// TestFlatMapStopsAfterEmitFailure 验证 Emit 中途失败时此前输出会保留，后续输出不会继续发送。
func TestFlatMapStopsAfterEmitFailure(t *testing.T) {
	emitErr := errors.New("emit failed")
	op := operator.NewFlatMap(func(int) []string {
		return []string{"one", "two", "three", "four"}
	})

	output := &failingAtCollector[string]{
		failAt: 3,
		err:    emitErr,
	}
	err := op.Process(context.Background(), yaspe.Record[int]{Value: 42}, output)

	if !errors.Is(err, emitErr) {
		t.Fatalf("got error %v, want %v", err, emitErr)
	}

	if output.calls != 3 {
		t.Fatalf("got %d Emit calls, want 3", output.calls)
	}

	got := make([]string, 0, len(output.records))
	for _, record := range output.records {
		got = append(got, record.Value)
	}

	want := []string{"one", "two"}
	if !slices.Equal(got, want) {
		t.Fatalf("got emitted values %v, want %v", got, want)
	}
}
