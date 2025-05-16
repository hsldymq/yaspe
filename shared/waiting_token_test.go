package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWaitingImpl_IsDone(t *testing.T) {
	waiting := NewWaitingTokenImpl()

	assert.False(t, waiting.IsDone(), "Waiting should not be done")
	waiting.Done()
	waiting.Done()
	assert.True(t, waiting.IsDone(), "Waiting should be done")
}
