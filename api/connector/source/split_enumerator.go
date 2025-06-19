package source

type SplitEnumerator[TSplit SourceSplit, TChkPnt any] interface {
	Start()

	Close()

	HandleSplitRequest()

	AddSplitsBack()

	AddReader()

	GetSnapshotState(id CheckpointID) (TChkPnt, error)

	NotifyCheckpointComplete(id CheckpointID) error

	NotifyCheckpointAborted(id CheckpointID) error

	HandleSourceEvent(subtaskID int, event any) error
}
