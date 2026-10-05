// Package memorytest is the conformance suite every memory backend must pass
// (ADR 0001 §4). It proves a backend implements what the Store relies on:
// records come back as written, deletion by subject removes events and facts,
// reads honour the retention cutoff, DeleteBefore removes what is older, and
// backends opened with different namespaces on one storage never see or delete
// each other's records. It cannot prove that a given deletion on a live
// external store happened.
package memorytest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/trust"
)

// Open opens a backend inside a namespace, over one storage: every backend
// one Open returns shares it.
type Open func(namespace string) memory.Backend

// Option adds checks to the suite.
type Option func(*config)

type config struct{ lexical bool }

// Lexical checks the lexical recall contract as well, for a backend that
// matches a fact by the words it holds: a fact matches a query when it holds
// any word of it, and only then. A backend that matches by meaning, which
// also returns near matches, leaves it out.
func Lexical(c *config) { c.lexical = true }

// Run runs the suite. newStorage returns an Open over an empty storage for each
// test.
func Run(t *testing.T, newStorage func(t *testing.T) Open, opts ...Option) {
	t.Helper()
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	cases := []struct {
		name string
		test func(t *testing.T, b memory.Backend)
	}{
		{"RecordsComeBackAsWritten", recordsComeBackAsWritten},
		{"HistoryIsOneSessionOldestFirst", historyIsOneSessionOldestFirst},
		{"LayersAreSeparate", layersAreSeparate},
		{"RecallHonoursTheLimit", recallHonoursTheLimit},
		{"RecallFindsAFactByItsText", recallFindsAFactByItsText},
		{"RecallNeverCrossesSubjects", recallNeverCrossesSubjects},
		{"DeleteSubjectRemovesEventsAndFacts", deleteSubjectRemovesEventsAndFacts},
		{"ReadsHonourTheCutoff", readsHonourTheCutoff},
		{"DeleteBeforeRemovesOlderRecords", deleteBeforeRemovesOlderRecords},
	}
	if cfg.lexical {
		cases = append(cases, struct {
			name string
			test func(t *testing.T, b memory.Backend)
		}{"RecallMatchesAnyWord", recallMatchesAnyWord})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.test(t, newStorage(t)("suite")) })
	}
	t.Run("NamespacesNeverSeeEachOther", func(t *testing.T) { namespacesNeverSeeEachOther(t, newStorage(t)) })
}

// namespacesNeverSeeEachOther opens two namespaces on one storage and writes
// the same subject in both: neither reads the other's records, and neither
// deletes them, by subject or by age.
func namespacesNeverSeeEachOther(t *testing.T, open Open) {
	a, b := open("ns-a"), open("ns-b")
	assert.Equal(t, "ns-a", a.Namespace())
	assert.Equal(t, "ns-b", b.Namespace())
	write(t, a, event("ana", "s1", "a's event", start), fact("ana", "a's fact", start))
	write(t, b, event("ana", "s1", "b's event", start), fact("ana", "b's fact", start))
	for _, c := range []struct {
		b    memory.Backend
		want string
	}{{a, "a's"}, {b, "b's"}} {
		h, err := c.b.History(ctx, "ana", "s1", never)
		require.NoError(t, err)
		assert.Equal(t, []string{c.want + " event"}, texts(h))
		f, err := c.b.Recall(ctx, "ana", "", 10, never)
		require.NoError(t, err)
		assert.Equal(t, []string{c.want + " fact"}, texts(f))
		f, err = c.b.Recall(ctx, "ana", "fact", 10, never)
		require.NoError(t, err)
		assert.Equal(t, []string{c.want + " fact"}, texts(f), "a query too")
	}

	require.NoError(t, b.DeleteSubject(ctx, "ana"))
	require.NoError(t, b.DeleteBefore(ctx, start.Add(time.Hour)))
	h, err := a.History(ctx, "ana", "s1", never)
	require.NoError(t, err)
	assert.Equal(t, []string{"a's event"}, texts(h), "untouched by the other's deletes")
	f, err := a.Recall(ctx, "ana", "", 10, never)
	require.NoError(t, err)
	assert.Equal(t, []string{"a's fact"}, texts(f))
	f, err = b.Recall(ctx, "ana", "", 10, never)
	require.NoError(t, err)
	assert.Empty(t, f)
}

var (
	ctx   = context.Background()
	start = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	never = time.Time{}
)

