package yaspe

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type runtimeTestSource struct {
	values  []int
	next    int
	signal  chan struct{}
	context SourceContext
	openFn  func(SourceContext) error
	readFn  func() (ReadResult[int], error)
	closeFn func(context.Context) error
}

func newRuntimeTestSource(values ...int) *runtimeTestSource {
	return &runtimeTestSource{
		values: values,
		signal: make(chan struct{}, 1),
	}
}
func (s *runtimeTestSource) Open(ctx SourceContext) error {
	s.context = ctx
	if s.openFn != nil {
		return s.openFn(ctx)
	}
	return nil
}
func (s *runtimeTestSource) Available() <-chan struct{} {
	return s.signal
}
func (s *runtimeTestSource) TryRead() (ReadResult[int], error) {
	if s.readFn != nil {
		return s.readFn()
	}
	if s.next == len(s.values) {
		return ReadResult[int]{
			State: ReadFinished,
		}, nil
	}
	value := s.values[s.next]
	s.next++
	return ReadResult[int]{
		State: ReadReady,
		Value: value,
	}, nil
}
func (s *runtimeTestSource) Close(ctx context.Context) error {
	if s.closeFn != nil {
		return s.closeFn(ctx)
	}
	return nil
}

type runtimeTestSink struct {
	context  SinkContext
	values   []int
	calls    int
	openFn   func(SinkContext) error
	acceptFn func(context.Context, []SinkItem[int], SinkResultReporter[int]) (SinkAcceptStatus, error)
	closeFn  func(context.Context) error
}

func (s *runtimeTestSink) Open(ctx SinkContext) error {
	s.context = ctx
	if s.openFn != nil {
		return s.openFn(ctx)
	}
	return nil
}
func (s *runtimeTestSink) Accept(ctx context.Context, items []SinkItem[int], reporter SinkResultReporter[int]) (SinkAcceptStatus, error) {
	s.calls++
	if s.acceptFn != nil {
		return s.acceptFn(ctx, items, reporter)
	}
	results := make([]SinkItemResult[int], len(items))
	for i, item := range items {
		s.values = append(s.values, item.Record.Value)
		results[i] = SinkItemResult[int]{
			Item:    item,
			Outcome: SinkSucceeded,
		}
	}
	reporter.Report(results)
	return SinkAccepted, nil
}
func (s *runtimeTestSink) Close(ctx context.Context) error {
	if s.closeFn != nil {
		return s.closeFn(ctx)
	}
	return nil
}

type runtimeTestOperator struct {
	openFn    func(context.Context) error
	processFn func(context.Context, Record[int], Collector[int]) error
	closeFn   func(context.Context) error
}

func (o *runtimeTestOperator) Open(ctx context.Context) error {
	if o.openFn != nil {
		return o.openFn(ctx)
	}
	return nil
}
func (o *runtimeTestOperator) Process(ctx context.Context, input Record[int], output Collector[int]) error {
	if o.processFn != nil {
		return o.processFn(ctx, input, output)
	}
	return output.Emit(input)
}
func (o *runtimeTestOperator) Close(ctx context.Context) error {
	if o.closeFn != nil {
		return o.closeFn(ctx)
	}
	return nil
}

func runtimeJob(t *testing.T, source *runtimeTestSource, sink *runtimeTestSink, operators ...Operator[int, int]) Job {
	t.Helper()
	stream := NewJobDraft("test runtime").FromFunc(func() (Source[int], error) {
		return source, nil
	})
	for _, op := range operators {
		stream = stream.TransformFunc(func() (Operator[int, int], error) {
			return op, nil
		})
	}
	job, err := stream.SinkToFunc(func() (Sink[int], error) {
		return sink, nil
	}).Build()
	if err != nil {
		t.Fatal(err)
	}
	return job
}
func runtimeOptions() RuntimeOptions {
	return RuntimeOptions{
		Parallelism:      1,
		MaxInFlightWorks: 2,
		ShutdownTimeout:  time.Second,
	}
}
func requireRunError(t *testing.T, err, cause error) *RunError {
	t.Helper()
	var runErr *RunError
	if !errors.As(err, &runErr) || !errors.Is(err, cause) {
		t.Fatalf("got %v, want RunError containing %v", err, cause)
	}
	return runErr
}

