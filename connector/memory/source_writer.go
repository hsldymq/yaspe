package memory

import (
	"context"
	"errors"
)

// SourceWriter 供生产者并发提交输入, 声明结束或报告失败.
// 它不提供 Close; Source 的关闭由读取方负责. 零值无效.
type SourceWriter[T any] struct {
	state *sourceState[T]
}

// Submit 有容量时接受 value, 缓冲满时等待容量, 并响应调用方及 Source 生命周期取消.
// 返回 nil 才转移 value 及其可达引用的 ownership, 返回错误则不转移.
// 并发提交按成功入队的顺序交付, 不承诺等待者公平性.
func (w *SourceWriter[T]) Submit(ctx context.Context, value T) error {
	if w == nil || w.state == nil {
		return ErrInvalidSource
	}
	if ctx == nil {
		return ErrNilContext
	}
	state := w.state
	for {
		state.mu.Lock()
		if err := state.submissionError(); err != nil {
			state.mu.Unlock()
			return err
		}
		if err := ctx.Err(); err != nil {
			state.mu.Unlock()
			return err
		}
		var lifecycleDone <-chan struct{}
		if state.lifecycle != nil {
			if err := state.lifecycle.Err(); err != nil {
				state.mu.Unlock()
				return err
			}
			lifecycleDone = state.lifecycle.Done()
		}
		if state.size < len(state.values) {
			tail := (state.head + state.size) % len(state.values)
			state.values[tail] = value
			state.size++
			state.notifyReader()
			state.mu.Unlock()
			return nil
		}
		changed := state.changed
		state.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-lifecycleDone:
		case <-changed:
		}
	}
}

// Finish 声明不再提交, 唤醒等待者, 并允许 Reader 交付已缓存记录后正常结束.
// 重复 Finish 幂等, 不等待下游完成. 已失败或关闭时返回对应生命周期错误.
func (w *SourceWriter[T]) Finish() error {
	if w == nil || w.state == nil {
		return ErrInvalidSource
	}
	state := w.state
	state.mu.Lock()
	defer state.mu.Unlock()
	switch state.phase {
	case sourceFinishing, sourceFinished:
		return nil
	case sourceFailed, sourceClosed:
		return state.submissionError()
	}
	state.phase = sourceFinishing
	state.wakeSubmitters()
	state.notifyReader()
	return nil
}

// Fail 锁存非 nil 根因, 唤醒等待者, 并在 Source 已 Open 时独立报告给 Runtime.
// 返回 nil 表示本地失败状态已接受, 不表示 Runtime 已停止. 重复 Fail 保留首个根因.
// Reader 已发布正常结束或 Source 已关闭时, 返回对应生命周期错误.
func (w *SourceWriter[T]) Fail(cause error) error {
	if w == nil || w.state == nil {
		return ErrInvalidSource
	}
	if cause == nil {
		return ErrNilFailure
	}
	state := w.state
	state.mu.Lock()
	switch state.phase {
	case sourceFailed:
		state.mu.Unlock()
		return nil
	case sourceFinished, sourceClosed:
		err := state.submissionError()
		state.mu.Unlock()
		return err
	}
	state.phase = sourceFailed
	state.failure = cause
	state.failedSubmission = errors.Join(ErrSourceFailed, cause)
	runtime := state.runtime
	state.wakeSubmitters()
	state.notifyReader()
	state.mu.Unlock()

	if runtime != nil {
		// 不持锁调用 Runtime, 避免报告与读取或关闭互相等待.
		_ = runtime.ReportFailure(cause)
	}
	return nil
}
