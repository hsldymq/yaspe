package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hsldymq/yaspe"
)

var (
	ErrInvalidCapacity   = errors.New("memory source: capacity must be positive")
	ErrInvalidSource     = errors.New("memory source: source must be created with NewSource")
	ErrSourceNotOpen     = errors.New("memory source: source is not open")
	ErrSourceAlreadyOpen = errors.New("memory source: source is already open")
	ErrSourceFinished    = errors.New("memory source: source has finished accepting input")
	ErrSourceFailed      = errors.New("memory source: source has failed")
	ErrSourceClosed      = errors.New("memory source: source is closed")
	ErrNilFailure        = errors.New("memory source: failure must not be nil")
	ErrNilContext        = errors.New("memory source: context must not be nil")
)

// Source 是按成功提交顺序读取的有界内存输入, 不提供持久化或重放.
// 使用 NewSource 构造, 零值无效. TryRead 由一个读取方串行调用.
type Source[T any] struct {
	state *sourceState[T]
}

// NewSource 创建一对独立的 Source 和 Producer, capacity 必须大于零.
// Producer 可在 Open 前提交和声明结束. 每次工厂调用应创建新的一对实例.
func NewSource[T any](capacity int) (*Source[T], *SourceProducer[T], error) {
	if capacity <= 0 {
		return nil, nil, fmt.Errorf("%w: %d", ErrInvalidCapacity, capacity)
	}
	state := &sourceState[T]{
		values:    make([]T, capacity),
		available: make(chan struct{}, 1),
		changed:   make(chan struct{}),
	}
	return &Source[T]{
		state: state,
	}, &SourceProducer[T]{
		state: state,
	}, nil
}

// Open 绑定 Runtime 环境, 不创建后台任务. 重复 Open 返回错误.
// 在 Open 前注入的失败会在绑定环境后报告, Reader 仍保留同一个根因.
func (s *Source[T]) Open(runtime yaspe.SourceContext) error {
	if s == nil || s.state == nil {
		return ErrInvalidSource
	}
	if runtime == nil {
		return ErrNilContext
	}
	lifecycle := runtime.LifecycleContext()
	if lifecycle == nil {
		return ErrNilContext
	}
	state := s.state
	state.mu.Lock()
	if state.phase == sourceClosed {
		state.mu.Unlock()
		return ErrSourceClosed
	}
	if state.opened {
		state.mu.Unlock()
		return ErrSourceAlreadyOpen
	}
	if err := lifecycle.Err(); err != nil {
		state.mu.Unlock()
		return err
	}
	state.opened = true
	state.lifecycle = lifecycle
	state.runtime = runtime
	failure := state.failure
	state.wakeSubmitters()
	state.notifyReader()
	state.mu.Unlock()

	if failure != nil {
		// 报告状态不能替换输入失败, Runtime 关闭后可以拒绝迟到报告.
		_ = runtime.ReportFailure(failure)
	}
	return nil
}

// Available 始终返回同一个容量为 1 的通知 channel, Close 不会关闭它.
// 通知可合并或过期, 读取方应重新调用 TryRead 判断状态. 无效 Source 返回 nil.
func (s *Source[T]) Available() <-chan struct{} {
	if s == nil || s.state == nil {
		return nil
	}
	return s.state.available
}

