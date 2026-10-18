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
		Memory: &registry.MemoryConfig{SlotConfig: registry.SlotConfig{Impl: impl}, Namespace: "test", Retention: retention},
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
	require.NoError(t, c.Memory.Remember(ctx, "ana", content.Provenance{Kind: content.KindUser}, "prefers mail"))
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
	b := &closing{Backend: inmem.New("test")}
	require.NoError(t, r.RegisterMemory("mine", func(string, json.RawMessage) (memory.Backend, error) { return b, nil }))
	assert.Contains(t, r.Names(registry.SlotMemory), "mine")
	assert.Contains(t, r.Names(registry.SlotMemory), registry.MemoryInMem)

	c, err := r.Assemble(memoryConfig("mine", "1h"))
	require.NoError(t, err)
	require.NoError(t, c.Memory.Remember(context.Background(), "ana", content.Provenance{Kind: content.KindUser}, "a fact"))
	recs, err := b.Recall(context.Background(), "test/ana", "", 10, time.Time{})
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
	rec.Event(context.Background(), record.Event{Slot: "test", Name: "x"})
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

// The assembled store scrubs what it writes with the program's own resolver,
// the one the agent and its hooks scrub with.
func TestTheAssembledMemoryScrubsWithTheProgramsSecrets(t *testing.T) {
	t.Setenv("BONYAN_REGISTRY_MEMORY_SECRET", "SECRET-b52")
	c, err := newRegistry(t).Assemble(memoryConfig(registry.MemoryInMem, "720h"), registry.WithSink(&events{}))
	require.NoError(t, err)
	ctx := context.Background()
	_, err = c.Secrets.Scope("BONYAN_REGISTRY_MEMORY_SECRET").Resolve(ctx, "BONYAN_REGISTRY_MEMORY_SECRET")
	require.NoError(t, err)
	require.NoError(t, c.Memory.Remember(ctx, "ana", content.Provenance{Kind: content.KindUser}, "her key is SECRET-b52"))
	got, err := c.Memory.Recall(ctx, "ana", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.NotContains(t, got[0].(content.Untrusted).Raw(), "SECRET-b52")
}

// Memory needs a namespace, and the backend must be opened with it.
func TestMemoryNeedsANamespace(t *testing.T) {
	r := newRegistry(t)
	called := false
	require.NoError(t, r.RegisterMemory("counted", func(ns string, _ json.RawMessage) (memory.Backend, error) { called = true; return inmem.New(ns), nil }))
	cfg := memoryConfig("counted", "1h")
	cfg.Memory.Namespace = ""
	_, err := r.Assemble(cfg)
	require.ErrorContains(t, err, "namespace")
	assert.False(t, called, "no backend is opened without a namespace")

	r = newRegistry(t)
	elsewhere := &closing{Backend: inmem.New("other")}
	require.NoError(t, r.RegisterMemory("elsewhere", func(string, json.RawMessage) (memory.Backend, error) { return elsewhere, nil }))
	_, err = r.Assemble(memoryConfig("elsewhere", "1h"))
	require.ErrorContains(t, err, `opened with namespace "other", not "test"`)
	assert.True(t, elsewhere.closed, "the refused backend is closed")

	r = newRegistry(t)
	var got string
	require.NoError(t, r.RegisterMemory("seen", func(ns string, _ json.RawMessage) (memory.Backend, error) { got = ns; return inmem.New(ns), nil }))
	_, err = r.Assemble(memoryConfig("seen", "1h"))
	require.NoError(t, err)
	assert.Equal(t, "test", got, "the factory is given the configured namespace")
}

// A backend registered with secrets resolves the names the memory
// configuration grants, through the program's resolver, so what it resolves is
// scrubbed like any other secret; a name outside the grant is refused.
func TestASecretMemoryBackendResolvesItsGrant(t *testing.T) {
	t.Setenv("BONYAN_REGISTRY_MEMORY_TOKEN", "TOKEN-7f3")
	t.Setenv("BONYAN_REGISTRY_MEMORY_OTHER", "OTHER-7f3")
	r := newRegistry(t)
	var got *secret.Scoped
	require.NoError(t, r.RegisterMemoryWithSecrets("tokened", func(ns string, _ json.RawMessage, s *secret.Scoped) (memory.Backend, error) {
		got = s
		return inmem.New(ns), nil
	}))
	cfg := memoryConfig("tokened", "1h")
	cfg.Memory.Secrets = []string{"BONYAN_REGISTRY_MEMORY_TOKEN"}
	c, err := r.Assemble(cfg)
	require.NoError(t, err)
	require.NotNil(t, got)

	ctx := context.Background()
	v, err := got.Resolve(ctx, "BONYAN_REGISTRY_MEMORY_TOKEN")
	require.NoError(t, err)
	assert.Equal(t, "TOKEN-7f3", v.Reveal())
	assert.Equal(t, "the token is "+secret.Redacted, c.Secrets.Scrubber().Scrub("the token is TOKEN-7f3"))

	_, err = got.Resolve(ctx, "BONYAN_REGISTRY_MEMORY_OTHER")
	require.ErrorIs(t, err, secret.ErrNotGranted)
}

// With no grant configured, a backend registered with secrets can resolve
// nothing.
func TestASecretMemoryBackendWithoutAGrantResolvesNothing(t *testing.T) {
	t.Setenv("BONYAN_REGISTRY_MEMORY_TOKEN", "TOKEN-7f3")
	r := newRegistry(t)
	var got *secret.Scoped
	require.NoError(t, r.RegisterMemoryWithSecrets("tokened", func(ns string, _ json.RawMessage, s *secret.Scoped) (memory.Backend, error) {
		got = s
		return inmem.New(ns), nil
	}))
	_, err := r.Assemble(memoryConfig("tokened", "1h"))
	require.NoError(t, err)
	_, err = got.Resolve(context.Background(), "BONYAN_REGISTRY_MEMORY_TOKEN")
	require.ErrorIs(t, err, secret.ErrNotGranted)
}

// A memory implementation name is taken once, whichever way it was registered.
func TestMemoryNamesAreSharedByBothRegistrations(t *testing.T) {
	r := newRegistry(t)
	require.NoError(t, r.RegisterMemory("taken", func(ns string, _ json.RawMessage) (memory.Backend, error) { return inmem.New(ns), nil }))
	err := r.RegisterMemoryWithSecrets("taken", func(ns string, _ json.RawMessage, _ *secret.Scoped) (memory.Backend, error) {
		return inmem.New(ns), nil
	})
	require.ErrorIs(t, err, registry.ErrDuplicate)
	require.Error(t, r.RegisterMemoryWithSecrets("nil", nil))
	require.Error(t, r.RegisterMemory("nil", nil))
}
