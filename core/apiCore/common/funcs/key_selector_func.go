package funcs

type KeySelectorFunc[TIn any, TKey comparable] func(TIn) (TKey, error)
