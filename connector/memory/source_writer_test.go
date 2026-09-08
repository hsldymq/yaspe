package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hsldymq/yaspe"
)

// TestSubmitBackpressureAndCapacityRelease 验证缓冲满时 Submit 保持等待, 读取释放容量后提交成功且仅交付一次.
func TestSubmitBackpressureAndCapacityRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source, writer := newPair[int](t, 1)
		openSource(t, source)
		requireError(t, writer.Submit(context.Background(), 1), nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- writer.Submit(ctx, 2)
		}()
		synctest.Wait()
		if len(done) != 0 || source.state.size != 1 {
			t.Fatal("full buffer did not block Submit")
		}
		requireRead(t, source, yaspe.ReadReady, 1)
		synctest.Wait()
		requireError(t, <-done, nil)
		requireRead(t, source, yaspe.ReadReady, 2)
		requireRead(t, source, yaspe.ReadUnavailable, 0)
	})
}

// TestSubmitCancellationDoesNotTransferValue 验证等待中的提交可取消, 失败提交不交接或残留输入, nil context 和 nil 失败原因不改变数据状态.
func TestSubmitCancellationDoesNotTransferValue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source, writer := newPair[*int](t, 1)
		openSource(t, source)
		accepted, rejected := new(int), new(int)
		requireError(t, writer.Submit(context.Background(), accepted), nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- writer.Submit(ctx, rejected)
		}()
		synctest.Wait()
		if len(done) != 0 {
			t.Fatal("Submit did not wait for capacity")
		}
		cancel()
		synctest.Wait()
		requireError(t, <-done, context.Canceled)
		*rejected = 99
		requireRead(t, source, yaspe.ReadReady, accepted)
		requireRead(t, source, yaspe.ReadUnavailable, (*int)(nil))
		requireError(t, writer.Submit(ctx, rejected), context.Canceled)
		requireError(t, writer.Submit(nil, rejected), ErrNilContext)
		requireError(t, writer.Fail(nil), ErrNilFailure)
		requireRead(t, source, yaspe.ReadUnavailable, (*int)(nil))
	})
}

// TestSubmitDeadline 验证满缓冲下 Submit 在调用 deadline 到期时返回超时错误, 不接管该输入.
func TestSubmitDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source, writer := newPair[int](t, 1)
		openSource(t, source)
		requireError(t, writer.Submit(context.Background(), 1), nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- writer.Submit(ctx, 2)
		}()
		requireError(t, <-done, context.DeadlineExceeded)
		requireRead(t, source, yaspe.ReadReady, 1)
		requireRead(t, source, yaspe.ReadUnavailable, 0)
	})
}

// TestAllBlockedSubmittersWakeOnStop 验证 Finish, Fail, Close 和生命周期取消会唤醒所有阻塞生产者, 并返回对应错误.
func TestAllBlockedSubmittersWakeOnStop(t *testing.T) {
	cause := errors.New("source failed")
	cases := []struct {
		name string
		stop func(*Source[int], *SourceWriter[int], context.CancelFunc) error
		want error
	}{
		{
			name: "finish",
			stop: func(_ *Source[int], writer *SourceWriter[int], _ context.CancelFunc) error {
				return writer.Finish()
			},
			want: ErrSourceFinished,
		},
		{
			name: "fail",
			stop: func(_ *Source[int], writer *SourceWriter[int], _ context.CancelFunc) error {
				return writer.Fail(cause)
			},
			want: ErrSourceFailed,
		},
		{
			name: "close",
			stop: func(source *Source[int], _ *SourceWriter[int], _ context.CancelFunc) error {
				return source.Close(context.Background())
			},
			want: ErrSourceClosed,
		},
		{
			name: "lifecycle cancel",
			stop: func(_ *Source[int], _ *SourceWriter[int], cancel context.CancelFunc) error {
				cancel()
				return nil
			},
			want: context.Canceled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				source, writer := newPair[int](t, 1)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				runtime := &testSourceContext{ctx: ctx}
				requireError(t, source.Open(runtime), nil)
				requireError(t, writer.Submit(context.Background(), 0), nil)
				done := make(chan error, 4)
				for i := range 4 {
					go func() {
						done <- writer.Submit(ctx, i+1)
					}()
				}
				synctest.Wait()
				if len(done) != 0 {
					t.Fatal("Submit bypassed full buffer")
				}
				requireError(t, tc.stop(source, writer, cancel), nil)
				synctest.Wait()
				for range 4 {
					err := <-done
					requireError(t, err, tc.want)
					if tc.name == "fail" {
						requireError(t, err, cause)
					}
				}
				if tc.name == "fail" && (len(runtime.reports) != 1 || runtime.reports[0] != cause) {
					t.Fatal("failure was not reported independently of reads")
				}
			})
		})
	}
}

// TestOpenUpdatesAlreadyWaitingSubmitter 验证 Open 前因满缓冲而等待的生产者, 能在 Open 后响应 Source 生命周期取消.
func TestOpenUpdatesAlreadyWaitingSubmitter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source, writer := newPair[int](t, 1)
		requireError(t, writer.Submit(context.Background(), 1), nil)
		done := make(chan error, 1)
		go func() {
			done <- writer.Submit(context.Background(), 2)
		}()
		synctest.Wait()
		if len(done) != 0 {
			t.Fatal("pre-open buffer was not bounded")
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		requireError(t, source.Open(&testSourceContext{ctx: ctx}), nil)
		synctest.Wait()
		cancel()
		synctest.Wait()
		requireError(t, <-done, context.Canceled)
		_, err := source.TryRead()
		requireError(t, err, context.Canceled)
	})
}

