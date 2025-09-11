package funcs

type ReduceFunc[T any] func(T, T) (T, error)
