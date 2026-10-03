package record

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync/atomic"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/prompt"
	"github.com/yaad-index/bonyan/secret"
)

// Recorder applies bonyan's rules to what is recorded, then hands it to a
// sink. It is not a slot: every sink receives entries only through it.
type Recorder struct {
	sink  Sink
	red   redactor
	seq   atomic.Int64
	fails atomic.Int64
	// queue hands finished runs to a queue for live evaluation; nil hands
	// off none.
	queue *handoff
}

// NewRecorder returns a recorder writing to sink. Every resolved value known to
// scrub is removed from what is recorded. Both rules are checked here rather
// than left to the sink or the caller: a nil scrub is refused, since recording
// without one would write resolved secrets to disk, and so is a full sink that
// reports no subject, since its recordings could not be deleted by subject.
func NewRecorder(sink Sink, scrub *secret.Scrubber, opts ...Option) (*Recorder, error) {
	if sink == nil {
		return nil, errors.New("record: no sink")
	}
	if scrub == nil {
		return nil, errors.New("record: a recorder needs the resolver's scrubber")
	}
	if sink.Full() && sink.Subject() == "" {
		return nil, errors.New("record: a full recording needs a subject")
	}
	r := &Recorder{sink: sink, red: redactor{full: sink.Full(), scrub: scrub}}
	for _, o := range opts {
		if err := o(r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// WriteFailures reports how many entries the sink failed to write. Recording
// never fails the call it records, so this is where a broken sink shows.
func (r *Recorder) WriteFailures() int64 { return r.fails.Load() }

// promptOf is the prompt ctx says a call is made with, or nil.
func promptOf(ctx context.Context) *PromptRef {
	r, ok := prompt.RefOf(ctx)
	if !ok {
		return nil
	}
	return &PromptRef{ID: r.ID, Hash: r.Hash}
}

type runKey struct{}

// WithRun returns ctx inside run id: every entry recorded with the returned
// context, or one derived from it, names the run.
func WithRun(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runKey{}, id)
}

// runOf is the run ctx is inside, or "" outside any.
func runOf(ctx context.Context) string {
	id, _ := ctx.Value(runKey{}).(string)
	return id
}

// Start records the start of the run ctx is inside, marked as evaluation
// when ctx is (WithEvaluation).
func (r *Recorder) Start(ctx context.Context, s Start) {
	s.Evaluation = Evaluating(ctx)
	var failure string
	if r.queue != nil {
		failure = r.queue.start(ctx, runOf(ctx))
	}
	e := Entry{Start: &s}
	r.write(ctx, e, &e)
	r.dropped(ctx, failure)
}

// End records the end of the run ctx is inside, and hands the run to the
// queue when it was kept for one.
func (r *Recorder) End(ctx context.Context, end End) {
	e := Entry{End: &end}
	r.write(ctx, e, &e)
	if r.queue != nil {
		r.dropped(ctx, r.queue.end(ctx, runOf(ctx)))
	}
}

// DeleteSubject deletes subject from live evaluation: the subject's runs the
// recorder is still keeping for the queue are forgotten, so none is queued
// when it ends, and then the subject's queued items are deleted. With no queue
// it does nothing. It does not touch the sink; record.DeleteSubject deletes
// full recordings.
func (r *Recorder) DeleteSubject(ctx context.Context, subject string) error {
	if r.queue == nil {
		return nil
	}
	// Runs first: a run ending between the two steps is either already
	// queued, and deleted next, or no longer kept.
	r.queue.deleteSubject(subject)
	return r.queue.q.DeleteSubject(ctx, subject)
}

// dropped records that the run in ctx was to be queued and was not.
func (r *Recorder) dropped(ctx context.Context, failure string) {
	if failure != "" {
		r.Event(ctx, Event{Slot: SlotQueue, Decision: DecisionDropped, Failure: failure})
	}
}

// Event records an event.
func (r *Recorder) Event(ctx context.Context, ev Event) {
	e := Entry{Event: &ev}
	r.write(ctx, e, &e)
}

// Call records one model call, redacted.
func (r *Recorder) Call(ctx context.Context, modelName string, req model.ChatRequest, resp model.ChatResponse, callErr error) {
	seq := r.seq.Add(1)
	build := func(red redactor) *Entry {
		rec := red.request(req)
		c := Call{
			Seq:         seq,
			Kind:        KindChat,
			Model:       modelName,
			Fingerprint: fingerprint(KindChat, modelName, rec),
			Request:     &rec,
			Prompt:      promptOf(ctx),
		}
		if callErr != nil {
			c.ErrorKind = errorKind(callErr)
		} else {
			c.Response = red.response(resp)
		}
		return &Entry{Call: &c}
	}
	r.writeBoth(ctx, build)
}

// Classify records one classifier call, redacted like a chat request's parts.
func (r *Recorder) Classify(ctx context.Context, modelName string, text content.Untrusted, resp model.ClassifyResponse, callErr error) {
	seq := r.seq.Add(1)
	build := func(red redactor) *Entry {
		in := red.part(text)
		c := Call{
			Seq:         seq,
			Kind:        KindClassify,
			Model:       modelName,
			Fingerprint: fingerprint(KindClassify, modelName, in),
			Input:       &in,
			Prompt:      promptOf(ctx),
		}
		if callErr != nil {
			c.ErrorKind = errorKind(callErr)
		} else {
			c.Response = red.labels(resp)
		}
		return &Entry{Call: &c}
	}
	r.writeBoth(ctx, build)
}

// writeBoth writes a call redacted for the sink, and, when the queue keeps
// a different kind of recording, redacted again for the queue.
func (r *Recorder) writeBoth(ctx context.Context, build func(redactor) *Entry) {
	e := build(r.red)
	queued := e
	if r.queue != nil && r.queue.red.full != r.red.full {
		queued = build(r.queue.red)
	}
	r.write(ctx, *e, queued)
}

// write writes e to the sink, and queued, the same entry redacted for the
// queue, to the run's kept recording when the run is kept for the queue.
func (r *Recorder) write(ctx context.Context, e Entry, queued *Entry) {
	e.Run = runOf(ctx)
	if err := r.sink.Write(e); err != nil {
		r.fails.Add(1)
	}
	if r.queue != nil && queued != nil {
		q := *queued
		q.Run = e.Run
		r.queue.add(q)
	}
}

// Chat wraps a chat model so that every call through it is recorded.
func Chat(inner model.Chat, modelName string, rec *Recorder) model.Chat {
	return recordedChat{inner: inner, name: modelName, rec: rec}
}

// Classifier wraps a classifier so that every call through it is recorded.
func Classifier(inner model.Classifier, modelName string, rec *Recorder) model.Classifier {
	return recordedClassifier{inner: inner, name: modelName, rec: rec}
}

type recordedClassifier struct {
	inner model.Classifier
	name  string
	rec   *Recorder
}

func (c recordedClassifier) Classify(ctx context.Context, text content.Untrusted) (model.ClassifyResponse, error) {
	resp, err := c.inner.Classify(ctx, text)
	c.rec.Classify(ctx, c.name, text, resp, err)
	return resp, err
}

type recordedChat struct {
	inner model.Chat
	name  string
	rec   *Recorder
}

func (c recordedChat) Chat(ctx context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	resp, err := c.inner.Chat(ctx, req)
	c.rec.Call(ctx, c.name, req, resp, err)
	return resp, err
}

// errorKind classifies a call error without its text, which could carry
// content.
func errorKind(err error) model.ErrorKind {
	var ce *model.CallError
	if errors.As(err, &ce) && ce.Kind != "" {
		return ce.Kind
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return model.ErrTimeout
	case errors.Is(err, context.Canceled):
		return model.ErrCanceled
	}
	return ErrorOther
}

// fingerprint identifies a redacted call, so replay can check that it is
// answering the call that was recorded.
func fingerprint(kind, modelName string, input any) string {
	b, err := json.Marshal(struct {
		Kind  string `json:"kind"`
		Model string `json:"model"`
		Input any    `json:"input"`
	}{kind, modelName, input})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// redactor turns requests and responses into their recorded form.
type redactor struct {
	full  bool
	scrub *secret.Scrubber
}

func (r redactor) text(s string) string {
	if r.scrub == nil { // only on replay, which writes nothing
		return s
	}
	return r.scrub.Scrub(s)
}

// raw scrubs JSON. If scrubbing leaves it invalid, it is kept as a JSON string.
func (r redactor) raw(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return b
	}
	s := r.text(string(b))
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	q, _ := json.Marshal(s)
	return q
}

func (r redactor) request(req model.ChatRequest) Request {
	out := Request{Schema: r.raw(req.Schema), MaxOutputTokens: req.MaxOutputTokens}
	for _, m := range req.Messages {
		rm := Message{Role: m.Role, ToolCallID: m.ToolCallID}
		for _, p := range m.Parts {
			rm.Parts = append(rm.Parts, r.part(p))
		}
		for _, tc := range m.ToolCalls {
			rm.ToolCalls = append(rm.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Name, Arguments: r.raw(tc.Arguments)})
		}
		out.Messages = append(out.Messages, rm)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, Tool{
			Name: t.Name, Description: r.text(t.Description), Parameters: r.raw(t.Parameters),
		})
	}
	return out
}

