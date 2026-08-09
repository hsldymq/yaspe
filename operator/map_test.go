package operator_test

import (
	"context"
	"errors"
	"github.com/hsldymq/yaspe"
	"github.com/hsldymq/yaspe/operator"
	"strconv"
	"testing"
)

type collectingSink[T any] struct {
	records []yaspe.Record[T]
	err     error
}

func (c *collectingSink[T]) Emit(
	_ context.Context,
	record yaspe.Record[T],
) error {
	if c.err != nil {
		return c.err
	}

	c.records = append(c.records, record)
	return nil
}

func TestMapTransformsRecord(t *testing.T) {
	op := operator.NewMap(func(
		_ context.Context,
		value int,
	) (string, error) {
		return strconv.Itoa(value), nil
	})

	output := &collectingSink[string]{}

	err := op.Process(
		context.Background(),
		yaspe.Record[int]{Value: 42},
		output,
	)

	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	if len(output.records) != 1 {
		t.Fatalf("got %d records, want 1", len(output.records))
	}

	if output.records[0].Value != "42" {
		t.Fatalf(
			"got value %q, want %q",
			output.records[0].Value,
			"42",
		)
	}
}

func TestMapDoesNotEmitWhenTransformFails(t *testing.T) {
	transformErr := errors.New("transform failed")

	op := operator.NewMap(func(
		_ context.Context,
		_ int,
	) (string, error) {
		return "", transformErr
	})

	output := &collectingSink[string]{}

	err := op.Process(
		context.Background(),
		yaspe.Record[int]{Value: 42},
		output,
	)

	if !errors.Is(err, transformErr) {
		t.Fatalf("got error %v, want %v", err, transformErr)
	}

	if len(output.records) != 0 {
		t.Fatalf(
			"got %d emitted records, want 0",
			len(output.records),
		)
	}
}

func TestMapReturnsEmitFailure(t *testing.T) {
	emitErr := errors.New("emit failed")

	op := operator.NewMap(func(
		_ context.Context,
		value int,
	) (string, error) {
		return strconv.Itoa(value), nil
	})

	output := &collectingSink[string]{
		err: emitErr,
	}

	err := op.Process(
		context.Background(),
		yaspe.Record[int]{Value: 42},
		output,
	)

	if !errors.Is(err, emitErr) {
		t.Fatalf("got error %v, want %v", err, emitErr)
	}
}

func TestMapPassesContextToTransform(t *testing.T) {
	type contextKey struct{}

	ctx := context.WithValue(
		context.Background(),
		contextKey{},
		"expected",
	)

	op := operator.NewMap(func(
		got context.Context,
		value int,
	) (int, error) {
		if got.Value(contextKey{}) != "expected" {
			return 0, errors.New("context not propagated")
		}

		return value * 2, nil
	})

	output := &collectingSink[int]{}

	if err := op.Process(
		ctx,
		yaspe.Record[int]{Value: 21},
		output,
	); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
}
