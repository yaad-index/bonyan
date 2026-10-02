// Package memorytest is the conformance suite every memory backend must pass
// (ADR 0001 §4). It proves a backend implements what the Store relies on:
// records come back as written, deletion by subject removes events and facts,
// reads honour the retention cutoff, and DeleteBefore removes what is older. It
// cannot prove that a given deletion on a live external store happened.
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

// Run runs the suite. newBackend returns an empty backend for each test.
func Run(t *testing.T, newBackend func(t *testing.T) memory.Backend) {
	t.Helper()
	for _, c := range []struct {
		name string
		test func(t *testing.T, b memory.Backend)
	}{
		{"RecordsComeBackAsWritten", recordsComeBackAsWritten},
		{"HistoryIsOneSessionOldestFirst", historyIsOneSessionOldestFirst},
		{"LayersAreSeparate", layersAreSeparate},
		{"RecallHonoursTheLimit", recallHonoursTheLimit},
		{"RecallFindsAFactByItsText", recallFindsAFactByItsText},
		{"RecallMatchesAnyWord", recallMatchesAnyWord},
		{"DeleteSubjectRemovesEventsAndFacts", deleteSubjectRemovesEventsAndFacts},
		{"ReadsHonourTheCutoff", readsHonourTheCutoff},
		{"DeleteBeforeRemovesOlderRecords", deleteBeforeRemovesOlderRecords},
	} {
		t.Run(c.name, func(t *testing.T) { c.test(t, newBackend(t)) })
	}
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
	f.Origin = content.KindFetched
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
	assert.Equal(t, e.Text, got[0].Text)
	assert.True(t, e.At.Equal(got[0].At), "time %s, got %s", e.At, got[0].At)
	assert.Equal(t, e.Decision, got[0].Decision)
	assert.Equal(t, e.ID, got[0].ID)

	facts, err := b.Recall(ctx, "ana", "", 10, never)
	require.NoError(t, err)
	require.Len(t, facts, 1)
	assert.Equal(t, ids[1], facts[0].ID)
	assert.Equal(t, content.KindFetched, facts[0].Origin)
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

func recallFindsAFactByItsText(t *testing.T, b memory.Backend) {
	write(t, b,
		fact("ana", "lives in a small town", start),
		fact("ana", "prefers mail over calls", start.Add(time.Second)),
		fact("bo", "prefers mail over calls", start.Add(2*time.Second)),
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
