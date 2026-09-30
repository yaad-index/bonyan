package registry

import (
	"encoding/json"
	"errors"

	"github.com/yaad-index/bonyan/record"
)

// Failure classifies why a wrapped call did not produce a usable result. It is
// recorded instead of the error text, which could carry content.
type Failure string

// The failures a wrapper records.
const (
	FailError      Failure = "error"       // the implementation returned an error
	FailPanic      Failure = "panic"       // the implementation panicked
	FailDeadline   Failure = "deadline"    // the context ended before it returned
	FailNoDecision Failure = "no decision" // a policy returned no verdict
)

// SinkFile is the name the file recording sink is registered under. Its
// options are {"dir": ..., "full": ..., "subject": ...}, as record.FileOptions.
const SinkFile = "file"

func fileSink(options json.RawMessage) (record.Sink, error) {
	var o struct {
		Dir     string `json:"dir"`
		Full    bool   `json:"full"`
		Subject string `json:"subject"`
	}
	if len(options) > 0 {
		if err := json.Unmarshal(options, &o); err != nil {
			return nil, err
		}
	}
	return record.OpenFile(record.FileOptions{Dir: o.Dir, Full: o.Full, Subject: o.Subject})
}

// Option changes how Assemble builds the parts.
type Option func(*assembly)

type assembly struct {
	sink record.Sink
}

// WithSink records to sink, a sink the program built itself, instead of one
// selected by Config.Recording. The program keeps ownership of it:
// Components.Close does not close it.
func WithSink(sink record.Sink) Option {
	return func(a *assembly) { a.sink = sink }
}

// events is where the wrappers send what they record. With no recording
// configured, events are dropped, which Assemble allows only when nothing
// configured has to be recorded (see ErrNoRecorder).
type events interface {
	Event(ev record.Event)
}

type discard struct{}

func (discard) Event(record.Event) {}

var errTwoSinks = errors.New("registry: a recording is configured and a sink is also passed with WithSink")