// TestAvailabilityDoesNotLoseWakeup 分别验证记录到达, 正常结束, 失败和关闭发生在等待前或等待后时, 通知均可唤醒读取方.
func TestAvailabilityDoesNotLoseWakeup(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Source[int], *SourceWriter[int]) error
		state  yaspe.ReadState
		value  int
		err    error
	}{
		{
			name: "record",
			change: func(_ *Source[int], w *SourceWriter[int]) error {
				return w.Submit(context.Background(), 7)
			},
			state: yaspe.ReadReady,
			value: 7,
		},
		{
			name: "finish",
			change: func(_ *Source[int], w *SourceWriter[int]) error {
				return w.Finish()
			},
			state: yaspe.ReadFinished,
		},
		{
			name: "fail",
			change: func(_ *Source[int], w *SourceWriter[int]) error {
				return w.Fail(ErrSourceFailed)
			},
			err: ErrSourceFailed,
		},
		{
			name: "close",
			change: func(s *Source[int], _ *SourceWriter[int]) error {
				return s.Close(context.Background())
			},
			err: ErrSourceClosed,
		},
	}
	for _, tc := range cases {
		for _, order := range []string{"notify before wait", "wait before notify"} {
			t.Run(tc.name+"/"+order, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					source, writer := newPair[int](t, 2)
					openSource(t, source)
					available := source.Available()
					consumeNotification(t, available)
					requireRead(t, source, yaspe.ReadUnavailable, 0)
					done := make(chan struct{})
					wait := func() {
						<-available
						close(done)
					}
					if order == "notify before wait" {
						requireError(t, tc.change(source, writer), nil)
						go wait()
					} else {
						go wait()
						synctest.Wait()
						select {
						case <-done:
							t.Fatal("reader did not wait for state change")
						default:
						}
						requireError(t, tc.change(source, writer), nil)
					}
					synctest.Wait()
					<-done
					if tc.err != nil {
						_, err := source.TryRead()
						requireError(t, err, tc.err)
					} else {
						requireRead(t, source, tc.state, tc.value)
					}
				})
			})
		}
	}
}

// TestAvailabilityCoalescesAndMayBeStale 验证多次状态变化可合并成一个通知, 消费过期通知后仍须通过 TryRead 判断数据是否可用.
func TestAvailabilityCoalescesAndMayBeStale(t *testing.T) {
	source, writer := newPair[int](t, 2)
	openSource(t, source)
	consumeNotification(t, source.Available())
	requireError(t, writer.Submit(context.Background(), 1), nil)
	requireError(t, writer.Submit(context.Background(), 2), nil)
	requireRead(t, source, yaspe.ReadReady, 1)
	requireRead(t, source, yaspe.ReadReady, 2)
	consumeNotification(t, source.Available())
	requireNoNotification(t, source.Available())
	requireRead(t, source, yaspe.ReadUnavailable, 0)
}

// TestCancelAndCapacityRaceKeepsOwnershipConsistent 验证取消与容量恢复竞争时, Submit 成功才会交付输入, 返回取消错误则不会交付.
func TestCancelAndCapacityRaceKeepsOwnershipConsistent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source, writer := newPair[int](t, 1)
		openSource(t, source)
		requireError(t, writer.Submit(context.Background(), 1), nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- writer.Submit(ctx, 2)
		}()
		synctest.Wait()
		start := make(chan struct{})
		readDone := make(chan yaspe.ReadResult[int], 1)
		readError := make(chan error, 1)
		go func() {
			<-start
			cancel()
		}()
		go func() {
			<-start
			result, err := source.TryRead()
			readDone <- result
			readError <- err
		}()
		close(start)
		synctest.Wait()
		requireError(t, <-readError, nil)
		if result := <-readDone; result.State != yaspe.ReadReady || result.Value != 1 {
			t.Fatalf("initial value was not handed off: %+v", result)
		}
		err := <-done
		if err == nil {
			requireRead(t, source, yaspe.ReadReady, 2)
		} else {
			requireError(t, err, context.Canceled)
		}
		requireRead(t, source, yaspe.ReadUnavailable, 0)
	})
}

// TestConcurrentProducersDeliverOnceInProducerOrder 验证多个生产者在小容量缓冲下的全部输入无丢失或重复, 且保留每个生产者的提交顺序.
func TestConcurrentProducersDeliverOnceInProducerOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type input struct {
			producer int
			sequence int
		}
		const producers, perProducer = 8, 40
		source, writer := newPair[input](t, 3)
		openSource(t, source)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var group sync.WaitGroup
		for producer := range producers {
			group.Go(func() {
				for sequence := range perProducer {
					if err := writer.Submit(ctx, input{producer: producer, sequence: sequence}); err != nil {
						t.Errorf("Submit: %v", err)
						return
					}
				}
			})
		}
		go func() {
			group.Wait()
			if err := writer.Finish(); err != nil {
				t.Errorf("Finish: %v", err)
			}
		}()
		next := make([]int, producers)
		count := 0
		for {
			result, err := source.TryRead()
			if err != nil {
				t.Fatal(err)
			}
			switch result.State {
			case yaspe.ReadReady:
				value := result.Value
				if value.producer < 0 || value.producer >= producers || value.sequence != next[value.producer] {
					t.Fatalf("duplicate, lost or reordered input: %+v, next: %v", value, next)
				}
				next[value.producer]++
				count++
			case yaspe.ReadUnavailable:
				<-source.Available()
			case yaspe.ReadFinished:
				if count != producers*perProducer {
					t.Fatalf("read %d records, want %d", count, producers*perProducer)
				}
				return
			default:
				t.Fatalf("unexpected read state: %v", result.State)
			}
		}
	})
}
