package memory

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/hsldymq/yaspe"
)

type testSourceContext struct {
	ctx       context.Context
	mu        sync.Mutex
	reports   []error
	reportErr error
	onReport  func(error)
}

func (c *testSourceContext) LifecycleContext() context.Context {
	return c.ctx
}

func (*testSourceContext) ControlReporter() yaspe.SourceControlReporter {
	return nil
}

func (c *testSourceContext) ReportFailure(err error) error {
	c.mu.Lock()
	c.reports = append(c.reports, err)
	c.mu.Unlock()
	if c.onReport != nil {
		c.onReport(err)
	}
	return c.reportErr
}

func newPair[T any](t *testing.T, capacity int) (*Source[T], *SourceProducer[T]) {
	t.Helper()
	source, producer, err := NewSource[T](capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return source, producer
}

func openSource[T any](t *testing.T, source *Source[T]) *testSourceContext {
	t.Helper()
	runtime := &testSourceContext{ctx: context.Background()}
	if err := source.Open(runtime); err != nil {
		t.Fatal(err)
	}
	return runtime
}

func requireError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("got error %v, want %v", got, want)
	}
}

func requireRead[T any](t *testing.T, source *Source[T], state yaspe.ReadState, value T) {
	t.Helper()
	result, err := source.TryRead()
	if err != nil {
		t.Fatal(err)
	}
	if result.State != state || !reflect.DeepEqual(result.Value, value) || result.Positioned != nil {
		t.Fatalf("TryRead() = %+v, want state %v, value %v and no position", result, state, value)
	}
}

func consumeNotification(t *testing.T, available <-chan struct{}) {
	t.Helper()
	select {
	case _, ok := <-available:
		if !ok {
			t.Fatal("Available channel was closed")
		}
	default:
		t.Fatal("missing availability notification")
	}
}

func requireNoNotification(t *testing.T, available <-chan struct{}) {
	t.Helper()
	select {
	case _, ok := <-available:
		t.Fatalf("unexpected availability notification, channel open = %v", ok)
	default:
	}
}

// TestNewSource 验证容量必须为正数, 每次构造得到独立缓冲和容量为 1 的通知 channel.
func TestNewSource(t *testing.T) {
	for _, capacity := range []int{-1, 0} {
		source, producer, err := NewSource[int](capacity)
		requireError(t, err, ErrInvalidCapacity)
		if source != nil || producer != nil {
			t.Fatal("invalid capacity returned usable handles")
		}
	}
	first, firstProducer := newPair[int](t, 1)
	second, _ := newPair[int](t, 1)
	var _ yaspe.Source[int] = first
	if first.Available() == second.Available() || cap(first.Available()) != 1 {
		t.Fatal("sources must have independent capacity-1 notifications")
	}
	requireError(t, firstProducer.Submit(context.Background(), 42), nil)
	openSource(t, first)
	openSource(t, second)
	requireRead(t, first, yaspe.ReadReady, 42)
	requireRead(t, second, yaspe.ReadUnavailable, 0)
}

// TestSourceOpenAndClose 验证 Open 参数及重复调用, 读取的生命周期边界, 幂等 Close 和缓存释放, 以及通知 channel 始终不变且不关闭.
func TestSourceOpenAndClose(t *testing.T) {
	source, producer := newPair[int](t, 1)
	available := source.Available()
	_, err := source.TryRead()
	requireError(t, err, ErrSourceNotOpen)
	requireError(t, source.Open(nil), ErrNilContext)
	requireError(t, source.Open(&testSourceContext{}), ErrNilContext)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	requireError(t, source.Open(&testSourceContext{ctx: cancelled}), context.Canceled)
	runtime := openSource(t, source)
	requireError(t, source.Open(runtime), ErrSourceAlreadyOpen)
	requireRead(t, source, yaspe.ReadUnavailable, 0)
	requireError(t, producer.Submit(context.Background(), 1), nil)
	requireError(t, source.Close(cancelled), nil)
	requireError(t, source.Close(context.Background()), nil)
	_, err = source.TryRead()
	requireError(t, err, ErrSourceClosed)
	requireError(t, source.Open(runtime), ErrSourceClosed)
	requireError(t, producer.Submit(context.Background(), 2), ErrSourceClosed)
	requireError(t, producer.Finish(), ErrSourceClosed)
	requireError(t, producer.Fail(errors.New("late")), ErrSourceClosed)
	if source.Available() != available {
		t.Fatal("Close replaced Available")
	}
	consumeNotification(t, available)
	requireNoNotification(t, available)
	if source.state.values != nil || source.state.runtime != nil || source.state.lifecycle != nil {
		t.Fatal("Close retained buffered data or runtime environment")
	}
}

// TestInvalidHandles 验证 nil 和零值 Source 或 Producer 返回明确错误, 不暴露可用的通知 channel.
func TestInvalidHandles(t *testing.T) {
	for _, source := range []*Source[int]{nil, {}} {
		requireError(t, source.Open(&testSourceContext{ctx: context.Background()}), ErrInvalidSource)
		requireError(t, source.Close(context.Background()), ErrInvalidSource)
		_, err := source.TryRead()
		requireError(t, err, ErrInvalidSource)
		if source.Available() != nil {
			t.Fatal("invalid Source returned an availability channel")
		}
	}
	for _, producer := range []*SourceProducer[int]{nil, {}} {
		requireError(t, producer.Submit(context.Background(), 1), ErrInvalidSource)
		requireError(t, producer.Finish(), ErrInvalidSource)
		requireError(t, producer.Fail(errors.New("failure")), ErrInvalidSource)
	}
}

