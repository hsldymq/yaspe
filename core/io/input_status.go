package io

type InputStatus = int

const (
	MoreAvailable = iota

	NothingAvailable

	EndOfInput
)
