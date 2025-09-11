package states

type ValueState[T any] interface {
	State

	Value() (T, error)

	Update(T) error
}
