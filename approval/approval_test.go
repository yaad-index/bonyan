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

func receive(t *testing.T, ch <-chan approval.Decision) approval.Decision {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		require.Fail(t, "no decision delivered")
		return approval.Decision{}
	}
}

func nothingMore(t *testing.T, ch <-chan approval.Decision) {
	t.Helper()
	select {
	case v := <-ch:
		require.Failf(t, "a decision delivered twice or from nowhere", "%+v", v)
	case <-time.After(50 * time.Millisecond):
	}
}

func action(id string, approvers ...string) approval.Pending {
	return approval.Pending{ID: id, Tool: "search", Approvers: approvers}
}

// Each approver decides on its own, once; every waiter receives each
// decision, a waiter added later receives the decisions made already first,
// and the action is held until it is dropped.
func TestEachApproverDecidesOnItsOwn(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			first, err := s.Hold(ctx, action("a1", "ana", "bo", "cy"))
			require.NoError(t, err)
			require.NoError(t, s.Decide(ctx, "a1", "bo", true))
			assert.Equal(t, approval.Decision{By: "bo", Approve: true}, receive(t, first))

			second, err := s.Hold(ctx, action("a1", "ana", "bo", "cy"))
			require.NoError(t, err)
			assert.Equal(t, approval.Decision{By: "bo", Approve: true}, receive(t, second), "made already")
			held, err := s.List(ctx)
			require.NoError(t, err)
			assert.Equal(t, []approval.Pending{action("a1", "ana", "cy")}, held, "who has not decided")

			require.ErrorIs(t, s.Decide(ctx, "a1", "bo", false), approval.ErrDecided)
			require.ErrorIs(t, s.Decide(ctx, "a1", "dee", true), approval.ErrUnknown, "not one of its approvers")
			require.NoError(t, s.Decide(ctx, "a1", "cy", false))
			for _, ch := range []<-chan approval.Decision{first, second} {
				assert.Equal(t, approval.Decision{By: "cy", Approve: false}, receive(t, ch))
			}
			held, err = s.List(ctx)
			require.NoError(t, err)
			assert.Empty(t, held, "denied: nothing to wait for")

			require.NoError(t, s.Decide(ctx, "a1", "ana", true), "held until dropped")
			for _, ch := range []<-chan approval.Decision{first, second} {
				assert.Equal(t, approval.Decision{By: "ana", Approve: true}, receive(t, ch))
				nothingMore(t, ch)
			}
			require.ErrorIs(t, s.Decide(ctx, "a1", "ana", true), approval.ErrDecided)
		})
	}
}

// Every approver approving leaves nothing waiting.
func TestAllApprovedLeavesNothingWaiting(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			_, err := s.Hold(ctx, action("a1", "ana", "bo"))
			require.NoError(t, err)
			require.NoError(t, s.Decide(ctx, "a1", "ana", true))
			held, err := s.List(ctx)
			require.NoError(t, err)
			assert.Equal(t, []approval.Pending{action("a1", "bo")}, held)
			require.NoError(t, s.Decide(ctx, "a1", "bo", true))
			held, err = s.List(ctx)
			require.NoError(t, err)
			assert.Empty(t, held)
		})
	}
}

// An action held again must be for the same tool and approvers, and one
// with no approver, a nameless one or one named twice is refused.
func TestHoldingRefusesAnotherAction(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			_, err := s.Hold(ctx, action("a1", "ana", "bo"))
			require.NoError(t, err)
			other := action("a1", "ana", "bo")
			other.Tool = "delete"
			_, err = s.Hold(ctx, other)
			require.Error(t, err, "another tool under the same id")
			_, err = s.Hold(ctx, action("a1", "ana"))
			require.Error(t, err, "other approvers under the same id")
			_, err = s.Hold(ctx, action("a1", "bo", "ana"))
			require.Error(t, err, "the same approvers in another order")
			for _, bad := range []approval.Pending{
				{Tool: "search", Approvers: []string{"ana"}},
				action("a2"),
				action("a2", "ana", ""),
				action("a2", "ana", "bo", "ana"),
			} {
				_, err = s.Hold(ctx, bad)
				require.Error(t, err, "%+v", bad)
			}
			held, err := s.List(ctx)
			require.NoError(t, err)
			assert.Equal(t, []approval.Pending{action("a1", "ana", "bo")}, held, "only the first")
		})
	}
}

// A dropped action, or one never held, is unknown.
func TestADroppedActionIsUnknown(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			_, err := s.Hold(ctx, action("a1", "ana"))
			require.NoError(t, err)
			require.NoError(t, s.Drop(ctx, "a1"))
			require.ErrorIs(t, s.Decide(ctx, "a1", "ana", true), approval.ErrUnknown)
			require.ErrorIs(t, s.Decide(ctx, "never", "ana", true), approval.ErrUnknown)
		})
	}
}

// A Dir keeps the decisions made while no process waits, through a restart,
// and delivers them to the next waiter at once; a decision made through
// another handle on the directory, as by another process, reaches a waiter.
// Purge removes what was held before a time.
func TestADirKeepsDecisionsForTheNextWaiter(t *testing.T) {
	path := t.TempDir() + "/approvals"
	before, err := approval.OpenDir(path)
	require.NoError(t, err)
	hctx, stop := context.WithCancel(ctx)
	_, err = before.Hold(hctx, action("a1", "ana", "bo"))
	require.NoError(t, err)
	stop() // the process stopped while it waited

	after, err := approval.OpenDir(path)
	require.NoError(t, err)
	after.PollEvery = 5 * time.Millisecond
	require.NoError(t, after.Decide(ctx, "a1", "bo", true))
	ch, err := after.Hold(ctx, action("a1", "ana", "bo"))
	require.NoError(t, err)
	select {
	case v := <-ch:
		assert.Equal(t, approval.Decision{By: "bo", Approve: true}, v)
	default:
		require.Fail(t, "a kept decision is in the channel when Hold returns")
	}
	other, err := approval.OpenDir(path)
	require.NoError(t, err)
	require.NoError(t, other.Decide(ctx, "a1", "ana", false))
	assert.Equal(t, approval.Decision{By: "ana", Approve: false}, receive(t, ch))
	nothingMore(t, ch)

	now := time.Now()
	after.Now = func() time.Time { return now.Add(time.Hour) }
	_, err = after.Hold(ctx, action("a3", "ana"))
	require.NoError(t, err)
	require.NoError(t, after.Purge(ctx, now.Add(time.Minute)))
	require.ErrorIs(t, after.Decide(ctx, "a1", "ana", true), approval.ErrUnknown)
	held, err := after.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []approval.Pending{action("a3", "ana")}, held, "only what was held after the cut")
}
