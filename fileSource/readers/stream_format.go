package readers

type StreamFormat[T any] interface {
	CreateReader( /* TODO */ ) (Reader[T], error)

	RestoreReader( /* TODO */ ) (Reader[T], error)

	IsSplittable() bool

	GetProducedType() string
}
