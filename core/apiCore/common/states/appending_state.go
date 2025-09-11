package states

type AppendingState[TIn, TOut any] interface {
	State

	Add(TIn) error

	Get() (TOut, error)
}
