package memory

import (
    "context"
    "errors"
    "fmt"
    "slices"
    "sync"

    "github.com/hsldymq/yaspe"
)

var (
    ErrInvalidSink        = errors.New("memory sink: sink must be created with NewSink")
    ErrInvalidSinkOptions = errors.New("memory sink: invalid options")
    ErrInvalidSinkInput   = errors.New("memory sink: invalid input")
    ErrSinkNotOpen        = errors.New("memory sink: sink is not open")
    ErrSinkAlreadyOpen    = errors.New("memory sink: sink is already open")
    ErrSinkClosed         = errors.New("memory sink: sink is closed")
)

// SinkOptions 配置固定失败计划, 不接受失败回调. 零值表示所有合法接管均成功.
// Failures 与 AlwaysFail 不能同时设置. 构造时复制 Failures, 不深拷贝 error 对象.
type SinkOptions struct {
    // Failures 按合法接管的线性化顺序消费, nil 表示成功, 序列耗尽后均成功.
    // 参数无效, 未 Open, 已关闭或取消的调用不消耗计划.
    Failures []error
    // AlwaysFail 使每次合法接管都返回同一个错误.
    AlwaysFail error
}

// Sink 同步保存完整输出组, 不创建后台任务. 使用 NewSink 构造, 零值无效.
// 结果按实际接管顺序保留, 不限制累计结果数量, 不提供持久化或跨组回滚.
type Sink[T any] struct {
    state *sinkState[T]
}

// NewSink 创建独立的结果集合和失败计划. 每次工厂调用应创建新的 Sink.
func NewSink[T any](options SinkOptions) (*Sink[T], error) {
    if len(options.Failures) > 0 && options.AlwaysFail != nil {
        return nil, fmt.Errorf("%w: Failures and AlwaysFail cannot be combined", ErrInvalidSinkOptions)
    }
    return &Sink[T]{state: &sinkState[T]{
        failures:   slices.Clone(options.Failures),
        alwaysFail: options.AlwaysFail,
    }}, nil
}

// Open 绑定生命周期, 不创建运行资源.
// 同一实例成功 Open 后不能再次 Open.
func (s *Sink[T]) Open(runtime yaspe.SinkContext) error {
    if s == nil || s.state == nil {
        return ErrInvalidSink
    }
    if runtime == nil {
        return fmt.Errorf("%w: nil SinkContext", ErrInvalidSinkInput)
    }
    lifecycle := runtime.LifecycleContext()
    if lifecycle == nil {
        return fmt.Errorf("%w: nil lifecycle context", ErrInvalidSinkInput)
    }
    state := s.state
    state.mu.Lock()
    defer state.mu.Unlock()
    if state.closed {
        return ErrSinkClosed
    }
    if state.opened {
        return ErrSinkAlreadyOpen
    }
    if err := lifecycle.Err(); err != nil {
        return err
    }
    state.opened = true
    state.lifecycle = lifecycle
    return nil
}

// Accept 全收或全拒一个非空输出组. 只有 SinkAccepted, nil 转移整组 ownership.
// 成功时保留记录顺序, 并在返回前通过 reporter 同步报告所有 item 成功.
// reporter 和 context 必须非 nil. 返回错误时不保存任何输入, 不调用 reporter.
func (s *Sink[T]) Accept(ctx context.Context, items []yaspe.SinkItem[T], reporter yaspe.SinkResultReporter[T]) (yaspe.SinkAcceptStatus, error) {
    if s == nil || s.state == nil {
        return yaspe.SinkAcceptStatusInvalid, ErrInvalidSink
    }
    if ctx == nil || reporter == nil || len(items) == 0 {
        return yaspe.SinkAcceptStatusInvalid, fmt.Errorf("%w: context, reporter and nonempty items are required", ErrInvalidSinkInput)
    }
    state := s.state
    if state.beforeAccept != nil {
        state.beforeAccept()
    }
    results, err := state.accept(ctx, items)
    if err != nil {
        return yaspe.SinkAcceptStatusInvalid, err
    }
    defer state.finishAccept()

    // 不持锁调用报告器, 允许报告时读取快照. 转移 results 后不再访问它.
    reporter.Report(results)
    return yaspe.SinkAccepted, nil
}

