package fs

type FileStatus interface {
	IsDir() bool

	GetPath() *Path

	GetLen() int64

	GetBlockSize() int64

	GetReplication() int16

	GetModificationTime() int64

	GetAccessTime() int64
}
