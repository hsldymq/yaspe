package yaspe

import (
	"context"
	"time"
)

// Source 是每次运行独立创建的输入实例. Open 成功前不得读取, Close 开始后不得再读取.
// Factory 只创建实例; I/O 和后台任务应在 Open 中启动.
type Source[T any] interface {
	Reader[T]
	Open(SourceContext) error
	Close(context.Context) error
}

// SourceContext 由 Runtime 提供. 最终失败报告独立于数据读取和背压.
type SourceContext interface {
	LifecycleContext() context.Context
	ControlReporter() SourceControlReporter
	ReportFailure(error) error
}

// Reader 非阻塞地交接输入. Available 始终返回同一个容量为 1, 永不关闭的通知 channel.
// 通知只是重新检查的提示; 发布状态必须先于通知.
type Reader[T any] interface {
	TryRead() (ReadResult[T], error)
	Available() <-chan struct{}
}

// ReadState 描述非阻塞读取的结果; 零值不是合法的读取状态.
type ReadState uint8

const (
	ReadStateInvalid ReadState = iota
	ReadReady
	ReadUnavailable
	ReadFinished
)

// ReadResult 仅在 ReadReady 时交接 Value 的 ownership. 非 nil error 优先于此结果.
// Positioned 仅用于带恢复位置的 ReadReady; 普通内存输入保持 nil.
type ReadResult[T any] struct {
	State      ReadState
	Value      T
	Positioned *PositionedRead
}

// SplitID 是 Source 内非空, 唯一的分片标识, Runtime 不解析其内容.
type SplitID string

// PositionedRead 的 Position 由 Connector 定义且交接后不可修改.
// ownership generation 由 Runtime 绑定, 不由 Source 填写.
type PositionedRead struct {
	Split    SplitID
	Position any
}

// SplitPosition 保存 Connector 可原样提交的不透明位置.
type SplitPosition struct {
	Split    SplitID
	Position any
}

// PositionCommitter 是 positioned Source 的可选能力; nil 返回表示提交已获外部确认.
type PositionCommitter interface {
	CommitPositions(context.Context, []SplitPosition) error
}

// SourceControlReporter 接收串行发起的动态 ownership 变更. 每批 ID 必须非空且不重复.
// 不支持动态 split 的 Source 无须调用它.
type SourceControlReporter interface {
	Assign(context.Context, []SplitID) error
	BeginRevoke(context.Context, []SplitID, RevokeOptions) (RevokeHandle, error)
	Lost(context.Context, []SplitID) error
}

// RevokeOptions 在 BeginRevoke 的总 deadline 内为提交预留时间.
// context 必须有 deadline; CommitReserve 不得为负.
type RevokeOptions struct {
	CommitReserve time.Duration
}

// RevokeHandle 提供 drain 后冻结的位置; Connector 完成提交后调用 Complete.
type RevokeHandle interface {
	Positions() []SplitPosition
	Complete(commitErr error) error
}
