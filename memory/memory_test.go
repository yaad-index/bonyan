package memory_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/inmem"
	"github.com/yaad-index/bonyan/trust"
)

var ctx = context.Background()

// policy answers with whatever it is set to, and records what it was asked.
type policy struct {
	mu      sync.Mutex
	verdict trust.Verdict
	err     error
	asked   []content.Provenance
}

func (p *policy) set(v trust.Verdict, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verdict, p.err = v, err
}

func (p *policy) Classify(_ context.Context, src content.Provenance) (trust.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, src)
	return trust.Decision{Verdict: p.verdict}, p.err
}

// clock is a settable time.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(t *testing.T, b memory.Backend, p trust.Policy, c *clock) *memory.Store {
	t.Helper()
	s, err := memory.NewStore(b, memory.Options{Policy: p, PolicyName: "program", Retention: 24 * time.Hour, Now: c.now})
	require.NoError(t, err)
	return s
}

var start = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

// What comes back is trusted only when the policy trusted it both when it was
// stored and now.
func TestTheStricterDecisionWins(t *testing.T) {
	for _, tc := range []struct {
		stored, now trust.Verdict
		trusted     bool
	}{
		{trust.Trusted, trust.Trusted, true},
		{trust.Trusted, trust.Untrusted, false},
		{trust.Untrusted, trust.Trusted, false},
		{trust.Untrusted, trust.Untrusted, false},
	} {
		t.Run(tc.stored.String()+" then "+tc.now.String(), func(t *testing.T) {
			p := &policy{}
			s := newStore(t, inmem.New(), p, &clock{start})
			p.set(tc.stored, nil)
			require.NoError(t, s.Remember(ctx, "ana", content.KindUser, "prefers mail"))
			require.NoError(t, s.Append(ctx, "ana", "s1", content.KindUser, "hello"))
			p.set(tc.now, nil)

			facts, err := s.Recall(ctx, "ana", "", 10)
			require.NoError(t, err)
			events, err := s.History(ctx, "ana", "s1")
			require.NoError(t, err)
			for _, got := range [][]content.Text{facts, events} {
				require.Len(t, got, 1)
				assert.Equal(t, tc.trusted, got[0].Trusted())
				if tr, ok := got[0].(content.Trusted); ok {
					assert.Equal(t, content.KindMemory, tr.Provenance().Kind, "trusted memory is still memory")
					assert.Equal(t, content.KindUser, tr.Provenance().Origin)
				}
			}
		})
	}
}

// A policy that fails is an untrusted decision, when storing and when reading.
func TestAFailingPolicyIsUntrusted(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		storeErr, readErr    error
		storeVerdict, readOK trust.Verdict
	}{
		{name: "when storing", storeErr: errors.New("down"), storeVerdict: trust.Trusted, readOK: trust.Trusted},
		{name: "when reading", readErr: errors.New("down"), storeVerdict: trust.Trusted, readOK: trust.Trusted},
		{name: "with no decision", storeVerdict: trust.NoDecision, readOK: trust.Trusted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &policy{}
			s := newStore(t, inmem.New(), p, &clock{start})
			p.set(tc.storeVerdict, tc.storeErr)
			require.NoError(t, s.Remember(ctx, "ana", content.KindUser, "prefers mail"))
			p.set(tc.readOK, tc.readErr)
			got, err := s.Recall(ctx, "ana", "", 10)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.False(t, got[0].Trusted())
		})
	}
}

// The decision a record is stored under is the Store's own, from the policy,
// with the policy's name; a caller has no way to supply one.
func TestTheStoredDecisionIsTheStores(t *testing.T) {
	b := inmem.New()
	p := &policy{verdict: trust.Trusted}
	s := newStore(t, b, p, &clock{start})
	require.NoError(t, s.Remember(ctx, "ana", content.KindFetched, "lives in a small town"))

	recs, err := b.Recall(ctx, "ana", "", 10, time.Time{})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, memory.Decision{Verdict: trust.Trusted, Policy: "program"}, recs[0].Decision)
	assert.True(t, recs[0].At.Equal(start))
	require.Len(t, p.asked, 1)
	assert.Equal(t, content.Provenance{Kind: content.KindMemory, Origin: content.KindFetched}, p.asked[0],
		"the policy classifies memory that came from fetched material")
}

