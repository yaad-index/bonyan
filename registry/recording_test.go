package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/trust"
)

func fileRecording(t *testing.T, dir string) *registry.SlotConfig {
	t.Helper()
	opts, err := json.Marshal(map[string]any{"dir": dir})
	require.NoError(t, err)
	return &registry.SlotConfig{Impl: registry.SinkFile, Options: opts}
}

func onlyFile(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var files []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".jsonl" {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	require.Len(t, files, 1)
	return files[0]
}

// A configured recording receives the chat and classifier calls and the
// wrappers' events, scrubbed with the assembled resolver's scrubber.
func TestConfiguredRecordingReceivesCallsAndEvents(t *testing.T) {
	t.Setenv("BONYAN_TEST_REGISTRY_REC", "rec-secret-31")
	dir := t.TempDir()
	c, err := newRegistry(t).Assemble(registry.Config{
		Chat:       registry.SlotConfig{Impl: "basic"},
		Classifier: &registry.SlotConfig{Impl: "basic"},
		Recording:  fileRecording(t, dir),
	})
	require.NoError(t, err)
	require.NotNil(t, c.Recorder)

	ctx := context.Background()
	_, err = c.Secrets.Scope("BONYAN_TEST_REGISTRY_REC").Resolve(ctx, "BONYAN_TEST_REGISTRY_REC")
	require.NoError(t, err)
	_, err = c.Chat.Chat(ctx, model.ChatRequest{Messages: []model.Message{
		{Role: model.RoleUser, Parts: []content.Text{content.From(content.Provenance{Kind: content.KindUser}, "my key is rec-secret-31")}},
	}})
	require.NoError(t, err)
	_, err = c.Classifier.Classify(ctx, content.From(content.Provenance{Kind: content.KindFetched}, "hello"))
	require.NoError(t, err)
	_, err = c.Trust.Classify(ctx, content.Provenance{Kind: content.KindTool})
	require.NoError(t, err)
	require.NoError(t, c.Close())

	raw, err := os.ReadFile(onlyFile(t, dir))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "rec-secret-31")
	assert.Contains(t, string(raw), "my key is [REDACTED]")
	assert.Contains(t, string(raw), `"event":{"slot":"trust","name":"default","source":"tool","decision":"untrusted"}`)

	f, err := os.Open(onlyFile(t, dir))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	_, calls, err := record.Read(f)
	require.NoError(t, err)
	require.Len(t, calls, 2)
	assert.Equal(t, record.KindChat, calls[0].Kind)
	assert.Equal(t, "basic", calls[0].Model)
	assert.Equal(t, record.KindClassify, calls[1].Kind)
}

func TestRecordingSatisfiesTheRecorderRequirement(t *testing.T) {
	r := newRegistry(t)
	require.NoError(t, r.RegisterPolicy("program", func(json.RawMessage) (trust.Policy, error) { return trust.Default{}, nil }))
	c, err := r.Assemble(registry.Config{
		Chat:      registry.SlotConfig{Impl: "basic"},
		Trust:     &registry.SlotConfig{Impl: "program"},
		Recording: fileRecording(t, t.TempDir()),
	})
	require.NoError(t, err)
	require.NoError(t, c.Close())
}

func TestSinkOwnership(t *testing.T) {
	sink := &closeCounter{}
	c, err := newRegistry(t).Assemble(registry.Config{Chat: registry.SlotConfig{Impl: "basic"}}, registry.WithSink(sink))
	require.NoError(t, err)
	require.NoError(t, c.Close())
	assert.Zero(t, sink.closed, "a sink passed with WithSink is the program's to close")

	r := newRegistry(t)
	owned := &closeCounter{}
	require.NoError(t, r.RegisterSink("counted", func(json.RawMessage) (record.Sink, error) { return owned, nil }))
	c, err = r.Assemble(registry.Config{Chat: registry.SlotConfig{Impl: "basic"}, Recording: &registry.SlotConfig{Impl: "counted"}})
	require.NoError(t, err)
	require.NoError(t, c.Close())
	assert.Equal(t, 1, owned.closed, "a sink the registry opened is closed by Close")

	c, err = newRegistry(t).Assemble(registry.Config{Chat: registry.SlotConfig{Impl: "basic"}})
	require.NoError(t, err)
	assert.Nil(t, c.Recorder)
	require.NoError(t, c.Close(), "nothing to close")
}

func TestRecordingConfigurationErrors(t *testing.T) {
	chat := registry.SlotConfig{Impl: "basic"}

	_, err := newRegistry(t).Assemble(registry.Config{Chat: chat, Recording: fileRecording(t, t.TempDir())}, registry.WithSink(&events{}))
	require.Error(t, err, "a configured recording and a WithSink sink at once")

	_, err = newRegistry(t).Assemble(registry.Config{Chat: chat, Recording: &registry.SlotConfig{Impl: "tape"}})
	require.ErrorIs(t, err, registry.ErrUnknown)
	assert.Contains(t, err.Error(), `recording "tape" (registered: [file])`)

	// A full recording with no subject is refused by the file sink.
	opts, err := json.Marshal(map[string]any{"dir": t.TempDir(), "full": true})
	require.NoError(t, err)
	_, err = newRegistry(t).Assemble(registry.Config{Chat: chat, Recording: &registry.SlotConfig{Impl: registry.SinkFile, Options: opts}})
	require.Error(t, err)

	// A sink the recorder refuses is closed again, not leaked.
	r := newRegistry(t)
	liar := &closeCounter{full: true}
	require.NoError(t, r.RegisterSink("liar", func(json.RawMessage) (record.Sink, error) { return liar, nil }))
	_, err = r.Assemble(registry.Config{Chat: chat, Recording: &registry.SlotConfig{Impl: "liar"}})
	require.Error(t, err)
	assert.Equal(t, 1, liar.closed)
}

// A configuration that fails to assemble opens no recording.
func TestFailedAssemblyLeavesNoRecording(t *testing.T) {
	dir := t.TempDir()
	r := newRegistry(t)
	require.NoError(t, r.RegisterClassifier("broken", func(json.RawMessage) (model.Classifier, error) {
		return nil, errors.New("bad options")
	}))
	_, err := r.Assemble(registry.Config{
		Chat:       registry.SlotConfig{Impl: "basic"},
		Classifier: &registry.SlotConfig{Impl: "broken"},
		Recording:  fileRecording(t, dir),
	})
	require.Error(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

type closeCounter struct {
	events
	full   bool
	closed int
}

func (c *closeCounter) Full() bool { return c.full }
func (c *closeCounter) Close() error {
	c.closed++
	return nil
}
