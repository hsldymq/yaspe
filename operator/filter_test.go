package operator_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hsldymq/yaspe"
	"github.com/hsldymq/yaspe/operator"
)

// TestFilterEmitsMatchingRecord 验证简单 predicate 匹配时 Filter 会输出原记录。
func TestFilterEmitsMatchingRecord(t *testing.T) {
	op := operator.NewFilter(func(value int) bool {
		return value > 0
	})

	input := yaspe.Record[int]{Value: 42}
	output := &collectingCollector[int]{}

	if err := op.Process(context.Background(), input, output); err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	if len(output.records) != 1 {
		t.Fatalf("got %d records, want 1", len(output.records))
	}

	if output.records[0] != input {
		t.Fatalf("got record %#v, want %#v", output.records[0], input)
	}
}

// TestFilterDoesNotEmitNonMatchingRecord 验证简单 predicate 不匹配时 Filter 会正常结束且不输出记录。
func TestFilterDoesNotEmitNonMatchingRecord(t *testing.T) {
	op := operator.NewFilter(func(value int) bool {
		return value > 0
	})

	output := &collectingCollector[int]{}

	if err := op.Process(
		context.Background(),
		yaspe.Record[int]{Value: -1},
		output,
	); err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	if len(output.records) != 0 {
		t.Fatalf("got %d emitted records, want 0", len(output.records))
	}
}

// TestFilterWithContextPassesContextToPredicate 验证完整版 Filter 会把 Process 的 context 传给 predicate。
func TestFilterWithContextPassesContextToPredicate(t *testing.T) {
	type contextKey struct{}

	ctx := context.WithValue(context.Background(), contextKey{}, "expected")
	op := operator.NewFilterWithContext(func(
		got context.Context,
		value int,
	) (bool, error) {
		if got.Value(contextKey{}) != "expected" {
			return false, errors.New("context not propagated")
		}

		return value > 0, nil
	})

	output := &collectingCollector[int]{}

	if err := op.Process(ctx, yaspe.Record[int]{Value: 42}, output); err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	if len(output.records) != 1 {
		t.Fatalf("got %d records, want 1", len(output.records))
	}
}

// TestFilterWithContextDoesNotEmitWhenPredicateFails 验证 predicate 失败时 Filter 会返回错误且不输出记录。
func TestFilterWithContextDoesNotEmitWhenPredicateFails(t *testing.T) {
	predicateErr := errors.New("predicate failed")
	op := operator.NewFilterWithContext(func(
		_ context.Context,
		_ int,
	) (bool, error) {
		return false, predicateErr
	})

	output := &collectingCollector[int]{}
	err := op.Process(context.Background(), yaspe.Record[int]{Value: 42}, output)

	if !errors.Is(err, predicateErr) {
		t.Fatalf("got error %v, want %v", err, predicateErr)
	}

	if len(output.records) != 0 {
		t.Fatalf("got %d emitted records, want 0", len(output.records))
	}
}

// TestFilterReturnsEmitFailure 验证匹配记录无法提交给 Collector 时 Filter 会传播 Emit 错误。
func TestFilterReturnsEmitFailure(t *testing.T) {
	emitErr := errors.New("emit failed")
	op := operator.NewFilter(func(int) bool {
		return true
	})

	output := &collectingCollector[int]{err: emitErr}
	err := op.Process(context.Background(), yaspe.Record[int]{Value: 42}, output)

	if !errors.Is(err, emitErr) {
		t.Fatalf("got error %v, want %v", err, emitErr)
	}
}
