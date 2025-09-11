package types

type GeneralMap[TKey comparable, TVal any] interface {
	~map[TKey]TVal
}
