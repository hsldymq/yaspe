package yaspe

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// RuntimeOptions 配置执行资源. 零值 Parallelism 使用 GOMAXPROCS, 零值容量使用两倍并行度.
// ShutdownTimeout 零值使用 30 秒. 负值无效, 配置错误由 Run 返回.
type RuntimeOptions struct {
	Parallelism      int
	MaxInFlightWorks int
	ShutdownTimeout  time.Duration
	// OnWorkCompleted 在每个成功输入完成后调用一次, 零输出也计数.
	// 回调必须快速, 非阻塞且并发安全; panic 不影响作业结果.
	OnWorkCompleted func()
}

// Runtime 是只能运行一次的执行容器. Job 可被不同 Runtime 重复运行.
// 并行度大于一时不保证跨输入输出顺序. 零值无效, 使用 NewRuntime 构造.
type Runtime struct {
	options RuntimeOptions
	// 区分通过 NewRuntime 构造的实例与非法零值.
	configured bool
	// 原子抢占唯一一次 Run, 同时拒绝并发调用和结束后的复用.
	used atomic.Bool
	// Run 返回前保存的统计快照, 用于包内验证, 不随迟到任务继续更新.
	stats executionStats
	// 运行前注入的测试和采样入口, 不属于公开配置.
	hooks *runtimeHooks
}

// NewRuntime 保存资源选项, 不创建组件或 goroutine.
func NewRuntime(options RuntimeOptions) *Runtime {
	return &Runtime{
		options:    options,
		configured: true,
	}
}

// Run 创建并打开组件, 处理所有输入后关闭资源. 非正常结束统一返回 *RunError.
// Source 必须不带 position, Sink 必须在 Accept 返回前报告全组最终结果.
func (r *Runtime) Run(ctx context.Context, job Job) error {
	failed := func(err error) error {
		return &RunError{
			primary: err,
		}
	}
	if r == nil || !r.configured {
		return failed(ErrInvalidRuntime)
	}
	if !r.used.CompareAndSwap(false, true) {
		return failed(&RuntimeAlreadyUsedError{})
	}
	if ctx == nil {
		return failed(fmt.Errorf("%w: nil context", ErrInvalidRuntime))
	}

	options, err := normalizeRuntimeOptions(r.options)
	if err != nil {
		return failed(err)
	}
	if len(job.nodes) < 2 || job.name == "" {
		return failed(ErrInvalidJob)
	}
	if err := ctx.Err(); err != nil {
		return failed(err)
	}

	state := newRunState(ctx, options, r.hooks)
	done := state.spawn("runtime", func() {
		state.execute(job)
	})
	var timeout <-chan time.Time
	var timer *time.Timer
	shutdown := state.shutdown
	hostDone := ctx.Done()
	for {
		select {
		case <-done:
			// execute 的所有可控子任务已回收, 完成后再冻结诊断和统计.
			err := state.freeze(false)
			r.stats = state.statsSnapshot()
			if timer != nil {
				timer.Stop()
			}
			return err
		case <-hostDone:
			state.fail(ctx.Err())
			hostDone = nil
		case <-shutdown:
			state.mu.Lock()
			deadline := state.deadline
			state.mu.Unlock()
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
			shutdown = nil
		case <-timeout:
			if err := ctx.Err(); err != nil {
				state.fail(err)
			} else if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
				state.fail(context.DeadlineExceeded)
			}
			err := state.freeze(true)
			r.stats = state.statsSnapshot()
			return err
		}
	}
}

func normalizeRuntimeOptions(o RuntimeOptions) (RuntimeOptions, error) {
	if o.Parallelism < 0 || o.MaxInFlightWorks < 0 || o.ShutdownTimeout < 0 {
		return o, ErrInvalidRuntime
	}
	if o.Parallelism == 0 {
		o.Parallelism = runtime.GOMAXPROCS(0)
	}
	if o.MaxInFlightWorks == 0 {
		if o.Parallelism > int(^uint(0)>>1)/2 {
			return o, ErrInvalidRuntime
		}
		o.MaxInFlightWorks = 2 * o.Parallelism
	}
	if o.ShutdownTimeout == 0 {
		o.ShutdownTimeout = 30 * time.Second
	}
	return o, nil
}

// executionStats 区分已接管输入和空预留槽, 用于资源断言及基准统计.
// 运行期间由 runState.mu 保护, Run 返回时复制为最终快照.
type executionStats struct {
	// admitted 为累计接管输入数, 其余三个计数分别统计成功, 失败和取消的输入.
	// 统计单位是输入 work, 不是 Operator 输出条数.
	admitted, succeeded, failed, cancelled uint64
	// inFlight 为尚未终结的已接管输入数, peak 为其峰值, reserved 为空预留槽数.
	// inFlight 与 reserved 共同占用 MaxInFlightWorks 容量.
	inFlight, peak, reserved int
}

