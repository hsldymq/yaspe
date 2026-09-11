package yaspe

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
)

type testCollector[T any] struct {
	values []T
	calls  int
	failAt int
	err    error
}

func (c *testCollector[T]) Emit(record Record[T]) error {
	c.calls++
	if c.failAt > 0 && c.calls == c.failAt {
		return c.err
	}
	c.values = append(c.values, record.Value)
	return nil
}

func lastOperator[I, O any](t *testing.T, stream Stream[O]) Operator[I, O] {
	t.Helper()
	adapter, ok := stream.tail.adapter.(operatorAdapter[I, O])
	if !ok {
		t.Fatal("unexpected operator adapter")
	}
	first, err := adapter.factory.Create()
	if err != nil {
		t.Fatal(err)
	}
	second, err := adapter.factory.Create()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("built-in factory shared an Operator instance")
	}
	return first
}

// TestFluentBuiltins 验证链式 Map 的类型转换, Filter 的零输出, FlatMap 的零或多输出及顺序, 并检查包装实例独立.
func TestFluentBuiltins(t *testing.T) {
	base := sourceForTest()
	t.Run("map changes type", func(t *testing.T) {
		op := lastOperator[int, string](t, base.Map(strconv.Itoa))
		out := &testCollector[string]{}
		if err := op.Process(context.Background(), Record[int]{
			Value: 42,
		}, out); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(out.values, []string{
			"42",
		}) {
			t.Fatal(out.values)
		}
	})
	t.Run("filter", func(t *testing.T) {
		op := lastOperator[int, int](t, base.Filter(func(v int) bool {
			return v > 0
		}))
		out := &testCollector[int]{}
		for _, v := range []int{
			-1,
			1,
			0,
			2,
		} {
			if err := op.Process(context.Background(), Record[int]{
				Value: v,
			}, out); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(out.values, []int{
			1,
			2,
		}) {
			t.Fatal(out.values)
		}
	})
	t.Run("flatmap zero and multiple", func(t *testing.T) {
		op := lastOperator[int, string](t, base.FlatMap(func(v int) []string {
			if v == 0 {
				return nil
			}
			return []string{
				strconv.Itoa(v),
				"next",
			}
		}))
		out := &testCollector[string]{}
		for _, v := range []int{
			0,
			3,
		} {
			if err := op.Process(context.Background(), Record[int]{
				Value: v,
			}, out); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(out.values, []string{
			"3",
			"next",
		}) {
			t.Fatal(out.values)
		}
	})
}

// TestFluentContextAndErrorPropagation 验证内置转换接收原始 context 和输入, 用户函数失败时返回原始错误且不 Emit.
func TestFluentContextAndErrorPropagation(t *testing.T) {
	base := sourceForTest()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failure := errors.New("user failure")
	check := func(got context.Context, value int) {
		t.Helper()
		if got != ctx || got.Err() != context.Canceled || value != 5 {
			t.Fatal("input/context lost")
		}
	}
	cases := []struct {
		name   string
		stream Stream[int]
	}{
		{
			"map",
			base.MapWithContext(func(got context.Context, v int) (int, error) {
				check(got, v)
				return 99, failure
			}),
		},
		{
			"filter",
			base.FilterWithContext(func(got context.Context, v int) (bool, error) {
				check(got, v)
				return true, failure
			}),
		},
		{
			"flatmap",
			base.FlatMapWithContext(func(got context.Context, v int) ([]int, error) {
				check(got, v)
				return []int{
					1,
					2,
				}, failure
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := lastOperator[int, int](t, tc.stream)
			out := &testCollector[int]{}
			if err := op.Process(ctx, Record[int]{
				Value: 5,
			}, out); !errors.Is(err, failure) {
				t.Fatalf("got %v", err)
			}
			if out.calls != 0 {
				t.Fatal("failed transform emitted output")
			}
		})
	}
}

// TestFluentEmitFailureStopsOutput 验证内置转换传播 Collector 错误, FlatMap 在首次 Emit 失败后停止后续输出.
func TestFluentEmitFailureStopsOutput(t *testing.T) {
	base := sourceForTest()
	failure := errors.New("collector failed")
	cases := []struct {
		name   string
		stream Stream[int]
		failAt int
		want   []int
	}{
		{
			"map",
			base.Map(func(v int) int {
				return v
			}),
			1,
			nil,
		},
		{
			"filter",
			base.Filter(func(int) bool {
				return true
			}),
			1,
			nil,
		},
		{
			"flatmap",
			base.FlatMap(func(v int) []int {
				return []int{
					v,
					v + 1,
					v + 2,
				}
			}),
			2,
			[]int{
				5,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := lastOperator[int, int](t, tc.stream)
			out := &testCollector[int]{
				failAt: tc.failAt,
				err:    failure,
			}
			if err := op.Process(context.Background(), Record[int]{
				Value: 5,
			}, out); !errors.Is(err, failure) {
				t.Fatalf("got %v", err)
			}
			if out.calls != tc.failAt || !reflect.DeepEqual(out.values, tc.want) {
				t.Fatalf("continued after Emit failure: %+v", out)
			}
		})
	}
}
