package states

type MergingState[TIn, TOut any] interface {
	AppendingState[TIn, TOut]
}
