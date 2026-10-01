package registry_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/inmem"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/trust"
)

func memoryConfig(impl, retention string) registry.Config {
	return registry.Config{
		Chat:   registry.SlotConfig{Impl: "basic"},
		Memory: &registry.MemoryConfig{SlotConfig: registry.SlotConfig{Impl: impl}, Retention: retention},
	}
}

// The assembled store classifies through the assembled policy, so its
// decisions are recorded under the policy's name like any other.
func TestMemoryClassifiesThroughTheAssembledPolicy(t *testing.T) {
	sink := &events{}
	c, err := newRegistry(t).Assemble(memoryConfig(registry.MemoryInMem, "720h"), registry.WithSink(sink))
	require.NoError(t, err)
	require.NotNil(t, c.Memory)

	ctx := context.Background()
	require.NoError(t, c.Memory.Remember(ctx, "ana", content.KindUser, "prefers mail"))
	got, err := c.Memory.Recall(ctx, "ana", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, got[0].Trusted())

	var decisions []string
	for _, e := range sink.all {
		if e.Slot == registry.SlotTrust {
			assert.Equal(t, trust.DefaultName, e.Name)
			assert.Equal(t, string(content.KindMemory), e.Source)
			decisions = append(decisions, e.Decision)
		}
	}
	assert.Equal(t, []string{"untrusted", "untrusted"}, decisions, "classified once when stored and once when read")
}

func TestMemoryNeedsAPositiveRetention(t *testing.T) {
	for _, retention := range []string{"", "0s", "-1h", "a month"} {
		_, err := newRegistry(t).Assemble(memoryConfig(registry.MemoryInMem, retention))
		require.Error(t, err, "retention %q", retention)
	}
}

func TestAnUnknownMemoryBackendFails(t *testing.T) {
	_, err := newRegistry(t).Assemble(memoryConfig("missing", "720h"))
	require.ErrorIs(t, err, registry.ErrUnknown)
}

// closing records whether it was closed.
type closing struct {
	*inmem.Backend
	closed bool
}

func (c *closing) Close() error {
	c.closed = true
	return nil
}

func TestARegisteredMemoryBackendIsUsedAndClosed(t *testing.T) {
	r := newRegistry(t)
	b := &closing{Backend: inmem.New()}
	require.NoError(t, r.RegisterMemory("mine", func(json.RawMessage) (memory.Backend, error) { return b, nil }))
	assert.Contains(t, r.Names(registry.SlotMemory), "mine")
	assert.Contains(t, r.Names(registry.SlotMemory), registry.MemoryInMem)

	c, err := r.Assemble(memoryConfig("mine", "1h"))
	require.NoError(t, err)
	require.NoError(t, c.Memory.Remember(context.Background(), "ana", content.KindUser, "a fact"))
	recs, err := b.Recall(context.Background(), "ana", "", 10, time.Time{})
	require.NoError(t, err)
	assert.Len(t, recs, 1)

	require.NoError(t, c.Close())
	assert.True(t, b.closed)
}

// With file recordings configured, deleting a subject from memory also
// deletes the subject's full recordings.
func TestDeletingASubjectDeletesItsFullRecordings(t *testing.T) {
	dir := t.TempDir()
	full, err := record.OpenFile(record.FileOptions{Dir: dir, Full: true, Subject: "ana"})
	require.NoError(t, err)
	rec, err := record.NewRecorder(full, secret.NewScrubber())
	require.NoError(t, err)
	rec.Event(record.Event{Slot: "test", Name: "x"})
	require.NoError(t, full.Close())
	var recordings []string
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			recordings = append(recordings, path)
		}
		return err
	}))
	require.Len(t, recordings, 1, "the positive control: the full recording exists")

	cfg := memoryConfig(registry.MemoryInMem, "720h")
	opts, _ := json.Marshal(map[string]any{"dir": dir})
	cfg.Recording = &registry.SlotConfig{Impl: registry.SinkFile, Options: opts}
	c, err := newRegistry(t).Assemble(cfg)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.Memory.DeleteSubject(context.Background(), "ana"))
	_, err = os.Stat(recordings[0])
	assert.True(t, os.IsNotExist(err), "the full recording is gone")
}