// TestSourceFIFOAndReferenceTransfer 验证缓冲环绕后的 FIFO 顺序和原始引用交接, 并确保读取后缓冲不再持有该引用.
func TestSourceFIFOAndReferenceTransfer(t *testing.T) {
	source, producer := newPair[*int](t, 2)
	openSource(t, source)
	first, second, third := new(int), new(int), new(int)
	*first, *second, *third = 1, 2, 3
	requireError(t, producer.Submit(context.Background(), first), nil)
	requireError(t, producer.Submit(context.Background(), second), nil)
	result, err := source.TryRead()
	requireError(t, err, nil)
	if result.State != yaspe.ReadReady || result.Value != first {
		t.Fatal("first value was lost, copied or reordered")
	}
	*result.Value = 99
	for _, retained := range source.state.values {
		if retained == first {
			t.Fatal("read value is still retained in the source buffer")
		}
	}
	requireError(t, producer.Submit(context.Background(), third), nil)
	for _, want := range []*int{second, third} {
		result, err := source.TryRead()
		requireError(t, err, nil)
		if result.State != yaspe.ReadReady || result.Value != want {
			t.Fatalf("FIFO after buffer wrap: got %+v, want %p", result, want)
		}
	}
	requireRead(t, source, yaspe.ReadUnavailable, (*int)(nil))
}

// TestFinishDrainsPreloadedInput 验证 Open 前可预装输入并 Finish, 拒绝后续提交, 读完缓存后永久返回 finished.
func TestFinishDrainsPreloadedInput(t *testing.T) {
	source, producer := newPair[int](t, 2)
	requireError(t, producer.Submit(context.Background(), 1), nil)
	requireError(t, producer.Submit(context.Background(), 2), nil)
	requireError(t, producer.Finish(), nil)
	requireError(t, producer.Finish(), nil)
	requireError(t, producer.Submit(context.Background(), 3), ErrSourceFinished)
	openSource(t, source)
	requireRead(t, source, yaspe.ReadReady, 1)
	requireRead(t, source, yaspe.ReadReady, 2)
	for range 2 {
		requireRead(t, source, yaspe.ReadFinished, 0)
	}
	requireError(t, producer.Finish(), nil)
	requireError(t, producer.Fail(errors.New("too late")), ErrSourceFinished)
}

// TestFailureOverridesBufferedInputAndPreservesFirstCause 验证 Open 前, 接收中及结束中的失败都优先于缓存, 只报告首因一次, 报告错误和 Close 不替换首因.
func TestFailureOverridesBufferedInputAndPreservesFirstCause(t *testing.T) {
	for _, when := range []string{"before open", "while accepting", "while finishing"} {
		t.Run(when, func(t *testing.T) {
			source, producer := newPair[int](t, 1)
			runtime := &testSourceContext{ctx: context.Background(), reportErr: errors.New("report rejected")}
			if when != "before open" {
				requireError(t, source.Open(runtime), nil)
			}
			requireError(t, producer.Submit(context.Background(), 42), nil)
			if when == "while finishing" {
				requireError(t, producer.Finish(), nil)
			}
			first, second := errors.New("first"), errors.New("second")
			requireError(t, producer.Fail(first), nil)
			requireError(t, producer.Fail(second), nil)
			if when == "before open" {
				requireError(t, source.Open(runtime), nil)
			}
			if len(runtime.reports) != 1 || runtime.reports[0] != first {
				t.Fatalf("failure report = %v, want first cause once", runtime.reports)
			}
			result, err := source.TryRead()
			if err != first || result.State == yaspe.ReadReady {
				t.Fatalf("TryRead() = %+v, %v; want original failure", result, err)
			}
			for _, err := range []error{producer.Submit(context.Background(), 7), producer.Finish()} {
				requireError(t, err, ErrSourceFailed)
				requireError(t, err, first)
			}
			requireError(t, source.Close(context.Background()), nil)
			if source.state.failure != first {
				t.Fatal("Close replaced failure cause")
			}
		})
	}
}

// TestFailureAfterLastValueBeforeFinished 验证最后一条记录已交接但 Reader 尚未发布 finished 时, Fail 仍可使 Source 失败.
func TestFailureAfterLastValueBeforeFinished(t *testing.T) {
	source, producer := newPair[int](t, 1)
	openSource(t, source)
	requireError(t, producer.Submit(context.Background(), 1), nil)
	requireError(t, producer.Finish(), nil)
	requireRead(t, source, yaspe.ReadReady, 1)
	cause := errors.New("failed before finished was published")
	requireError(t, producer.Fail(cause), nil)
	_, err := source.TryRead()
	requireError(t, err, cause)
}

// TestFailureReportDoesNotHoldSourceLock 验证失败报告可以同步触发读取和关闭, 不因 Source 持锁而死锁.
func TestFailureReportDoesNotHoldSourceLock(t *testing.T) {
	source, producer := newPair[int](t, 1)
	cause := errors.New("failed")
	runtime := &testSourceContext{
		ctx: context.Background(),
		onReport: func(err error) {
			requireError(t, err, cause)
			_, readErr := source.TryRead()
			requireError(t, readErr, cause)
			requireError(t, source.Close(context.Background()), nil)
		},
	}
	requireError(t, source.Open(runtime), nil)
	requireError(t, producer.Fail(cause), nil)
}
