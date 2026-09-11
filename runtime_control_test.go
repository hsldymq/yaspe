package yaspe

import (
	"context"
	"errors"
	"reflect"
	goruntime "runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// TestRuntimeCancellationAroundAdmission 验证取消发生在预留后或 ready 绑定后时, 不启动处理且恰好释放一次容量.
func TestRuntimeCancellationAroundAdmission(t *testing.T) {
	for _, afterBind := range []bool{
		false,
		true,
	} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := newRuntimeTestSource(1)
			sink := &runtimeTestSink{}
			runtime := NewRuntime(runtimeOptions())
			runtime.hooks = &runtimeHooks{}
			if afterBind {
				runtime.hooks.afterBind = cancel
			} else {
				runtime.hooks.afterReserve = cancel
			}
			requireRunError(t, runtime.Run(ctx, runtimeJob(t, source, sink)), context.Canceled)
			wantAdmitted := uint64(0)
			if afterBind {
				wantAdmitted = 1
			}
			if runtime.stats.admitted != wantAdmitted || runtime.stats.cancelled != wantAdmitted || runtime.stats.inFlight != 0 || sink.calls != 0 {
				t.Fatal(runtime.stats)
			}
		})
	}
}

// TestRuntimeBackpressureBoundsAdmission 验证慢 Sink 填满有界责任容量后 Source 不继续读取, 容量恢复后全部输入完成.
func TestRuntimeBackpressureBoundsAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := newRuntimeTestSource(1, 2, 3, 4, 5, 6)
		blocked, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() {
			close(release)
		})
		defer unblock()
		sink := &runtimeTestSink{}
		sink.acceptFn = func(ctx context.Context, items []SinkItem[int], reporter SinkResultReporter[int]) (SinkAcceptStatus, error) {
			if sink.calls == 1 {
				close(blocked)
				<-release
			}
			results := make([]SinkItemResult[int], len(items))
			for i, item := range items {
				results[i] = SinkItemResult[int]{
					Item:    item,
					Outcome: SinkSucceeded,
				}
			}
			reporter.Report(results)
			return SinkAccepted, nil
		}
		options := runtimeOptions()
		options.Parallelism = 2
		options.MaxInFlightWorks = 3
		runtime := NewRuntime(options)
		job := runtimeJob(t, source, sink)
		done := make(chan error, 1)
		go func() {
			done <- runtime.Run(context.Background(), job)
		}()
		<-blocked
		synctest.Wait()
		if source.next != 3 {
			t.Fatalf("read %d, want exactly 3 bounded inputs", source.next)
		}
		unblock()
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if runtime.stats.peak > 3 || runtime.stats.succeeded != 6 || runtime.stats.inFlight != 0 {
			t.Fatal(runtime.stats)
		}
	})
}

// TestRuntimeCapacityNotificationDuringAccept 验证容量通知发生在 Backpressured 返回前时不会丢失唤醒或忙轮询.
func TestRuntimeCapacityNotificationDuringAccept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sink := &runtimeTestSink{}
		sink.acceptFn = func(ctx context.Context, items []SinkItem[int], reporter SinkResultReporter[int]) (SinkAcceptStatus, error) {
			if sink.calls == 1 {
				sink.context.CapacityNotifier().NotifyAvailable()
				return SinkBackpressured, nil
			}
			reporter.Report([]SinkItemResult[int]{
				{
					Item:    items[0],
					Outcome: SinkSucceeded,
				},
			})
			return SinkAccepted, nil
		}
		if err := NewRuntime(runtimeOptions()).Run(context.Background(), runtimeJob(t, newRuntimeTestSource(1), sink)); err != nil {
			t.Fatal(err)
		}
		if sink.calls != 2 {
			t.Fatalf("calls=%d", sink.calls)
		}
		sink.context.CapacityNotifier().NotifyAvailable()
	})
}

// TestRuntimeCancelsCapacityWait 验证 Sink 持续背压时宿主取消能终止等待, 不重新提交或误计成功.
func TestRuntimeCancelsCapacityWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sink := &runtimeTestSink{
			acceptFn: func(context.Context, []SinkItem[int], SinkResultReporter[int]) (SinkAcceptStatus, error) {
				return SinkBackpressured, nil
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runtime := NewRuntime(runtimeOptions())
		done := make(chan error, 1)
		job := runtimeJob(t, newRuntimeTestSource(1), sink)
		go func() {
			done <- runtime.Run(ctx, job)
		}()
		synctest.Wait()
		if sink.calls != 1 {
			t.Fatalf("busy polling: %d", sink.calls)
		}
		cancel()
		synctest.Wait()
		requireRunError(t, <-done, context.Canceled)
		if runtime.stats.succeeded != 0 || runtime.stats.inFlight != 0 {
			t.Fatal(runtime.stats)
		}
	})
}

