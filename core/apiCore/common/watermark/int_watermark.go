package watermark

type IntWatermark struct {
	id  string
	val int
}

func NewIntWatermark(id string, val int) *IntWatermark {
	return &IntWatermark{
		id:  id,
		val: val,
	}
}

func (iw *IntWatermark) Identifier() string {
	return iw.id
}

func (iw *IntWatermark) Value() int {
	return iw.val
}

func (iw *IntWatermark) EqualTo(obj any) bool {
	w, ok := obj.(*IntWatermark)
	if !ok || w == nil {
		return false
	}

	return *iw == *w
}
