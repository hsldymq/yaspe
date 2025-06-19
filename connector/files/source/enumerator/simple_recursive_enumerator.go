package enumerator

type FileFilter = func(string) bool

type SimpleEnumerator struct {
	fileFilter      FileFilter
	directoryFilter FileFilter
}

func NewSimpleRecursiveEnumerator() *SimpleEnumerator {
	return &SimpleEnumerator{
		fileFilter:      func(string) bool { return true },
		directoryFilter: func(string) bool { return true },
	}
}
