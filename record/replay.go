package record

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/secret"
)

// ErrMismatch reports a request that is not the one recorded next.
var ErrMismatch = errors.New("record: request does not match the recording")

// ErrExhausted reports a request after every recorded call has been served.
var ErrExhausted = errors.New("record: no recorded call left")

// Read reads a recording: its header and its calls, in order. Events are
// skipped. A recording in another format or version is refused.
func Read(r io.Reader) (Header, []Call, error) {
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
	if h.Format != Format || h.Version != Version {
		return Header{}, nil, fmt.Errorf("record: unsupported recording %q version %d", h.Format, h.Version)
	}
	var calls []Call
	for line := 2; sc.Scan(); line++ {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return Header{}, nil, fmt.Errorf("record: line %d: %w", line, err)
		}
		if e.Call != nil {
			calls = append(calls, *e.Call)
		}
	}
	if err := sc.Err(); err != nil {
		return Header{}, nil, fmt.Errorf("record: %w", err)
	}
	return h, calls, nil
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
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next >= len(r.calls) {
		return model.ChatResponse{}, ErrExhausted
	}
	want := r.calls[r.next]
	if want.Model != c.name {
		return model.ChatResponse{}, fmt.Errorf("%w: call %d was to %q, not %q", ErrMismatch, want.Seq, want.Model, c.name)
	}
	if got := fingerprint(c.name, r.red.request(req)); got != want.Fingerprint {
		return model.ChatResponse{}, fmt.Errorf("%w: call %d", ErrMismatch, want.Seq)
	}
	r.next++
	if want.ErrorKind != "" {
		return model.ChatResponse{}, &model.CallError{Kind: want.ErrorKind}
	}
	return want.Response.model(), nil
}
