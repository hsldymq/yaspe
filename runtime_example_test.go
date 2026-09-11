package yaspe_test

import (
	"context"
	"fmt"

	"github.com/hsldymq/yaspe"
	"github.com/hsldymq/yaspe/connector/memory"
)

// ExampleRuntime_Run 演示通过工厂创建内存输入和输出, 执行转换并在运行结束后读取结果.
func ExampleRuntime_Run() {
	var output *memory.Sink[int]
	job, err := yaspe.NewJobDraft("double").FromFunc(func() (yaspe.Source[int], error) {
		source, producer, err := memory.NewSource[int](2)
		if err != nil {
			return nil, err
		}
		for _, value := range []int{
			1,
			2,
		} {
			if err := producer.Submit(context.Background(), value); err != nil {
				return nil, err
			}
		}
		return source, producer.Finish()
	}).Map(func(value int) int {
		return value * 2
	}).SinkToFunc(func() (yaspe.Sink[int], error) {
		var err error
		output, err = memory.NewSink[int](memory.SinkOptions{})
		return output, err
	}).Build()
	if err != nil {
		panic(err)
	}
	runtime := yaspe.NewRuntime(yaspe.RuntimeOptions{
		Parallelism:      1,
		MaxInFlightWorks: 2,
	})
	if err := runtime.Run(context.Background(), job); err != nil {
		panic(err)
	}
	for _, record := range output.Records() {
		fmt.Println(record.Value)
	}
	// Output:
	// 2
	// 4
}
