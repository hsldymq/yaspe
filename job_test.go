package yaspe

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type stubSource[T any] struct{ available chan struct{} }

func (*stubSource[T]) Open(SourceContext) error {
	return nil
}

func (*stubSource[T]) Close(context.Context) error {
	return nil
}

func (*stubSource[T]) TryRead() (ReadResult[T], error) {
	return ReadResult[T]{State: ReadFinished}, nil
}

func (s *stubSource[T]) Available() <-chan struct{} {
	return s.available
}

type stubSink[T any] struct{ marker byte }

func (*stubSink[T]) Open(SinkContext) error {
	return nil
}

func (*stubSink[T]) Close(context.Context) error {
	return nil
}

func (*stubSink[T]) Accept(context.Context, []SinkItem[T], SinkResultReporter[T]) (SinkAcceptStatus, error) {
	return SinkAccepted, nil
}

type stubSourceFactory struct{}

func (*stubSourceFactory) Create() (Source[int], error) {
	return &stubSource[int]{}, nil
}

type stubOperatorFactory struct{}

func (*stubOperatorFactory) Create() (Operator[int, int], error) {
	return nil, nil
}

type stubSinkFactory struct{}

func (*stubSinkFactory) Create() (Sink[int], error) {
	return &stubSink[int]{}, nil
}

type nilMapFactory map[string]int

func (nilMapFactory) Create() (Source[int], error) {
	panic("nil map factory must not be called")
}

func sourceForTest() Stream[int] {
	return NewJobDraft("test").FromFunc(func() (Source[int], error) {
		return &stubSource[int]{}, nil
	})
}

func sinkForTest[T any](s Stream[T]) JobBuilder {
	return s.SinkToFunc(func() (Sink[T], error) {
		return &stubSink[T]{}, nil
	})
}