func (r redactor) part(p content.Text) Part {
	switch v := p.(type) {
	case content.Trusted:
		p := v.Provenance()
		if p == (content.Provenance{}) {
			return Part{Trusted: true, Text: r.text(v.String())}
		}
		// Text the policy declared trusted keeps its provenance, and memory
		// is excluded whether trusted or not.
		prov := Provenance{Kind: p.Kind, Origin: p.Origin, Server: p.Server, ID: p.ID}
		if prov.Kind == content.KindMemory && !r.full {
			return Part{Trusted: true, Provenance: &prov, Excluded: true}
		}
		return Part{Trusted: true, Text: r.text(v.String()), Provenance: &prov}
	case content.Untrusted:
		p := v.Provenance()
		prov := Provenance{Kind: p.Kind, Origin: p.Origin, Server: p.Server, ID: p.ID}
		if prov.Kind == content.KindMemory && !r.full {
			return Part{Provenance: &prov, Excluded: true}
		}
		return Part{Text: r.text(v.Raw()), Provenance: &prov}
	case content.Marked:
		return r.part(v.Section())
	case content.Section:
		out := Part{Section: v.Label()}
		for _, it := range v.Items() {
			out.Items = append(out.Items, r.part(it))
		}
		return out
	}
	return Part{Excluded: true}
}

func (r redactor) labels(resp model.ClassifyResponse) Response {
	var out Response
	for _, l := range resp.Labels {
		out.Labels = append(out.Labels, Label{Name: l.Name, Confidence: l.Confidence})
	}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	}
	return out
}

func (r redactor) response(resp model.ChatResponse) Response {
	out := Response{Content: r.text(resp.Content), StopReason: resp.StopReason}
	for _, tc := range resp.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Name, Arguments: r.raw(tc.Arguments)})
	}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	}
	return out
}
