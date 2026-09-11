package yaspe

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
)

// openedComponent 记录已成功 Open 的组件, 按登记顺序的逆序执行 Close.
// 未成功 Open 的组件不进入此清理列表.
type openedComponent struct {
	// 包含组件类别及 lane/节点位置, 用于关闭超时诊断.
	name string
	// 绑定该运行实例的清理函数, 使用统一关闭 context 调用.
	close func(context.Context) error
}

func (s *runState) execute(job Job) {
	var opened []openedComponent
	var sourceDone, workersDone <-chan struct{}
	defer func() {
		if value := recover(); value != nil {
			// 先交给外层监督边界记录内部故障, 不在损坏状态下继续启动组件.
			s.fail(&InternalPanicError{
				PanicError{
					component: "runtime",
					value:     value,
					stack:     debug.Stack(),
				},
			})
		}
		if sourceDone != nil {
			<-sourceDone
		}
		if workersDone != nil {
			<-workersDone
		}
		if sourceDone != nil {
			for work := range s.pending {
				s.finish(work, "cancelled")
			}
		}
		if workersDone != nil {
			for work := range s.terminal {
				s.finish(work, "cancelled")
			}
		}
		s.cleanup(opened)
	}()
	createSink, ok := job.nodes[len(job.nodes)-1].adapter.(sinkInstantiator)
	if !ok {
		s.fail(ErrInvalidJob)
		return
	}
	s.phase("runtime", "sink factory")
	sink, err := createSink.createSink()
	if err != nil {
		s.fail(err)
		return
	}
	chains := make([][]runtimeOperator, s.options.Parallelism)
	for lane := range chains {
		for _, node := range job.nodes[1 : len(job.nodes)-1] {
			if !s.canStart() {
				return
			}
			factory, ok := node.adapter.(operatorInstantiator)
			if !ok {
				s.fail(ErrInvalidJob)
				return
			}
			s.phase("runtime", fmt.Sprintf("operator factory lane %d node %d", lane, node.id))
			op, err := factory.createOperator()
			if err != nil {
				s.fail(err)
				return
			}
			chains[lane] = append(chains[lane], op)
		}
	}
	if !s.canStart() {
		return
	}
	createSource, ok := job.nodes[0].adapter.(sourceInstantiator)
	if !ok {
		s.fail(ErrInvalidJob)
		return
	}
	s.phase("runtime", "source factory")
	source, err := createSource.createSource()
	if err != nil {
		s.fail(err)
		return
	}
	open := func(name string, fn func() error, close func(context.Context) error) bool {
		if !s.canStart() {
			return false
		}
		s.phase("runtime", name+" Open")
		if err := fn(); err != nil {
			s.fail(err)
			return false
		}
		opened = append(opened, openedComponent{
			name:  name,
			close: close,
		})
		return true
	}
	if !open("sink", func() error {
		return sink.open(s.sinkContext)
	}, sink.close) {
		return
	}
	for lane, chain := range chains {
		for i, op := range chain {
			if !open(fmt.Sprintf("operator lane %d node %d", lane, i+1), func() error {
				return op.open(s.process)
			}, op.close) {
				return
			}
		}
	}
	if !open("source", func() error {
		return source.open(s.sourceContext)
	}, source.close) {
		return
	}
	if !s.canStart() {
		return
	}
	var available <-chan struct{}
	if err := invokeUser("source Available", func() error {
		available = source.available()
		return nil
	}); err != nil {
		s.fail(err)
		return
	}
	if available == nil || cap(available) != 1 {
		s.fail(fmt.Errorf("yaspe: Available must be a capacity-1 channel"))
		return
	}

	var workers []<-chan struct{}
	for lane, chain := range chains {
		name := fmt.Sprintf("worker %d", lane)
		workers = append(workers, s.spawn(name, func() {
			s.worker(name, chain)
		}))
	}
	workersDone = s.spawn("worker group", func() {
		for _, done := range workers {
			<-done
		}
		close(s.terminal)
	})
	sourceDone = s.spawn("source", func() {
		defer close(s.pending)
		s.admit(source, available)
	})
	s.phase("runtime", "sink coordinator")
	s.coordinate(sink)
	s.phase("runtime", "waiting for source and workers")
}

func (s *runState) cleanup(opened []openedComponent) {
	s.mu.Lock()
	s.startShutdownLocked()
	deadline := s.deadline
	s.mu.Unlock()
	s.cancelSource()
	s.cancelProcess()
	ctx, cancel := context.WithDeadline(context.WithoutCancel(s.host), deadline)
	defer cancel()
	for i := len(opened) - 1; i >= 0; i-- {
		if s.isFenced() {
			return
		}
		component := opened[i]
		s.phase("runtime", component.name+" Close")
		if component.name == "sink" {
			s.cancelSink()
		}
		if err := component.close(ctx); err != nil {
			s.fail(err)
		}
		if component.name == "source" {
			s.sourceContext.fence()
		}
	}
	if err := s.host.Err(); err != nil {
		s.fail(err)
	}
}

