package approval_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/approval"
)

var ctx = context.Background()

func stores(t *testing.T) map[string]approval.Store {
	t.Helper()
	d, err := approval.OpenDir(t.TempDir() + "/approvals")
	require.NoError(t, err)
	d.PollEvery = 5 * time.Millisecond
	return map[string]approval.Store{"memory": approval.NewMemory(), "dir": d}
}

func receive(t *testing.T, ch <-chan bool) bool {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		require.Fail(t, "no decision delivered")
		return false
	}
}

// Holding an action held already adds a waiter for the same decision, as a
// resumed run does; it must be the same tool. A decision is made once and
// reaches every waiter.
func TestAHeldActionCanBeWaitedOnAgain(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			first, err := s.Hold(ctx, approval.Pending{ID: "a1", Tool: "search"})
			require.NoError(t, err)
			second, err := s.Hold(ctx, approval.Pending{ID: "a1", Tool: "search"})
			require.NoError(t, err)
			_, err = s.Hold(ctx, approval.Pending{ID: "a1", Tool: "delete"})
			require.Error(t, err, "another tool under the same id")
			held, err := s.List(ctx)
			require.NoError(t, err)
			assert.Equal(t, []approval.Pending{{ID: "a1", Tool: "search"}}, held, "one action")
			require.NoError(t, s.Decide(ctx, "a1", false))
			assert.False(t, receive(t, first))
			assert.False(t, receive(t, second))
			require.ErrorIs(t, s.Decide(ctx, "a1", true), approval.ErrUnknown, "decided once")
			held, err = s.List(ctx)
			require.NoError(t, err)
			assert.Empty(t, held, "nothing waits for a decision")
		})
	}
}

// A dropped action, or one never held, is unknown.
func TestADroppedActionIsUnknown(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			_, err := s.Hold(ctx, approval.Pending{ID: "a1", Tool: "search"})
			require.NoError(t, err)
			require.NoError(t, s.Drop(ctx, "a1"))
			require.ErrorIs(t, s.Decide(ctx, "a1", true), approval.ErrUnknown)
			require.ErrorIs(t, s.Decide(ctx, "never", true), approval.ErrUnknown)
			_, err = s.Hold(ctx, approval.Pending{Tool: "search"})
			require.Error(t, err, "no id")
		})
	}
}

// A Dir keeps a decision made while no process waits, through a restart,
// and delivers it to the next waiter at once; a decision made through
// another handle on the directory, as by another process, reaches a waiter.
// Purge removes what was held before a time.
func TestADirKeepsADecisionForTheNextWaiter(t *testing.T) {
	path := t.TempDir() + "/approvals"
	before, err := approval.OpenDir(path)
	require.NoError(t, err)
	hctx, stop := context.WithCancel(ctx)
	_, err = before.Hold(hctx, approval.Pending{ID: "a1", Tool: "search"})
	require.NoError(t, err)
	stop() // the process stopped while it waited

	after, err := approval.OpenDir(path)
	require.NoError(t, err)
	require.NoError(t, after.Decide(ctx, "a1", true))
	ch, err := after.Hold(ctx, approval.Pending{ID: "a1", Tool: "search"})
	require.NoError(t, err)
	select {
	case v := <-ch:
		assert.True(t, v)
	default:
		require.Fail(t, "a kept decision is in the channel when Hold returns")
	}

	other, err := approval.OpenDir(path)
	require.NoError(t, err)
	after.PollEvery = 5 * time.Millisecond
	ch, err = after.Hold(ctx, approval.Pending{ID: "a2", Tool: "search"})
	require.NoError(t, err)
	require.NoError(t, other.Decide(ctx, "a2", false))
	assert.False(t, receive(t, ch))

	now := time.Now()
	after.Now = func() time.Time { return now.Add(time.Hour) }
	_, err = after.Hold(ctx, approval.Pending{ID: "a3", Tool: "search"})
	require.NoError(t, err)
	require.NoError(t, after.Purge(ctx, now.Add(time.Minute)))
	require.ErrorIs(t, after.Decide(ctx, "a1", true), approval.ErrUnknown)
	held, err := after.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []approval.Pending{{ID: "a3", Tool: "search"}}, held, "only what was held after the cut")
}
