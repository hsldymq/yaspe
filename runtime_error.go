package yaspe

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"
)

var (
	ErrInvalidRuntime       = errors.New("yaspe: invalid runtime options or instance")
	ErrInvalidJob           = errors.New("yaspe: invalid job")
	ErrCollectorClosed      = errors.New("yaspe: collector scope has ended")
	ErrSourceReporterClosed = errors.New("yaspe: source reporter is closed")
	ErrNilSourceFailure     = errors.New("yaspe: source failure must not be nil")
	ErrUnsupportedSource    = errors.New("yaspe: positioned sources are not supported by this runtime")
	ErrUnsupportedSink      = errors.New("yaspe: sink must report all results before Accept returns")
)

// RunError 是一次非正常运行的不可变错误快照. 查询方法返回独立 slice.
type RunError struct {
	// 首个使运行失败的原因, 正常构造的错误快照中非 nil.
	primary error
	// 停止时其他活跃失败输入的摘要, 不重复包含 primary 对应输入.
	active []WorkFailure
	// 停止期间观察到的其他错误, 保持与 primary 分离.
	secondary []error
}

func (e *RunError) Error() string {
	return fmt.Sprintf("yaspe: run failed: %v (active=%d, secondary=%d)", e.primary, len(e.active), len(e.secondary))
}

func (e *RunError) Primary() error {
	return e.primary
}

func (e *RunError) ActiveFailures() []WorkFailure {
	return slices.Clone(e.active)
}

func (e *RunError) Secondary() []error {
	return slices.Clone(e.secondary)
}

func (e *RunError) Unwrap() []error {
	var result []error
	if e.primary != nil {
		result = append(result, e.primary)
	}
	for _, failure := range e.active {
		if failure.first != nil {
			result = append(result, failure.first)
		}
		if failure.last != nil && !sameError(failure.first, failure.last) {
			result = append(result, failure.last)
		}
	}
	return append(result, e.secondary...)
}

// WorkFailure 保存一个失败输入的有界错误摘要, 不公开内部输入身份.
type WorkFailure struct {
	// 该输入首次失败的根因, 后续尝试不覆盖.
	first error
	// 最近一次尝试的错误, 可以与 first 相同.
	last error
	// 已经进行的尝试次数.
	attempts int
	// 形成快照时记录的已用时长, 不是查询时重新计算的时间.
	elapsed time.Duration
}

func (f WorkFailure) FirstError() error {
	return f.first
}
func (f WorkFailure) LastError() error {
	return f.last
}
func (f WorkFailure) Attempts() int {
	return f.attempts
}
func (f WorkFailure) Elapsed() time.Duration {
	return f.elapsed
}

// RuntimeAlreadyUsedError 表示同一个 Runtime 被重复或并发运行.
type RuntimeAlreadyUsedError struct{}

func (*RuntimeAlreadyUsedError) Error() string {
	return "yaspe: runtime can only run once"
}

// ShutdownTimeoutError 记录截止时仍未退出的执行阶段.
type ShutdownTimeoutError struct {
	stages []string
}

func (e *ShutdownTimeoutError) Error() string {
	return fmt.Sprintf("yaspe: shutdown deadline exceeded: %v", e.stages)
}
func (e *ShutdownTimeoutError) Stages() []string {
	return slices.Clone(e.stages)
}

// PanicError 保存组件代码的 panic 值及发生位置, 不把 panic 传播给宿主.
type PanicError struct {
	// 捕获 panic 的组件调用边界, 用于定位来源.
	component string
	// 原始 panic 值, 不转换为普通 error 后丢弃其类型.
	value any
	// 捕获时保存的调用栈, Stack 返回副本.
	stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("yaspe: panic in %s: %v", e.component, e.value)
}
func (e *PanicError) Value() any {
	return e.value
}
func (e *PanicError) Stack() []byte {
	return slices.Clone(e.stack)
}
func (e *PanicError) Component() string {
	return e.component
}

// InternalPanicError 表示引擎不变量被破坏, 运行只能失败收尾.
type InternalPanicError struct {
	PanicError
}

// InvalidReadResultError 表示 Source 返回了无效读取状态或不支持的位置数据.
type InvalidReadResultError struct {
	State ReadState
}

func (e *InvalidReadResultError) Error() string {
	return fmt.Sprintf("yaspe: invalid read result (state=%d)", e.State)
}

// SinkProtocolError 表示接管状态或完成报告不符合协议.
type SinkProtocolError struct {
	reason string
}

func (e *SinkProtocolError) Error() string {
	return "yaspe: sink protocol error: " + e.reason
}

func sameError(a, b error) bool {
	return a != nil && b != nil && reflect.TypeOf(a).Comparable() && a == b
}
