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
	"github.com/yaad-index/bonyan/secret"
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
	s, err := memory.NewStore(b, memory.Options{Namespace: "test", Policy: p, PolicyName: "program", Retention: 24 * time.Hour, Now: c.now})
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
			s := newStore(t, inmem.New("test"), p, &clock{start})
			p.set(tc.stored, nil)
			require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindUser}, "prefers mail"))
			require.NoError(t, s.Append(ctx, "ana", "s1", content.Provenance{Kind: content.KindUser}, "hello"))
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
			s := newStore(t, inmem.New("test"), p, &clock{start})
			p.set(tc.storeVerdict, tc.storeErr)
			require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindUser}, "prefers mail"))
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
	b := inmem.New("test")
	p := &policy{verdict: trust.Trusted}
	s := newStore(t, b, p, &clock{start})
	require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindFetched}, "lives in a small town"))

	recs, err := b.Recall(ctx, "test/ana", "", 10, time.Time{})
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
	s := newStore(t, inmem.New("test"), nil, &clock{start})
	require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindTool}, "prefers mail"))
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

func (*careless) Namespace() string { return "test" }

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
	require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindUser}, "old"))
	require.NoError(t, s.Append(ctx, "ana", "s1", content.Provenance{Kind: content.KindUser}, "old"))
	c.t = start.Add(23 * time.Hour)
	require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindUser}, "recent"))
	require.NoError(t, s.Append(ctx, "ana", "s1", content.Provenance{Kind: content.KindUser}, "recent"))
	require.NoError(t, s.Remember(ctx, "bo", content.Provenance{Kind: content.KindUser}, "another subject"))
	require.NoError(t, s.Append(ctx, "ana", "s2", content.Provenance{Kind: content.KindUser}, "another session"))
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
	b := inmem.New("test")
	s := newStore(t, b, nil, c)
	require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindUser}, "old"))
	c.t = start.Add(23 * time.Hour)
	require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindUser}, "recent"))
	c.t = start.Add(25 * time.Hour)
	require.NoError(t, s.Purge(ctx))

	recs, err := b.Recall(ctx, "test/ana", "", 10, time.Time{})
	require.NoError(t, err)
	require.Len(t, recs, 1, "read with no cutoff, so only the purge explains the absence")
	assert.Equal(t, "recent", recs[0].Text)
}

func TestDeleteSubject(t *testing.T) {
	s := newStore(t, inmem.New("test"), nil, &clock{start})
	require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindUser}, "a fact"))
	require.NoError(t, s.Append(ctx, "ana", "s1", content.Provenance{Kind: content.KindUser}, "an event"))
	require.NoError(t, s.DeleteSubject(ctx, "ana"))
	facts, err := s.Recall(ctx, "ana", "", 10)
	require.NoError(t, err)
	assert.Empty(t, facts)
	events, err := s.History(ctx, "ana", "s1")
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestInvalidCalls(t *testing.T) {
	_, err := memory.NewStore(inmem.New("test"), memory.Options{Namespace: "test"})
	require.ErrorIs(t, err, memory.ErrInvalid, "no retention")
	_, err = memory.NewStore(nil, memory.Options{Namespace: "test", Retention: time.Hour})
	require.ErrorIs(t, err, memory.ErrInvalid, "no backend")

	s := newStore(t, inmem.New("test"), nil, &clock{start})
	for name, err := range map[string]error{
		"empty subject":  s.Remember(ctx, "", content.Provenance{Kind: content.KindUser}, "x"),
		"empty origin":   s.Remember(ctx, "ana", content.Provenance{Kind: ""}, "x"),
		"empty session":  s.Append(ctx, "ana", "", content.Provenance{Kind: content.KindUser}, "x"),
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
	s := newStore(t, inmem.New("test"), p, &clock{start})
	require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindUser}, "prefers mail"))
	require.NoError(t, s.Append(ctx, "ana", "s1", content.Provenance{Kind: content.KindUser}, "hello"))
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
	s := newStore(t, inmem.New("test"), nil, &clock{start})
	require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindUser}, "a fact"))
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
	s2 := newStore(t, inmem.New("test"), nil, &clock{start})
	s2.OnDeleteSubject("only", func(context.Context, string) error { called = append(called, "only"); return nil })
	require.NoError(t, s2.DeleteSubject(ctx, "ana"))
	assert.Equal(t, []string{"only"}, called)
}

// trustServer trusts memory extracted from remote tool output of the one
// server it names.
type trustServer string

func (p trustServer) Classify(_ context.Context, src content.Provenance) (trust.Decision, error) {
	if src.Kind == content.KindMemory && src.Origin == content.KindRemoteTool && src.Server == string(p) {
		return trust.Decision{Verdict: trust.Trusted}, nil
	}
	return trust.Decision{Verdict: trust.Untrusted}, nil
}

