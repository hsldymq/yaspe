package watermark

type WatermarkManager interface {
	EmitWatermark(Watermark)
}
