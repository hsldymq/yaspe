package types

import "iter"

type Iterable[T any] interface {
	Iter() iter.Seq[T]
}

type Iterable2[T1, T2 any] interface {
	Iter2() iter.Seq2[T1, T2]
}
