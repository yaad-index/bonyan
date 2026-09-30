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
	"github.com/yaad-index/bonyan/secret"
)

// Recorder applies bonyan's rules to what is recorded, then hands it to a
// sink. It is not a slot: every sink receives entries only through it.
type Recorder struct {
	sink  Sink
	red   redactor
	seq   atomic.Int64
	fails atomic.Int64
}

// NewRecorder returns a recorder writing to sink. Every resolved value known to
// scrub is removed from what is recorded. Both rules are checked here rather
// than left to the sink or the caller: a nil scrub is refused, since recording
// without one would write resolved secrets to disk, and so is a full sink that
// reports no subject, since its recordings could not be deleted by subject.
func NewRecorder(sink Sink, scrub *secret.Scrubber) (*Recorder, error) {
	if sink == nil {
		return nil, errors.New("record: no sink")
	}
	if scrub == nil {
		return nil, errors.New("record: a recorder needs the resolver's scrubber")
	}
	if sink.Full() && sink.Subject() == "" {
		return nil, errors.New("record: a full recording needs a subject")
	}
	return &Recorder{sink: sink, red: redactor{full: sink.Full(), scrub: scrub}}, nil
}

// WriteFailures reports how many entries the sink failed to write. Recording
// never fails the call it records, so this is where a broken sink shows.
func (r *Recorder) WriteFailures() int64 { return r.fails.Load() }

// Event records an event.
func (r *Recorder) Event(ev Event) {
	r.write(Entry{Event: &ev})
}

// Call records one model call, redacted.
func (r *Recorder) Call(modelName string, req model.ChatRequest, resp model.ChatResponse, callErr error) {
	rec := r.red.request(req)
	c := Call{
		Seq:         r.seq.Add(1),
		Model:       modelName,
		Fingerprint: fingerprint(modelName, rec),
		Request:     rec,
	}
	if callErr != nil {
		c.ErrorKind = errorKind(callErr)
	} else {
		c.Response = r.red.response(resp)
	}
	r.write(Entry{Call: &c})
}

func (r *Recorder) write(e Entry) {
	if err := r.sink.Write(e); err != nil {
		r.fails.Add(1)
	}
}

// Chat wraps a chat model so that every call through it is recorded.
func Chat(inner model.Chat, modelName string, rec *Recorder) model.Chat {
	return recordedChat{inner: inner, name: modelName, rec: rec}
}

type recordedChat struct {
	inner model.Chat
	name  string
	rec   *Recorder
}

func (c recordedChat) Chat(ctx context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	resp, err := c.inner.Chat(ctx, req)
	c.rec.Call(c.name, req, resp, err)
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

// fingerprint identifies a redacted request, so replay can check that it is
// answering the call that was recorded.
func fingerprint(modelName string, req Request) string {
	b, err := json.Marshal(struct {
		Model   string  `json:"model"`
		Request Request `json:"request"`
	}{modelName, req})
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
		return Part{Trusted: true, Text: r.text(v.String())}
	case content.Untrusted:
		p := v.Provenance()
		prov := Provenance{Kind: p.Kind, Origin: p.Origin, ID: p.ID}
		if prov.Kind == content.KindMemory && !r.full {
			return Part{Provenance: &prov, Excluded: true}
		}
		return Part{Text: r.text(v.Raw()), Provenance: &prov}
	}
	return Part{Excluded: true}
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
