package memory

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hsldymq/yaspe"
)

type acceptResult struct {
	status yaspe.SinkAcceptStatus
	err    error
}

func requireAccepted(t *testing.T, result acceptResult) {
	t.Helper()
	if result.err != nil || result.status != yaspe.SinkAccepted {
		t.Fatalf("Accept() = %v, %v", result.status, result.err)
	}
}

// TestSinkSnapshotAroundAcceptance 验证在接管线性化前后读取的快照分别为空和完整组, 不会看到部分输出.
func TestSinkSnapshotAroundAcceptance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sink := newTestSink[int](t, SinkOptions{})
		openTestSink(t, sink)
		entered, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() {
			close(release)
		})
		defer unblock()
		sink.state.beforeAccept = func() {
			close(entered)
			<-release
		}
		done := make(chan acceptResult, 1)
		go func() {
			status, err := sink.Accept(context.Background(), sinkItems(1, 2, 3), &testSinkReporter[int]{})
			done <- acceptResult{status: status, err: err}
		}()
		<-entered
		synctest.Wait()
		before := sink.Groups()
		if len(before) != 0 || len(sink.Records()) != 0 {
			t.Fatal("snapshot included input before handoff")
		}
		unblock()
		synctest.Wait()
		requireAccepted(t, <-done)
		if len(before) != 0 || len(sink.Groups()) != 1 || len(sink.Groups()[0]) != 3 {
			t.Fatal("snapshot did not preserve its observation point or group boundary")
		}
	})
}

// TestSinkCloseWinsBeforeAcceptance 验证 Close 先于接管生效时, 已进入但尚未接管的调用全拒且不报告成功.
func TestSinkCloseWinsBeforeAcceptance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sink := newTestSink[int](t, SinkOptions{})
		openTestSink(t, sink)
		entered, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() {
			close(release)
		})
		defer unblock()
		sink.state.beforeAccept = func() {
			close(entered)
			<-release
		}
		reporter := &testSinkReporter[int]{}
		done := make(chan acceptResult, 1)
		go func() {
			status, err := sink.Accept(context.Background(), sinkItems(1, 2), reporter)
			done <- acceptResult{status: status, err: err}
		}()
		<-entered
		synctest.Wait()
		requireError(t, sink.Close(context.Background()), nil)
		unblock()
		synctest.Wait()
		result := <-done
		requireError(t, result.err, ErrSinkClosed)
		if result.status == yaspe.SinkAccepted || len(sink.Groups()) != 0 || len(reporter.batches) != 0 {
			t.Fatal("input was retained or reported after Close won")
		}
	})
}

// TestSinkCloseWaitsForAcceptedReport 验证接管先生效时 Close 等待同步报告结束, 期间拒绝新接管且快照只含完整组.
func TestSinkCloseWaitsForAcceptedReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sink := newTestSink[int](t, SinkOptions{})
		openTestSink(t, sink)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() {
			close(release)
		})
		defer unblock()
		reporter := &testSinkReporter[int]{
			onReport: func([]yaspe.SinkItemResult[int]) {
				close(entered)
				<-release
			},
		}
		accepted := make(chan acceptResult, 1)
		go func() {
			status, err := sink.Accept(ctx, sinkItems(1, 2), reporter)
			accepted <- acceptResult{status: status, err: err}
		}()
		<-entered
		cancel()
		closed := make(chan error, 2)
		for range 2 {
			go func() {
				closed <- sink.Close(context.Background())
			}()
		}
		synctest.Wait()
		if len(closed) != 0 || len(accepted) != 0 {
			t.Fatal("Close or Accept returned before synchronous report completed")
		}
		want := []yaspe.Record[int]{{Value: 1}, {Value: 2}}
		if !reflect.DeepEqual(sink.Records(), want) {
			t.Fatal("accepted group was not visible as a whole")
		}
		_, err := sink.Accept(context.Background(), sinkItems(3), &testSinkReporter[int]{})
		requireError(t, err, ErrSinkClosed)
		unblock()
		synctest.Wait()
		requireAccepted(t, <-accepted)
		for range 2 {
			requireError(t, <-closed, nil)
		}
		if !reflect.DeepEqual(sink.Records(), want) {
			t.Fatal("post-accept cancellation or Close changed results")
		}
	})
}

// TestSinkCloseDeadlineKeepsResultsStable 验证同步报告未返回时 Close 受 deadline 限制, 后续报告完成也不会改变已保存结果.
func TestSinkCloseDeadlineKeepsResultsStable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sink := newTestSink[int](t, SinkOptions{})
		openTestSink(t, sink)
		entered, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() {
			close(release)
		})
		defer unblock()
		reporter := &testSinkReporter[int]{
			onReport: func([]yaspe.SinkItemResult[int]) {
				close(entered)
				<-release
			},
		}
		accepted := make(chan acceptResult, 1)
		go func() {
			status, err := sink.Accept(context.Background(), sinkItems(1), reporter)
			accepted <- acceptResult{status: status, err: err}
		}()
		<-entered
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		requireError(t, sink.Close(ctx), context.DeadlineExceeded)
		before := sink.Groups()
		_, err := sink.Accept(context.Background(), sinkItems(2), &testSinkReporter[int]{})
		requireError(t, err, ErrSinkClosed)
		unblock()
		synctest.Wait()
		requireAccepted(t, <-accepted)
		requireError(t, sink.Close(context.Background()), nil)
		if !reflect.DeepEqual(sink.Groups(), before) {
			t.Fatal("late report changed closed results")
		}
	})
}

