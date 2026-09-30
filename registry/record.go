package registry

// TODO(phase 6, recording and replay): replace Recorder with the recorder slot
// and the stable recording format, and revisit ErrNoRecorder with it.

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

// Event is one entry a wrapper records: which slot and implementation, what it
// decided or how it failed. It never holds content.
type Event struct {
	Slot string
	// Name is the configured implementation's name.
	Name string
	// Point is the hook point, for hook events.
	Point string
	// Source is the kind of source a policy classified, for trust events.
	Source string
	// Decision is the verdict applied, for trust events.
	Decision string
	// Failure is empty when the call succeeded.
	Failure Failure
}

// Recorder receives the events of assembled parts.
type Recorder interface {
	Record(ev Event)
}

// Option changes how Assemble builds the parts.
type Option func(*assembly)

type assembly struct {
	rec Recorder
}

// WithRecorder sends the wrappers' events to rec. Without it they are dropped,
// which Assemble allows only when nothing configured has to be recorded (see
// ErrNoRecorder). A nil rec counts as none.
func WithRecorder(rec Recorder) Option {
	return func(a *assembly) { a.rec = rec }
}

type discard struct{}

func (discard) Record(Event) {}