// A fact extracted from a tool server's output keeps the server's name, so on
// recall it is classified again under that server, never another or the
// program's own tools.
func TestARecalledFactKeepsItsServer(t *testing.T) {
	for _, tc := range []struct {
		policy  trust.Policy
		trusted bool
	}{
		{trustServer("docs"), true},
		{trustServer("web"), false},
		{trustOrigin(content.KindTool), false},
	} {
		s := newStore(t, inmem.New("test"), tc.policy, &clock{t: start})
		require.NoError(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindRemoteTool, Server: "docs", ID: "c1"}, "the docs say so"))
		got, err := s.Recall(ctx, "ana", "", 10)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, tc.trusted, got[0].Trusted(), "%v", tc.policy)
		var from content.Provenance
		switch v := got[0].(type) {
		case content.Trusted:
			from = v.Provenance()
		case content.Untrusted:
			from = v.Provenance()
		}
		assert.Equal(t, content.KindRemoteTool, from.Origin)
		assert.Equal(t, "docs", from.Server)
	}
}

// Only remote tool output has a server.
func TestAServerNeedsRemoteToolOutput(t *testing.T) {
	s := newStore(t, inmem.New("test"), nil, &clock{t: start})
	require.ErrorIs(t, s.Remember(ctx, "ana", content.Provenance{Kind: content.KindTool, Server: "docs"}, "x"), memory.ErrInvalid)
	require.ErrorIs(t, s.Append(ctx, "ana", "s1", content.Provenance{Kind: content.KindUser, Server: "docs"}, "x"), memory.ErrInvalid)
}

// trustOrigin trusts memory extracted from material of one kind.
type trustOrigin content.Kind

func (k trustOrigin) Classify(_ context.Context, src content.Provenance) (trust.Decision, error) {
	if src.Kind == content.KindMemory && src.Origin == content.Kind(k) {
		return trust.Decision{Verdict: trust.Trusted}, nil
	}
	return trust.Decision{Verdict: trust.Untrusted}, nil
}

// oneSecret is a secret source holding one value under one name.
type oneSecret struct{ name, value string }

func (o oneSecret) Lookup(_ context.Context, name string) (string, error) {
	if name != o.name {
		return "", secret.ErrNotFound
	}
	return o.value, nil
}

// Every text the Store writes reaches the backend with each resolved secret
// scrubbed, an event or a fact alike, whatever wrote it.
func TestEveryWriteIsScrubbed(t *testing.T) {
	ctx := context.Background()
	res := secret.NewResolver(oneSecret{"key", "SECRET-a71"})
	_, err := res.Scope("key").Resolve(ctx, "key")
	require.NoError(t, err)
	b := inmem.New("test")
	s, err := memory.NewStore(b, memory.Options{Namespace: "test", Retention: time.Hour, Scrubber: res.Scrubber()})
	require.NoError(t, err)
	user := content.Provenance{Kind: content.KindUser}
	require.NoError(t, s.Append(ctx, "ana", "s1", user, "my key is SECRET-a71"))
	require.NoError(t, s.Remember(ctx, "ana", user, "her key is SECRET-a71"))

	events, err := b.History(ctx, "test/ana", "s1", time.Time{})
	require.NoError(t, err)
	facts, err := b.Recall(ctx, "test/ana", "", 10, time.Time{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Len(t, facts, 1)
	for _, r := range append(events, facts...) {
		assert.NotContains(t, r.Text, "SECRET-a71", "as the backend holds it")
		assert.Contains(t, r.Text, "key is")
	}
}

// A Store needs a namespace, and the backend's own.
func TestAStoreNeedsTheBackendsNamespace(t *testing.T) {
	_, err := memory.NewStore(inmem.New(""), memory.Options{Retention: time.Hour})
	require.ErrorIs(t, err, memory.ErrInvalid, "no namespace")
	_, err = memory.NewStore(inmem.New("b"), memory.Options{Namespace: "a", Retention: time.Hour})
	require.ErrorIs(t, err, memory.ErrInvalid, "another namespace")
}

// blind is a backend that ignores namespaces: every one opened over the same
// records reports whatever namespace it is asked for and reads them all.
type blind struct {
	*inmem.Backend
	ns string
}

func (b blind) Namespace() string { return b.ns }

// The Store keeps namespaces apart itself, not only through the backend: over
// a backend that ignores them, two Stores whose namespace and subject would
// spell the same key unescaped still see only their own records and delete
// only their own.
func TestTheStoreKeepsNamespacesApartOverABlindBackend(t *testing.T) {
	shared := inmem.New("shared")
	open := func(ns string) *memory.Store {
		s, err := memory.NewStore(blind{shared, ns}, memory.Options{Namespace: ns, Retention: time.Hour})
		require.NoError(t, err)
		return s
	}
	a, b := open("a"), open("a/x")
	user := content.Provenance{Kind: content.KindUser}
	require.NoError(t, a.Remember(ctx, "x/ana", user, "a's fact"))
	require.NoError(t, b.Remember(ctx, "ana", user, "b's fact"))
	require.NoError(t, a.Append(ctx, "x/ana", "s1", user, "a's event"))
	require.NoError(t, b.Append(ctx, "ana", "s1", user, "b's event"))

	for _, c := range []struct {
		s       *memory.Store
		subject string
		want    string
	}{{a, "x/ana", "a's"}, {b, "ana", "b's"}} {
		facts, err := c.s.Recall(ctx, c.subject, "", 10)
		require.NoError(t, err)
		assert.Equal(t, []string{c.want + " fact"}, raws(facts))
		events, err := c.s.History(ctx, c.subject, "s1")
		require.NoError(t, err)
		assert.Equal(t, []string{c.want + " event"}, raws(events))
	}
	require.NoError(t, b.DeleteSubject(ctx, "ana"))
	facts, err := a.Recall(ctx, "x/ana", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"a's fact"}, raws(facts), "not deleted with the other's subject")
}

func raws(ts []content.Text) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.(content.Untrusted).Raw()
	}
	return out
}