func event(subject, session, text string, at time.Time) memory.Record {
	return memory.Record{
		Layer: memory.ShortTerm, Subject: subject, Session: session, Origin: content.KindUser, Text: text, At: at,
		Decision: memory.Decision{Verdict: trust.Untrusted, Policy: "default"},
	}
}

func fact(subject, text string, at time.Time) memory.Record {
	return memory.Record{
		Layer: memory.LongTerm, Subject: subject, Origin: content.KindTool, Text: text, At: at,
		Decision: memory.Decision{Verdict: trust.Untrusted, Policy: "default"},
	}
}

func write(t *testing.T, b memory.Backend, recs ...memory.Record) []string {
	t.Helper()
	ids := make([]string, len(recs))
	for i, r := range recs {
		id, err := b.Write(ctx, r)
		require.NoError(t, err)
		require.NotEmpty(t, id, "the backend assigns an ID")
		ids[i] = id
	}
	return ids
}

func texts(recs []memory.Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Text
	}
	return out
}

func recordsComeBackAsWritten(t *testing.T, b memory.Backend) {
	e := event("ana", "s1", "the order is late", start)
	e.Decision = memory.Decision{Verdict: trust.Trusted, Policy: "program"}
	f := fact("ana", "prefers mail", start.Add(time.Second))
	f.Origin, f.Server = content.KindRemoteTool, "docs"
	ids := write(t, b, e, f)
	assert.NotEqual(t, ids[0], ids[1], "IDs are distinct")

	got, err := b.History(ctx, "ana", "s1", never)
	require.NoError(t, err)
	require.Len(t, got, 1)
	e.ID = ids[0]
	assert.Equal(t, e.Layer, got[0].Layer)
	assert.Equal(t, e.Subject, got[0].Subject)
	assert.Equal(t, e.Session, got[0].Session)
	assert.Equal(t, e.Origin, got[0].Origin)
	assert.Empty(t, got[0].Server)
	assert.Equal(t, e.Text, got[0].Text)
	assert.True(t, e.At.Equal(got[0].At), "time %s, got %s", e.At, got[0].At)
	assert.Equal(t, e.Decision, got[0].Decision)
	assert.Equal(t, e.ID, got[0].ID)

	facts, err := b.Recall(ctx, "ana", "", 10, never)
	require.NoError(t, err)
	require.Len(t, facts, 1)
	assert.Equal(t, ids[1], facts[0].ID)
	assert.Equal(t, content.KindRemoteTool, facts[0].Origin)
	assert.Equal(t, "docs", facts[0].Server, "the tool server's name comes back")
	assert.Equal(t, "prefers mail", facts[0].Text)
	assert.Equal(t, f.Decision, facts[0].Decision)
	assert.Empty(t, facts[0].Session)
}

func historyIsOneSessionOldestFirst(t *testing.T, b memory.Backend) {
	write(t, b,
		event("ana", "s1", "first", start),
		event("ana", "s2", "other session", start.Add(time.Second)),
		event("bo", "s1", "other subject", start.Add(2*time.Second)),
		event("ana", "s1", "second", start.Add(3*time.Second)),
	)
	got, err := b.History(ctx, "ana", "s1", never)
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second"}, texts(got))
}

func layersAreSeparate(t *testing.T, b memory.Backend) {
	write(t, b, event("ana", "s1", "an event", start), fact("ana", "a fact", start))
	h, err := b.History(ctx, "ana", "s1", never)
	require.NoError(t, err)
	assert.Equal(t, []string{"an event"}, texts(h))
	f, err := b.Recall(ctx, "ana", "", 10, never)
	require.NoError(t, err)
	assert.Equal(t, []string{"a fact"}, texts(f))
}