// TestRuntimeEnteredSinkDrainsAfterCancellation 验证已进入的 Sink 接管不随处理取消被撤回, 可信成功仍计入完成.
func TestRuntimeEnteredSinkDrainsAfterCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() {
			close(release)
		})
		defer unblock()
		sink := &runtimeTestSink{
			acceptFn: func(ctx context.Context, items []SinkItem[int], reporter SinkResultReporter[int]) (SinkAcceptStatus, error) {
				close(entered)
				<-release
				if ctx.Err() != nil {
					return SinkAcceptStatusInvalid, ctx.Err()
				}
				reporter.Report([]SinkItemResult[int]{
					{
						Item:    items[0],
						Outcome: SinkSucceeded,
					},
				})
				return SinkAccepted, nil
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runtime := NewRuntime(runtimeOptions())
		done := make(chan error, 1)
		job := runtimeJob(t, newRuntimeTestSource(1), sink)
		go func() {
			done <- runtime.Run(ctx, job)
		}()
		<-entered
		cancel()
		synctest.Wait()
		if len(done) != 0 {
			t.Fatal("Run returned before entered Sink completed")
		}
		unblock()
		synctest.Wait()
		requireRunError(t, <-done, context.Canceled)
		if runtime.stats.succeeded != 1 || runtime.stats.inFlight != 0 {
			t.Fatal(runtime.stats)
		}
	})
}

// TestRuntimeShutdownTimeoutFencesLateWork 验证不响应取消的 Operator 导致有界超时, 迟到输出不进入 Sink 或改变冻结统计.
func TestRuntimeShutdownTimeoutFencesLateWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() {
			close(release)
		})
		defer unblock()
		var saved Collector[int]
		op := &runtimeTestOperator{
			processFn: func(ctx context.Context, input Record[int], output Collector[int]) error {
				saved = output
				close(entered)
				<-release
				return output.Emit(input)
			},
		}
		sink := &runtimeTestSink{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runtime := NewRuntime(runtimeOptions())
		done := make(chan error, 1)
		job := runtimeJob(t, newRuntimeTestSource(1), sink, op)
		go func() {
			done <- runtime.Run(ctx, job)
		}()
		<-entered
		cancel()
		err := <-done
		runErr := requireRunError(t, err, context.Canceled)
		var timeout *ShutdownTimeoutError
		if !errors.As(err, &timeout) || len(timeout.Stages()) == 0 {
			t.Fatalf("timeout diagnostics: %v", err)
		}
		before, secondary := runtime.stats, runErr.Secondary()
		unblock()
		synctest.Wait()
		if sink.calls != 0 || runtime.stats != before || !reflect.DeepEqual(runErr.Secondary(), secondary) {
			t.Fatal("late work changed frozen result")
		}
		if !errors.Is(saved.Emit(Record[int]{
			Value: 2,
		}), ErrCollectorClosed) {
			t.Fatal("late collector remained valid")
		}
	})
}

// TestRuntimeCloseUsesOneDeadline 验证各组件 Close 共用一个绝对截止时间, 原始处理失败不会被 Close 错误替换.
func TestRuntimeCloseUsesOneDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failure, cleanup := errors.New("process failed"), errors.New("close failed")
		var deadlines []time.Time
		close := func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Error("missing close deadline")
			}
			deadlines = append(deadlines, deadline)
			return cleanup
		}
		source := newRuntimeTestSource(1)
		source.closeFn = close
		op := &runtimeTestOperator{
			processFn: func(context.Context, Record[int], Collector[int]) error {
				return failure
			},
			closeFn: close,
		}
		sink := &runtimeTestSink{
			closeFn: close,
		}
		err := NewRuntime(runtimeOptions()).Run(context.Background(), runtimeJob(t, source, sink, op))
		runErr := requireRunError(t, err, failure)
		if runErr.Primary() != failure || !errors.Is(err, cleanup) || len(deadlines) != 3 {
			t.Fatalf("%v, %v", err, deadlines)
		}
		for _, deadline := range deadlines {
			if deadline != deadlines[0] {
				t.Fatal("deadline restarted")
			}
		}
	})
}

