//go:build unix

package runstore

import (
	"context"
	"errors"
	"time"

	"github.com/yaad-index/bonyan/internal/dirstore"
)

// Dir is a Store in an owner-only directory, one file per run, that survives
// a restart and that several processes on one machine can share: every
// operation holds a lock on the directory, and every write is synced before
// it is reported done, so a run marked proceeding stays marked through a
// crash. It is for a local filesystem only: its lock is not reliable on a
// network mount. It is built on unix systems only.
type Dir struct {
	d *dirstore.Dir
}

// OpenDir opens the store in the directory at path, creating it owner-only.
func OpenDir(path string) (*Dir, error) {
	d, err := dirstore.Open(path)
	if err != nil {
		return nil, err
	}
	return &Dir{d: d}, nil
}

type dirEntry struct {
	State      State  `json:"state"`
	Claim      string `json:"claim,omitempty"`
	Proceeding bool   `json:"proceeding,omitempty"`
}

func get(tx dirstore.Tx, run string) (dirEntry, bool, error) {
	var e dirEntry
	err := tx.Get(run, &e)
	if errors.Is(err, dirstore.ErrNotFound) {
		return e, false, nil
	}
	return e, err == nil, err
}

// Save keeps s unclaimed.
func (d *Dir) Save(_ context.Context, s State, token string) error {
	if s.Run == "" {
		return errors.New("runstore: empty run")
	}
	return d.d.Locked(func(tx dirstore.Tx) error {
		e, ok, err := get(tx, s.Run)
		if err != nil {
			return err
		}
		if ok && e.Claim != token {
			return ErrClaimed
		}
		return tx.Put(s.Run, dirEntry{State: s})
	})
}

// Claim takes the run under token.
func (d *Dir) Claim(_ context.Context, run, token string, now time.Time) (State, error) {
	if token == "" {
		return State{}, errors.New("runstore: empty token")
	}
	var out State
	err := d.d.Locked(func(tx dirstore.Tx) error {
		e, ok, err := get(tx, run)
		switch {
		case err != nil:
			return err
		case !ok:
			return ErrUnknown
		case e.Proceeding:
			return ErrProceeding
		case e.Claim != "" && now.Before(e.State.Deadline):
			return ErrClaimed
		}
		e.Claim = token
		out = e.State
		return tx.Put(run, e)
	})
	return out, err
}

// Proceed marks the run's action proceeding.
func (d *Dir) Proceed(_ context.Context, run, token string) error {
	return d.d.Locked(func(tx dirstore.Tx) error {
		e, ok, err := get(tx, run)
		switch {
		case err != nil:
			return err
		case !ok:
			return ErrUnknown
		case e.Claim != token:
			return ErrClaimed
		case e.Proceeding:
			return ErrProceeding
		}
		e.Proceeding = true
		return tx.Put(run, e)
	})
}

// Delete removes the run when token is its claim.
func (d *Dir) Delete(_ context.Context, run, token string) error {
	return d.d.Locked(func(tx dirstore.Tx) error {
		e, ok, err := get(tx, run)
		if err != nil || !ok {
			return err
		}
		if e.Claim != token {
			return ErrClaimed
		}
		return tx.Delete(run)
	})
}

// List returns the runs held.
func (d *Dir) List(context.Context) ([]State, error) {
	var out []State
	err := d.d.Locked(func(tx dirstore.Tx) error {
		return d.each(tx, func(_ string, e dirEntry) error {
			out = append(out, e.State)
			return nil
		})
	})
	return out, err
}

// DeleteSubject removes every run of subject.
func (d *Dir) DeleteSubject(_ context.Context, subject string) error {
	return d.d.Locked(func(tx dirstore.Tx) error {
		return d.each(tx, func(name string, e dirEntry) error {
			if e.State.Subject != subject {
				return nil
			}
			return tx.Delete(name)
		})
	})
}

// DeleteBefore removes every run saved before t.
func (d *Dir) DeleteBefore(_ context.Context, t time.Time) error {
	return d.d.Locked(func(tx dirstore.Tx) error {
		return d.each(tx, func(name string, e dirEntry) error {
			if !e.State.Saved.Before(t) {
				return nil
			}
			return tx.Delete(name)
		})
	})
}

func (d *Dir) each(tx dirstore.Tx, fn func(name string, e dirEntry) error) error {
	names, err := tx.Names()
	if err != nil {
		return err
	}
	for _, n := range names {
		var e dirEntry
		if err := tx.Get(n, &e); err != nil {
			return err
		}
		if err := fn(n, e); err != nil {
			return err
		}
	}
	return nil
}
