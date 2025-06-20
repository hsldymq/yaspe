package fs

type BlockLocation interface {
	GetHosts() ([]string, error)

	GetOffset() int64

	GetLength() int64

	CompareTo(BlockLocation) int
}