// Untrusted memory comes back as memory, with the kind it came from.
func TestUntrustedMemoryKeepsItsOrigin(t *testing.T) {
	s := newStore(t, inmem.New(), nil, &clock{start})
	require.NoError(t, s.Remember(ctx, "ana", content.KindTool, "prefers mail"))
	got, err := s.Recall(ctx, "ana", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	u, ok := got[0].(content.Untrusted)
	require.True(t, ok, "got %T", got[0])
	assert.Equal(t, content.KindMemory, u.Provenance().Kind)
	assert.Equal(t, content.KindTool, u.Provenance().Origin)
	assert.Equal(t, "prefers mail", u.Raw())
}

// careless returns every record it holds, ignoring the layer, subject, session
// and cutoff, as a faulty backend might.
type careless struct{ recs []memory.Record }

func (c *careless) Write(_ context.Context, r memory.Record) (string, error) {
	c.recs = append(c.recs, r)
	return "x", nil
}

func (c *careless) History(context.Context, string, string, time.Time) ([]memory.Record, error) {
	return c.recs, nil
}

func (c *careless) Recall(context.Context, string, string, int, time.Time) ([]memory.Record, error) {
	return c.recs, nil
}
func (c *careless) DeleteSubject(context.Context, string) error   { return nil }
func (c *careless) DeleteBefore(context.Context, time.Time) error { return nil }

// The Store leaves out what is older than the retention period or from another
// layer, subject or session, whatever the backend returns.
func TestTheStoreHonoursRetentionAndSubjectOnRead(t *testing.T) {
	c := &clock{start}
	b := &careless{}
	s := newStore(t, b, nil, c)
	require.NoError(t, s.Remember(ctx, "ana", content.KindUser, "old"))
	require.NoError(t, s.Append(ctx, "ana", "s1", content.KindUser, "old"))
	c.t = start.Add(23 * time.Hour)
	require.NoError(t, s.Remember(ctx, "ana", content.KindUser, "recent"))
	require.NoError(t, s.Append(ctx, "ana", "s1", content.KindUser, "recent"))
	require.NoError(t, s.Remember(ctx, "bo", content.KindUser, "another subject"))
	require.NoError(t, s.Append(ctx, "ana", "s2", content.KindUser, "another session"))
	b.recs = append(b.recs, memory.Record{Layer: memory.LongTerm, Subject: "ana", Session: "s1", Origin: content.KindUser, Text: "a fact in a session", At: c.t})
	c.t = start.Add(25 * time.Hour)

	facts, err := s.Recall(ctx, "ana", "", 10)
	require.NoError(t, err)
	events, err := s.History(ctx, "ana", "s1")
	require.NoError(t, err)
	for _, got := range [][]content.Text{facts, events} {
		require.Len(t, got, 1)
		assert.Equal(t, "recent", got[0].(content.Untrusted).Raw())
	}
}

// Purge deletes what is older than the retention period from the backend.
func TestPurgeDeletesWhatRetentionExpired(t *testing.T) {
	c := &clock{start}
	b := inmem.New()
	s := newStore(t, b, nil, c)
	require.NoError(t, s.Remember(ctx, "ana", content.KindUser, "old"))
	c.t = start.Add(23 * time.Hour)
	require.NoError(t, s.Remember(ctx, "ana", content.KindUser, "recent"))
	c.t = start.Add(25 * time.Hour)
	require.NoError(t, s.Purge(ctx))

	recs, err := b.Recall(ctx, "ana", "", 10, time.Time{})
	require.NoError(t, err)
	require.Len(t, recs, 1, "read with no cutoff, so only the purge explains the absence")
	assert.Equal(t, "recent", recs[0].Text)
}

func TestDeleteSubject(t *testing.T) {
	s := newStore(t, inmem.New(), nil, &clock{start})
	require.NoError(t, s.Remember(ctx, "ana", content.KindUser, "a fact"))
	require.NoError(t, s.Append(ctx, "ana", "s1", content.KindUser, "an event"))
	require.NoError(t, s.DeleteSubject(ctx, "ana"))
	facts, err := s.Recall(ctx, "ana", "", 10)
	require.NoError(t, err)
	assert.Empty(t, facts)
	events, err := s.History(ctx, "ana", "s1")
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestInvalidCalls(t *testing.T) {
	_, err := memory.NewStore(inmem.New(), memory.Options{})
	require.ErrorIs(t, err, memory.ErrInvalid, "no retention")
	_, err = memory.NewStore(nil, memory.Options{Retention: time.Hour})
	require.ErrorIs(t, err, memory.ErrInvalid, "no backend")

	s := newStore(t, inmem.New(), nil, &clock{start})
	for name, err := range map[string]error{
		"empty subject":  s.Remember(ctx, "", content.KindUser, "x"),
		"empty origin":   s.Remember(ctx, "ana", "", "x"),
		"empty session":  s.Append(ctx, "ana", "", content.KindUser, "x"),
		"zero limit":     func() error { _, err := s.Recall(ctx, "ana", "", 0); return err }(),
		"history no sub": func() error { _, err := s.History(ctx, "", "s1"); return err }(),
		"delete no sub":  s.DeleteSubject(ctx, ""),
	} {
		assert.ErrorIs(t, err, memory.ErrInvalid, name)
	}
}

// The policy runs on every read, so a recall's decision is made and recorded
// even when the stored one already settles the answer.
func TestThePolicyRunsOnEveryRead(t *testing.T) {
	p := &policy{verdict: trust.Untrusted}
	s := newStore(t, inmem.New(), p, &clock{start})
	require.NoError(t, s.Remember(ctx, "ana", content.KindUser, "prefers mail"))
	require.NoError(t, s.Append(ctx, "ana", "s1", content.KindUser, "hello"))
	_, err := s.Recall(ctx, "ana", "", 10)
	require.NoError(t, err)
	_, err = s.History(ctx, "ana", "s1")
	require.NoError(t, err)
	assert.Len(t, p.asked, 4, "two writes and two reads")
}

// DeleteSubject deletes from the backend and from everything added with
// OnDeleteSubject, tries each even when another fails, and names every one
// that failed: it never reports a partial deletion as done.
func TestDeleteSubjectReachesEveryDeleter(t *testing.T) {
	s := newStore(t, inmem.New(), nil, &clock{start})
	require.NoError(t, s.Remember(ctx, "ana", content.KindUser, "a fact"))
	var called []string
	s.OnDeleteSubject("first", func(_ context.Context, subject string) error {
		called = append(called, "first:"+subject)
		return errors.New("disk full")
	})
	s.OnDeleteSubject("second", func(_ context.Context, subject string) error {
		called = append(called, "second:"+subject)
		return nil
	})
	err := s.DeleteSubject(ctx, "ana")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "first")
	assert.NotContains(t, err.Error(), "second")
	assert.Equal(t, []string{"first:ana", "second:ana"}, called, "every deleter is tried")
	got, rerr := s.Recall(ctx, "ana", "", 10)
	require.NoError(t, rerr)
	assert.Empty(t, got, "the backend's part is done all the same")

	called = nil
	s2 := newStore(t, inmem.New(), nil, &clock{start})
	s2.OnDeleteSubject("only", func(context.Context, string) error { called = append(called, "only"); return nil })
	require.NoError(t, s2.DeleteSubject(ctx, "ana"))
	assert.Equal(t, []string{"only"}, called)
}
