package watermark

type WatermarkHandlingResult int

const (
	HandlingResultPeek WatermarkHandlingResult = iota
	HandlingResultPoll
)

type WatermarkHandlingStrategy int

const (
	HandlingStrategyIgnore WatermarkHandlingStrategy = iota
	HandlingStrategyForward
)
