package yaspe_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/hsldymq/yaspe"
	"github.com/hsldymq/yaspe/connector/memory"
)

// TestRuntimeMemoryPipeline 验证真实内存两端串联 Filter 和多输出转换, 所有输入完成后才返回并按输入统计成功.
func TestRuntimeMemoryPipeline(t *testing.T) {
	for _, parallelism := range []int{
		1,
		4,
	} {
		var output *memory.Sink[int]
		job, err := yaspe.NewJobDraft("memory pipeline").FromFunc(func() (yaspe.Source[int], error) {
			source, producer, err := memory.NewSource[int](4)
			if err != nil {
				return nil, err
			}
			for _, value := range []int{
				1,
				2,
				3,
				4,
			} {
				if err := producer.Submit(context.Background(), value); err != nil {
					return nil, err
				}
			}
			return source, producer.Finish()
		}).Filter(func(value int) bool {
			return value%2 == 0
		}).FlatMap(func(value int) []int {
			return []int{
				value,
				value * 10,
			}
		}).SinkToFunc(func() (yaspe.Sink[int], error) {
			var err error
			output, err = memory.NewSink[int](memory.SinkOptions{})
			return output, err
		}).Build()
		if err != nil {
			t.Fatal(err)
		}
		var completed atomic.Int64
		runtime := yaspe.NewRuntime(yaspe.RuntimeOptions{
			Parallelism:      parallelism,
			MaxInFlightWorks: 3,
			OnWorkCompleted: func() {
				completed.Add(1)
			},
		})
		if err := runtime.Run(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		if completed.Load() != 4 {
			t.Fatalf("completed=%d, want 4", completed.Load())
		}
		var values []int
		for _, group := range output.Groups() {
			if len(group) != 2 || group[1].Value != group[0].Value*10 {
				t.Fatalf("partial/interleaved group: %v", group)
			}
			values = append(values, group[0].Value, group[1].Value)
		}
		if parallelism == 1 && !reflect.DeepEqual(values, []int{
			2,
			20,
			4,
			40,
		}) {
			t.Fatal(values)
		}
		slices.Sort(values)
		if !reflect.DeepEqual(values, []int{
			2,
			4,
			20,
			40,
		}) {
			t.Fatal(values)
		}
	}
}

// TestRuntimeMemoryProducerFailureUnderBackpressure 验证输入容量耗尽时生产端仍可独立报告失败并取消处理.
func TestRuntimeMemoryProducerFailureUnderBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var producer *memory.SourceProducer[int]
		ready := make(chan struct{})
		entered := make(chan struct{})
		cause := errors.New("producer failed")
		job, err := yaspe.NewJobDraft("source failure").FromFunc(func() (yaspe.Source[int], error) {
			source, p, err := memory.NewSource[int](1)
			producer = p
			close(ready)
			return source, err
		}).MapWithContext(func(ctx context.Context, value int) (int, error) {
			close(entered)
			<-ctx.Done()
			return 0, ctx.Err()
		}).SinkToFunc(func() (yaspe.Sink[int], error) {
			return memory.NewSink[int](memory.SinkOptions{})
		}).Build()
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			done <- yaspe.NewRuntime(yaspe.RuntimeOptions{
				Parallelism:      1,
				MaxInFlightWorks: 1,
			}).Run(context.Background(), job)
		}()
		<-ready
		if err := producer.Submit(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		<-entered
		if err := producer.Fail(cause); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := <-done; !errors.Is(err, cause) {
			t.Fatalf("got %v", err)
		}
	})
}
