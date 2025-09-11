package states

type ReducingState[T any] interface {
	MergingState[T, T]
}