// TestRuntimeLateSourceReportsAreFenced 验证 Source 报告入口拒绝 nil, 重复报告不替换首因, 结束后迟到报告不能复活运行.
func TestRuntimeLateSourceReportsAreFenced(t *testing.T) {
	cause := errors.New("source failed in Open")
	source := newRuntimeTestSource()
	source.openFn = func(ctx SourceContext) error {
		if !errors.Is(ctx.ReportFailure(nil), ErrNilSourceFailure) {
			t.Error("nil failure accepted")
		}
		if err := ctx.ReportFailure(cause); err != nil {
			t.Error(err)
		}
		if err := ctx.ReportFailure(errors.New("second")); err != nil {
			t.Error(err)
		}
		return nil
	}
	err := NewRuntime(runtimeOptions()).Run(context.Background(), runtimeJob(t, source, &runtimeTestSink{}))
	runErr := requireRunError(t, err, cause)
	before := runErr.Secondary()
	if !errors.Is(source.context.ReportFailure(errors.New("late")), ErrSourceReporterClosed) {
		t.Fatal("late report accepted")
	}
	if !reflect.DeepEqual(before, runErr.Secondary()) {
		t.Fatal("late report mutated snapshot")
	}
}

// TestRuntimeInvalidReadResults 验证无效状态和未支持的恢复位置信息不能被当作正常输入处理.
func TestRuntimeInvalidReadResults(t *testing.T) {
	for _, result := range []ReadResult[int]{
		{},
		{
			State: 99,
		},
		{
			State: ReadReady,
			Value: 1,
			Positioned: &PositionedRead{
				Split:    "x",
				Position: 1,
			},
		},
	} {
		source := newRuntimeTestSource()
		source.readFn = func() (ReadResult[int], error) {
			return result, nil
		}
		err := NewRuntime(runtimeOptions()).Run(context.Background(), runtimeJob(t, source, &runtimeTestSink{}))
		var invalid *InvalidReadResultError
		if !errors.As(err, &invalid) {
			t.Fatalf("got %v", err)
		}
	}
}

// TestRuntimeSinkProtocolValidation 验证同步报告的缺失, 伪造, 矛盾与非法状态被拒绝, 相同结果重复报告不会重复计数.
func TestRuntimeSinkProtocolValidation(t *testing.T) {
	cases := []struct {
		name   string
		accept func(context.Context, []SinkItem[int], SinkResultReporter[int]) (SinkAcceptStatus, error)
		valid  bool
	}{
		{
			"missing",
			func(context.Context, []SinkItem[int], SinkResultReporter[int]) (SinkAcceptStatus, error) {
				return SinkAccepted, nil
			},
			false,
		},
		{
			"invalid status",
			func(context.Context, []SinkItem[int], SinkResultReporter[int]) (SinkAcceptStatus, error) {
				return SinkAcceptStatusInvalid, nil
			},
			false,
		},
		{
			"foreign",
			func(ctx context.Context, items []SinkItem[int], r SinkResultReporter[int]) (SinkAcceptStatus, error) {
				r.Report([]SinkItemResult[int]{
					{
						Item:    SinkItem[int]{},
						Outcome: SinkSucceeded,
					},
				})
				return SinkAccepted, nil
			},
			false,
		},
		{
			"reported rejection",
			func(ctx context.Context, items []SinkItem[int], r SinkResultReporter[int]) (SinkAcceptStatus, error) {
				r.Report([]SinkItemResult[int]{
					{
						Item:    items[0],
						Outcome: SinkSucceeded,
					},
				})
				return SinkBackpressured, nil
			},
			false,
		},
		{
			"conflict",
			func(ctx context.Context, items []SinkItem[int], r SinkResultReporter[int]) (SinkAcceptStatus, error) {
				r.Report([]SinkItemResult[int]{
					{
						Item:    items[0],
						Outcome: SinkSucceeded,
					},
				})
				r.Report([]SinkItemResult[int]{
					{
						Item:    items[0],
						Outcome: SinkUnknown,
						Err:     errors.New("unknown"),
					},
				})
				return SinkAccepted, nil
			},
			false,
		},
		{
			"duplicate",
			func(ctx context.Context, items []SinkItem[int], r SinkResultReporter[int]) (SinkAcceptStatus, error) {
				r.Report([]SinkItemResult[int]{
					{
						Item:    items[0],
						Outcome: SinkSucceeded,
					},
				})
				r.Report([]SinkItemResult[int]{
					{
						Item:    items[0],
						Outcome: SinkSucceeded,
					},
				})
				return SinkAccepted, nil
			},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime := NewRuntime(runtimeOptions())
			err := runtime.Run(context.Background(), runtimeJob(t, newRuntimeTestSource(1), &runtimeTestSink{
				acceptFn: tc.accept,
			}))
			if tc.valid {
				if err != nil || runtime.stats.succeeded != 1 {
					t.Fatalf("%v %+v", err, runtime.stats)
				}
			} else if err == nil || runtime.stats.succeeded != 0 {
				t.Fatalf("%v %+v", err, runtime.stats)
			}
		})
	}
}

