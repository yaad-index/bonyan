package registry_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/approval"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/runstore"
)

func storesConfig(approvals, runs registry.SlotConfig, retention string) registry.Config {
	return registry.Config{
		Chat:      registry.SlotConfig{Impl: "basic"},
		Approvals: &approvals,
		RunStore:  &registry.RunStoreConfig{SlotConfig: runs, Retention: retention},
	}
}

func saved(run, subject string, at time.Time) runstore.State {
	return runstore.State{Run: run, Subject: subject, Saved: at, Deadline: at.Add(time.Hour), Data: []byte("x")}
}

func runsHeld(t *testing.T, s runstore.Store) []string {
	t.Helper()
	list, err := s.List(context.Background())
	require.NoError(t, err)
	var out []string
	for _, st := range list {
		out = append(out, st.Run)
	}
	return out
}

// The approval store and the run store are slots: configured by name, they
// are assembled and work.
func TestTheStoresAreAssembled(t *testing.T) {
	inmem := registry.SlotConfig{Impl: registry.RunStoreInMem}
	c, err := newRegistry(t).Assemble(storesConfig(registry.SlotConfig{Impl: registry.ApprovalsInMem}, inmem, "720h"))
	require.NoError(t, err)
	require.NotNil(t, c.Approvals)
	require.NotNil(t, c.RunStore)
	ctx := context.Background()
	_, err = c.Approvals.Hold(ctx, approval.Pending{ID: "a1", Tool: "search"})
	require.NoError(t, err)
	require.NoError(t, c.RunStore.Save(ctx, saved("r1", "ana", time.Now()), ""))
	assert.Equal(t, []string{"r1"}, runsHeld(t, c.RunStore))

	none, err := newRegistry(t).Assemble(registry.Config{Chat: registry.SlotConfig{Impl: "basic"}})
	require.NoError(t, err)
	assert.Nil(t, none.Approvals)
	assert.Nil(t, none.RunStore)
}

func TestTheStoresAreChecked(t *testing.T) {
	inmem := registry.SlotConfig{Impl: registry.RunStoreInMem}
	approvals := registry.SlotConfig{Impl: registry.ApprovalsInMem}
	for name, cfg := range map[string]registry.Config{
		"a run store with no approval store": {Chat: registry.SlotConfig{Impl: "basic"}, RunStore: &registry.RunStoreConfig{SlotConfig: inmem, Retention: "720h"}},
		"no retention":                       storesConfig(approvals, inmem, ""),
		"a retention that is not positive":   storesConfig(approvals, inmem, "0s"),
		"an unknown run store":               storesConfig(approvals, registry.SlotConfig{Impl: "missing"}, "720h"),
		"an unknown approval store":          storesConfig(registry.SlotConfig{Impl: "missing"}, inmem, "720h"),
		"an option the store does not have":  storesConfig(approvals, registry.SlotConfig{Impl: registry.RunStoreInMem, Options: json.RawMessage(`{"path":"x"}`)}, "720h"),
	} {
		_, err := newRegistry(t).Assemble(cfg)
		require.Error(t, err, name)
	}
}

// Purge removes the runs saved longer ago than the retention period.
func TestARunStorePurgesWhatRetentionExpired(t *testing.T) {
	c, err := newRegistry(t).Assemble(storesConfig(registry.SlotConfig{Impl: registry.ApprovalsInMem}, registry.SlotConfig{Impl: registry.RunStoreInMem}, "1h"))
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, c.RunStore.Save(ctx, saved("old", "ana", time.Now().Add(-2*time.Hour)), ""))
	require.NoError(t, c.RunStore.Save(ctx, saved("new", "ana", time.Now()), ""))
	require.NoError(t, c.RunStore.Purge(ctx))
	assert.Equal(t, []string{"new"}, runsHeld(t, c.RunStore))
}

// Deleting a subject from memory deletes the subject's saved runs.
func TestDeletingASubjectDeletesItsSavedRuns(t *testing.T) {
	cfg := storesConfig(registry.SlotConfig{Impl: registry.ApprovalsInMem}, registry.SlotConfig{Impl: registry.RunStoreInMem}, "720h")
	cfg.Memory = &registry.MemoryConfig{SlotConfig: registry.SlotConfig{Impl: registry.MemoryInMem}, Retention: "720h"}
	c, err := newRegistry(t).Assemble(cfg)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, c.RunStore.Save(ctx, saved("r1", "ana", time.Now()), ""))
	require.NoError(t, c.RunStore.Save(ctx, saved("r2", "bo", time.Now()), ""))
	require.NoError(t, c.Memory.DeleteSubject(ctx, "ana"))
	assert.Equal(t, []string{"r2"}, runsHeld(t, c.RunStore))
}

func TestTheStoreSlotsAreNamed(t *testing.T) {
	r := newRegistry(t)
	assert.Contains(t, r.Names(registry.SlotApprovals), registry.ApprovalsInMem)
	assert.Contains(t, r.Names(registry.SlotRunStore), registry.RunStoreInMem)
	require.NoError(t, r.RegisterRunStore("mine", func(json.RawMessage) (runstore.Store, error) { return runstore.NewMemory(), nil }))
	require.NoError(t, r.RegisterApprovals("mine", func(json.RawMessage) (approval.Store, error) { return approval.NewMemory(), nil }))
	assert.Contains(t, r.Names(registry.SlotRunStore), "mine")
	assert.Contains(t, r.Names(registry.SlotApprovals), "mine")
	assert.NotEmpty(t, r.Names(registry.SlotQueue))
}
