package record

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

// Queue holds finished runs' recordings for live evaluation, which reads them
// after the runs end and never inside them (ADR 0001 §8). It is the slot for
// where evaluation runs. A queued item is a recording, under the same rules as
// one written to a sink, and a queue must keep them: it deletes every item of a
// subject when asked and drops items older than its retention.
type Queue interface {
	// Put adds an item.
	Put(ctx context.Context, it Item) error
	// Take removes and returns the oldest item, and false when there is none.
	Take(ctx context.Context) (Item, bool, error)
	// DeleteSubject deletes every item of subject.
	DeleteSubject(ctx context.Context, subject string) error
}

// Item is one run's recording, queued for evaluation.
type Item struct {
	// Run is the run's ID.
	Run string
	// Subject is who the run was about, so the item can be deleted by subject.
	Subject string
	// At is when the run ended.
	At time.Time
	// Recording is the run's recording: a header and the run's entries, in
	// the form a file holds.
	Recording []byte
}

// QueueOptions configure the hand-off of runs to a Queue.
type QueueOptions struct {
	// Rate is the share of runs handed off, above 0 and at most 1.
	Rate float64
	// MaxRunBytes caps the recording kept for one run while it runs. A run
	// whose recording grows past it is not queued and is recorded as dropped,
	// never queued in part. Zero means DefaultMaxRunBytes.
	MaxRunBytes int
	// Full queues full recordings, which keep recalled memory. Without it a
	// queued recording excludes memory, whatever the recorder's sink keeps.
	Full bool
	// Sample decides whether a run is handed off, given Rate; nil draws at
	// random.
	Sample func(rate float64) bool
}

// DefaultMaxRunBytes is QueueOptions.MaxRunBytes when it is zero.
const DefaultMaxRunBytes = 4 << 20

// The event a recorder writes when a run that was to be queued is not: slot
// SlotQueue, decision DecisionDropped, and one of these failures.
const (
	SlotQueue       = "eval_queue"
	DecisionDropped = "dropped"
	QueueTooLarge   = "too_large"  // the run's recording passed MaxRunBytes
	QueueNoSubject  = "no_subject" // the run named no subject, so it could not be deleted by subject
	QueueFailed     = "failed"     // an entry could not be kept, or the queue refused the run
)

// Option configures a Recorder.
type Option func(*Recorder) error

// WithQueue hands off each finished run to q, as opts say. Runs inside
// WithEvaluation are never handed off, so evaluating a run cannot queue more
// runs to evaluate.
func WithQueue(q Queue, opts QueueOptions) Option {
	return func(r *Recorder) error {
		if q == nil {
			return errors.New("record: no queue")
		}
		if !(opts.Rate > 0 && opts.Rate <= 1) {
			return errors.New("record: the queue's rate must be above 0 and at most 1")
		}
		if opts.MaxRunBytes < 0 {
			return errors.New("record: the queue's run cap must not be negative")
		}
		if opts.MaxRunBytes == 0 {
			opts.MaxRunBytes = DefaultMaxRunBytes
		}
		if opts.Sample == nil {
			opts.Sample = func(rate float64) bool { return rate >= 1 || rand.Float64() < rate }
		}
		r.queue = &handoff{q: q, opts: opts, red: redactor{full: opts.Full, scrub: r.red.scrub}, runs: map[string]*buffered{}}
		return nil
	}
}

type evaluationKey struct{}

// WithEvaluation returns ctx marked as evaluation: a run inside it is
// recorded as one, and is never handed to a queue.
func WithEvaluation(ctx context.Context) context.Context {
	return context.WithValue(ctx, evaluationKey{}, true)
}

// Evaluating reports whether ctx is marked as evaluation.
func Evaluating(ctx context.Context) bool {
	v, _ := ctx.Value(evaluationKey{}).(bool)
	return v
}

type runSubjectKey struct{}

// WithSubject returns ctx naming who the run inside it is about. A run is
// queued for evaluation only with a subject, so its item can be deleted by
// subject.
func WithSubject(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, runSubjectKey{}, subject)
}

func subjectOf(ctx context.Context) string {
	s, _ := ctx.Value(runSubjectKey{}).(string)
	return s
}

