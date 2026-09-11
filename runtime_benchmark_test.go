package yaspe

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const benchmarkBatch = 128

func benchmarkCompute(value int) int {
	for range 128 {
		value = (value*1664525 + 1013904223) & 0x7fffffff
	}
	return value
}

// BenchmarkRuntime 测量固定批次的完整运行, 覆盖资源矩阵, 零/多输出及 CPU 和真实墙钟等待.
// 输入使用只读内存 slice, Sink 消费并核对结果而不累计所有输出.
func BenchmarkRuntime(b *testing.B) {
	parallelisms := []int{
		1,
		runtime.GOMAXPROCS(0),
		2 * runtime.GOMAXPROCS(0),
	}
	parallelisms = slices.Compact(parallelisms)
	for _, workload := range []string{
		"overhead",
		"cpu",
		"io100us",
	} {
		outputs := []int{
			1,
		}
		if workload == "overhead" {
			outputs = []int{
				0,
				1,
				4,
			}
		}
		for _, outputCount := range outputs {
			for _, parallelism := range parallelisms {
				for _, factor := range []int{
					1,
					2,
					8,
				} {
					b.Run(fmt.Sprintf("%s/P%d/N%d/out%d", workload, parallelism, parallelism*factor, outputCount), func(b *testing.B) {
						benchmarkRuntime(b, workload, outputCount, parallelism, parallelism*factor, nil)
					})
				}
			}
		}
	}
}

func benchmarkRuntime(b *testing.B, workload string, outputs, parallelism, capacity int, hooks *runtimeHooks) {
	values := make([]int, benchmarkBatch)
	var expectedSum int64
	for i := range values {
		values[i] = i
		value := i
		if workload == "cpu" {
			value = benchmarkCompute(value)
		}
		for j := range outputs {
			expectedSum += int64(value + j)
		}
	}
	var completed atomic.Int64
	var itemCount, groupCount int
	var sum int64
	job, err := NewJobDraft("benchmark").FromFunc(func() (Source[int], error) {
		return newRuntimeTestSource(values...), nil
	}).FlatMapWithContext(func(ctx context.Context, value int) ([]int, error) {
		if workload == "cpu" {
			value = benchmarkCompute(value)
		}
		if workload == "io100us" {
			time.Sleep(100 * time.Microsecond)
		}
		result := make([]int, outputs)
		for i := range result {
			result[i] = value + i
		}
		return result, nil
	}).SinkToFunc(func() (Sink[int], error) {
		itemCount, groupCount, sum = 0, 0, 0
		return &runtimeTestSink{
			acceptFn: func(ctx context.Context, items []SinkItem[int], reporter SinkResultReporter[int]) (SinkAcceptStatus, error) {
				groupCount++
				results := make([]SinkItemResult[int], len(items))
				for i, item := range items {
					itemCount++
					sum += int64(item.Record.Value)
					results[i] = SinkItemResult[int]{
						Item:    item,
						Outcome: SinkSucceeded,
					}
				}
				reporter.Report(results)
				return SinkAccepted, nil
			},
		}, nil
	}).Build()
	if err != nil {
		b.Fatal(err)
	}
	options := RuntimeOptions{
		Parallelism:      parallelism,
		MaxInFlightWorks: capacity,
		OnWorkCompleted: func() {
			completed.Add(1)
		},
	}
	b.ReportAllocs()
	var memoryStart, memoryEnd runtime.MemStats
	runtime.ReadMemStats(&memoryStart)
	peak := 0
	for b.Loop() {
		completed.Store(0)
		execution := NewRuntime(options)
		execution.hooks = hooks
		if err := execution.Run(context.Background(), job); err != nil {
			b.Fatal(err)
		}
		groups := benchmarkBatch
		if outputs == 0 {
			groups = 0
		}
		if completed.Load() != benchmarkBatch || itemCount != benchmarkBatch*outputs || groupCount != groups || sum != expectedSum || execution.stats.inFlight != 0 || execution.stats.peak > capacity {
			b.Fatalf("incorrect execution: completed=%d items=%d groups=%d sum=%d stats=%+v", completed.Load(), itemCount, groupCount, sum, execution.stats)
		}
		peak = max(peak, execution.stats.peak)
	}
	runtime.ReadMemStats(&memoryEnd)
	works := float64(b.N * benchmarkBatch)
	b.ReportMetric(works/b.Elapsed().Seconds(), "works/s")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/works, "ns/work")
	b.ReportMetric(float64(memoryEnd.TotalAlloc-memoryStart.TotalAlloc)/works, "B/work")
	b.ReportMetric(float64(memoryEnd.Mallocs-memoryStart.Mallocs)/works, "allocs/work")
	b.ReportMetric(float64(peak), "peak-inflight")
}

