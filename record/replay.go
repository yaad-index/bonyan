package record

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/secret"
)

// ErrMismatch reports a request that is not the one recorded next.
var ErrMismatch = errors.New("record: request does not match the recording")

// ErrExhausted reports a request after every recorded call has been served.
var ErrExhausted = errors.New("record: no recorded call left")

// Read reads a recording: its header and its calls, in order. Events and run
// boundaries are skipped. A recording in another format, or a version this
// package does not read, is refused.
func Read(r io.Reader) (Header, []Call, error) {
	h, entries, err := readEntries(r)
	if err != nil {
		return Header{}, nil, err
	}
	var calls []Call
	for _, e := range entries {
		if e.Call != nil {
			calls = append(calls, *e.Call)
		}
	}
	return h, calls, nil
}

// Run is one run's part of a recording.
type Run struct {
	// ID is the run's identifier. It is empty for the entries written outside
	// any run, which are every entry of a version 1 recording.
	ID string
	// Agent is the agent's name from the run's start, if it had one.
	Agent string
	// End is how the run ended; nil when the recording holds no end for it, as
	// for a version 1 recording or a run that never finished.
	End    *End
	Calls  []Call
	Events []Event
}

// ReadRuns reads a recording and returns its entries grouped by run, in the
// order each run first appears, so runs recorded at the same time come apart.
func ReadRuns(r io.Reader) (Header, []Run, error) {
	h, entries, err := readEntries(r)
	if err != nil {
		return Header{}, nil, err
	}
	var runs []*Run
	byID := map[string]*Run{}
	for _, e := range entries {
		run, ok := byID[e.Run]
		if !ok {
			run = &Run{ID: e.Run}
			byID[e.Run] = run
			runs = append(runs, run)
		}
		switch {
		case e.Start != nil:
			run.Agent = e.Start.Agent
		case e.End != nil:
			end := *e.End
			run.End = &end
		case e.Call != nil:
			run.Calls = append(run.Calls, *e.Call)
		case e.Event != nil:
			run.Events = append(run.Events, *e.Event)
		}
	}
	out := make([]Run, len(runs))
	for i, run := range runs {
		out[i] = *run
	}
	return h, out, nil
}

// readEntries reads a recording's header and entries. It reads every version
// from 1 to Version; a version 1 recording's entries name no run.
func readEntries(r io.Reader) (Header, []Entry, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return Header{}, nil, fmt.Errorf("record: %w", err)
		}
		return Header{}, nil, errors.New("record: empty recording")
	}
	var h Header
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
		return Header{}, nil, fmt.Errorf("record: header: %w", err)
	}
	if h.Format != Format || h.Version < 1 || h.Version > Version {
		return Header{}, nil, fmt.Errorf("record: unsupported recording %q version %d", h.Format, h.Version)
	}
	var entries []Entry
	for line := 2; sc.Scan(); line++ {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return Header{}, nil, fmt.Errorf("record: line %d: %w", line, err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return Header{}, nil, fmt.Errorf("record: %w", err)
	}
	return h, entries, nil
}

// Replay serves recorded calls in the order they were recorded, so a test runs
// the real loop without a provider. Each request is redacted the way it was
// when recorded and checked against the recorded fingerprint: a request that
// differs is refused with ErrMismatch rather than answered with the wrong
// response. It is safe for concurrent use; calls are served one at a time.
type Replay struct {
	mu    sync.Mutex
	calls []Call
	next  int
	red   redactor
}

// NewReplay returns a replay of calls from a recording with header h. scrub
// must know the same resolved values the recorder's did, or a request that
// carried one will not match; nil scrubs nothing.
func NewReplay(h Header, calls []Call, scrub *secret.Scrubber) *Replay {
	return &Replay{calls: calls, red: redactor{full: h.Full, scrub: scrub}}
}

// Model returns a chat model that answers as the model recorded under name.
func (r *Replay) Model(name string) model.Chat {
	return replayChat{r: r, name: name}
}

// Classifier returns a classifier that answers as the classifier recorded
// under name. It shares the replay's sequence with Model.
func (r *Replay) Classifier(name string) model.Classifier {
	return replayClassifier{r: r, name: name}
}

// take returns the next recorded call if it is a kind call to name whose
// fingerprint is fp, and advances past it.
func (r *Replay) take(kind, name string, input any) (Call, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next >= len(r.calls) {
		return Call{}, ErrExhausted
	}
	want := r.calls[r.next]
	if want.Kind != kind || want.Model != name {
		return Call{}, fmt.Errorf("%w: call %d was a %s call to %q, not a %s call to %q", ErrMismatch, want.Seq, want.Kind, want.Model, kind, name)
	}
	if fingerprint(kind, name, input) != want.Fingerprint {
		return Call{}, fmt.Errorf("%w: call %d", ErrMismatch, want.Seq)
	}
	r.next++
	return want, nil
}

// Remaining reports how many recorded calls have not been served.
func (r *Replay) Remaining() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls) - r.next
}

type replayChat struct {
	r    *Replay
	name string
}

func (c replayChat) Chat(ctx context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return model.ChatResponse{}, err
	}
	want, err := c.r.take(KindChat, c.name, c.r.red.request(req))
	if err != nil {
		return model.ChatResponse{}, err
	}
	if want.ErrorKind != "" {
		return model.ChatResponse{}, &model.CallError{Kind: want.ErrorKind}
	}
	return want.Response.model(), nil
}

type replayClassifier struct {
	r    *Replay
	name string
}

func (c replayClassifier) Classify(ctx context.Context, text content.Untrusted) (model.ClassifyResponse, error) {
	if err := ctx.Err(); err != nil {
		return model.ClassifyResponse{}, err
	}
	want, err := c.r.take(KindClassify, c.name, c.r.red.part(text))
	if err != nil {
		return model.ClassifyResponse{}, err
	}
	if want.ErrorKind != "" {
		return model.ClassifyResponse{}, &model.CallError{Kind: want.ErrorKind}
	}
	return want.Response.classify(), nil
}
