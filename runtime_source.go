package yaspe

import (
	"context"
	"sync"
)

// runtimeSourceContext 将 Source 的首次失败转交给运行协调器, 独立于数据背压.
// life 在构造后固定, 其余字段由 mu 保护; fence 后只保留可安全拒绝迟到调用的轻量入口.
type runtimeSourceContext struct {
	life context.Context
	mu   sync.Mutex
	// 当前运行的失败接收方, fence 时置 nil, 防止迟到入口保留完整运行状态.
	target *runState
	// Source 独立报告与 TryRead 失败共用的首因锁存值, 后续错误不重复投递.
	first error
	// 报告入口已失效, 与 Source 是否有缓存记录无关.
	closed bool
}

func (c *runtimeSourceContext) LifecycleContext() context.Context {
	return c.life
}
func (c *runtimeSourceContext) ControlReporter() SourceControlReporter {
	return c
}
func (c *runtimeSourceContext) ReportFailure(err error) error {
	if err == nil {
		return ErrNilSourceFailure
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrSourceReporterClosed
	}
	if c.first != nil {
		return nil
	}
	c.first = err
	c.target.fail(err)
	return nil
}
func (c *runtimeSourceContext) readFailure(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if c.first == nil {
		c.first = err
		c.target.fail(err)
	}
}
func (c *runtimeSourceContext) fence() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.target = nil
	c.first = nil
}
func (c *runtimeSourceContext) Assign(context.Context, []SplitID) error {
	return c.unsupported()
}
func (c *runtimeSourceContext) Lost(context.Context, []SplitID) error {
	return c.unsupported()
}
func (c *runtimeSourceContext) BeginRevoke(context.Context, []SplitID, RevokeOptions) (RevokeHandle, error) {
	return nil, c.unsupported()
}
func (c *runtimeSourceContext) unsupported() error {
	if err := c.ReportFailure(ErrUnsupportedSource); err != nil {
		return err
	}
	return ErrUnsupportedSource
}
