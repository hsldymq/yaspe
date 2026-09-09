package memory_test

import (
	"context"
	"fmt"

	"github.com/hsldymq/yaspe"
	"github.com/hsldymq/yaspe/connector/memory"
)

type exampleSourceContext struct{}

func (exampleSourceContext) LifecycleContext() context.Context {
	return context.Background()
}

func (exampleSourceContext) ControlReporter() yaspe.SourceControlReporter {
	return nil
}

func (exampleSourceContext) ReportFailure(err error) error {
	return nil
}

// ExampleNewSource 演示通过 Producer 提交和声明结束, 再由 Source 读取已缓存记录并正常结束.
func ExampleNewSource() {
	source, producer, err := memory.NewSource[int](2)
	if err != nil {
		panic(err)
	}
	if err := producer.Submit(context.Background(), 42); err != nil {
		panic(err)
	}
	if err := producer.Finish(); err != nil {
		panic(err)
	}

	// 独立使用时由读取方提供环境并管理生命周期.
	if err := source.Open(exampleSourceContext{}); err != nil {
		panic(err)
	}
	defer source.Close(context.Background())
	for {
		result, err := source.TryRead()
		if err != nil {
			panic(err)
		}
		switch result.State {
		case yaspe.ReadReady:
			fmt.Println(result.Value)
		case yaspe.ReadUnavailable:
			<-source.Available()
		case yaspe.ReadFinished:
			fmt.Println("finished")
			return
		}
	}
	// Output:
	// 42
	// finished
}