// TestSinkFailureDoesNotRollBackAcceptedGroups 验证其他接管失败时, 已保存且正在报告的成功组保留, 关闭后新输入被拒绝.
func TestSinkFailureDoesNotRollBackAcceptedGroups(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failure := errors.New("second accept failed")
		sink := newTestSink[int](t, SinkOptions{Failures: []error{nil, failure}})
		openTestSink(t, sink)
		entered, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() {
			close(release)
		})
		defer unblock()
		reporter := &testSinkReporter[int]{
			onReport: func([]yaspe.SinkItemResult[int]) {
				close(entered)
				<-release
			},
		}
		done := make(chan acceptResult, 1)
		go func() {
			status, err := sink.Accept(context.Background(), sinkItems(1, 2), reporter)
			done <- acceptResult{status: status, err: err}
		}()
		<-entered
		rejected := &testSinkReporter[int]{}
		_, err := sink.Accept(context.Background(), sinkItems(3, 4), rejected)
		requireError(t, err, failure)
		if len(rejected.batches) != 0 || len(sink.Groups()) != 1 {
			t.Fatal("failure affected another group's ownership")
		}
		unblock()
		synctest.Wait()
		requireAccepted(t, <-done)
		requireError(t, sink.Close(context.Background()), nil)
		_, err = sink.Accept(context.Background(), sinkItems(5), rejected)
		requireError(t, err, ErrSinkClosed)
		if len(sink.Records()) != 2 {
			t.Fatal("failed or stopped group was retained")
		}
	})
}

// TestSinkConcurrentAcceptSnapshotsAndClose 验证并发接管, 快照和关闭时每组全收或全拒, 最终快照与成功调用一一对应且保持组内顺序.
func TestSinkConcurrentAcceptSnapshotsAndClose(t *testing.T) {
	for _, mode := range []string{"close during calls", "close after calls"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const workers, iterations = 8, 20
				sink := newTestSink[int](t, SinkOptions{})
				openTestSink(t, sink)
				start := make(chan struct{})
				type outcome struct {
					id      int
					result  acceptResult
					reports int
				}
				done := make(chan outcome, workers*iterations)
				var writers sync.WaitGroup
				writersDone := make(chan struct{})
				for worker := range workers {
					writers.Go(func() {
						<-start
						for i := range iterations {
							id := worker*iterations + i
							reporter := &testSinkReporter[int]{}
							status, err := sink.Accept(context.Background(), sinkItems(id, -id-1), reporter)
							done <- outcome{id: id, result: acceptResult{status: status, err: err}, reports: len(reporter.batches)}
						}
					})
				}
				viewErrors := make(chan string, 1)
				go func() {
					<-start
					for range iterations {
						for _, group := range sink.Groups() {
							if len(group) != 2 || group[1].Value != -group[0].Value-1 {
								viewErrors <- "snapshot contained a partial or interleaved group"
								return
							}
						}
					}
					records := sink.Records()
					if len(records)%2 != 0 {
						viewErrors <- "flattened snapshot contained a partial group"
						return
					}
					for i := 0; i < len(records); i += 2 {
						if records[i+1].Value != -records[i].Value-1 {
							viewErrors <- "flattened snapshot interleaved groups"
							return
						}
					}
					viewErrors <- ""
				}()
				closed := make(chan error, 1)
				go func() {
					<-start
					if mode == "close after calls" {
						<-writersDone
					}
					closed <- sink.Close(context.Background())
				}()
				close(start)
				writers.Wait()
				close(writersDone)
				synctest.Wait()
				requireError(t, <-closed, nil)
				if err := <-viewErrors; err != "" {
					t.Fatal(err)
				}
				accepted := make(map[int]bool)
				for range workers * iterations {
					outcome := <-done
					if outcome.result.err == nil {
						requireAccepted(t, outcome.result)
						if outcome.reports != 1 {
							t.Fatal("successful group was not reported once")
						}
						accepted[outcome.id] = true
					} else {
						requireError(t, outcome.result.err, ErrSinkClosed)
						if outcome.reports != 0 {
							t.Fatal("rejected group reported success")
						}
					}
				}
				if mode == "close after calls" && len(accepted) != workers*iterations {
					t.Fatal("concurrent successful input was lost")
				}
				groups := sink.Groups()
				if len(groups) != len(accepted) {
					t.Fatalf("got %d groups, want %d", len(groups), len(accepted))
				}
				next := make([]int, workers)
				for _, group := range groups {
					if len(group) != 2 || !accepted[group[0].Value] || group[1].Value != -group[0].Value-1 {
						t.Fatalf("missing, duplicate or partial group: %v", group)
					}
					worker, sequence := group[0].Value/iterations, group[0].Value%iterations
					if sequence != next[worker] {
						t.Fatal("sequential calls from a single worker were reordered")
					}
					next[worker]++
					delete(accepted, group[0].Value)
				}
				if !reflect.DeepEqual(sink.Groups(), groups) {
					t.Fatal("closed snapshot changed")
				}
			})
		})
	}
}
