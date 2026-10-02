package record_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/secret"
)

// memSink keeps entries, as a full or a redacted recording about subject.
type memSink struct {
	full    bool
	subject string
	entries []record.Entry
}

func (s *memSink) Write(e record.Entry) error { s.entries = append(s.entries, e); return nil }
func (s *memSink) Full() bool                 { return s.full }
func (s *memSink) Subject() string            { return s.subject }
func (s *memSink) Close() error               { return nil }

func (s *memSink) events() []record.Event {
	var out []record.Event
	for _, e := range s.entries {
		if e.Event != nil {
			out = append(out, *e.Event)
		}
	}
	return out
}

func newQueue(t *testing.T) *record.MemQueue {
	t.Helper()
	q, err := record.NewMemQueue(time.Hour)
	require.NoError(t, err)
	return q
}

func queued(t *testing.T, sink record.Sink, q record.Queue, opts record.QueueOptions) *record.Recorder {
	t.Helper()
	rec, err := record.NewRecorder(sink, secret.NewScrubber(), record.WithQueue(q, opts))
	require.NoError(t, err)
	return rec
}

// run records one run with a call, inside ctx.
func run(t *testing.T, rec *record.Recorder, ctx context.Context, req model.ChatRequest) {
	t.Helper()
	rec.Start(ctx, record.Start{Agent: "helper"})
	_, err := record.Chat(&scripted{steps: steps}, "main", rec).Chat(ctx, req)
	require.NoError(t, err)
	rec.End(ctx, record.End{Outcome: record.OutcomeCleared, Steps: 1})
}

func inRun(id, subject string) context.Context {
	return record.WithSubject(record.WithRun(context.Background(), id), subject)
}

func takeAll(t *testing.T, q record.Queue) []record.Item {
	t.Helper()
	var out []record.Item
	for {
		it, ok, err := q.Take(context.Background())
		require.NoError(t, err)
		if !ok {
			return out
		}
		out = append(out, it)
	}
}

// A finished run is queued as a recording of that run alone, under its
// subject, and reads back like a file.
func TestAFinishedRunIsQueued(t *testing.T) {
	q := newQueue(t)
	sink := &memSink{}
	rec := queued(t, sink, q, record.QueueOptions{Rate: 1})
	a, b := inRun("a", "ana"), inRun("b", "ben")
	rec.Start(a, record.Start{Agent: "first"})
	rec.Start(b, record.Start{Agent: "second"})
	chat := record.Chat(&scripted{steps: steps}, "main", rec)
	_, err := chat.Chat(a, requests[0])
	require.NoError(t, err)
	_, err = chat.Chat(b, requests[1])
	require.NoError(t, err)
	rec.Event(context.Background(), record.Event{Slot: "trust", Name: "outside any run"})
	rec.End(a, record.End{Outcome: record.OutcomeCleared, Steps: 1})
	assert.Equal(t, 1, q.Len(), "a run is queued when it ends, not before")
	rec.End(b, record.End{Outcome: "step_limit", Steps: 1})

	items := takeAll(t, q)
	require.Len(t, items, 2)
	assert.Equal(t, "a", items[0].Run)
	assert.Equal(t, "ana", items[0].Subject)
	assert.Equal(t, "ben", items[1].Subject)
	h, runs, err := record.ReadRuns(bytes.NewReader(items[0].Recording))
	require.NoError(t, err)
	assert.Equal(t, record.Version, h.Version)
	assert.False(t, h.Full)
	require.Len(t, runs, 1, "only the run's own entries")
	assert.Equal(t, "a", runs[0].ID)
	assert.Equal(t, "first", runs[0].Agent)
	require.Len(t, runs[0].Calls, 1)
	assert.Equal(t, int64(1), runs[0].Calls[0].Seq)
	assert.Equal(t, &record.End{Outcome: record.OutcomeCleared, Steps: 1}, runs[0].End)
	for _, e := range sink.events() {
		assert.NotEqual(t, record.SlotQueue, e.Slot, "nothing was dropped")
	}
}

