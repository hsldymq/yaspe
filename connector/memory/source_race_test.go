package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/hsldymq/yaspe"
)

// TestSubmitRacesWithTerminalTransitions 验证 Submit 与 Finish, Fail 或 Close 竞争时, 返回结果, 缓存责任和最终读取状态保持一致.
func TestSubmitRacesWithTerminalTransitions(t *testing.T) {
	for _, terminal := range []string{"finish", "fail", "close"} {
		t.Run(terminal, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				source, writer := newPair[int](t, 1)
				openSource(t, source)
				cause := errors.New("failure")
				start := make(chan struct{})
				submitted, stopped := make(chan error, 1), make(chan error, 1)
				go func() {
					<-start
					submitted <- writer.Submit(context.Background(), 42)
				}()
				go func() {
					<-start
					switch terminal {
					case "finish":
						stopped <- writer.Finish()
					case "fail":
						stopped <- writer.Fail(cause)
					case "close":
						stopped <- source.Close(context.Background())
					}
				}()
				close(start)
				synctest.Wait()
				requireError(t, <-stopped, nil)
				submitErr := <-submitted
				switch terminal {
				case "finish":
					if submitErr == nil {
						requireRead(t, source, yaspe.ReadReady, 42)
					} else {
						requireError(t, submitErr, ErrSourceFinished)
					}
					requireRead(t, source, yaspe.ReadFinished, 0)
				case "fail":
					if submitErr != nil {
						requireError(t, submitErr, ErrSourceFailed)
					}
					if (source.state.size == 1) != (submitErr == nil) {
						t.Fatal("Submit result disagrees with ownership transfer")
					}
					_, err := source.TryRead()
					requireError(t, err, cause)
				case "close":
					if submitErr != nil {
						requireError(t, submitErr, ErrSourceClosed)
					}
					_, err := source.TryRead()
					requireError(t, err, ErrSourceClosed)
					if source.state.size != 0 {
						t.Fatal("Close retained unhanded input")
					}
				}
			})
		})
	}
}

// TestFailureRacesWithPublishingFinished 验证 Fail 与 Reader 发布 finished 竞争时只有一个终态生效, 正常结束后不得再报告失败.
func TestFailureRacesWithPublishingFinished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source, writer := newPair[int](t, 1)
		runtime := openSource(t, source)
		requireError(t, writer.Finish(), nil)
		cause := errors.New("failed")
		start := make(chan struct{})
		failed := make(chan error, 1)
		type readResult struct {
			result yaspe.ReadResult[int]
			err    error
		}
		read := make(chan readResult, 1)
		go func() {
			<-start
			failed <- writer.Fail(cause)
		}()
		go func() {
			<-start
			result, err := source.TryRead()
			read <- readResult{result: result, err: err}
		}()
		close(start)
		synctest.Wait()
		failureErr, observed := <-failed, <-read
		if failureErr == nil {
			requireError(t, observed.err, cause)
			if len(runtime.reports) != 1 || runtime.reports[0] != cause {
				t.Fatal("accepted failure was not reported")
			}
		} else {
			requireError(t, failureErr, ErrSourceFinished)
			requireError(t, observed.err, nil)
			if observed.result.State != yaspe.ReadFinished || len(runtime.reports) != 0 {
				t.Fatal("failure rewrote published finished state")
			}
		}
	})
}

// TestOpenAndConcurrentFailuresReportOneCause 验证 Open 与多个 Fail 并发时只报告一个根因, 且 Reader 返回同一个错误.
func TestOpenAndConcurrentFailuresReportOneCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source, writer := newPair[int](t, 1)
		runtime := &testSourceContext{ctx: context.Background()}
		start := make(chan struct{})
		done := make(chan error, 3)
		first, second := errors.New("first contender"), errors.New("second contender")
		go func() {
			<-start
			done <- source.Open(runtime)
		}()
		for _, cause := range []error{first, second} {
			go func() {
				<-start
				done <- writer.Fail(cause)
			}()
		}
		close(start)
		synctest.Wait()
		for range 3 {
			requireError(t, <-done, nil)
		}
		_, err := source.TryRead()
		if err != first && err != second {
			t.Fatalf("unexpected failure: %v", err)
		}
		if len(runtime.reports) != 1 || runtime.reports[0] != err {
			t.Fatalf("Reader and reporter disagree: %v, %v", err, runtime.reports)
		}
	})
}

// TestCloseDoesNotWaitForInProgressFailureReport 验证失败报告暂停在回调中时 Close 仍可完成, 拒绝后续 Fail, 并保留在途报告的根因.
func TestCloseDoesNotWaitForInProgressFailureReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source, writer := newPair[int](t, 1)
		entered := make(chan struct{})
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() {
			close(release)
		})
		defer unblock()
		cause := errors.New("source failed")
		runtime := &testSourceContext{
			ctx: context.Background(),
			onReport: func(error) {
				close(entered)
				<-release
			},
		}
		requireError(t, source.Open(runtime), nil)
		done := make(chan error, 1)
		go func() {
			done <- writer.Fail(cause)
		}()
		<-entered
		synctest.Wait()
		if len(done) != 0 {
			t.Fatal("test did not hold the failure report")
		}
		requireError(t, source.Close(context.Background()), nil)
		requireError(t, writer.Fail(errors.New("late")), ErrSourceClosed)
		if source.state.failure != cause {
			t.Fatal("Close replaced the failure being reported")
		}
		unblock()
		synctest.Wait()
		requireError(t, <-done, nil)
	})
}
