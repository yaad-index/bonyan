// Package approval holds actions waiting for a decision that comes after an
// approver said it was pending (ADR 0001 §7). A program decides an action
// through the store it gave the agent, for example from its own interface.
//
// A decision for an action the store no longer holds is refused as unknown,
// never applied: the action was decided already, timed out, or was held by a
// process that has since stopped. The in-memory store holds nothing across a
// restart, so a restart cancels every pending action.
package approval

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrUnknown is what Decide returns for an action the store does not hold.
var ErrUnknown = errors.New("approval: unknown action")

// Pending is an action held for a decision.
type Pending struct {
	ID string
	// Tool is the name of the tool the action calls.
	Tool string
}

// Store holds pending actions until each is decided or dropped. Every method
// must be safe for concurrent use.
type Store interface {
	// Hold keeps p and returns a channel that receives the decision: true to
	// approve. The channel receives at most once.
	Hold(ctx context.Context, p Pending) (<-chan bool, error)
	// Decide decides the action id. It returns ErrUnknown when the store does
	// not hold it.
	Decide(ctx context.Context, id string, approve bool) error
	// Drop stops holding id, decided or not.
	Drop(ctx context.Context, id string) error
	// List returns the actions held.
	List(ctx context.Context) ([]Pending, error)
}

// Memory is a Store in process memory.
type Memory struct {
	mu   sync.Mutex
	held map[string]held
}

type held struct {
	p  Pending
	ch chan bool
}

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{held: map[string]held{}} }

// Hold keeps p.
func (m *Memory) Hold(_ context.Context, p Pending) (<-chan bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == "" {
		return nil, errors.New("approval: empty id")
	}
	if _, ok := m.held[p.ID]; ok {
		return nil, fmt.Errorf("approval: %q is already held", p.ID)
	}
	ch := make(chan bool, 1)
	m.held[p.ID] = held{p: p, ch: ch}
	return ch, nil
}

// Decide decides id and stops holding it.
func (m *Memory) Decide(_ context.Context, id string, approve bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.held[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknown, id)
	}
	delete(m.held, id)
	h.ch <- approve
	return nil
}

// Drop stops holding id.
func (m *Memory) Drop(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.held, id)
	return nil
}

// List returns the actions held.
func (m *Memory) List(context.Context) ([]Pending, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Pending, 0, len(m.held))
	for _, h := range m.held {
		out = append(out, h.p)
	}
	return out, nil
}
