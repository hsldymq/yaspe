package watermark

type BoolCombinationType = int

const (
	BoolCombinationAnd BoolCombinationType = iota
	BoolCombinationOr
)

type NumericCombinationType = int

const (
	NumericCombinationMin NumericCombinationType = iota
	NumericCombinationMax
)
