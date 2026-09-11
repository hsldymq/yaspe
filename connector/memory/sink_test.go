package memory

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/hsldymq/yaspe"
)

type testSinkContext struct {
	ctx context.Context
}

func (c testSinkContext) LifecycleContext() context.Context {
	return c.ctx
}

func (testSinkContext) CapacityNotifier() yaspe.SinkCapacityNotifier {
	return nil
}

type testSinkReporter[T any] struct {
	mu       sync.Mutex
	batches  [][]yaspe.SinkItemResult[T]
	onReport func([]yaspe.SinkItemResult[T])
}

func (r *testSinkReporter[T]) Report(results []yaspe.SinkItemResult[T]) {
	r.mu.Lock()
	r.batches = append(r.batches, results)
	r.mu.Unlock()
	if r.onReport != nil {
		r.onReport(results)
	}
}

func newTestSink[T any](t *testing.T, options SinkOptions) *Sink[T] {
	t.Helper()
	sink, err := NewSink[T](options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sink.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return sink
}

func openTestSink[T any](t *testing.T, sink *Sink[T]) {
	t.Helper()
	requireError(t, sink.Open(testSinkContext{
		ctx: context.Background(),
	}), nil)
}

func sinkItems[T any](values ...T) []yaspe.SinkItem[T] {
	items := make([]yaspe.SinkItem[T], len(values))
	for i, value := range values {
		items[i] = yaspe.SinkItem[T]{
			Record: yaspe.Record[T]{
				Value: value,
			},
		}
	}
	return items
}

func acceptOK[T any](t *testing.T, sink *Sink[T], values ...T) {
	t.Helper()
	reporter := &testSinkReporter[T]{}
	status, err := sink.Accept(context.Background(), sinkItems(values...), reporter)
	if err != nil || status != yaspe.SinkAccepted {
		t.Fatalf("Accept() = %v, %v", status, err)
	}
}

// TestNewSink 验证每次构造的结果和失败计划独立, 且互斥失败配置被拒绝.
func TestNewSink(t *testing.T) {
	failure := errors.New("failure")
	invalid, err := NewSink[int](SinkOptions{
		Failures: []error{
			nil,
		},
		AlwaysFail: failure,
	})
	requireError(t, err, ErrInvalidSinkOptions)
	if invalid != nil {
		t.Fatal("invalid options returned a sink")
	}
	first := newTestSink[int](t, SinkOptions{})
	second := newTestSink[int](t, SinkOptions{})
	var _ yaspe.Sink[int] = first
	openTestSink(t, first)
	openTestSink(t, second)
	acceptOK(t, first, 1)
	if len(first.Records()) != 1 || len(second.Records()) != 0 {
		t.Fatal("sinks share result storage")
	}
}

// TestSinkOpenCloseAndInvalidHandles 验证零值, Open 参数与重复调用, 关闭幂等性及关闭后的接管拒绝.
func TestSinkOpenCloseAndInvalidHandles(t *testing.T) {
	for _, sink := range []*Sink[int]{
		nil,
		{},
	} {
		requireError(t, sink.Open(testSinkContext{
			ctx: context.Background(),
		}), ErrInvalidSink)
		requireError(t, sink.Close(context.Background()), ErrInvalidSink)
		_, err := sink.Accept(context.Background(), sinkItems(1), &testSinkReporter[int]{})
		requireError(t, err, ErrInvalidSink)
		if sink.Groups() != nil || sink.Records() != nil {
			t.Fatal("invalid sink returned results")
		}
	}
	sink := newTestSink[int](t, SinkOptions{})
	requireError(t, sink.Open(nil), ErrInvalidSinkInput)
	requireError(t, sink.Open(testSinkContext{}), ErrInvalidSinkInput)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	requireError(t, sink.Open(testSinkContext{
		ctx: cancelled,
	}), context.Canceled)
	_, err := sink.Accept(context.Background(), sinkItems(1), &testSinkReporter[int]{})
	requireError(t, err, ErrSinkNotOpen)
	openTestSink(t, sink)
	requireError(t, sink.Open(testSinkContext{
		ctx: context.Background(),
	}), ErrSinkAlreadyOpen)
	requireError(t, sink.Close(nil), ErrInvalidSinkInput)
	acceptOK(t, sink, 1, 2)
	requireError(t, sink.Close(cancelled), nil)
	requireError(t, sink.Close(context.Background()), nil)
	requireError(t, sink.Open(testSinkContext{
		ctx: context.Background(),
	}), ErrSinkClosed)
	reporter := &testSinkReporter[int]{}
	_, err = sink.Accept(context.Background(), sinkItems(3), reporter)
	requireError(t, err, ErrSinkClosed)
	if len(reporter.batches) != 0 || len(sink.Records()) != 2 {
		t.Fatal("closed sink changed results or reported rejected items")
	}
}

// TestSinkAcceptStoresWholeGroupAndReportsSynchronously 验证整组按序保存, 返回前逐项报告成功, 相同业务值也保留独立输出项.
func TestSinkAcceptStoresWholeGroupAndReportsSynchronously(t *testing.T) {
	sink := newTestSink[int](t, SinkOptions{})
	openTestSink(t, sink)
	items := sinkItems(7, 7, 8)
	expected := slices.Clone(items)
	reporter := &testSinkReporter[int]{}
	status, err := sink.Accept(context.Background(), items, reporter)
	requireError(t, err, nil)
	if status != yaspe.SinkAccepted || len(reporter.batches) != 1 || len(reporter.batches[0]) != 3 {
		t.Fatalf("status %v, reports %v", status, reporter.batches)
	}
	for i, result := range reporter.batches[0] {
		if result.Item != expected[i] || result.Outcome != yaspe.SinkSucceeded || result.Err != nil {
			t.Fatalf("invalid result at %d: %+v", i, result)
		}
	}
	want := [][]yaspe.Record[int]{
		{
			{
				Value: 7,
			},
			{
				Value: 7,
			},
			{
				Value: 8,
			},
		},
	}
	if !reflect.DeepEqual(sink.Groups(), want) {
		t.Fatalf("groups = %v, want %v", sink.Groups(), want)
	}
}

// TestSinkViewsPreserveGroupingAndCopySlices 验证分组与扁平快照顺序, slice 结构隔离及 Close 后结果保持稳定.
func TestSinkViewsPreserveGroupingAndCopySlices(t *testing.T) {
	sink := newTestSink[int](t, SinkOptions{})
	if len(sink.Groups()) != 0 || len(sink.Records()) != 0 {
		t.Fatal("new sink has results")
	}
	openTestSink(t, sink)
	acceptOK(t, sink, 1, 2)
	acceptOK(t, sink, 3)
	wantGroups := [][]yaspe.Record[int]{
		{
			{
				Value: 1,
			},
			{
				Value: 2,
			},
		},
		{
			{
				Value: 3,
			},
		},
	}
	wantRecords := []yaspe.Record[int]{
		{
			Value: 1,
		},
		{
			Value: 2,
		},
		{
			Value: 3,
		},
	}
	groups, records := sink.Groups(), sink.Records()
	if !reflect.DeepEqual(groups, wantGroups) || !reflect.DeepEqual(records, wantRecords) {
		t.Fatalf("unexpected views: %v, %v", groups, records)
	}
	groups[0][0] = yaspe.Record[int]{
		Value: 99,
	}
	groups[1] = nil
	groups = append(groups, []yaspe.Record[int]{
		{
			Value: 100,
		},
	})
	records[0] = yaspe.Record[int]{
		Value: 101,
	}
	if !reflect.DeepEqual(sink.Groups(), wantGroups) || !reflect.DeepEqual(sink.Records(), wantRecords) {
		t.Fatal("snapshot mutation changed stored slice structure")
	}
	requireError(t, sink.Close(context.Background()), nil)
	if !reflect.DeepEqual(sink.Groups(), wantGroups) || !reflect.DeepEqual(sink.Records(), wantRecords) {
		t.Fatal("Close discarded accepted results")
	}
}

// TestSinkSnapshotsDoNotDeepCopyValues 验证快照复制容器而保留原始引用, 并在报告器读取快照时避免持锁死锁.
func TestSinkSnapshotsDoNotDeepCopyValues(t *testing.T) {
	sink := newTestSink[*int](t, SinkOptions{})
	openTestSink(t, sink)
	value := new(int)
	*value = 42
	reporter := &testSinkReporter[*int]{
		onReport: func([]yaspe.SinkItemResult[*int]) {
			if got := sink.Records(); len(got) != 1 || got[0].Value != value {
				t.Fatalf("report observed incomplete or copied value: %v", got)
			}
		},
	}
	status, err := sink.Accept(context.Background(), sinkItems(value), reporter)
	requireError(t, err, nil)
	if status != yaspe.SinkAccepted || sink.Groups()[0][0].Value != value {
		t.Fatal("reference value was not preserved")
	}
}

// TestSinkFailurePlanRejectsEntireGroup 验证固定计划逐次消费, 失败组全拒且不报告, 序列耗尽后恢复成功并保留此前结果.
func TestSinkFailurePlanRejectsEntireGroup(t *testing.T) {
	failure := errors.New("second group failed")
	sink := newTestSink[int](t, SinkOptions{
		Failures: []error{
			nil,
			failure,
			nil,
		},
	})
	openTestSink(t, sink)
	acceptOK(t, sink, 1, 2)
	items := sinkItems(3, 4)
	reporter := &testSinkReporter[int]{}
	status, err := sink.Accept(context.Background(), items, reporter)
	if err != failure || status == yaspe.SinkAccepted || len(reporter.batches) != 0 {
		t.Fatalf("failure = %v, %v, reports %v", status, err, reporter.batches)
	}
	items[0].Record.Value = 99
	acceptOK(t, sink, 5)
	acceptOK(t, sink, 6)
	want := []yaspe.Record[int]{
		{
			Value: 1,
		},
		{
			Value: 2,
		},
		{
			Value: 5,
		},
		{
			Value: 6,
		},
	}
	if !reflect.DeepEqual(sink.Records(), want) {
		t.Fatalf("failed group retained or successful groups rolled back: %v", sink.Records())
	}
}

// TestSinkFailureOptionsAreFrozenAndIndependent 验证构造时复制错误序列, 同一配置创建的多个 Sink 各自消费计划.
func TestSinkFailureOptionsAreFrozenAndIndependent(t *testing.T) {
	failure := errors.New("planned")
	options := SinkOptions{
		Failures: []error{
			failure,
		},
	}
	first := newTestSink[int](t, options)
	second := newTestSink[int](t, options)
	options.Failures[0] = nil
	for _, sink := range []*Sink[int]{
		first,
		second,
	} {
		openTestSink(t, sink)
		_, err := sink.Accept(context.Background(), sinkItems(1), &testSinkReporter[int]{})
		requireError(t, err, failure)
		acceptOK(t, sink, 2)
	}
}

// TestSinkAlwaysFail 验证始终失败配置在多次调用中返回原始错误, 不保存或报告任何输入.
func TestSinkAlwaysFail(t *testing.T) {
	failure := errors.New("always rejected")
	sink := newTestSink[int](t, SinkOptions{
		AlwaysFail: failure,
	})
	openTestSink(t, sink)
	reporter := &testSinkReporter[int]{}
	for range 3 {
		status, err := sink.Accept(context.Background(), sinkItems(1, 2), reporter)
		if status == yaspe.SinkAccepted || err != failure {
			t.Fatalf("Accept() = %v, %v", status, err)
		}
	}
	if len(sink.Groups()) != 0 || len(reporter.batches) != 0 {
		t.Fatal("failed sink retained or reported input")
	}
}

// TestSinkInvalidAndCancelledCallsDoNotConsumePlan 验证空组, nil 参数及取消的调用全拒, 不消耗失败计划.
func TestSinkInvalidAndCancelledCallsDoNotConsumePlan(t *testing.T) {
	failure := errors.New("first valid call")
	sink := newTestSink[int](t, SinkOptions{
		Failures: []error{
			failure,
		},
	})
	openTestSink(t, sink)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	reporter := &testSinkReporter[int]{}
	cases := []struct {
		ctx      context.Context
		items    []yaspe.SinkItem[int]
		reporter yaspe.SinkResultReporter[int]
		want     error
	}{
		{
			context.Background(),
			nil,
			reporter,
			ErrInvalidSinkInput,
		},
		{
			context.Background(),
			[]yaspe.SinkItem[int]{},
			reporter,
			ErrInvalidSinkInput,
		},
		{
			nil,
			sinkItems(1),
			reporter,
			ErrInvalidSinkInput,
		},
		{
			context.Background(),
			sinkItems(1),
			nil,
			ErrInvalidSinkInput,
		},
		{
			cancelled,
			sinkItems(1),
			reporter,
			context.Canceled,
		},
	}
	for _, tc := range cases {
		status, err := sink.Accept(tc.ctx, tc.items, tc.reporter)
		requireError(t, err, tc.want)
		if status == yaspe.SinkAccepted {
			t.Fatal("invalid input accepted")
		}
	}
	_, err := sink.Accept(context.Background(), sinkItems(2), reporter)
	requireError(t, err, failure)
	if len(sink.Groups()) != 0 || len(reporter.batches) != 0 {
		t.Fatal("rejected calls retained or reported input")
	}
}

// TestSinkLifecycleCancellationRejectsNewInput 验证生命周期取消后不再接管新组, 已成功结果不受影响.
func TestSinkLifecycleCancellationRejectsNewInput(t *testing.T) {
	sink := newTestSink[int](t, SinkOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requireError(t, sink.Open(testSinkContext{
		ctx: ctx,
	}), nil)
	acceptOK(t, sink, 1)
	cancel()
	reporter := &testSinkReporter[int]{}
	_, err := sink.Accept(context.Background(), sinkItems(2), reporter)
	requireError(t, err, context.Canceled)
	requireError(t, sink.Close(context.Background()), nil)
	if len(sink.Records()) != 1 || len(reporter.batches) != 0 {
		t.Fatal("cancellation changed accepted results or accepted new input")
	}
}