func (s *runState) admit(source runtimeSource, available <-chan struct{}) {
	for {
		s.phase("source", "waiting for work capacity")
		work, ok := s.reserve()
		if !ok {
			return
		}
		if s.hooks != nil && s.hooks.afterReserve != nil {
			s.hooks.afterReserve()
		}
		if !s.canStart() {
			s.finish(work, "cancelled")
			return
		}
		s.phase("source", "TryRead")
		result, err := source.read()
		if err != nil {
			s.finish(work, "cancelled")
			s.sourceContext.readFailure(err)
			return
		}
		if result.state == ReadReady {
			// ready 交接先绑定预留 slot, 之后才观察失败或取消.
			s.bind(work, result.value)
			if s.hooks != nil && s.hooks.afterBind != nil {
				s.hooks.afterBind()
			}
		}
		if result.positioned || result.state < ReadReady || result.state > ReadFinished {
			s.finish(work, "failed")
			s.fail(&InvalidReadResultError{
				State: result.state,
			})
			return
		}
		switch result.state {
		case ReadReady:
			if !s.canStart() {
				s.finish(work, "cancelled")
				return
			}
			select {
			case s.pending <- work:
			case <-s.process.Done():
				s.finish(work, "cancelled")
				return
			}
		case ReadUnavailable:
			s.finish(work, "cancelled")
			s.phase("source", "waiting for availability")
			select {
			case _, ok := <-available:
				if !ok {
					s.fail(fmt.Errorf("yaspe: Available channel was closed"))
					return
				}
			case <-s.process.Done():
				return
			}
		case ReadFinished:
			s.finish(work, "cancelled")
			s.mu.Lock()
			s.startShutdownLocked()
			s.mu.Unlock()
			return
		}
	}
}

func (s *runState) worker(name string, chain []runtimeOperator) {
	for {
		s.phase(name, "waiting for input")
		select {
		case <-s.process.Done():
			return
		case work, ok := <-s.pending:
			if !ok {
				return
			}
			s.processWork(name, chain, work)
		}
	}
}

func (s *runState) processWork(name string, chain []runtimeOperator, work *runtimeWork) {
	owned := true
	defer func() {
		if owned {
			s.finish(work, "cancelled")
		}
	}()
	if s.hooks != nil && s.hooks.beforeProcess != nil {
		s.hooks.beforeProcess()
	}
	if !s.canStart() {
		return
	}
	s.phase(name, "operator Process")
	var call func(int, runtimeValue) error
	call = func(i int, value runtimeValue) error {
		if err := s.process.Err(); err != nil {
			return err
		}
		if i == len(chain) {
			work.outputs = append(work.outputs, value)
			return nil
		}
		return chain[i].process(s.process, value, func(output runtimeValue) error {
			return call(i+1, output)
		})
	}
	if err := call(0, work.input); err != nil {
		if errors.Is(err, context.Canceled) && !s.canStart() {
			return
		}
		s.fail(err)
		s.finish(work, "failed")
		owned = false
		return
	}
	if !s.canStart() {
		return
	}
	if len(work.outputs) == 0 {
		s.finish(work, "success")
		owned = false
		return
	}
	s.phase(name, "waiting for terminal queue")
	select {
	case s.terminal <- work:
		owned = false
	case <-s.process.Done():
	}
}

func (s *runState) coordinate(sink runtimeSink) {
	for {
		select {
		case <-s.process.Done():
			return
		case work, ok := <-s.terminal:
			if !ok {
				return
			}
			s.handoff(sink, work)
		}
	}
}

func (s *runState) handoff(sink runtimeSink, work *runtimeWork) {
	outcome := "cancelled"
	defer func() {
		s.finish(work, outcome)
	}()
	for {
		if s.hooks != nil && s.hooks.beforeSink != nil {
			s.hooks.beforeSink()
		}
		s.mu.Lock()
		eligible := !s.fenced && s.primary == nil && s.process.Err() == nil
		if eligible {
			work.sinkEntered = true
		}
		s.mu.Unlock()
		if !eligible {
			return
		}
		changed := s.sinkContext.signal.observe()
		s.phase("runtime", "sink Accept")
		err := sink.accept(s.sinkLife, work.outputs)
		if err == nil {
			outcome = "success"
			return
		}
		work.sinkEntered = false
		if !errors.Is(err, errSinkBackpressured) {
			outcome = "failed"
			if failures, ok := err.(*sinkResultErrors); ok {
				s.fail(failures.primary)
				for _, secondary := range failures.secondary {
					s.fail(secondary)
				}
			} else {
				s.fail(err)
			}
			return
		}
		s.phase("runtime", "waiting for sink capacity")
		select {
		case <-changed:
		case <-s.process.Done():
			return
		}
	}
}