func recallHonoursTheLimit(t *testing.T, b memory.Backend) {
	for i := range 5 {
		write(t, b, fact("ana", fmt.Sprintf("fact %d", i), start.Add(time.Duration(i)*time.Second)))
	}
	got, err := b.Recall(ctx, "ana", "", 2, never)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

// The fact holding the query's own text ranks above a newer one holding only
// some of its words, so a backend ranking by recency alone does not pass.
func recallFindsAFactByItsText(t *testing.T, b memory.Backend) {
	write(t, b,
		fact("ana", "prefers mail over calls", start),
		fact("ana", "prefers phone calls", start.Add(time.Second)),
		fact("ana", "lives in a small town", start.Add(2*time.Second)),
		fact("bo", "prefers mail over calls", start.Add(3*time.Second)),
	)
	got, err := b.Recall(ctx, "ana", "prefers mail over calls", 2, never)
	require.NoError(t, err)
	require.NotEmpty(t, got)
	assert.Equal(t, "prefers mail over calls", got[0].Text, "the fact with the query's own text comes first")
	for _, r := range got {
		assert.Equal(t, "ana", r.Subject)
	}
}

// A fact holding any word of the query matches, so a question asked in
// ordinary words finds the fact it is about.
func recallMatchesAnyWord(t *testing.T, b memory.Backend) {
	write(t, b,
		fact("ana", "lives in a small town", start),
		fact("ana", "prefers mail over calls", start.Add(time.Second)),
	)
	got, err := b.Recall(ctx, "ana", "how should we contact her, by mail?", 10, never)
	require.NoError(t, err)
	assert.Equal(t, []string{"prefers mail over calls"}, texts(got))
}

// Recall for one subject never returns another's facts, with a query both
// match as well as with none: a backend keeping subjects side by side in one
// store must keep them apart on every read.
func recallNeverCrossesSubjects(t *testing.T, b memory.Backend) {
	write(t, b,
		fact("ana", "ana rides trains", start),
		fact("bo", "bo rides trains", start.Add(time.Second)),
	)
	for _, c := range []struct{ subject, want string }{{"ana", "ana rides trains"}, {"bo", "bo rides trains"}} {
		for _, q := range []string{"", "rides trains", c.want} {
			got, err := b.Recall(ctx, c.subject, q, 10, never)
			require.NoError(t, err)
			assert.Equal(t, []string{c.want}, texts(got), "subject %s, query %q", c.subject, q)
		}
	}
}

func deleteSubjectRemovesEventsAndFacts(t *testing.T, b memory.Backend) {
	write(t, b,
		event("ana", "s1", "ana's event", start),
		event("ana", "s2", "ana's other session", start),
		fact("ana", "ana's fact", start),
		event("bo", "s1", "bo's event", start),
		fact("bo", "bo's fact", start),
	)
	require.NoError(t, b.DeleteSubject(ctx, "ana"))

	for _, s := range []string{"s1", "s2"} {
		h, err := b.History(ctx, "ana", s, never)
		require.NoError(t, err)
		assert.Empty(t, h, "session %s", s)
	}
	f, err := b.Recall(ctx, "ana", "", 10, never)
	require.NoError(t, err)
	assert.Empty(t, f)

	h, err := b.History(ctx, "bo", "s1", never)
	require.NoError(t, err)
	assert.Equal(t, []string{"bo's event"}, texts(h), "another subject is untouched")
	f, err = b.Recall(ctx, "bo", "", 10, never)
	require.NoError(t, err)
	assert.Equal(t, []string{"bo's fact"}, texts(f))
}

func readsHonourTheCutoff(t *testing.T, b memory.Backend) {
	write(t, b,
		event("ana", "s1", "old event", start),
		fact("ana", "old fact", start),
		event("ana", "s1", "new event", start.Add(2*time.Hour)),
		fact("ana", "new fact", start.Add(2*time.Hour)),
	)
	cutoff := start.Add(time.Hour)
	h, err := b.History(ctx, "ana", "s1", cutoff)
	require.NoError(t, err)
	assert.Equal(t, []string{"new event"}, texts(h))
	f, err := b.Recall(ctx, "ana", "", 10, cutoff)
	require.NoError(t, err)
	assert.Equal(t, []string{"new fact"}, texts(f))
}

func deleteBeforeRemovesOlderRecords(t *testing.T, b memory.Backend) {
	write(t, b,
		event("ana", "s1", "old event", start),
		fact("ana", "old fact", start),
		event("ana", "s1", "new event", start.Add(2*time.Hour)),
		fact("bo", "new fact", start.Add(2*time.Hour)),
	)
	require.NoError(t, b.DeleteBefore(ctx, start.Add(time.Hour)))

	h, err := b.History(ctx, "ana", "s1", never)
	require.NoError(t, err)
	assert.Equal(t, []string{"new event"}, texts(h), "read with no cutoff, so only deletion explains the absence")
	f, err := b.Recall(ctx, "ana", "", 10, never)
	require.NoError(t, err)
	assert.Empty(t, f)
	f, err = b.Recall(ctx, "bo", "", 10, never)
	require.NoError(t, err)
	assert.Equal(t, []string{"new fact"}, texts(f))
}
