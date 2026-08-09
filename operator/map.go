package operator

import (
	"context"

	"github.com/hsldymq/yaspe"
)

type MapFunc[I, O any] func(context.Context, I) (O, error)

type Map[I, O any] struct {
	transform MapFunc[I, O]
}

func NewMap[I, O any](transform MapFunc[I, O]) *Map[I, O] {
	return &Map[I, O]{transform: transform}
}

func (m *Map[I, O]) Process(
	ctx context.Context,
	input yaspe.Record[I],
	output yaspe.Collector[O],
) error {
	value, err := m.transform(ctx, input.Value)
	if err != nil {
		return err
	}

	return output.Emit(ctx, yaspe.Record[O]{
		Value: value,
	})
}
