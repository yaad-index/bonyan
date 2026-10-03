package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yaad-index/bonyan/approval"
	"github.com/yaad-index/bonyan/runstore"
)

// The names the built-in approval stores and run stores are registered under
// (ADR 0001 §7). "inmem" keeps nothing across a restart. "dir", on unix
// systems, keeps everything in an owner-only directory on a local
// filesystem and takes the option {"path": "<directory>"}.
const (
	ApprovalsInMem = "inmem"
	ApprovalsDir   = "dir"
	RunStoreInMem  = "inmem"
	RunStoreDir    = "dir"
)

// RunStoreConfig selects the run store and how long it keeps a saved run.
type RunStoreConfig struct {
	SlotConfig
	// Retention is how long a saved run is kept, as a Go duration such as
	// "720h". It is required.
	Retention string `json:"retention"`
}

// RunStore is the configured run store with its retention period.
type RunStore struct {
	runstore.Store
	retention time.Duration
	now       func() time.Time
}

// Purge removes every run saved longer ago than the retention period.
func (s *RunStore) Purge(ctx context.Context) error {
	return s.DeleteBefore(ctx, s.now().Add(-s.retention))
}

// RegisterApprovals registers an approval store implementation under name.
func (r *Registry) RegisterApprovals(name string, f Factory[approval.Store]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.approvals.register(name, f)
}

// RegisterRunStore registers a run store implementation under name.
func (r *Registry) RegisterRunStore(name string, f Factory[runstore.Store]) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runStores.register(name, f)
}

// registerStores registers the in-process stores, and the directory stores
// where they are built.
func registerStores(r *Registry) {
	r.approvals.factories[ApprovalsInMem] = func(options json.RawMessage) (approval.Store, error) {
		return approval.NewMemory(), decode(options, &struct{}{})
	}
	r.runStores.factories[RunStoreInMem] = func(options json.RawMessage) (runstore.Store, error) {
		return runstore.NewMemory(), decode(options, &struct{}{})
	}
	registerDirStores(r)
}

var errMissingPath = errors.New(`options need a "path"`)

// decode reads options into v, refusing a field v does not have.
func decode(options json.RawMessage, v any) error {
	if len(options) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(options))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// pathOption reads the options {"path": "<directory>"}.
func pathOption(options json.RawMessage) (string, error) {
	var o struct {
		Path string `json:"path"`
	}
	if err := decode(options, &o); err != nil {
		return "", err
	}
	if o.Path == "" {
		return "", errMissingPath
	}
	return o.Path, nil
}

// assembleStores builds the approval store and the run store, and has
// deleting a subject from memory delete the subject's saved runs.
func (r *Registry) assembleStores(cfg Config, out *Components) error {
	if cfg.RunStore != nil && cfg.Approvals == nil {
		return errors.New("registry: a run store needs an approval store, to resume the runs it keeps")
	}
	if cfg.Approvals != nil {
		a, err := r.approvals.build(*cfg.Approvals)
		if err != nil {
			return err
		}
		out.Approvals = guardedApprovals{inner: a}
	}
	if cfg.RunStore == nil {
		return nil
	}
	retention, err := time.ParseDuration(cfg.RunStore.Retention)
	if err != nil || retention <= 0 {
		return fmt.Errorf("registry: run store: retention must be a positive duration, got %q", cfg.RunStore.Retention)
	}
	s, err := r.runStores.build(cfg.RunStore.SlotConfig)
	if err != nil {
		return err
	}
	out.RunStore = &RunStore{Store: guardedRunStore{inner: s}, retention: retention, now: time.Now}
	if out.Memory != nil {
		out.Memory.OnDeleteSubject("suspended runs", out.RunStore.DeleteSubject)
	}
	return nil
}

// guardedApprovals is bonyan's wrapper around a configured approval store.
type guardedApprovals struct{ inner approval.Store }

func (g guardedApprovals) Hold(ctx context.Context, p approval.Pending) (<-chan bool, error) {
	return g.inner.Hold(ctx, p)
}

func (g guardedApprovals) Decide(ctx context.Context, id string, approve bool) error {
	return g.inner.Decide(ctx, id, approve)
}

func (g guardedApprovals) Drop(ctx context.Context, id string) error { return g.inner.Drop(ctx, id) }

func (g guardedApprovals) List(ctx context.Context) ([]approval.Pending, error) {
	return g.inner.List(ctx)
}

// guardedRunStore is bonyan's wrapper around a configured run store.
type guardedRunStore struct{ inner runstore.Store }

func (g guardedRunStore) Save(ctx context.Context, s runstore.State, token string) error {
	return g.inner.Save(ctx, s, token)
}

func (g guardedRunStore) Claim(ctx context.Context, run, token string, now time.Time) (runstore.State, error) {
	return g.inner.Claim(ctx, run, token, now)
}

func (g guardedRunStore) Proceed(ctx context.Context, run, token string) error {
	return g.inner.Proceed(ctx, run, token)
}

func (g guardedRunStore) Delete(ctx context.Context, run, token string) error {
	return g.inner.Delete(ctx, run, token)
}

func (g guardedRunStore) List(ctx context.Context) ([]runstore.State, error) {
	return g.inner.List(ctx)
}

func (g guardedRunStore) DeleteSubject(ctx context.Context, subject string) error {
	return g.inner.DeleteSubject(ctx, subject)
}

func (g guardedRunStore) DeleteBefore(ctx context.Context, t time.Time) error {
	return g.inner.DeleteBefore(ctx, t)
}
