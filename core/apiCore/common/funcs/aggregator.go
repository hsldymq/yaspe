package funcs

type Aggregator[TIn, TAcc, TOut any] interface {
	NewAccumulator() TAcc

	Add(TIn, TAcc) TAcc

	Result(TAcc) TOut

	Merge(TAcc, TAcc) TAcc
}
