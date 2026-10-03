// Package runstore keeps the saved state of runs suspended on a pending
// approval, so a run survives a restart with its action (ADR 0001 §7).
//
// The state is bonyan's own, written and read by package agent; a store keeps
// it as opaque bytes with the run's identifier, subject and deadline. A run
// suspended in one process is saved unclaimed. Resuming it claims it, and
// only one claim is held at a time, until the run's deadline. Before the
// pending action goes ahead, whichever side holds the run marks it proceeding,
// a compare-and-set that succeeds once: from then on the run cannot be
// resumed, so no action runs twice. A process that crashes after claiming a
// run leaves it claimed until its deadline, by which time the run has timed
// out: a crash while resuming fails closed.
package runstore

import (
	"context"
	"errors"
	"sync"
	"time"
)

// The errors a store reports.
var (
	// ErrUnknown is a run the store does not hold.
	ErrUnknown = errors.New("runstore: unknown run")
	// ErrClaimed is a run another caller holds.
	ErrClaimed = errors.New("runstore: the run is claimed")
	// ErrProceeding is a run whose pending action has been decided and is
	// going ahead, or has gone ahead: it can no longer be resumed.
	ErrProceeding = errors.New("runstore: the run's action is proceeding")
)

// State is one saved run.
type State struct {
	// Run is the run's identifier.
	Run string
	// Subject is who the run is about; deleting the subject deletes it.
	Subject string
	// Saved is when it was saved, for retention.
	Saved time.Time
	// Deadline is the run's deadline. A claim lasts until it.
	Deadline time.Time
	// Data is the run's state, as package agent wrote it.
	Data []byte
}

// Store keeps saved runs. Every method must be safe for concurrent use, and
// every compare-and-set in it atomic, also across processes for a store they
// share.
type Store interface {
	// Save keeps s, replacing the run's earlier state, and leaves it
	// unclaimed. token is the caller's claim: empty for the process that ran
	// the run from its start. Save fails with ErrClaimed when another caller
	// holds the run.
	Save(ctx context.Context, s State, token string) error
	// Claim takes the run for the caller under token and returns its state.
	// It fails with ErrUnknown for a run the store does not hold, ErrClaimed
	// when another claim holds it and has not passed the run's deadline, and
	// ErrProceeding once its action is proceeding.
	Claim(ctx context.Context, run, token string, now time.Time) (State, error)
	// Proceed marks the run's pending action decided and going ahead, once:
	// it fails with ErrProceeding when it is already marked, ErrClaimed when
	// token is not the run's claim (empty for an unclaimed run), and
	// ErrUnknown for a run the store does not hold.
	Proceed(ctx context.Context, run, token string) error
	// Delete removes the run when token is its claim; otherwise it fails
	// with ErrClaimed. A run the store does not hold is not an error.
	Delete(ctx context.Context, run, token string) error
	// List returns the runs held, so a program can resume them after a
	// restart.
	List(ctx context.Context) ([]State, error)
	// DeleteSubject removes every run of subject, claimed or not.
	DeleteSubject(ctx context.Context, subject string) error
	// DeleteBefore removes every run saved before t.
	DeleteBefore(ctx context.Context, t time.Time) error
}

// Memory is a Store in process memory. It holds nothing across a restart.
type Memory struct {
	mu   sync.Mutex
	runs map[string]*entry
}

type entry struct {
	state      State
	claim      string
	proceeding bool
}

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{runs: map[string]*entry{}} }

// Save keeps s unclaimed.
func (m *Memory) Save(_ context.Context, s State, token string) error {
	if s.Run == "" {
		return errors.New("runstore: empty run")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.runs[s.Run]; ok && e.claim != token {
		return ErrClaimed
	}
	s.Data = append([]byte(nil), s.Data...)
	m.runs[s.Run] = &entry{state: s}
	return nil
}

// Claim takes the run under token.
func (m *Memory) Claim(_ context.Context, run, token string, now time.Time) (State, error) {
	if token == "" {
		return State{}, errors.New("runstore: empty token")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.runs[run]
	switch {
	case !ok:
		return State{}, ErrUnknown
	case e.proceeding:
		return State{}, ErrProceeding
	case e.claim != "" && now.Before(e.state.Deadline):
		return State{}, ErrClaimed
	}
	e.claim = token
	s := e.state
	s.Data = append([]byte(nil), s.Data...)
	return s, nil
}

// Proceed marks the run's action proceeding.
func (m *Memory) Proceed(_ context.Context, run, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.runs[run]
	switch {
	case !ok:
		return ErrUnknown
	case e.proceeding:
		return ErrProceeding
	case e.claim != token:
		return ErrClaimed
	}
	e.proceeding = true
	return nil
}

// Delete removes the run when token is its claim.
func (m *Memory) Delete(_ context.Context, run, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.runs[run]
	if !ok {
		return nil
	}
	if e.claim != token {
		return ErrClaimed
	}
	delete(m.runs, run)
	return nil
}

// List returns the runs held.
func (m *Memory) List(context.Context) ([]State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]State, 0, len(m.runs))
	for _, e := range m.runs {
		s := e.state
		s.Data = append([]byte(nil), s.Data...)
		out = append(out, s)
	}
	return out, nil
}

// DeleteSubject removes every run of subject.
func (m *Memory) DeleteSubject(_ context.Context, subject string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.runs {
		if e.state.Subject == subject {
			delete(m.runs, id)
		}
	}
	return nil
}

// DeleteBefore removes every run saved before t.
func (m *Memory) DeleteBefore(_ context.Context, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.runs {
		if e.state.Saved.Before(t) {
			delete(m.runs, id)
		}
	}
	return nil
}
