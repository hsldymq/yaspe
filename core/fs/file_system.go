package fs

// TODO
type FileSystem interface {
	GetWorkingDirectory() string // return Path

	GetHomeDirectory() string // return Path

	GetUri() string // return URI

	GetFileStatus() (FileStatus, error)

	GetFileBlockLocations(status FileStatus, start, length int64) ([]BlockLocation, error)
}