// BenchmarkRuntimeLatency 对每 16 个接纳输入采样一次, 保留最近 4096 个样本计算延迟分位数.
// 时间戳和样本写入会增加开销, 与不采样的吞吐基准分开报告.
func BenchmarkRuntimeLatency(b *testing.B) {
	b.Run("sample1of16_recent4096", func(b *testing.B) {
		var mu sync.Mutex
		samples := make([]time.Duration, 4096)
		count := 0
		hooks := &runtimeHooks{
			traceEvery: 16,
			onCompleted: func(elapsed time.Duration) {
				mu.Lock()
				samples[count%len(samples)] = elapsed
				count++
				mu.Unlock()
			},
		}
		p := runtime.GOMAXPROCS(0)
		benchmarkRuntime(b, "overhead", 1, p, 2*p, hooks)
		samples = samples[:min(count, len(samples))]
		if len(samples) == 0 {
			b.Fatal("no latency samples")
		}
		slices.Sort(samples)
		for _, percentile := range []int{
			50,
			95,
			99,
		} {
			index := (len(samples) - 1) * percentile / 100
			b.ReportMetric(float64(samples[index].Nanoseconds()), fmt.Sprintf("p%d-ns", percentile))
		}
		b.ReportMetric(float64(len(samples)), "samples")
	})
}

// BenchmarkRuntimeShutdown 单独测量宿主取消到 Run 返回的时间, 不与正常吞吐指标混合.
func BenchmarkRuntimeShutdown(b *testing.B) {
	var entered chan struct{}
	job, err := NewJobDraft("shutdown benchmark").FromFunc(func() (Source[int], error) {
		return newRuntimeTestSource(1), nil
	}).MapWithContext(func(ctx context.Context, value int) (int, error) {
		close(entered)
		<-ctx.Done()
		return 0, ctx.Err()
	}).SinkToFunc(func() (Sink[int], error) {
		return &runtimeTestSink{}, nil
	}).Build()
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		b.StopTimer()
		ctx, cancel := context.WithCancel(context.Background())
		entered = make(chan struct{})
		execution := NewRuntime(RuntimeOptions{
			Parallelism:      1,
			MaxInFlightWorks: 1,
		})
		done := make(chan error, 1)
		go func() {
			done <- execution.Run(ctx, job)
		}()
		<-entered
		b.StartTimer()
		cancel()
		err := <-done
		b.StopTimer()
		if !errors.Is(err, context.Canceled) || execution.stats.succeeded != 0 || execution.stats.cancelled != 1 || execution.stats.inFlight != 0 {
			b.Fatalf("shutdown: %v %+v", err, execution.stats)
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N), "ns/shutdown")
}

// BenchmarkRuntimeEmptyRun 单独测量无输入作业的组件创建, 打开, 调度启动和关闭成本.
func BenchmarkRuntimeEmptyRun(b *testing.B) {
	job, err := NewJobDraft("empty benchmark").FromFunc(func() (Source[int], error) {
		return newRuntimeTestSource(), nil
	}).SinkToFunc(func() (Sink[int], error) {
		return &runtimeTestSink{}, nil
	}).Build()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		execution := NewRuntime(RuntimeOptions{
			Parallelism:      1,
			MaxInFlightWorks: 1,
		})
		if err := execution.Run(context.Background(), job); err != nil {
			b.Fatal(err)
		}
		if execution.stats.admitted != 0 || execution.stats.inFlight != 0 {
			b.Fatal(execution.stats)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N), "ns/startup-close")
}
