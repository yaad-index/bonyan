package approval_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/approval"
)

// Holding an action held already adds a waiter for the same decision, as a
// resumed run does; it must be the same tool.
func TestAHeldActionCanBeWaitedOnAgain(t *testing.T) {
	ctx := context.Background()
	s := approval.NewMemory()
	first, err := s.Hold(ctx, approval.Pending{ID: "a1", Tool: "search"})
	require.NoError(t, err)
	second, err := s.Hold(ctx, approval.Pending{ID: "a1", Tool: "search"})
	require.NoError(t, err)
	_, err = s.Hold(ctx, approval.Pending{ID: "a1", Tool: "delete"})
	require.Error(t, err, "another tool under the same id")
	held, err := s.List(ctx)
	require.NoError(t, err)
	assert.Len(t, held, 1, "one action")
	require.NoError(t, s.Decide(ctx, "a1", true))
	assert.True(t, <-first)
	assert.True(t, <-second)
	require.ErrorIs(t, s.Decide(ctx, "a1", true), approval.ErrUnknown)
}
