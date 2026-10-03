//go:build unix

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
)

// The directory stores keep what they hold across a new assembly, as across
// a restart, and need a path.
func TestTheDirectoryStoresSurviveReassembly(t *testing.T) {
	dir := t.TempDir()
	cfg := storesConfig(
		registry.SlotConfig{Impl: registry.ApprovalsDir, Options: json.RawMessage(`{"path":"` + dir + `/approvals"}`)},
		registry.SlotConfig{Impl: registry.RunStoreDir, Options: json.RawMessage(`{"path":"` + dir + `/runs"}`)},
		"720h")
	c, err := newRegistry(t).Assemble(cfg)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, c.RunStore.Save(ctx, saved("r1", "ana", time.Now()), ""))
	_, err = c.Approvals.Hold(ctx, approval.Pending{ID: "a1", Tool: "search", Approvers: []string{"a"}})
	require.NoError(t, err)

	again, err := newRegistry(t).Assemble(cfg)
	require.NoError(t, err)
	assert.Equal(t, []string{"r1"}, runsHeld(t, again.RunStore))
	held, err := again.Approvals.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []approval.Pending{{ID: "a1", Tool: "search", Approvers: []string{"a"}}}, held)

	for _, impl := range []string{registry.ApprovalsDir, registry.RunStoreDir} {
		cfg := storesConfig(registry.SlotConfig{Impl: registry.ApprovalsInMem}, registry.SlotConfig{Impl: registry.RunStoreInMem}, "720h")
		if impl == registry.ApprovalsDir {
			cfg.Approvals = &registry.SlotConfig{Impl: impl}
		} else {
			cfg.RunStore.SlotConfig = registry.SlotConfig{Impl: impl}
		}
		_, err := newRegistry(t).Assemble(cfg)
		require.Error(t, err, "%s without a path", impl)
	}
}
