package yaspe

import (
	"context"
	"errors"
	"sync"
)

var errSinkBackpressured = errors.New("yaspe: sink backpressured")

// sinkInstance 保留 Sink 的实际输入类型, 构造输出身份并校验同步完成结果.
type sinkInstance[T any] struct {
	sink Sink[T]
}

func (s *sinkInstance[T]) open(ctx SinkContext) error {
	return invokeUser("sink Open", func() error {
		return s.sink.Open(ctx)
	})
}
func (s *sinkInstance[T]) close(ctx context.Context) error {
	return invokeUser("sink Close", func() error {
		return s.sink.Close(ctx)
	})
}
func (s *sinkInstance[T]) accept(ctx context.Context, values []runtimeValue) error {
	items := make([]SinkItem[T], len(values))
	identities := make([]sinkItemIdentity, len(values))
	reporter := &synchronousReporter[T]{
		identities: identities,
		outcomes:   make([]SinkOutcome, len(values)),
		errors:     make([]error, len(values)),
	}
	for i, value := range values {
		record, ok := value.(Record[T])
		if !ok {
			panic(internalFault("sink input type mismatch"))
		}
		identities[i].index = i
		items[i] = SinkItem[T]{
			Record:   record,
			identity: &identities[i],
		}
	}
	var status SinkAcceptStatus
	err := invokeUser("sink Accept", func() error {
		var acceptErr error
		status, acceptErr = s.sink.Accept(ctx, items, reporter)
		return acceptErr
	})
	return reporter.finish(status, err)
}

// synchronousReporter 暂存一次 Accept 中的逐项报告, 确认接管成功后才形成执行结果.
// mu 保护全部可变状态; finish 后清除逐项存储, 迟到 Report 直接返回.
type synchronousReporter[T any] struct {
	mu sync.Mutex
	// 本次交接的私有身份数组, 同时校验索引范围和指针归属以拒绝外来 item.
	identities []sinkItemIdentity
	// 按身份索引保存首次合法 outcome, Invalid 表示尚未收到合法结果.
	outcomes []SinkOutcome
	// 与 outcomes 对应的首次合法错误, 用于识别重复或矛盾报告.
	errors []error
	// 已有合法结果的不同 item 数, 重复报告不增加.
	count int
	// 锁存首个协议违规, 防止重复违规无限追加诊断.
	protocol error
	// 按观察顺序保留逐项失败和首个协议错误, 供协调器区分主因与附加错误.
	causes []error
	// Accept 返回后的校验已结束, 此后不再接受或累积报告.
	closed bool
}

func (r *synchronousReporter[T]) Report(results []SinkItemResult[T]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	for _, result := range results {
		id := result.Item.identity
		if id == nil || id.index < 0 || id.index >= len(r.identities) || id != &r.identities[id.index] {
			r.protocolFailure("foreign item")
			continue
		}
		valid := result.Outcome == SinkSucceeded && result.Err == nil || (result.Outcome == SinkNotApplied || result.Outcome == SinkUnknown) && result.Err != nil
		if !valid {
			r.protocolFailure("invalid outcome or error")
			continue
		}
		i := id.index
		if r.outcomes[i] != SinkOutcomeInvalid {
			if r.outcomes[i] != result.Outcome || !(r.errors[i] == nil && result.Err == nil || sameError(r.errors[i], result.Err)) {
				r.protocolFailure("conflicting duplicate result")
			}
			continue
		}
		r.outcomes[i], r.errors[i] = result.Outcome, result.Err
		r.count++
		if result.Err != nil {
			r.causes = append(r.causes, result.Err)
		}
	}
}

func (r *synchronousReporter[T]) protocolFailure(reason string) {
	if r.protocol == nil {
		r.protocol = &SinkProtocolError{
			reason: reason,
		}
		r.causes = append(r.causes, r.protocol)
	}
}

// sinkResultErrors 在 Sink adapter 与运行协调器之间保留多个失败的观察顺序.
type sinkResultErrors struct {
	primary   error
	secondary []error
}

func (e *sinkResultErrors) Error() string {
	return e.primary.Error()
}
func (e *sinkResultErrors) Unwrap() error {
	return e.primary
}

func (r *synchronousReporter[T]) finish(status SinkAcceptStatus, err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	defer func() {
		r.identities = nil
		r.outcomes = nil
		r.errors = nil
		r.causes = nil
		r.protocol = nil
	}()
	if status != SinkAccepted || err != nil {
		if r.count != 0 || r.protocol != nil {
			return errors.Join(err, &SinkProtocolError{
				reason: "reported results for a rejected group",
			})
		}
		if err != nil {
			return err
		}
		if status == SinkBackpressured {
			return errSinkBackpressured
		}
		return &SinkProtocolError{
			reason: "invalid accept status",
		}
	}
	if len(r.causes) > 0 {
		return &sinkResultErrors{
			primary:   r.causes[0],
			secondary: append([]error(nil), r.causes[1:]...),
		}
	}
	if r.count != len(r.identities) {
		return ErrUnsupportedSink
	}
	return nil
}

// capacitySignal 用关闭并替换 channel 表示容量通知代次, 避免检查与等待之间丢失唤醒.
// 所有字段由 mu 保护, fence 后不再发布新代次.
type capacitySignal struct {
	mu sync.Mutex
	// 当前通知代次, 交接前取得后可等待调用期间或之后的通知.
	changed chan struct{}
	// 通知入口已 fence, 迟到 NotifyAvailable 只返回而不恢复调度.
	closed bool
}

func newCapacitySignal() *capacitySignal {
	return &capacitySignal{
		changed: make(chan struct{}),
	}
}
func (s *capacitySignal) NotifyAvailable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	close(s.changed)
	s.changed = make(chan struct{})
}
func (s *capacitySignal) observe() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}
func (s *capacitySignal) fence() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.changed)
	}
}

// runtimeSinkContext 向 Sink 提供独立生命周期和容量通知, 字段引用在创建后固定.
type runtimeSinkContext struct {
	life   context.Context
	signal *capacitySignal
}

func (c *runtimeSinkContext) LifecycleContext() context.Context {
	return c.life
}
func (c *runtimeSinkContext) CapacityNotifier() SinkCapacityNotifier {
	return c.signal
}