// runtimeWork 是一个可复用的输入责任槽, 从预留直到输入终结一直占用容量.
// 它随队列交接由 admission, Worker 和 Sink 协调器依次持有, 终结后归还 free.
type runtimeWork struct {
	// Source 交出的类型擦除 Record, 绑定前为 nil, 终结时释放引用.
	input runtimeValue
	// 本次完整 Chain 的末端暂存结果, attempt 失败时整组撤销.
	outputs []runtimeValue
	// 是否已越过 ready 交接点, 用来区分空 reservation 与真实输入.
	admitted bool
	// 是否已进入当前 Sink 接管调用, 使已进入调用的可信成功可在停止期间完成.
	// Backpressured 或 error 返回后清除, 该标记本身不代表接管成功.
	sinkEntered bool
	// 被采样输入的接管时刻, 不是 Operator 开始时刻; 零值表示本输入不采样.
	started time.Time
}

// runtimeHooks 为包内测试提供时序暂停点, 为基准提供延迟采样入口.
// 必须在 Run 前配置且运行期间不修改; 各回调可能由不同执行 goroutine 调用.
type runtimeHooks struct {
	// 取得空槽后, 检查取消和读取 Source 之前执行.
	afterReserve func()
	// ready 已绑定到槽后, 检查停止状态和进入调度队列之前执行.
	afterBind func()
	// Worker 已取得输入, 但尚未确认处理资格时执行.
	beforeProcess func()
	// 每次尝试 Sink 交接前执行, 背压重试也会调用.
	beforeSink func()
	// 仅对成功且被采样的输入报告接管到完成的耗时.
	onCompleted func(time.Duration)
	// 按接管序号每隔多少个输入采样一次, 小于等于 1 时全部采样.
	traceEvery int
}

// runState 持有单次 Run 的执行资源, 失败状态与关闭预算, 不跨运行复用.
// context 和队列引用在构造后固定, 共享状态由 mu 保护, work 内容按队列 ownership 交接.
type runState struct {
	// 宿主提供的 context, 用于保留更早的 deadline 和最终取消原因.
	host    context.Context
	options RuntimeOptions
	hooks   *runtimeHooks
	// Worker 和 admission 等待使用的 context, FailJob 时取消.
	process context.Context
	// 只取消尚未越过 Sink 交接边界的处理与等待.
	cancelProcess context.CancelFunc
	// 传给 Source 的生命周期, 与业务处理取消同步收敛.
	sourceLife context.Context
	// 用于停止 Source 自身活动及唤醒生产端等待者.
	cancelSource context.CancelFunc
	// 独立于宿主直接取消, 使已进入 Sink 的调用仍有关闭预算可以收尾.
	sinkLife context.Context
	// 在关闭 Sink 或整个关闭预算耗尽时取消 Sink 活动.
	cancelSink context.CancelFunc
	// Source 可持有的报告入口, 终止时清除其对 runState 的引用.
	sourceContext *runtimeSourceContext
	// Sink 持有的生命周期与容量通知入口, 不暴露调度状态.
	sinkContext *runtimeSinkContext
	// 保护从 primary 到 stats 的共享字段, 不保护 work 中按 ownership 交接的数据.
	// 持锁时不调用组件的 Open, Process, Accept 或 Close.
	mu sync.Mutex
	// 第一个使运行失败的原因, 一旦设置不再替换.
	primary error
	// 主因成立后观察到的其他失败或关闭错误, 冻结时复制给 RunError.
	secondary []error
	// 运行结果已冻结, 后续任务不能再推进错误集合或统计.
	fenced bool
	// 统一关闭预算已开始, 正常 EOF 也会设置; 不等同于 FailJob.
	stopping bool
	// 所有收尾阶段共用的绝对截止时间, 阶段切换不重新计时.
	deadline time.Time
	// 首次开始关闭时关闭一次, 通知 Run 建立截止计时器.
	shutdown chan struct{}
	// 执行任务名到当前阶段的映射, 为关闭超时提供未退出位置.
	tasks map[string]string
	// 当前输入计数与容量占用, 正常回收后在途和空预留均归零.
	stats executionStats
	// 预先分配的空槽池, 每取走一个槽就占用一份端到端容量.
	free chan *runtimeWork
	// 已绑定但尚待 Worker 处理的输入队列.
	pending chan *runtimeWork
	// 整个 Chain 已成功但尚待 Sink 接管的输出组队列, 每组占一个位置.
	terminal chan *runtimeWork
}

func newRunState(host context.Context, options RuntimeOptions, hooks *runtimeHooks) *runState {
	base := context.WithoutCancel(host)
	process, cancelProcess := context.WithCancel(host)
	source, cancelSource := context.WithCancel(host)
	sink, cancelSink := context.WithCancel(base)
	capacity := options.MaxInFlightWorks
	terminalCapacity := capacity
	if options.Parallelism <= (capacity-1)/2 {
		terminalCapacity = 2 * options.Parallelism
	}
	s := &runState{
		host:          host,
		options:       options,
		hooks:         hooks,
		process:       process,
		cancelProcess: cancelProcess,
		sourceLife:    source,
		cancelSource:  cancelSource,
		sinkLife:      sink,
		cancelSink:    cancelSink,
		shutdown:      make(chan struct{}),
		tasks:         make(map[string]string),
		free:          make(chan *runtimeWork, capacity),
		pending:       make(chan *runtimeWork, capacity),
		terminal:      make(chan *runtimeWork, terminalCapacity),
	}
	for range capacity {
		s.free <- &runtimeWork{}
	}
	s.sourceContext = &runtimeSourceContext{
		life:   source,
		target: s,
	}
	s.sinkContext = &runtimeSinkContext{
		life:   sink,
		signal: newCapacitySignal(),
	}
	return s
}

