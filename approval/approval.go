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
	// approve. The channel receives at most once. Holding an action the store
	// holds already adds a waiter for the same decision, which is how a run
	// resumed after a restart waits on its action again (ADR 0001 §7); a
	// store that keeps a decision made while nothing waited delivers it to
	// the next waiter.
	Hold(ctx context.Context, p Pending) (<-chan bool, error)
	// Decide decides the action id, for every waiter. It returns ErrUnknown
	// when the store does not hold it.
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
	p       Pending
	waiters []chan bool
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
	h, ok := m.held[p.ID]
	if ok && h.p.Tool != p.Tool {
		return nil, fmt.Errorf("approval: %q is held for another tool", p.ID)
	}
	ch := make(chan bool, 1)
	h.p = p
	h.waiters = append(h.waiters, ch)
	m.held[p.ID] = h
	return ch, nil
}

// Decide decides id for every waiter and stops holding it.
func (m *Memory) Decide(_ context.Context, id string, approve bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.held[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknown, id)
	}
	delete(m.held, id)
	for _, ch := range h.waiters {
		ch <- approve
	}
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
