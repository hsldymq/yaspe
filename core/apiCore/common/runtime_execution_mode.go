package common

type RuntimeExecutionMode int

const (
	Streaming RuntimeExecutionMode = iota

	Batch

	Auto
)
