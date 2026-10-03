//go:build unix

// Package dirstore is a directory of small JSON files that several processes
// share safely: every operation runs under an exclusive lock on a file in the
// directory, which the operating system releases when the process holding it
// dies, so a crash never leaves the store locked; and every write goes to a
// temporary file that is synced, renamed into place and followed by a sync of
// the directory, so a crash leaves either the old file or the new one, never
// a part of one and never a write that was reported done and is lost.
//
// The lock is flock(2), which is reliable on local filesystems only: a store
// on a network mount may let two processes in at once.
package dirstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	lockName   = ".lock"
	suffix     = ".json"
	tempPrefix = ".tmp-"
)

// Dir is an opened store.
type Dir struct {
	path string
}

// Open opens the store at path, creating it owner-only if it does not exist.
// A directory others can read or write is refused.
func Open(path string) (*Dir, error) {
	if path == "" {
		return nil, errors.New("dirstore: empty path")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("dirstore: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("dirstore: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("dirstore: %s is open to others (%s); it must be owner-only", path, info.Mode().Perm())
	}
	f, err := os.OpenFile(filepath.Join(path, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("dirstore: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("dirstore: %w", err)
	}
	return &Dir{path: path}, nil
}

// Tx is what an operation does under the lock.
type Tx struct {
	d *Dir
}

// Locked runs fn holding the store's lock, which no other process or
// goroutine using the store holds at the same time. Every write happens
// under the lock, so a temporary file found while holding it was left by a
// writer that crashed before its rename; it can hold a whole record, of a
// subject being deleted for instance, so it is removed before fn runs.
func (d *Dir) Locked(fn func(tx Tx) error) error {
	f, err := os.OpenFile(filepath.Join(d.path, lockName), os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("dirstore: %w", err)
	}
	defer func() { _ = f.Close() }()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("dirstore: lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	if err := d.removeTemps(); err != nil {
		return err
	}
	return fn(Tx{d: d})
}

// removeTemps removes every temporary file a crashed write left.
func (d *Dir) removeTemps() error {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return fmt.Errorf("dirstore: %w", err)
	}
	removed := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			if err := os.Remove(filepath.Join(d.path, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("dirstore: %w", err)
			}
			removed = true
		}
	}
	if removed {
		return d.syncDir()
	}
	return nil
}

// ErrNotFound is a name the store holds no file under.
var ErrNotFound = errors.New("dirstore: not found")

// validName refuses a name that is not one plain file in the store.
func validName(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("dirstore: invalid name %q", name)
	}
	return nil
}

// Get reads the file under name into v.
func (tx Tx) Get(name string, v any) error {
	if err := validName(name); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(tx.d.path, name+suffix))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("dirstore: %w", err)
	}
	return json.Unmarshal(b, v)
}

// Put writes v under name, replacing what was there, durably.
func (tx Tx) Put(name string, v any) error {
	if err := validName(name); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tx.d.path, tempPrefix)
	if err != nil {
		return fmt.Errorf("dirstore: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("dirstore: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("dirstore: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("dirstore: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(tx.d.path, name+suffix)); err != nil {
		return fmt.Errorf("dirstore: %w", err)
	}
	return tx.d.syncDir()
}

// Delete removes the file under name, durably. A name with no file is not an
// error.
func (tx Tx) Delete(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(tx.d.path, name+suffix))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dirstore: %w", err)
	}
	return tx.d.syncDir()
}

// Names lists the names the store holds files under.
func (tx Tx) Names() ([]string, error) {
	entries, err := os.ReadDir(tx.d.path)
	if err != nil {
		return nil, fmt.Errorf("dirstore: %w", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && !strings.HasPrefix(n, ".") && strings.HasSuffix(n, suffix) {
			out = append(out, strings.TrimSuffix(n, suffix))
		}
	}
	return out, nil
}

func (d *Dir) syncDir() error {
	f, err := os.Open(d.path)
	if err != nil {
		return fmt.Errorf("dirstore: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("dirstore: %w", err)
	}
	return nil
}
