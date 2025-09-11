package states

import "github.com/hsldymq/yaspe/shared/types"

type ListState[T any] interface {
	MergingState[T, types.Iterable[T]]

	Update(T) error

	UpdateFromSlice([]T) error

	AddFromSlice([]T) error
}
