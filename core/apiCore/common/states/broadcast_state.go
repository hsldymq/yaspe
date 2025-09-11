package states

import (
	"iter"

	"github.com/hsldymq/yaspe/shared/types"
)

type BroadcastState[TKey comparable, TVal any, TMap types.GeneralMap[TKey, TMap]] interface {
	ReadonlyBroadcastState[TKey, TVal]

	Put(TKey, TVal) error

	PutMap(TMap) error

	Remove(TKey) error

	Iter() (iter.Seq2[TKey, TVal], error)
}

type ReadonlyBroadcastState[TKey comparable, TVal any] interface {
	State

	Get(TKey) (TVal, error)

	Has(TKey) (bool, error)
}
