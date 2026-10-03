// Package approval holds actions waiting for decisions that come after
// approvers said they were pending (ADR 0001 §7, §12). Each approver that said
// pending decides on its own, under its own name, and the store keeps a
// decision per approver. A program decides an action through the store it
// gave the agent, for example from its own interface; who may decide under a
// name, and authenticating whoever submits a decision, are the program's.
//
// A decision for an action the store no longer holds is refused as unknown,
// never applied: the action was decided already, timed out, or was held by a
// process that has since stopped. So is one under a name that is not among
// the action's approvers. A second decision from an approver is refused as
// decided already. The in-memory store holds nothing across a restart, so a
// restart cancels every pending action.
package approval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// The errors Decide returns.
var (
	// ErrUnknown is an action the store does not hold, or a name that is not
	// one of the action's approvers.
	ErrUnknown = errors.New("approval: unknown action")
	// ErrDecided is an approver that decided the action already.
	ErrDecided = errors.New("approval: decided already")
)

// Pending is an action held for decisions.
type Pending struct {
	ID string
	// Tool is the name of the tool the action calls.
	Tool string
	// Approvers names the approvers that said the decision is pending, each
	// of which decides on its own.
	Approvers []string
}

// Decision is one approver's decision.
type Decision struct {
	// By names the approver.
	By string
	// Approve is true to approve, false to deny.
	Approve bool
}

// Store holds pending actions until each is dropped. Every method must be
// safe for concurrent use.
type Store interface {
	// Hold keeps p and returns a channel that receives each approver's
	// decision once, as it is made, with those made already first; it is
	// buffered for every approver. Holding an action the store holds already,
	// for the same tool and approvers, adds a waiter for the same decisions,
	// which is how a run resumed after a restart waits on its action again
	// and keeps the decisions made (ADR 0001 §7).
	Hold(ctx context.Context, p Pending) (<-chan Decision, error)
	// Decide records by's decision on the action id, for every waiter. It
	// returns ErrUnknown when the store does not hold the action or when by
	// is not one of its approvers, and ErrDecided when by decided already.
	Decide(ctx context.Context, id, by string, approve bool) error
	// Drop stops holding id, decided or not.
	Drop(ctx context.Context, id string) error
	// List returns the actions still waiting: those no approver denied, with
	// an approver that has not decided. Each lists, as its Approvers, only
	// the approvers that have not decided.
	List(ctx context.Context) ([]Pending, error)
}

// check refuses an action a store cannot hold.
func check(p Pending) error {
	if p.ID == "" {
		return errors.New("approval: empty id")
	}
	if len(p.Approvers) == 0 {
		return fmt.Errorf("approval: %q has no approver", p.ID)
	}
	for i, a := range p.Approvers {
		if a == "" {
			return fmt.Errorf("approval: %q has an approver with no name", p.ID)
		}
		if slices.Contains(p.Approvers[:i], a) {
			return fmt.Errorf("approval: %q names approver %q twice", p.ID, a)
		}
	}
	return nil
}

// same reports whether p is the action held as h, held again.
func same(h, p Pending) error {
	if h.Tool != p.Tool {
		return fmt.Errorf("approval: %q is held for another tool", p.ID)
	}
	if !slices.Equal(h.Approvers, p.Approvers) {
		return fmt.Errorf("approval: %q is held for other approvers", p.ID)
	}
	return nil
}

// decide checks that by may decide the action held as p, given the decisions
// made already.
func decide(p Pending, made []Decision, by string) error {
	switch {
	case !slices.Contains(p.Approvers, by):
		return fmt.Errorf("%w: %q by %q", ErrUnknown, p.ID, by)
	case slices.ContainsFunc(made, func(d Decision) bool { return d.By == by }):
		return fmt.Errorf("%w: %q by %q", ErrDecided, p.ID, by)
	}
	return nil
}

// waiting returns the action held as p as List reports it, and whether it
// still waits.
func waiting(p Pending, made []Decision) (Pending, bool) {
	out := Pending{ID: p.ID, Tool: p.Tool}
	for _, a := range p.Approvers {
		if !slices.ContainsFunc(made, func(d Decision) bool { return d.By == a }) {
			out.Approvers = append(out.Approvers, a)
		}
	}
	denied := slices.ContainsFunc(made, func(d Decision) bool { return !d.Approve })
	return out, !denied && len(out.Approvers) > 0
}

// Memory is a Store in process memory.
type Memory struct {
	mu   sync.Mutex
	held map[string]*held
}

type held struct {
	p       Pending
	made    []Decision
	waiters []chan Decision
}

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{held: map[string]*held{}} }

// Hold keeps p.
func (m *Memory) Hold(_ context.Context, p Pending) (<-chan Decision, error) {
	if err := check(p); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.held[p.ID]
	if ok {
		if err := same(h.p, p); err != nil {
			return nil, err
		}
	} else {
		h = &held{p: Pending{ID: p.ID, Tool: p.Tool, Approvers: slices.Clone(p.Approvers)}}
		m.held[p.ID] = h
	}
	ch := make(chan Decision, len(p.Approvers))
	for _, d := range h.made {
		ch <- d
	}
	h.waiters = append(h.waiters, ch)
	return ch, nil
}

// Decide records by's decision on id for every waiter.
func (m *Memory) Decide(_ context.Context, id, by string, approve bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.held[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknown, id)
	}
	if err := decide(h.p, h.made, by); err != nil {
		return err
	}
	d := Decision{By: by, Approve: approve}
	h.made = append(h.made, d)
	for _, ch := range h.waiters {
		ch <- d
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

// List returns the actions still waiting.
func (m *Memory) List(context.Context) ([]Pending, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Pending, 0, len(m.held))
	for _, h := range m.held {
		if p, ok := waiting(h.p, h.made); ok {
			out = append(out, p)
		}
	}
	return out, nil
}