// A queued recording excludes memory even when the recorder's sink is a full
// recording, unless the queue itself is full.
func TestAQueuedRecordingKeepsMemoryOnlyWhenTheQueueIsFull(t *testing.T) {
	req := model.ChatRequest{Messages: []model.Message{system("be brief"), {Role: model.RoleUser, Parts: []content.Text{memory("likes red wine")}}}, MaxOutputTokens: 10}
	for name, tc := range map[string]struct {
		sinkFull, queueFull, inQueue bool
	}{
		"full sink, redacted queue": {sinkFull: true, queueFull: false, inQueue: false},
		"full sink, full queue":     {sinkFull: true, queueFull: true, inQueue: true},
		"redacted sink, full queue": {sinkFull: false, queueFull: true, inQueue: true},
		"redacted both":             {sinkFull: false, queueFull: false, inQueue: false},
	} {
		t.Run(name, func(t *testing.T) {
			q := newQueue(t)
			sink := &memSink{full: tc.sinkFull}
			if tc.sinkFull {
				sink.subject = "ana"
			}
			rec := queued(t, sink, q, record.QueueOptions{Rate: 1, Full: tc.queueFull})
			run(t, rec, inRun("r", "ana"), req)

			var inSink bool
			for _, e := range sink.entries {
				if e.Call != nil && strings.Contains(e.Call.Request.Messages[1].Parts[0].Text, "red wine") {
					inSink = true
				}
			}
			assert.Equal(t, tc.sinkFull, inSink, "the sink keeps what its own mode says")
			items := takeAll(t, q)
			require.Len(t, items, 1)
			assert.Equal(t, tc.inQueue, strings.Contains(string(items[0].Recording), "red wine"))
			h, runs, err := record.ReadRuns(bytes.NewReader(items[0].Recording))
			require.NoError(t, err)
			assert.Equal(t, tc.queueFull, h.Full)
			assert.Equal(t, int64(1), runs[0].Calls[0].Seq, "both copies share the sequence")
		})
	}
}

// A run inside WithEvaluation is recorded as evaluation and never queued.
func TestAnEvaluationRunIsNeverQueued(t *testing.T) {
	q := newQueue(t)
	sink := &memSink{}
	rec := queued(t, sink, q, record.QueueOptions{Rate: 1})
	run(t, rec, record.WithEvaluation(inRun("judge", "ana")), requests[0])
	assert.Zero(t, q.Len())
	assert.Empty(t, sink.events(), "not a drop: evaluation is never queued")
	require.NotNil(t, sink.entries[0].Start)
	assert.True(t, sink.entries[0].Start.Evaluation)

	run(t, rec, inRun("plain", "ana"), requests[0])
	assert.Equal(t, 1, q.Len(), "the same run outside evaluation is queued")
}

// A run that names no subject cannot be deleted by subject, so it is not
// queued, and the drop is recorded.
func TestARunWithNoSubjectIsDropped(t *testing.T) {
	q := newQueue(t)
	sink := &memSink{}
	rec := queued(t, sink, q, record.QueueOptions{Rate: 1})
	run(t, rec, record.WithRun(context.Background(), "r"), requests[0])
	assert.Zero(t, q.Len())
	assert.Equal(t, []record.Event{{Slot: record.SlotQueue, Decision: record.DecisionDropped, Failure: record.QueueNoSubject}}, sink.events())
}

// A run whose recording grows past the cap is dropped whole and recorded as
// dropped; a run under it is queued whole.
func TestARunPastTheCapIsDroppedNotTruncated(t *testing.T) {
	small := &memSink{}
	q := newQueue(t)
	rec := queued(t, small, q, record.QueueOptions{Rate: 1, MaxRunBytes: 4096})
	// The same run ID as below: it is in every entry, so it counts.
	run(t, rec, inRun("r", "ana"), requests[0])
	items := takeAll(t, q)
	require.Len(t, items, 1, "under the cap")
	// The cap is on the run's entries; the header is added when it is queued.
	entries := len(items[0].Recording) - headerLen(t, items[0].Recording)

	for name, tc := range map[string]struct {
		cap    int
		queued bool
	}{
		"just enough": {cap: entries, queued: true},
		"one short":   {cap: entries - 1, queued: false},
	} {
		t.Run(name, func(t *testing.T) {
			sink := &memSink{}
			q := newQueue(t)
			rec := queued(t, sink, q, record.QueueOptions{Rate: 1, MaxRunBytes: tc.cap})
			run(t, rec, inRun("r", "ana"), requests[0])
			got := takeAll(t, q)
			if tc.queued {
				require.Len(t, got, 1)
				assert.Empty(t, sink.events())
				return
			}
			assert.Empty(t, got)
			assert.Equal(t, []record.Event{{Slot: record.SlotQueue, Decision: record.DecisionDropped, Failure: record.QueueTooLarge}}, sink.events())
		})
	}
}