// TestRuntimeCloseTimeoutAndLateReturn 验证组件 Close 忽略取消时 Run 仍按总预算返回, 迟到关闭不会修改错误快照.
func TestRuntimeCloseTimeoutAndLateReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() {
			close(release)
		})
		defer unblock()
		sink := &runtimeTestSink{
			closeFn: func(context.Context) error {
				<-release
				return errors.New("late close failure")
			},
		}
		runtime := NewRuntime(runtimeOptions())
		err := runtime.Run(context.Background(), runtimeJob(t, newRuntimeTestSource(), sink))
		var timeout *ShutdownTimeoutError
		if !errors.As(err, &timeout) {
			t.Fatalf("got %v", err)
		}
		runErr := err.(*RunError)
		before := runErr.Secondary()
		unblock()
		synctest.Wait()
		if !reflect.DeepEqual(before, runErr.Secondary()) {
			t.Fatal("late Close changed RunError")
		}
	})
}

// TestRuntimeHostDeadlineReachesProcess 验证宿主 deadline 可被 Operator 观察, 到期后协作退出并保留超时根因.
func TestRuntimeHostDeadlineReachesProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		deadline, _ := ctx.Deadline()
		op := &runtimeTestOperator{
			processFn: func(ctx context.Context, input Record[int], output Collector[int]) error {
				if got, ok := ctx.Deadline(); !ok || got != deadline {
					return errors.New("host deadline lost")
				}
				<-ctx.Done()
				return ctx.Err()
			},
		}
		err := NewRuntime(runtimeOptions()).Run(ctx, runtimeJob(t, newRuntimeTestSource(1), &runtimeTestSink{}, op))
		requireRunError(t, err, context.DeadlineExceeded)
	})
}

// TestRuntimeFactoryFailureAndPanic 验证启动期工厂错误或 panic 不会调用 Open, 不会被当作输入级重试.
func TestRuntimeFactoryFailureAndPanic(t *testing.T) {
	cause := errors.New("factory failed")
	for _, panicFactory := range []bool{
		false,
		true,
	} {
		job, err := NewJobDraft("factory error").FromFunc(func() (Source[int], error) {
			t.Error("source factory should not be called")
			return newRuntimeTestSource(), nil
		}).SinkToFunc(func() (Sink[int], error) {
			if panicFactory {
				panic("factory panic")
			}
			return nil, cause
		}).Build()
		if err != nil {
			t.Fatal(err)
		}
		err = NewRuntime(runtimeOptions()).Run(context.Background(), job)
		if panicFactory {
			var p *PanicError
			if !errors.As(err, &p) || len(p.Stack()) == 0 {
				t.Fatal(err)
			}
		} else {
			requireRunError(t, err, cause)
		}
	}
}

// TestRuntimeReadFailureKeepsSourceCause 验证读取失败和独立报告共享首因, 不把读取错误归入某个输入的成功结果.
func TestRuntimeReadFailureKeepsSourceCause(t *testing.T) {
	for _, reported := range []bool{
		false,
		true,
	} {
		cause := errors.New("read failed")
		source := newRuntimeTestSource()
		source.readFn = func() (ReadResult[int], error) {
			if reported {
				if err := source.context.ReportFailure(cause); err != nil {
					return ReadResult[int]{}, err
				}
			}
			return ReadResult[int]{
				State: ReadReady,
				Value: 1,
			}, cause
		}
		runtime := NewRuntime(runtimeOptions())
		err := runtime.Run(context.Background(), runtimeJob(t, source, &runtimeTestSink{}))
		runErr := requireRunError(t, err, cause)
		if runErr.Primary() != cause || len(runErr.Secondary()) != 0 || runtime.stats.admitted != 0 || runtime.stats.inFlight != 0 {
			t.Fatalf("%v %+v", err, runtime.stats)
		}
	}
}

