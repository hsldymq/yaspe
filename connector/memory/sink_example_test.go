package memory_test

import (
	"context"
	"fmt"

	"github.com/hsldymq/yaspe"
	"github.com/hsldymq/yaspe/connector/memory"
)

type exampleSinkContext struct{}

func (exampleSinkContext) LifecycleContext() context.Context {
	return context.Background()
}

func (exampleSinkContext) CapacityNotifier() yaspe.SinkCapacityNotifier {
	return nil
}

type exampleSinkReporter struct{}

func (exampleSinkReporter) Report(results []yaspe.SinkItemResult[int]) {
	fmt.Println("completed items:", len(results))
}

// ExampleNewSink 演示独立接管完整输出组, 同步接收成功报告, 以及关闭后读取结果快照.
func ExampleNewSink() {
	sink, err := memory.NewSink[int](memory.SinkOptions{})
	if err != nil {
		panic(err)
	}
	if err := sink.Open(exampleSinkContext{}); err != nil {
		panic(err)
	}
	items := []yaspe.SinkItem[int]{
		{Record: yaspe.Record[int]{Value: 42}},
		{Record: yaspe.Record[int]{Value: 43}},
	}
	status, err := sink.Accept(context.Background(), items, exampleSinkReporter{})
	if err != nil || status != yaspe.SinkAccepted {
		panic("group was not accepted")
	}
	if err := sink.Close(context.Background()); err != nil {
		panic(err)
	}
	for _, record := range sink.Records() {
		fmt.Println(record.Value)
	}
	// Output:
	// completed items: 2
	// 42
	// 43
}