// headerLen is the length of a recording's header line, newline included.
func headerLen(t *testing.T, recording []byte) int {
	t.Helper()
	i := bytes.IndexByte(recording, '\n')
	require.Positive(t, i)
	return i + 1
}

func TestSamplingSkipsRunsQuietly(t *testing.T) {
	q := newQueue(t)
	sink := &memSink{}
	var asked []float64
	rec := queued(t, sink, q, record.QueueOptions{Rate: 0.25, Sample: func(rate float64) bool {
		asked = append(asked, rate)
		return len(asked) == 2
	}})
	for _, id := range []string{"one", "two", "three"} {
		run(t, rec, inRun(id, "ana"), requests[0])
	}
	items := takeAll(t, q)
	require.Len(t, items, 1)
	assert.Equal(t, "two", items[0].Run)
	assert.Equal(t, []float64{0.25, 0.25, 0.25}, asked)
	assert.Empty(t, sink.events(), "a run not sampled is not a drop")
}

type refusing struct{ record.Queue }

func (refusing) Put(context.Context, record.Item) error { return errors.New("full") }

func TestARunTheQueueRefusesIsRecordedAsDropped(t *testing.T) {
	sink := &memSink{}
	rec := queued(t, sink, refusing{newQueue(t)}, record.QueueOptions{Rate: 1})
	run(t, rec, inRun("r", "ana"), requests[0])
	assert.Equal(t, []record.Event{{Slot: record.SlotQueue, Decision: record.DecisionDropped, Failure: record.QueueFailed}}, sink.events())
}

func TestQueueOptionsAreChecked(t *testing.T) {
	q := newQueue(t)
	for name, tc := range map[string]struct {
		q    record.Queue
		opts record.QueueOptions
	}{
		"no queue":     {nil, record.QueueOptions{Rate: 1}},
		"rate zero":    {q, record.QueueOptions{}},
		"rate above 1": {q, record.QueueOptions{Rate: 1.5}},
		"negative cap": {q, record.QueueOptions{Rate: 1, MaxRunBytes: -1}},
	} {
		_, err := record.NewRecorder(&memSink{}, secret.NewScrubber(), record.WithQueue(tc.q, tc.opts))
		require.Error(t, err, name)
	}
	_, err := record.NewMemQueue(0)
	require.Error(t, err)
}

func TestTheMemQueue(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()
	now := time.Now()
	require.NoError(t, q.Put(ctx, record.Item{Run: "1", Subject: "ana", At: now}))
	require.NoError(t, q.Put(ctx, record.Item{Run: "old", Subject: "ana", At: now.Add(-2 * time.Hour)}))
	require.NoError(t, q.Put(ctx, record.Item{Run: "2", Subject: "ben", At: now}))
	require.NoError(t, q.Put(ctx, record.Item{Run: "3", Subject: "ana", At: now}))
	assert.Equal(t, 3, q.Len(), "an item past the retention is dropped")

	require.NoError(t, q.DeleteSubject(ctx, "ana"))
	items := takeAll(t, q)
	require.Len(t, items, 1)
	assert.Equal(t, "2", items[0].Run)
}