// TryRead 立即返回当前状态, 不等待记录到达. ReadReady 转移值及其可达引用的 ownership.
// 失败优先于缓存记录; 正常结束则先交付缓存. Open 前和 Close 后读取返回错误.
func (s *Source[T]) TryRead() (yaspe.ReadResult[T], error) {
	if s == nil || s.state == nil {
		return yaspe.ReadResult[T]{}, ErrInvalidSource
	}
	state := s.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.phase == sourceClosed {
		return yaspe.ReadResult[T]{}, ErrSourceClosed
	}
	if !state.opened {
		return yaspe.ReadResult[T]{}, ErrSourceNotOpen
	}
	if state.phase == sourceFailed {
		return yaspe.ReadResult[T]{}, state.failure
	}
	if state.phase == sourceFinished {
		return yaspe.ReadResult[T]{
			State: yaspe.ReadFinished,
		}, nil
	}
	if err := state.lifecycle.Err(); err != nil {
		return yaspe.ReadResult[T]{}, err
	}
	if state.size > 0 {
		full := state.size == len(state.values)
		value := state.values[state.head]
		var zero T
		state.values[state.head] = zero
		state.head = (state.head + 1) % len(state.values)
		state.size--
		if full {
			state.wakeSubmitters()
		}
		return yaspe.ReadResult[T]{
			State: yaspe.ReadReady,
			Value: value,
		}, nil
	}
	if state.phase == sourceFinishing {
		state.phase = sourceFinished
		state.notifyReader()
		return yaspe.ReadResult[T]{
			State: yaspe.ReadFinished,
		}, nil
	}
	return yaspe.ReadResult[T]{
		State: yaspe.ReadUnavailable,
	}, nil
}

// Close 同步丢弃未交接的缓存, 唤醒等待者且幂等. 已交接的值不受影响.
// 清理不等待外部操作, 即使关闭 context 已取消也会释放缓存.
func (s *Source[T]) Close(context.Context) error {
	if s == nil || s.state == nil {
		return ErrInvalidSource
	}
	state := s.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.phase == sourceClosed {
		return nil
	}
	state.phase = sourceClosed
	state.values = nil
	state.head = 0
	state.size = 0
	state.runtime = nil
	state.lifecycle = nil
	state.wakeSubmitters()
	state.notifyReader()
	return nil
}

// sourcePhase 描述生产和读取终态, 与是否已绑定 Runtime 的 opened 标记独立.
type sourcePhase uint8

const (
	sourceAccepting sourcePhase = iota
	sourceFinishing
	sourceFinished
	sourceFailed
	sourceClosed
)

// sourceState 是 Source 和 SourceProducer 共享的有界缓冲及生命周期状态.
// 所有可变字段由 mu 保护, available 的 channel 引用固定且永不关闭.
type sourceState[T any] struct {
	mu sync.Mutex
	// 固定容量的环形缓冲, 读取后清空对应槽, Close 时释放整个 slice.
	values []T
	// 下一条待交接记录的环形索引.
	head int
	// 仍由 Source 持有的记录数, 不包括已经交给 Runtime 的输入.
	size int
	// 控制接收, 正常 drain, 失败和关闭; finishing 直到 Reader 发布 finished 才结束.
	phase sourcePhase
	// 是否已成功绑定 Runtime 环境, 生产者可以在它为 false 时预装数据.
	opened bool
	// 首次注入的原始失败, Reader 与独立报告共用, Close 不覆盖它.
	failure error
	// 包含 ErrSourceFailed 和原始根因的错误, 用于拒绝失败后的生产操作.
	failedSubmission error
	// 独立失败报告的接收环境, Close 时清空引用.
	runtime yaspe.SourceContext
	// 已绑定的 Source 生命周期, Submit 也监听其取消, Open 前为 nil.
	lifecycle context.Context
	// 读取方的可合并通知, 容量为 1, 仅表示需要重新检查状态.
	available chan struct{}
	// 生产者等待的广播代次, 容量恢复或终态变化时关闭并替换.
	changed chan struct{}
}

// 在持锁状态下发布通知, 确保读取方醒来时能观察到对应状态.
func (s *sourceState[T]) notifyReader() {
	select {
	case s.available <- struct{}{}:
	default:
	}
}

// changed 采用广播通知, 终态变化必须唤醒所有正在等待容量的生产者.
// 调用方持有 mu, 等待者也在同一把锁下取得对应通知 channel.
func (s *sourceState[T]) wakeSubmitters() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *sourceState[T]) submissionError() error {
	switch s.phase {
	case sourceFinishing, sourceFinished:
		return ErrSourceFinished
	case sourceFailed:
		return s.failedSubmission
	case sourceClosed:
		return ErrSourceClosed
	default:
		return nil
	}
}
