package source

type Source[TData any, TSplit SourceSplit, TEnumChk any] interface {
	GetBoundedness() Boundedness

	CreateEnumerator()
}
