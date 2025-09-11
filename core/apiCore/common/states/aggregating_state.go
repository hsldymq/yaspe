package states

type AggregatingState[TIn, TOut any] interface {
	AppendingState[TIn, TOut]
}