// TestRuntimeUnsupportedSourceControls 验证尚不支持的动态 ownership 调用明确失败, 关闭后入口返回失效错误.
func TestRuntimeUnsupportedSourceControls(t *testing.T) {
	for _, action := range []string{
		"assign",
		"lost",
		"revoke",
	} {
		source := newRuntimeTestSource()
		source.openFn = func(ctx SourceContext) error {
			control := ctx.ControlReporter()
			switch action {
			case "assign":
				return control.Assign(context.Background(), []SplitID{
					"x",
				})
			case "lost":
				return control.Lost(context.Background(), []SplitID{
					"x",
				})
			default:
				_, err := control.BeginRevoke(context.Background(), []SplitID{
					"x",
				}, RevokeOptions{})
				return err
			}
		}
		err := NewRuntime(runtimeOptions()).Run(context.Background(), runtimeJob(t, source, &runtimeTestSink{}))
		requireRunError(t, err, ErrUnsupportedSource)
		if err := source.context.ControlReporter().Lost(context.Background(), []SplitID{
			"x",
		}); !errors.Is(err, ErrSourceReporterClosed) {
			t.Fatal(err)
		}
	}
}

// TestRuntimeSinkFailuresKeepCausalOrder 验证同步报告多个失败时首个失败为主因, 其余失败作为附加错误且不会重试接管.
func TestRuntimeSinkFailuresKeepCausalOrder(t *testing.T) {
	first, second := errors.New("first item failed"), errors.New("second item failed")
	sink := &runtimeTestSink{
		acceptFn: func(ctx context.Context, items []SinkItem[int], reporter SinkResultReporter[int]) (SinkAcceptStatus, error) {
			reporter.Report([]SinkItemResult[int]{
				{
					Item:    items[1],
					Outcome: SinkUnknown,
					Err:     first,
				},
				{
					Item:    items[0],
					Outcome: SinkNotApplied,
					Err:     second,
				},
			})
			return SinkAccepted, nil
		},
	}
	op := &runtimeTestOperator{
		processFn: func(ctx context.Context, input Record[int], output Collector[int]) error {
			if err := output.Emit(input); err != nil {
				return err
			}
			return output.Emit(input)
		},
	}
	runtime := NewRuntime(runtimeOptions())
	err := runtime.Run(context.Background(), runtimeJob(t, newRuntimeTestSource(1), sink, op))
	runErr := requireRunError(t, err, first)
	if runErr.Primary() != first || !errors.Is(err, second) || sink.calls != 1 || runtime.stats.succeeded != 0 {
		t.Fatalf("%v %+v", err, runtime.stats)
	}
}

// TestRuntimeReadyAfterCancellationIsStillBound 验证 Source 在取消后仍返回 ready 时, 输入先绑定再取消, 不当作空 reservation 释放.
func TestRuntimeReadyAfterCancellationIsStillBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		source := newRuntimeTestSource()
		source.readFn = func() (ReadResult[int], error) {
			cancel()
			return ReadResult[int]{
				State: ReadReady,
				Value: 1,
			}, nil
		}
		execution := NewRuntime(runtimeOptions())
		sink := &runtimeTestSink{}
		requireRunError(t, execution.Run(ctx, runtimeJob(t, source, sink)), context.Canceled)
		if execution.stats.admitted != 1 || execution.stats.cancelled != 1 || execution.stats.inFlight != 0 || sink.calls != 0 {
			t.Fatal(execution.stats)
		}
	})
}

// TestRuntimeUnexpectedGoroutineExitFailsJob 验证组件调用 Goexit 时不会造成 Worker 静默消失或作业永久等待.
func TestRuntimeUnexpectedGoroutineExitFailsJob(t *testing.T) {
	op := &runtimeTestOperator{
		processFn: func(context.Context, Record[int], Collector[int]) error {
			goruntime.Goexit()
			return nil
		},
	}
	execution := NewRuntime(runtimeOptions())
	err := execution.Run(context.Background(), runtimeJob(t, newRuntimeTestSource(1, 2), &runtimeTestSink{}, op))
	var runErr *RunError
	if !errors.As(err, &runErr) || execution.stats.succeeded != 0 || execution.stats.inFlight != 0 {
		t.Fatalf("%v %+v", err, execution.stats)
	}
}
