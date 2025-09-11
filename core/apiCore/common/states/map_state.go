package states

import (
	"iter"

	"github.com/hsldymq/yaspe/shared/types"
)

type MapState[TKey comparable, TVal any, TMap types.GeneralMap[TKey, TMap]] interface {
	State

	Get(TKey) (TVal, error)

	Put(TKey, TVal) error

	PutMap(TMap) error

	Remove(TKey) error

	Has(TKey) (bool, error)

	IsEmpty() (bool, error)

	Keys() ([]TKey, error)

	Values() ([]TVal, error)

	Iter() (iter.Seq2[TKey, TVal], error)
}
