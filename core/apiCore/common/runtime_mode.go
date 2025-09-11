package common

type RuntimeMode int

const (
	Streaming RuntimeMode = iota

	Batch

	Auto
)
