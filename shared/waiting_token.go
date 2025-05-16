package shared

import "sync/atomic"

type WaitingToken interface {
	WaitDone()

	IsDone() bool
}

type WaitingTokenImpl struct {
	signal chan struct{}
	isDone int32
}

func NewWaitingTokenImpl() (w *WaitingTokenImpl) {
	return &WaitingTokenImpl{
		signal: make(chan struct{}),
		isDone: 0,
	}
}

func (w *WaitingTokenImpl) WaitDone() {
	<-w.signal
}

func (w *WaitingTokenImpl) IsDone() bool {
	return atomic.LoadInt32(&w.isDone) == 1
}

func (w *WaitingTokenImpl) Done() {
	if atomic.CompareAndSwapInt32(&w.isDone, 0, 1) {
		close(w.signal)
	}
}