// fixed returns the records it holds, as a backend that derived them would.
type fixed struct{ recs []memory.Record }

func (f fixed) Namespace() string { return "test" }
func (f fixed) Write(context.Context, memory.Record) (string, error) {
	return "", errors.New("read only")
}

func (f fixed) History(context.Context, string, string, time.Time) ([]memory.Record, error) {
	return nil, nil
}

func (f fixed) Recall(context.Context, string, string, int, time.Time) ([]memory.Record, error) {
	return f.recs, nil
}
func (fixed) DeleteSubject(context.Context, string) error   { return nil }
func (fixed) DeleteBefore(context.Context, time.Time) error { return nil }

// trustUsers trusts memory that came from a user's message, and nothing else.
type trustUsers struct{}

func (trustUsers) Classify(_ context.Context, p content.Provenance) (trust.Decision, error) {
	if p.Kind == content.KindMemory && p.Origin == content.KindUser {
		return trust.Decision{Verdict: trust.Trusted}, nil
	}
	return trust.Decision{Verdict: trust.Untrusted}, nil
}

// A fact the backend derived is classified as model output too: a policy
// trusting what users say does not trust a model's account of it, while the
// same text stored as the user's own stays trusted.
func TestADerivedFactIsClassifiedAsModelOutputToo(t *testing.T) {
	rec := func(text string, derived bool) memory.Record {
		return memory.Record{
			Layer: memory.LongTerm, Subject: "test/ana", Origin: content.KindUser, Derived: derived, Text: text, At: start,
			Decision: memory.Decision{Verdict: trust.Trusted, Policy: "users"},
		}
	}
	s, err := memory.NewStore(fixed{[]memory.Record{rec("said it", false), rec("derived it", true)}}, memory.Options{
		Namespace: "test", Policy: trustUsers{}, PolicyName: "users", Retention: 24 * time.Hour, Now: func() time.Time { return start },
	})
	require.NoError(t, err)
	got, err := s.Recall(ctx, "ana", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.True(t, got[0].Trusted(), "the user's own, as the policy trusts it")
	assert.False(t, got[1].Trusted(), "derived: also model output, which the policy does not trust")
	assert.Equal(t, content.KindUser, got[1].(content.Untrusted).Provenance().Origin, "its source kept")
}

// trustModel trusts memory from model output, and nothing else.
type trustModel struct{}

func (trustModel) Classify(_ context.Context, p content.Provenance) (trust.Decision, error) {
	if p.Kind == content.KindMemory && p.Origin == content.KindModel {
		return trust.Decision{Verdict: trust.Trusted}, nil
	}
	return trust.Decision{Verdict: trust.Untrusted}, nil
}

// Classifying a derived fact as model output never raises it: under a policy
// trusting model output but not users, a fact derived from a user's message
// stays untrusted.
func TestModelOutputNeverRaisesADerivedFact(t *testing.T) {
	s, err := memory.NewStore(fixed{[]memory.Record{{
		Layer: memory.LongTerm, Subject: "test/ana", Origin: content.KindUser, Derived: true, Text: "derived it", At: start,
		Decision: memory.Decision{Verdict: trust.Trusted, Policy: "model"},
	}}}, memory.Options{Namespace: "test", Policy: trustModel{}, Retention: 24 * time.Hour, Now: func() time.Time { return start }})
	require.NoError(t, err)
	got, err := s.Recall(ctx, "ana", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, got[0].Trusted())
}