func buildForTest(t *testing.T, b JobBuilder) Job {
	t.Helper()
	job, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// TestBuildIsLazy 验证构建和重复 Build 只保存定义, 不调用组件工厂或业务函数.
func TestBuildIsLazy(t *testing.T) {
	var calls atomic.Int32
	b := NewJobDraft("lazy").FromFunc(func() (Source[int], error) {
		calls.Add(1)
		panic("Create must not run during Build")
	}).Map(func(v int) string {
		calls.Add(1)
		panic("Map must not run during Build")
	}).Filter(func(string) bool {
		calls.Add(1)
		panic("Filter must not run during Build")
	}).FlatMap(func(string) []int {
		calls.Add(1)
		panic("FlatMap must not run during Build")
	}).TransformFunc(func() (Operator[int, int], error) {
		calls.Add(1)
		panic("Create must not run during Build")
	}).SinkToFunc(func() (Sink[int], error) {
		calls.Add(1)
		panic("Create must not run during Build")
	})
	for range 2 {
		j := buildForTest(t, b)
		if j.Name() != "lazy" || len(j.nodes) != 6 {
			t.Fatalf("unexpected job: %+v", j)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("Build invoked user code")
	}
}

// TestBuildAllowsDirectSourceToSinkAndPreservesName 验证 Source 可直接连接 Sink, 并保留原始名称及节点类型信息.
func TestBuildAllowsDirectSourceToSinkAndPreservesName(t *testing.T) {
	j := buildForTest(t, NewJobDraft(" direct ").From(&stubSourceFactory{}).SinkTo(&stubSinkFactory{}))
	if j.Name() != " direct " || len(j.nodes) != 2 {
		t.Fatalf("unexpected job: %+v", j)
	}
	if j.nodes[0].signature.output != reflect.TypeFor[int]() || j.nodes[1].signature.input != reflect.TypeFor[int]() {
		t.Fatal("typed factory metadata lost")
	}
}

// TestBuildRejectsInvalidPublicInputs 验证零值, 空白名称和 nil 工厂或函数被拒绝, 重复 Build 返回一致错误且不污染其他派生路径.
func TestBuildRejectsInvalidPublicInputs(t *testing.T) {
	var source *stubSourceFactory
	var op *stubOperatorFactory
	var sink *stubSinkFactory
	base := sourceForTest()
	cases := []struct {
		name    string
		builder JobBuilder
	}{
		{"zero builder", JobBuilder{}},
		{"zero draft", sinkForTest((JobDraft{}).From(&stubSourceFactory{}))},
		{"zero stream", sinkForTest(Stream[int]{})},
		{"zero stream derived", sinkForTest((Stream[int]{}).Map(func(v int) int {
			return v
		}))},
		{"empty name", sinkForTest(NewJobDraft("").From(&stubSourceFactory{}))},
		{"blank name", sinkForTest(NewJobDraft(" \t\n\u2003").From(&stubSourceFactory{}))},
		{"nil source", sinkForTest(NewJobDraft("x").From[int](nil))},
		{"typed nil source", sinkForTest(NewJobDraft("x").From(source))},
		{"nil map factory", sinkForTest(NewJobDraft("x").From(nilMapFactory(nil)))},
		{"nil source func", sinkForTest(NewJobDraft("x").FromFunc[int](nil))},
		{"nil operator", sinkForTest(base.Transform[int](nil))},
		{"typed nil operator", sinkForTest(base.Transform(op))},
		{"nil operator func", sinkForTest(base.TransformFunc[int](nil))},
		{"nil sink", base.SinkTo(nil)},
		{"typed nil sink", base.SinkTo(sink)},
		{"nil sink func", base.SinkToFunc(nil)},
		{"nil map", sinkForTest(base.Map[int](nil))},
		{"nil context map", sinkForTest(base.MapWithContext[int](nil))},
		{"nil filter", sinkForTest(base.Filter(nil))},
		{"nil context filter", sinkForTest(base.FilterWithContext(nil))},
		{"nil flatmap", sinkForTest(base.FlatMap[int](nil))},
		{"nil context flatmap", sinkForTest(base.FlatMapWithContext[int](nil))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var previous string
			for range 2 {
				job, err := tc.builder.Build()
				var buildErr *BuildError
				if !errors.As(err, &buildErr) {
					t.Fatalf("want BuildError, got %v", err)
				}
				if job.name != "" || len(job.nodes) != 0 {
					t.Fatal("failed Build exposed partial job")
				}
				if previous != "" && previous != err.Error() {
					t.Fatal("Build error changed across calls")
				}
				previous = err.Error()
			}
		})
	}
	// 无效派生不能污染仍被用户持有的旧 Stream.
	buildForTest(t, sinkForTest(base))
}

// TestBuildCopiesTopologyAndAssignsLocalIDs 验证不同派生路径和重复 Build 的节点快照互相独立, 结构序号及上游引用只属于当前 Job.
func TestBuildCopiesTopologyAndAssignsLocalIDs(t *testing.T) {
	base := sourceForTest()
	left := sinkForTest(base.Map(func(v int) string {
		return "left"
	}))
	right := sinkForTest(base.Filter(func(v int) bool {
		return v > 0
	}).FlatMap(func(v int) []int {
		return []int{v, v}
	}))
	a, b, c := buildForTest(t, left), buildForTest(t, left), buildForTest(t, right)
	if len(a.nodes) != 3 || len(b.nodes) != 3 || len(c.nodes) != 4 {
		t.Fatal("derived paths interfered")
	}
	for _, job := range []Job{a, b, c} {
		for i := range job.nodes {
			if job.nodes[i].id != i {
				t.Fatal("structural IDs are not local and ordered")
			}
			if i == 0 && job.nodes[i].upstream != nil {
				t.Fatal("source has an upstream")
			}
			if i > 0 && job.nodes[i].upstream != &job.nodes[i-1] {
				t.Fatal("upstream is outside snapshot")
			}
		}
	}
	if &a.nodes[0] == &b.nodes[0] {
		t.Fatal("Build shares mutable topology storage")
	}
	// 白盒修改一个快照, 证明它不与 Builder 或其他快照共享节点.
	a.nodes[0].id = 999
	a.nodes[1].upstream = nil
	if b.nodes[0].id != 0 || b.nodes[1].upstream != &b.nodes[0] {
		t.Fatal("snapshot aliases another job")
	}
	again := buildForTest(t, left)
	if again.nodes[0].id != 0 || again.nodes[1].upstream == nil {
		t.Fatal("snapshot aliases builder")
	}
	if len(buildForTest(t, sinkForTest(base)).nodes) != 2 {
		t.Fatal("base stream was mutated")
	}
}

// TestConcurrentDerivationAndBuild 验证多个 goroutine 可共享同一 Stream 和 Builder, 并发派生及构建独立 Job.
func TestConcurrentDerivationAndBuild(t *testing.T) {
	base := sourceForTest()
	builder := sinkForTest(base.Map(func(v int) int {
		return v + 1
	}))
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			for range 10 {
				job, err := builder.Build()
				if err != nil || len(job.nodes) != 3 {
					t.Errorf("Build: %v", err)
					return
				}
				fork, err := sinkForTest(base.Map(func(v int) int {
					return v + i
				})).Build()
				if err != nil || len(fork.nodes) != 3 {
					t.Errorf("fork: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// TestBuildRejectsCorruptPrivateTopology 验证 Build 拒绝缺失节点, 非线性结构, 环, 无效 adapter 和不一致的类型信息.
func TestBuildRejectsCorruptPrivateTopology(t *testing.T) {
	cases := []struct {
		name, reason string
		corrupt      func(*JobBuilder)
	}{
		{"missing path", "source and sink", func(b *JobBuilder) {
			b.tail = nil
		}},
		{"missing source", "expected source", func(b *JobBuilder) {
			b.tail.upstream.upstream = nil
		}},
		{"missing sink", "expected sink", func(b *JobBuilder) {
			b.tail = b.tail.upstream
		}},
		{"duplicate source", "expected operator", func(b *JobBuilder) {
			b.tail.upstream = newDefinitionNode(b.tail.upstream, sourceAdapter[int]{&stubSourceFactory{}})
		}},
		{"intermediate sink", "expected operator", func(b *JobBuilder) {
			b.tail.upstream = newDefinitionNode(b.tail.upstream, sinkAdapter[int]{&stubSinkFactory{}})
		}},
		{"cycle", "cycle or repeated", func(b *JobBuilder) {
			b.tail.upstream.upstream.upstream = b.tail
		}},
		{"missing adapter", "missing typed adapter", func(b *JobBuilder) {
			b.tail.adapter = nil
		}},
		{"typed nil adapter", "missing typed adapter", func(b *JobBuilder) {
			b.tail.adapter = (*sinkAdapter[int])(nil)
		}},
		{"bad metadata", "inconsistent adapter", func(b *JobBuilder) {
			b.tail.signature.input = reflect.TypeFor[string]()
		}},
		{"type mismatch", "incompatible upstream", func(b *JobBuilder) {
			b.tail = newDefinitionNode(b.tail.upstream, sinkAdapter[string]{sinkFactoryFunc[string](func() (Sink[string], error) {
				return nil, nil
			})})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := sinkForTest(sourceForTest().Map(func(v int) int {
				return v
			}))
			tc.corrupt(&b)
			_, err := b.Build()
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("want %q, got %v", tc.reason, err)
			}
		})
	}
}

// TestFactoriesRetainedAndCreateIndependentInstances 验证 Build 保留工厂而不执行它, 后续创建时保留原始错误并得到独立 Operator 实例.
func TestFactoriesRetainedAndCreateIndependentInstances(t *testing.T) {
	factoryErr := errors.New("factory failed")
	var calls int
	b := NewJobDraft("factory").FromFunc(func() (Source[int], error) {
		calls++
		return nil, factoryErr
	}).TransformFunc(func() (Operator[int, int], error) {
		return &functionOperator[int, int]{process: func(context.Context, Record[int], Collector[int]) error {
			return nil
		}}, nil
	}).SinkToFunc(func() (Sink[int], error) {
		return &stubSink[int]{}, nil
	})
	job := buildForTest(t, b)
	if calls != 0 {
		t.Fatal("Build created source")
	}
	_, err := job.nodes[0].adapter.(sourceAdapter[int]).factory.Create()
	if !errors.Is(err, factoryErr) || calls != 1 {
		t.Fatal("factory result was not preserved")
	}
	factory := job.nodes[1].adapter.(operatorAdapter[int, int]).factory
	first, _ := factory.Create()
	second, _ := factory.Create()
	if first == second {
		t.Fatal("operator instances shared")
	}
}