// handoff keeps the recordings of the runs to be queued until each ends.
type handoff struct {
	q    Queue
	opts QueueOptions
	red  redactor
	mu   sync.Mutex
	runs map[string]*buffered
}

// buffered is one run's recording so far. A run with drop set keeps no lines:
// it will not be queued, and is kept only so its end records why.
type buffered struct {
	subject string
	size    int
	lines   [][]byte
	drop    string
}

// start decides whether the run starting in ctx is handed off, and returns
// the failure to record when it is to be and cannot be.
func (h *handoff) start(ctx context.Context, id string) string {
	if id == "" || Evaluating(ctx) || !h.opts.Sample(h.opts.Rate) {
		return ""
	}
	subject := subjectOf(ctx)
	if subject == "" {
		return QueueNoSubject
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs[id] = &buffered{subject: subject}
	return ""
}

// add keeps e for its run, if the run is handed off.
func (h *handoff) add(e Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.runs[e.Run]
	if !ok || b.drop != "" {
		return
	}
	// A run missing an entry is dropped whole, never queued in part.
	line, err := json.Marshal(e)
	switch {
	case err != nil:
		b.lines, b.size, b.drop = nil, 0, QueueFailed
		return
	case b.size+len(line)+1 > h.opts.MaxRunBytes:
		b.lines, b.size, b.drop = nil, 0, QueueTooLarge
		return
	}
	b.lines = append(b.lines, line)
	b.size += len(line) + 1
}

// end queues the run that ended, and returns the failure to record when it
// was to be queued and was not.
func (h *handoff) end(ctx context.Context, id string) string {
	h.mu.Lock()
	b, ok := h.runs[id]
	delete(h.runs, id)
	h.mu.Unlock()
	if !ok {
		return ""
	}
	if b.drop != "" {
		return b.drop
	}
	now := time.Now().UTC()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(Header{Format: Format, Version: Version, Full: h.opts.Full, Created: now}); err != nil {
		return QueueFailed
	}
	for _, line := range b.lines {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	// The run's own context may have ended with it.
	err := h.q.Put(context.WithoutCancel(ctx), Item{Run: id, Subject: b.subject, At: now, Recording: buf.Bytes()})
	if err != nil {
		return QueueFailed
	}
	return ""
}

// deleteSubject forgets the runs of subject still being kept, so none of them
// is queued when it ends.
func (h *handoff) deleteSubject(subject string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, b := range h.runs {
		if b.subject == subject {
			delete(h.runs, id)
		}
	}
}

// MemQueue is a Queue in memory. It keeps items for its retention.
type MemQueue struct {
	retention time.Duration
	mu        sync.Mutex
	items     []Item
}

// NewMemQueue returns an empty MemQueue keeping items for retention, which
// must be positive.
func NewMemQueue(retention time.Duration) (*MemQueue, error) {
	if retention <= 0 {
		return nil, errors.New("record: a queue's retention must be positive")
	}
	return &MemQueue{retention: retention}, nil
}

// Put adds it.
func (q *MemQueue) Put(_ context.Context, it Item) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expire()
	q.items = append(q.items, it)
	return nil
}

// Take removes and returns the oldest item kept.
func (q *MemQueue) Take(context.Context) (Item, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expire()
	if len(q.items) == 0 {
		return Item{}, false, nil
	}
	it := q.items[0]
	q.items = q.items[1:]
	return it, true, nil
}

// DeleteSubject deletes every item of subject.
func (q *MemQueue) DeleteSubject(_ context.Context, subject string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.items[:0]
	for _, it := range q.items {
		if it.Subject != subject {
			kept = append(kept, it)
		}
	}
	clear(q.items[len(kept):])
	q.items = kept
	return nil
}

// Len reports how many items are kept.
func (q *MemQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expire()
	return len(q.items)
}

// expire drops the items older than the retention.
func (q *MemQueue) expire() {
	cutoff := time.Now().Add(-q.retention)
	kept := q.items[:0]
	for _, it := range q.items {
		if !it.At.Before(cutoff) {
			kept = append(kept, it)
		}
	}
	clear(q.items[len(kept):])
	q.items = kept
}