// Groups 返回按接管顺序排列的完整输出组, 每组内保持原顺序.
// 外层及组内 slice 均独立复制; Record 中的引用数据仍须只读, 不转移 ownership.
// Open 前和 Close 后也可读取结果. 无效 Sink 返回 nil.
func (s *Sink[T]) Groups() [][]yaspe.Record[T] {
    if s == nil || s.state == nil {
        return nil
    }
    state := s.state
    state.mu.Lock()
    defer state.mu.Unlock()
    groups := make([][]yaspe.Record[T], len(state.groups))
    for i, group := range state.groups {
        groups[i] = slices.Clone(group)
    }
    return groups
}

// Records 返回按接管顺序扁平化的记录快照. slice 独立复制, 引用数据仍须只读.
// 与 Groups 分别调用时可能观察到不同时间点. 无效 Sink 返回 nil.
func (s *Sink[T]) Records() []yaspe.Record[T] {
    if s == nil || s.state == nil {
        return nil
    }
    state := s.state
    state.mu.Lock()
    defer state.mu.Unlock()
    var count int
    for _, group := range state.groups {
        count += len(group)
    }
    records := make([]yaspe.Record[T], 0, count)
    for _, group := range state.groups {
        records = append(records, group...)
    }
    return records
}

// Close 停止新接管并等待已接受调用完成同步报告, 保留所有结果, 可以重复调用.
// 等待受 ctx 限制; 超时后仍保持关闭, 再次 Close 可继续等待在途调用结束.
// 没有在途调用时立即成功, 即使 ctx 已取消也不影响清理.
func (s *Sink[T]) Close(ctx context.Context) error {
    if s == nil || s.state == nil {
        return ErrInvalidSink
    }
    if ctx == nil {
        return fmt.Errorf("%w: nil close context", ErrInvalidSinkInput)
    }
    state := s.state
    state.mu.Lock()
    state.closed = true
    state.lifecycle = nil
    done := state.activeDone
    active := state.active
    state.mu.Unlock()
    if active == 0 {
        return nil
    }
    select {
    case <-done:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}

type sinkState[T any] struct {
    mu           sync.Mutex
    opened       bool
    closed       bool
    lifecycle    context.Context
    groups       [][]yaspe.Record[T]
    failures     []error
    failureIndex int
    alwaysFail   error
    active       int
    activeDone   chan struct{}
    // beforeAccept 仅供包内测试暂停在接管前, 必须在并发操作开始前设置.
    beforeAccept func()
}

func (s *sinkState[T]) accept(ctx context.Context, items []yaspe.SinkItem[T]) ([]yaspe.SinkItemResult[T], error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.closed {
        return nil, ErrSinkClosed
    }
    if !s.opened {
        return nil, ErrSinkNotOpen
    }
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    if err := s.lifecycle.Err(); err != nil {
        return nil, err
    }
    if s.alwaysFail != nil {
        return nil, s.alwaysFail
    }
    if s.failureIndex < len(s.failures) {
        err := s.failures[s.failureIndex]
        s.failureIndex++
        if err != nil {
            return nil, err
        }
    }

    group := make([]yaspe.Record[T], len(items))
    results := make([]yaspe.SinkItemResult[T], len(items))
    for i, item := range items {
        group[i] = item.Record
        results[i] = yaspe.SinkItemResult[T]{Item: item, Outcome: yaspe.SinkSucceeded}
    }
    s.groups = append(s.groups, group)
    if s.active == 0 {
        s.activeDone = make(chan struct{})
    }
    s.active++
    return results, nil
}

func (s *sinkState[T]) finishAccept() {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.active--
    if s.active == 0 {
        close(s.activeDone)
    }
}
