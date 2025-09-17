package watermark

type BoolWatermark struct {
	id  string
	val bool
}

func NewBoolWatermark(id string, val bool) *BoolWatermark {
	return &BoolWatermark{
		id:  id,
		val: val,
	}
}

func (bw *BoolWatermark) Identifier() string {
	return bw.id
}

func (bw *BoolWatermark) Value() bool {
	return bw.val
}

func (bw *BoolWatermark) EqualTo(obj any) bool {
	w, ok := obj.(*BoolWatermark)
	if !ok || w == nil {
		return false
	}

	return *bw == *w
}
