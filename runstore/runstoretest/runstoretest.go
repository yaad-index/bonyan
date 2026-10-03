// Package runstoretest is the conformance suite every run store must pass
// (ADR 0001 §7): a saved run comes back as saved, only one claim holds it at a
// time, its action proceeds once, and deleting a subject or what retention
// expired removes it.
package runstoretest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/runstore"
)

// Run runs the suite. newStore returns an empty store for each test.
func Run(t *testing.T, newStore func(t *testing.T) runstore.Store) {
	t.Helper()
	for _, c := range []struct {
		name string
		test func(t *testing.T, s runstore.Store)
	}{
		{"ASavedRunComesBackAsSaved", aSavedRunComesBackAsSaved},
		{"OnlyOneClaimHoldsARun", onlyOneClaimHoldsARun},
		{"AClaimPassesWithTheDeadline", aClaimPassesWithTheDeadline},
		{"AnActionProceedsOnce", anActionProceedsOnce},
		{"SavingAgainReleasesTheClaim", savingAgainReleasesTheClaim},
		{"OnlyTheHolderDeletes", onlyTheHolderDeletes},
		{"DeleteSubjectAndDeleteBefore", deleteSubjectAndDeleteBefore},
		{"ConcurrentClaimsHaveOneWinner", concurrentClaimsHaveOneWinner},
	} {
		t.Run(c.name, func(t *testing.T) { c.test(t, newStore(t)) })
	}
}

var (
	ctx   = context.Background()
	start = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
)

func state(run, subject string) runstore.State {
	return runstore.State{Run: run, Subject: subject, Saved: start, Deadline: start.Add(time.Hour), Data: []byte("state of " + run)}
}

func aSavedRunComesBackAsSaved(t *testing.T, s runstore.Store) {
	want := state("r1", "ana")
	require.NoError(t, s.Save(ctx, want, ""))
	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assertState(t, want, list[0])
	got, err := s.Claim(ctx, "r1", "t1", start)
	require.NoError(t, err)
	assertState(t, want, got)
	_, err = s.Claim(ctx, "nothing", "t1", start)
	require.ErrorIs(t, err, runstore.ErrUnknown)
}

func assertState(t *testing.T, want, got runstore.State) {
	t.Helper()
	assert.Equal(t, want.Run, got.Run)
	assert.Equal(t, want.Subject, got.Subject)
	assert.True(t, want.Saved.Equal(got.Saved), "saved %s, got %s", want.Saved, got.Saved)
	assert.True(t, want.Deadline.Equal(got.Deadline), "deadline %s, got %s", want.Deadline, got.Deadline)
	assert.Equal(t, want.Data, got.Data)
}

func onlyOneClaimHoldsARun(t *testing.T, s runstore.Store) {
	require.NoError(t, s.Save(ctx, state("r1", "ana"), ""))
	_, err := s.Claim(ctx, "r1", "t1", start)
	require.NoError(t, err)
	_, err = s.Claim(ctx, "r1", "t2", start.Add(time.Minute))
	require.ErrorIs(t, err, runstore.ErrClaimed)
	require.ErrorIs(t, s.Proceed(ctx, "r1", ""), runstore.ErrClaimed, "the process that ran it lost it to the claim")
	require.ErrorIs(t, s.Proceed(ctx, "r1", "t2"), runstore.ErrClaimed)
	require.NoError(t, s.Proceed(ctx, "r1", "t1"))
	require.ErrorIs(t, s.Proceed(ctx, "r1", ""), runstore.ErrClaimed, "once the holder went ahead, still claimed by it")
	require.ErrorIs(t, s.Proceed(ctx, "r1", "t1"), runstore.ErrProceeding)
}

func aClaimPassesWithTheDeadline(t *testing.T, s runstore.Store) {
	require.NoError(t, s.Save(ctx, state("r1", "ana"), ""))
	_, err := s.Claim(ctx, "r1", "t1", start)
	require.NoError(t, err)
	_, err = s.Claim(ctx, "r1", "t2", start.Add(time.Hour))
	require.NoError(t, err, "the first claim passed with the run's deadline")
	require.ErrorIs(t, s.Proceed(ctx, "r1", "t1"), runstore.ErrClaimed)
}

func anActionProceedsOnce(t *testing.T, s runstore.Store) {
	require.NoError(t, s.Save(ctx, state("r1", "ana"), ""))
	require.NoError(t, s.Proceed(ctx, "r1", ""))
	require.ErrorIs(t, s.Proceed(ctx, "r1", ""), runstore.ErrProceeding)
	_, err := s.Claim(ctx, "r1", "t1", start)
	require.ErrorIs(t, err, runstore.ErrProceeding, "a run whose action proceeds cannot be resumed")
	require.ErrorIs(t, s.Proceed(ctx, "nothing", ""), runstore.ErrUnknown)
}

func savingAgainReleasesTheClaim(t *testing.T, s runstore.Store) {
	require.NoError(t, s.Save(ctx, state("r1", "ana"), ""))
	_, err := s.Claim(ctx, "r1", "t1", start)
	require.NoError(t, err)
	require.ErrorIs(t, s.Save(ctx, state("r1", "ana"), ""), runstore.ErrClaimed, "only the holder saves")
	require.NoError(t, s.Proceed(ctx, "r1", "t1"))
	next := state("r1", "ana")
	next.Data = []byte("next action")
	require.NoError(t, s.Save(ctx, next, "t1"), "the holder saves its next pending action")
	got, err := s.Claim(ctx, "r1", "t2", start)
	require.NoError(t, err, "saved again: unclaimed, not proceeding")
	assert.Equal(t, []byte("next action"), got.Data)
}

func onlyTheHolderDeletes(t *testing.T, s runstore.Store) {
	require.NoError(t, s.Save(ctx, state("r1", "ana"), ""))
	_, err := s.Claim(ctx, "r1", "t1", start)
	require.NoError(t, err)
	require.ErrorIs(t, s.Delete(ctx, "r1", ""), runstore.ErrClaimed)
	require.NoError(t, s.Delete(ctx, "r1", "t1"))
	require.NoError(t, s.Delete(ctx, "r1", "t1"), "gone already")
	list, err := s.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func deleteSubjectAndDeleteBefore(t *testing.T, s runstore.Store) {
	old := state("r1", "ana")
	old.Saved = start.Add(-time.Hour)
	require.NoError(t, s.Save(ctx, old, ""))
	require.NoError(t, s.Save(ctx, state("r2", "ana"), ""))
	require.NoError(t, s.Save(ctx, state("r3", "bo"), ""))
	_, err := s.Claim(ctx, "r2", "t1", start)
	require.NoError(t, err)

	require.NoError(t, s.DeleteBefore(ctx, start))
	require.NoError(t, s.DeleteSubject(ctx, "ana"))
	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1, "claimed or not")
	assert.Equal(t, "r3", list[0].Run)
}

func concurrentClaimsHaveOneWinner(t *testing.T, s runstore.Store) {
	require.NoError(t, s.Save(ctx, state("r1", "ana"), ""))
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Claim(ctx, "r1", "t"+string(rune('a'+i)), start); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, won)
}