// TestRuntimeConstructionAndReuse 验证配置与零值校验, 同一实例只能运行一次, 正常完成返回 nil.
func TestRuntimeConstructionAndReuse(t *testing.T) {
	job := runtimeJob(t, newRuntimeTestSource(1), &runtimeTestSink{})
	requireRunError(t, (&Runtime{}).Run(context.Background(), job), ErrInvalidRuntime)
	for _, options := range []RuntimeOptions{
		{
			Parallelism: -1,
		},
		{
			MaxInFlightWorks: -1,
		},
		{
			ShutdownTimeout: -1,
		},
	} {
		requireRunError(t, NewRuntime(options).Run(context.Background(), job), ErrInvalidRuntime)
	}
	requireRunError(t, NewRuntime(runtimeOptions()).Run(nil, job), ErrInvalidRuntime)
	requireRunError(t, NewRuntime(runtimeOptions()).Run(context.Background(), Job{}), ErrInvalidJob)
	runtime := NewRuntime(runtimeOptions())
	if err := runtime.Run(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var reused *RuntimeAlreadyUsedError
	if err := runtime.Run(context.Background(), job); !errors.As(err, &reused) {
		t.Fatalf("reuse: %v", err)
	}
	if runtime.stats.succeeded != 1 || runtime.stats.inFlight != 0 {
		t.Fatal(runtime.stats)
	}
}

// TestRuntimeStartupAndReverseCleanup 验证工厂及 Open 顺序, 每条 lane 独立创建 Operator, 并按逆序 Close.
func TestRuntimeStartupAndReverseCleanup(t *testing.T) {
	var events []string
	var instances []*runtimeTestOperator
	job, err := NewJobDraft("lifecycle").FromFunc(func() (Source[int], error) {
		events = append(events, "create source")
		source := newRuntimeTestSource()
		source.openFn = func(SourceContext) error {
			events = append(events, "open source")
			return nil
		}
		source.closeFn = func(context.Context) error {
			events = append(events, "close source")
			return nil
		}
		return source, nil
	}).TransformFunc(func() (Operator[int, int], error) {
		id := len(instances)
		label := []string{
			"op0",
			"op1",
		}[id]
		events = append(events, "create "+label)
		op := &runtimeTestOperator{
			openFn: func(context.Context) error {
				events = append(events, "open "+label)
				return nil
			},
			closeFn: func(context.Context) error {
				events = append(events, "close "+label)
				return nil
			},
		}
		instances = append(instances, op)
		return op, nil
	}).SinkToFunc(func() (Sink[int], error) {
		events = append(events, "create sink")
		return &runtimeTestSink{
			openFn: func(SinkContext) error {
				events = append(events, "open sink")
				return nil
			},
			closeFn: func(context.Context) error {
				events = append(events, "close sink")
				return nil
			},
		}, nil
	}).Build()
	if err != nil {
		t.Fatal(err)
	}
	options := runtimeOptions()
	options.Parallelism = 2
	if err := NewRuntime(options).Run(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"create sink",
		"create op0",
		"create op1",
		"create source",
		"open sink",
		"open op0",
		"open op1",
		"open source",
		"close source",
		"close op1",
		"close op0",
		"close sink",
	}
	if !reflect.DeepEqual(events, want) || instances[0] == instances[1] {
		t.Fatalf("events: %v", events)
	}
}

// TestRuntimeOpenFailureRollsBackOnlyOpenedComponents 验证 Open 失败时仅清理此前成功打开的组件, 并保留首因与清理错误.
func TestRuntimeOpenFailureRollsBackOnlyOpenedComponents(t *testing.T) {
	failure, cleanup := errors.New("source open failed"), errors.New("sink close failed")
	var events []string
	source := newRuntimeTestSource()
	source.openFn = func(SourceContext) error {
		return failure
	}
	source.closeFn = func(context.Context) error {
		t.Error("failed Open must not be closed")
		return nil
	}
	op := &runtimeTestOperator{
		closeFn: func(context.Context) error {
			events = append(events, "operator")
			return nil
		},
	}
	sink := &runtimeTestSink{
		closeFn: func(context.Context) error {
			events = append(events, "sink")
			return cleanup
		},
	}
	runErr := requireRunError(t, NewRuntime(runtimeOptions()).Run(context.Background(), runtimeJob(t, source, sink, op)), failure)
	if runErr.Primary() != failure || !errors.Is(runErr, cleanup) || !reflect.DeepEqual(events, []string{
		"operator",
		"sink",
	}) {
		t.Fatalf("%v, %v", runErr, events)
	}
}

// TestRuntimeAttemptFailureDiscardsPartialOutput 验证多次 Emit 后失败不会把该输入的部分结果交给 Sink.
func TestRuntimeAttemptFailureDiscardsPartialOutput(t *testing.T) {
	failure := errors.New("attempt failed")
	sink := &runtimeTestSink{}
	op := &runtimeTestOperator{
		processFn: func(ctx context.Context, input Record[int], output Collector[int]) error {
			if err := output.Emit(input); err != nil {
				return err
			}
			if err := output.Emit(Record[int]{
				Value: 2,
			}); err != nil {
				return err
			}
			return failure
		},
	}
	runtime := NewRuntime(runtimeOptions())
	requireRunError(t, runtime.Run(context.Background(), runtimeJob(t, newRuntimeTestSource(1), sink, op)), failure)
	if sink.calls != 0 || runtime.stats.succeeded != 0 || runtime.stats.failed != 1 || runtime.stats.inFlight != 0 {
		t.Fatalf("calls=%d stats=%+v", sink.calls, runtime.stats)
	}
}

// TestRuntimeCollectorScopeAndIgnoredEmitError 验证 Collector 返回后失效, 下游 Emit 错误不能被上游忽略而形成成功.
func TestRuntimeCollectorScopeAndIgnoredEmitError(t *testing.T) {
	failure := errors.New("downstream failed")
	var saved Collector[int]
	first := &runtimeTestOperator{
		processFn: func(ctx context.Context, input Record[int], output Collector[int]) error {
			saved = output
			_ = output.Emit(input)
			return nil
		},
	}
	second := &runtimeTestOperator{
		processFn: func(context.Context, Record[int], Collector[int]) error {
			return failure
		},
	}
	sink := &runtimeTestSink{}
	err := NewRuntime(runtimeOptions()).Run(context.Background(), runtimeJob(t, newRuntimeTestSource(1), sink, first, second))
	requireRunError(t, err, failure)
	if !errors.Is(saved.Emit(Record[int]{
		Value: 2,
	}), ErrCollectorClosed) || sink.calls != 0 {
		t.Fatal("invalid collector scope or partial handoff")
	}
}

// TestRuntimeZeroOutputAndRecorderPanic 验证零输出输入直接成功且不调用 Sink, 指标回调 panic 不改变结果.
func TestRuntimeZeroOutputAndRecorderPanic(t *testing.T) {
	sink := &runtimeTestSink{}
	op := &runtimeTestOperator{
		processFn: func(context.Context, Record[int], Collector[int]) error {
			return nil
		},
	}
	options := runtimeOptions()
	options.OnWorkCompleted = func() {
		panic("observer failed")
	}
	runtime := NewRuntime(options)
	if err := runtime.Run(context.Background(), runtimeJob(t, newRuntimeTestSource(1, 2), sink, op)); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 0 || runtime.stats.succeeded != 2 || runtime.stats.inFlight != 0 {
		t.Fatal(runtime.stats)
	}
}

// TestRuntimePanicsBecomeErrors 验证组件 panic 与内部不变量 panic 分别返回对应错误, 并保留 stack.
func TestRuntimePanicsBecomeErrors(t *testing.T) {
	for _, internal := range []bool{
		false,
		true,
	} {
		runtime := NewRuntime(runtimeOptions())
		op := &runtimeTestOperator{
			processFn: func(context.Context, Record[int], Collector[int]) error {
				panic("user")
			},
		}
		if internal {
			runtime.hooks = &runtimeHooks{
				beforeProcess: func() {
					panic("internal")
				},
			}
		}
		err := runtime.Run(context.Background(), runtimeJob(t, newRuntimeTestSource(1), &runtimeTestSink{}, op))
		if internal {
			var p *InternalPanicError
			if !errors.As(err, &p) || len(p.Stack()) == 0 {
				t.Fatalf("%v", err)
			}
		} else {
			var p *PanicError
			if !errors.As(err, &p) || p.Value() != "user" || len(p.Stack()) == 0 {
				t.Fatalf("%v", err)
			}
		}
	}
}

// TestRunErrorSnapshots 验证错误角色和查询 slice 隔离, errors.Is 可识别主因及所有摘要错误.
func TestRunErrorSnapshots(t *testing.T) {
	primary, first, last, secondary := errors.New("primary"), errors.New("first"), errors.New("last"), errors.New("secondary")
	err := &RunError{
		primary: primary,
		active: []WorkFailure{
			{
				first:    first,
				last:     last,
				attempts: 2,
				elapsed:  time.Second,
			},
		},
		secondary: []error{
			secondary,
		},
	}
	for _, cause := range []error{
		primary,
		first,
		last,
		secondary,
	} {
		if !errors.Is(err, cause) {
			t.Fatal(cause)
		}
	}
	err.ActiveFailures()[0] = WorkFailure{}
	err.Secondary()[0] = nil
	err.Unwrap()[0] = nil
	if err.Primary() != primary || err.ActiveFailures()[0].Attempts() != 2 || err.ActiveFailures()[0].Elapsed() != time.Second || err.Secondary()[0] != secondary {
		t.Fatal("snapshot mutated")
	}
}

// TestRuntimeConcurrentJobRuns 验证同一 Job 在不同 Runtime 并发执行时创建独立实例和结果.
func TestRuntimeConcurrentJobRuns(t *testing.T) {
	var mu sync.Mutex
	var sinks []*runtimeTestSink
	job, err := NewJobDraft("reusable").FromFunc(func() (Source[int], error) {
		return newRuntimeTestSource(1, 2), nil
	}).SinkToFunc(func() (Sink[int], error) {
		sink := &runtimeTestSink{}
		mu.Lock()
		sinks = append(sinks, sink)
		mu.Unlock()
		return sink, nil
	}).Build()
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			if err := NewRuntime(runtimeOptions()).Run(context.Background(), job); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if len(sinks) != 4 {
		t.Fatal(len(sinks))
	}
	for _, sink := range sinks {
		slices.Sort(sink.values)
		if !reflect.DeepEqual(sink.values, []int{
			1,
			2,
		}) {
			t.Fatal(sink.values)
		}
	}
}

// TestRuntimeAlreadyRunningRejectsSecondRun 验证运行期间再次 Run 被拒绝, 第一次运行仍可正常取消.
func TestRuntimeAlreadyRunningRejectsSecondRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := newRuntimeTestSource()
		source.readFn = func() (ReadResult[int], error) {
			return ReadResult[int]{
				State: ReadUnavailable,
			}, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		job := runtimeJob(t, source, &runtimeTestSink{})
		runtime := NewRuntime(runtimeOptions())
		done := make(chan error, 1)
		go func() {
			done <- runtime.Run(ctx, job)
		}()
		synctest.Wait()
		var used *RuntimeAlreadyUsedError
		if err := runtime.Run(context.Background(), job); !errors.As(err, &used) {
			t.Fatal(err)
		}
		cancel()
		synctest.Wait()
		requireRunError(t, <-done, context.Canceled)
	})
}