// A queued recording is scrubbed of resolved secrets like any other, in both
// of the queue's modes.
func TestAQueuedRecordingIsScrubbed(t *testing.T) {
	t.Setenv("BONYAN_TEST_RECORD_QUEUE", "queue-secret-31")
	r := secret.NewResolver(secret.Env{})
	_, err := r.Scope("BONYAN_TEST_RECORD_QUEUE").Resolve(context.Background(), "BONYAN_TEST_RECORD_QUEUE")
	require.NoError(t, err)
	req := model.ChatRequest{Messages: []model.Message{system("be brief"), user("the token is queue-secret-31")}, MaxOutputTokens: 10}
	for _, full := range []bool{false, true} {
		q := newQueue(t)
		rec, err := record.NewRecorder(&memSink{}, r.Scrubber(), record.WithQueue(q, record.QueueOptions{Rate: 1, Full: full}))
		require.NoError(t, err)
		run(t, rec, inRun("r", "ana"), req)
		items := takeAll(t, q)
		require.Len(t, items, 1)
		assert.Contains(t, string(items[0].Recording), "the token is", "full %v", full)
		assert.NotContains(t, string(items[0].Recording), "queue-secret-31", "full %v", full)
	}
}

// Deleting a subject forgets its runs still open, so none is queued when it
// ends, and deletes its queued items; another subject's runs, and the
// subject's runs started after the deletion, are queued as usual.
func TestDeletingASubjectReachesRunsStillOpen(t *testing.T) {
	q := newQueue(t)
	rec := queued(t, &memSink{}, q, record.QueueOptions{Rate: 1})
	run(t, rec, inRun("done", "ana"), requests[0])
	open, other := inRun("open", "ana"), inRun("other", "ben")
	rec.Start(open, record.Start{})
	rec.Start(other, record.Start{})
	chat := record.Chat(&scripted{steps: steps}, "main", rec)
	_, err := chat.Chat(open, requests[0])
	require.NoError(t, err)

	require.NoError(t, rec.DeleteSubject(context.Background(), "ana"))
	_, err = chat.Chat(open, requests[1])
	require.NoError(t, err)
	rec.End(open, record.End{Outcome: record.OutcomeCleared})
	rec.End(other, record.End{Outcome: record.OutcomeCleared})
	run(t, rec, inRun("later", "ana"), requests[0])

	var ids []string
	for _, it := range takeAll(t, q) {
		ids = append(ids, it.Run)
	}
	assert.Equal(t, []string{"other", "later"}, ids)

	plain, err := record.NewRecorder(&memSink{}, secret.NewScrubber())
	require.NoError(t, err)
	require.NoError(t, plain.DeleteSubject(context.Background(), "ana"), "no queue, nothing to delete")
}

// A run with an entry that cannot be kept is dropped whole and recorded as
// dropped, never queued without the entry.
func TestARunWithAnEntryThatCannotBeKeptIsDropped(t *testing.T) {
	q := newQueue(t)
	sink := &memSink{}
	rec := queued(t, sink, q, record.QueueOptions{Rate: 1})
	ctx := inRun("r", "ana")
	rec.Start(ctx, record.Start{})
	// A label's confidence that JSON cannot hold.
	gate := record.Classifier(scriptedClassifier{resp: model.ClassifyResponse{Labels: []model.Label{{Name: "x", Confidence: math.NaN()}}}}, "gate", rec)
	_, err := gate.Classify(ctx, content.From(content.Provenance{Kind: content.KindTool}, "x"))
	require.NoError(t, err)
	_, err = record.Chat(&scripted{steps: steps}, "main", rec).Chat(ctx, requests[1])
	require.NoError(t, err)
	rec.End(ctx, record.End{Outcome: record.OutcomeCleared})
	assert.Zero(t, q.Len())
	assert.Equal(t, []record.Event{{Slot: record.SlotQueue, Decision: record.DecisionDropped, Failure: record.QueueFailed}}, sink.events())
}

// What a run's start recorded comes back on the run when read.
func TestARunsStartIsReadBack(t *testing.T) {
	f := openFile(t, record.FileOptions{})
	rec := newRecorder(t, f)
	ctx := record.WithEvaluation(record.WithRun(context.Background(), "judge"))
	rec.Start(ctx, record.Start{Agent: "judge", Trace: "4bf92f3577b34da6a3ce929d0e0e4736", Span: "00f067aa0ba902b7"})
	rec.End(ctx, record.End{Outcome: record.OutcomeCleared})
	_, _, raw := readFile(t, f)
	_, runs, err := record.ReadRuns(strings.NewReader(raw))
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "judge", runs[0].Agent)
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", runs[0].Trace)
	assert.Equal(t, "00f067aa0ba902b7", runs[0].Span)
	assert.True(t, runs[0].Evaluation)
}
