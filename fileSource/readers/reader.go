package readers

type Reader[T any] interface {
	Read() (T, error)

	Close() error

	GetCheckpointedPosition()
}