func (s *runState) startShutdownLocked() {
	if s.stopping {
		return
	}
	s.stopping = true
	s.deadline = time.Now().Add(s.options.ShutdownTimeout)
	if d, ok := s.host.Deadline(); ok && d.Before(s.deadline) {
		s.deadline = d
	}
	close(s.shutdown)
}

func (s *runState) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.fenced {
		s.mu.Unlock()
		return
	}
	if s.primary == nil {
		s.primary = err
	} else if !sameError(s.primary, err) {
		s.secondary = append(s.secondary, err)
	}
	s.startShutdownLocked()
	s.mu.Unlock()
	s.cancelProcess()
	s.cancelSource()
}

func (s *runState) canStart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.fenced && s.primary == nil && s.process.Err() == nil
}

func (s *runState) isFenced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fenced
}
func (s *runState) phase(task, phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.fenced {
		s.tasks[task] = phase
	}
}
func (s *runState) spawn(name string, fn func()) <-chan struct{} {
	done := make(chan struct{})
	s.phase(name, "starting")
	go func() {
		defer close(done)
		returned := false
		defer func() {
			if value := recover(); value != nil {
				s.fail(&InternalPanicError{
					PanicError{
						component: name,
						value:     value,
						stack:     debug.Stack(),
					},
				})
			} else if !returned {
				s.fail(fmt.Errorf("yaspe: %s exited without returning", name))
			}
			s.mu.Lock()
			if !s.fenced {
				delete(s.tasks, name)
			}
			s.mu.Unlock()
		}()
		fn()
		returned = true
	}()
	return done
}

func (s *runState) freeze(timeout bool) error {
	s.mu.Lock()
	if timeout {
		stages := make([]string, 0, len(s.tasks))
		for task, phase := range s.tasks {
			stages = append(stages, task+": "+phase)
		}
		sort.Strings(stages)
		err := &ShutdownTimeoutError{
			stages: stages,
		}
		if s.primary == nil {
			s.primary = err
		} else {
			s.secondary = append(s.secondary, err)
		}
	}
	s.fenced = true
	var result error
	if s.primary != nil {
		result = &RunError{
			primary:   s.primary,
			secondary: slices.Clone(s.secondary),
		}
	}
	s.mu.Unlock()
	s.cancelProcess()
	s.cancelSource()
	s.cancelSink()
	s.sourceContext.fence()
	s.sinkContext.signal.fence()
	return result
}

func (s *runState) statsSnapshot() executionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *runState) reserve() (*runtimeWork, bool) {
	if !s.canStart() {
		return nil, false
	}
	select {
	case <-s.process.Done():
		return nil, false
	case work := <-s.free:
		s.mu.Lock()
		if s.fenced {
			s.mu.Unlock()
			return nil, false
		}
		s.stats.reserved++
		s.mu.Unlock()
		return work, true
	}
}

func (s *runState) bind(work *runtimeWork, value runtimeValue) {
	work.input = value
	work.admitted = true
	work.started = time.Time{}
	s.mu.Lock()
	if !s.fenced {
		s.stats.admitted++
		s.stats.reserved--
		s.stats.inFlight++
		s.stats.peak = max(s.stats.peak, s.stats.inFlight)
		if s.hooks != nil && s.hooks.onCompleted != nil && (s.hooks.traceEvery <= 1 || s.stats.admitted%uint64(s.hooks.traceEvery) == 0) {
			work.started = time.Now()
		}
	}
	s.mu.Unlock()
}

func (s *runState) finish(work *runtimeWork, outcome string) {
	s.mu.Lock()
	fenced := s.fenced
	if outcome == "success" && s.primary != nil && !work.sinkEntered {
		outcome = "cancelled"
	}
	if !fenced {
		if work.admitted {
			s.stats.inFlight--
			switch outcome {
			case "success":
				s.stats.succeeded++
			case "failed":
				s.stats.failed++
			default:
				s.stats.cancelled++
			}
		} else {
			s.stats.reserved--
		}
	}
	s.mu.Unlock()
	count := !fenced && work.admitted && outcome == "success"
	sampled := !work.started.IsZero()
	var elapsed time.Duration
	if sampled {
		elapsed = time.Since(work.started)
	}
	work.input = nil
	work.outputs = nil
	work.admitted = false
	work.sinkEntered = false
	if !fenced {
		s.free <- work
	}
	if count && sampled && s.hooks != nil && s.hooks.onCompleted != nil {
		s.hooks.onCompleted(elapsed)
	}
	if count && s.options.OnWorkCompleted != nil {
		func() {
			defer func() {
				_ = recover()
			}()
			s.options.OnWorkCompleted()
		}()
	}
}
