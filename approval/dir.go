//go:build unix

package approval

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yaad-index/bonyan/internal/dirstore"
)

// Dir is a Store in an owner-only directory, one file per action, that
// survives a restart and that several processes on one machine can share. A
// decision made while no process waits on the action is kept, and the next
// process to hold the action receives it, which is how a run resumed after a
// restart learns the decisions made while it was down (ADR 0001 §7). A waiter
// sees a decision made by another process within PollEvery.
//
// An action stays in the directory, with its decisions, for a resumed run to
// receive, until it is dropped or Purge removes it. It is for a local filesystem only:
// its lock is not reliable on a network mount. It is built on unix systems
// only.
type Dir struct {
	d *dirstore.Dir
	// PollEvery is how often a waiter looks for a decision made by another
	// process; zero means DefaultPollEvery.
	PollEvery time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// DefaultPollEvery is how often a waiter looks for a decision when
// Dir.PollEvery is zero.
const DefaultPollEvery = 200 * time.Millisecond

// OpenDir opens the store in the directory at path, creating it owner-only.
func OpenDir(path string) (*Dir, error) {
	d, err := dirstore.Open(path)
	if err != nil {
		return nil, err
	}
	return &Dir{d: d}, nil
}

type dirAction struct {
	Pending Pending   `json:"pending"`
	Held    time.Time `json:"held"`
	// Made are the decisions made, in the order they were made.
	Made []Decision `json:"made,omitempty"`
}

func (d *Dir) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Hold keeps p, or waits on it again when it is held already, and delivers
// each decision made already at once and each later one when it is made. The
// waiter stops looking when ctx ends.
func (d *Dir) Hold(ctx context.Context, p Pending) (<-chan Decision, error) {
	if err := check(p); err != nil {
		return nil, err
	}
	var made []Decision
	err := d.d.Locked(func(tx dirstore.Tx) error {
		var a dirAction
		err := tx.Get(p.ID, &a)
		switch {
		case errors.Is(err, dirstore.ErrNotFound):
			return tx.Put(p.ID, dirAction{Pending: p, Held: d.now()})
		case err != nil:
			return err
		}
		made = a.Made
		return same(a.Pending, p)
	})
	if err != nil {
		return nil, err
	}
	ch := make(chan Decision, len(p.Approvers))
	for _, m := range made {
		ch <- m
	}
	if len(made) < len(p.Approvers) {
		go d.watch(ctx, p.ID, len(made), ch)
	}
	return ch, nil
}

// watch delivers id's decisions after the first sent on ch as they are made,
// and stops once every approver decided, or when ctx ends or the action is
// dropped.
func (d *Dir) watch(ctx context.Context, id string, sent int, ch chan<- Decision) {
	every := d.PollEvery
	if every <= 0 {
		every = DefaultPollEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var a dirAction
		var gone bool
		err := d.d.Locked(func(tx dirstore.Tx) error {
			err := tx.Get(id, &a)
			gone = errors.Is(err, dirstore.ErrNotFound)
			if gone {
				return nil
			}
			return err
		})
		switch {
		case gone:
			return
		case err != nil:
			continue
		}
		for ; sent < len(a.Made); sent++ {
			ch <- a.Made[sent]
		}
		if sent >= len(a.Pending.Approvers) {
			return
		}
	}
}

// Decide records by's decision on id.
func (d *Dir) Decide(_ context.Context, id, by string, approve bool) error {
	return d.d.Locked(func(tx dirstore.Tx) error {
		var a dirAction
		err := tx.Get(id, &a)
		switch {
		case errors.Is(err, dirstore.ErrNotFound):
			return fmt.Errorf("%w: %q", ErrUnknown, id)
		case err != nil:
			return err
		}
		if err := decide(a.Pending, a.Made, by); err != nil {
			return err
		}
		a.Made = append(a.Made, Decision{By: by, Approve: approve})
		return tx.Put(id, a)
	})
}

// Drop stops holding id, decided or not.
func (d *Dir) Drop(_ context.Context, id string) error {
	return d.d.Locked(func(tx dirstore.Tx) error { return tx.Delete(id) })
}

// List returns the actions still waiting.
func (d *Dir) List(context.Context) ([]Pending, error) {
	var out []Pending
	err := d.d.Locked(func(tx dirstore.Tx) error {
		return d.each(tx, func(_ string, a dirAction) error {
			if p, ok := waiting(a.Pending, a.Made); ok {
				out = append(out, p)
			}
			return nil
		})
	})
	return out, err
}

// Purge removes every action held before t, decided or not.
func (d *Dir) Purge(_ context.Context, t time.Time) error {
	return d.d.Locked(func(tx dirstore.Tx) error {
		return d.each(tx, func(name string, a dirAction) error {
			if !a.Held.Before(t) {
				return nil
			}
			return tx.Delete(name)
		})
	})
}

func (d *Dir) each(tx dirstore.Tx, fn func(name string, a dirAction) error) error {
	names, err := tx.Names()
	if err != nil {
		return err
	}
	for _, n := range names {
		var a dirAction
		if err := tx.Get(n, &a); err != nil {
			return err
		}
		if err := fn(n, a); err != nil {
			return err
		}
	}
	return nil
}
