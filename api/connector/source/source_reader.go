package source

import (
	coreIO "github.com/hsldymq/yaspe/core/io"
	"github.com/hsldymq/yaspe/shared"
)

type CheckpointID = int64

type SourceReader[TData any, TSplit SourceSplit] interface {
	Start()

	PollNext() (coreIO.InputStatus, error)

	GetAvailabilityToken() shared.WaitingToken

	AddSplits(splits []TSplit)

	NotifyNoMoreSplits()

	NotifyCheckpointComplete(id CheckpointID) error

	SnapshotState(id CheckpointID) []TSplit

	PauseOrResumeSplits(splitsToPause []string, splitsToResume []string)
}

type SourceReaderFactory interface {
}
