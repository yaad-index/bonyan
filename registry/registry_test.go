package registry_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/secret"
)

// fakeChat answers every request with its own name, so a test can tell which
// implementation served a call.
type fakeChat struct{ name string }

func (f fakeChat) Chat(context.Context, model.ChatRequest) (model.ChatResponse, error) {
	return model.ChatResponse{Content: f.name, StopReason: model.StopEnd}, nil
}

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, inputs []string) (model.EmbedResponse, error) {
	return model.EmbedResponse{Vectors: make([][]float32, len(inputs))}, nil
}

type fakeClassifier struct{}

func (fakeClassifier) Classify(context.Context, content.Untrusted) (model.ClassifyResponse, error) {
	return model.ClassifyResponse{Labels: []model.Label{{Name: "ok", Confidence: 1}}}, nil
}

func chatFactory(name string) registry.Factory[model.Chat] {
	return func(json.RawMessage) (model.Chat, error) { return fakeChat{name: name}, nil }
}

func newRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	r := registry.New()
	require.NoError(t, r.RegisterChat("basic", chatFactory("basic")))
	require.NoError(t, r.RegisterEmbedder("basic", func(json.RawMessage) (model.Embedder, error) {
		return fakeEmbedder{}, nil
	}))
	require.NoError(t, r.RegisterClassifier("basic", func(json.RawMessage) (model.Classifier, error) {
		return fakeClassifier{}, nil
	}))
	return r
}

func TestUnknownImplementationFailsAssembly(t *testing.T) {
	r := newRegistry(t)
	require.NoError(t, r.RegisterChat("other", chatFactory("other")))

	_, err := r.Assemble(registry.Config{Chat: registry.SlotConfig{Impl: "missing"}})
	require.ErrorIs(t, err, registry.ErrUnknown)
	assert.Contains(t, err.Error(), `chat "missing"`)
	assert.Contains(t, err.Error(), "[basic other]", "the error lists what is registered")

	_, err = r.Assemble(registry.Config{
		Chat:       registry.SlotConfig{Impl: "basic"},
		Classifier: &registry.SlotConfig{Impl: "missing"},
	})
	require.ErrorIs(t, err, registry.ErrUnknown)
	assert.Contains(t, err.Error(), `classifier "missing"`)
}

func TestProgramImplementationIsSelectableByName(t *testing.T) {
	r := newRegistry(t)
	require.NoError(t, r.RegisterChat("program", chatFactory("program")))

	for _, name := range []string{"basic", "program"} {
		c, err := r.Assemble(registry.Config{Chat: registry.SlotConfig{Impl: name}})
		require.NoError(t, err)
		resp, err := c.Chat.Chat(context.Background(), model.ChatRequest{})
		require.NoError(t, err)
		assert.Equal(t, name, resp.Content)
	}
}

func TestOptionsReachTheFactory(t *testing.T) {
	r := registry.New()
	var got json.RawMessage
	require.NoError(t, r.RegisterChat("opts", func(o json.RawMessage) (model.Chat, error) {
		got = o
		return fakeChat{}, nil
	}))

	var cfg registry.Config
	require.NoError(t, json.Unmarshal([]byte(`{"chat":{"impl":"opts","options":{"base":"x","n":2}}}`), &cfg))
	_, err := r.Assemble(cfg)
	require.NoError(t, err)
	assert.JSONEq(t, `{"base":"x","n":2}`, string(got))
}

func TestDuplicateNameFails(t *testing.T) {
	r := newRegistry(t)
	err := r.RegisterChat("basic", chatFactory("replacement"))
	require.ErrorIs(t, err, registry.ErrDuplicate)

	// The original is still the one assembled.
	c, err := r.Assemble(registry.Config{Chat: registry.SlotConfig{Impl: "basic"}})
	require.NoError(t, err)
	resp, err := c.Chat.Chat(context.Background(), model.ChatRequest{})
	require.NoError(t, err)
	assert.Equal(t, "basic", resp.Content)
}

func TestInvalidRegistrationFails(t *testing.T) {
	r := registry.New()
	require.Error(t, r.RegisterChat("", chatFactory("x")))
	require.Error(t, r.RegisterChat("nil", nil))
	assert.Empty(t, r.Names(registry.SlotChat))
}

func TestFactoryErrorNamesTheSlot(t *testing.T) {
	r := newRegistry(t)
	boom := errors.New("bad options")
	require.NoError(t, r.RegisterEmbedder("broken", func(json.RawMessage) (model.Embedder, error) {
		return nil, boom
	}))

	_, err := r.Assemble(registry.Config{
		Chat:     registry.SlotConfig{Impl: "basic"},
		Embedder: &registry.SlotConfig{Impl: "broken"},
	})
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), `embedder "broken"`)
}

func TestUnconfiguredOptionalSlotsAreNil(t *testing.T) {
	c, err := newRegistry(t).Assemble(registry.Config{Chat: registry.SlotConfig{Impl: "basic"}})
	require.NoError(t, err)
	assert.NotNil(t, c.Chat)
	assert.Nil(t, c.Embedder)
	assert.Nil(t, c.Classifier)
}

// Every assembled part is bonyan's wrapper, never the registered
// implementation: its concrete type is declared in the registry package, or,
// for Secrets and Recorder, is the secret or record package's own type around
// the sources or the sink. The calls still reach the implementation through it.
func TestEveryPartIsWrapped(t *testing.T) {
	c, err := newRegistry(t).Assemble(registry.Config{
		Chat:       registry.SlotConfig{Impl: "basic"},
		Embedder:   &registry.SlotConfig{Impl: "basic"},
		Classifier: &registry.SlotConfig{Impl: "basic"},
	}, registry.WithSink(&events{}))
	require.NoError(t, err)

	registryPkg := reflect.TypeOf(registry.Registry{}).PkgPath()
	wrapperPkg := map[string]string{
		"Secrets":  reflect.TypeOf(secret.Resolver{}).PkgPath(),
		"Recorder": reflect.TypeOf(record.Recorder{}).PkgPath(),
	}
	parts := reflect.ValueOf(c)
	for i := 0; i < parts.NumField(); i++ {
		if !parts.Type().Field(i).IsExported() {
			continue
		}
		field := parts.Type().Field(i).Name
		v := parts.Field(i)
		require.False(t, v.IsNil(), "%s not assembled", field)
		want := cmp.Or(wrapperPkg[field], registryPkg)
		assert.Equal(t, want, v.Elem().Type().PkgPath(), "%s is not bonyan's wrapper", field)
	}

	ctx := context.Background()
	emb, err := c.Embedder.Embed(ctx, []string{"a", "b"})
	require.NoError(t, err)
	assert.Len(t, emb.Vectors, 2)
	cls, err := c.Classifier.Classify(ctx, content.From(content.Provenance{Kind: content.KindUser}, "x"))
	require.NoError(t, err)
	assert.Equal(t, "ok", cls.Labels[0].Name)
}

func TestNamesAreSorted(t *testing.T) {
	r := registry.New()
	for _, n := range []string{"c", "a", "b"} {
		require.NoError(t, r.RegisterChat(n, chatFactory(n)))
	}
	assert.Equal(t, []string{"a", "b", "c"}, r.Names(registry.SlotChat))
	assert.Empty(t, r.Names(registry.SlotEmbedder))
	assert.Nil(t, r.Names("no-such-slot"))
}

func TestFactoryReturningNoImplementationFails(t *testing.T) {
	r := newRegistry(t)
	require.NoError(t, r.RegisterChat("nil", func(json.RawMessage) (model.Chat, error) { return nil, nil }))
	require.NoError(t, r.RegisterChat("nil-pointer", func(json.RawMessage) (model.Chat, error) {
		var c *fakeChatPtr
		return c, nil
	}))
	require.NoError(t, r.RegisterClassifier("nil", func(json.RawMessage) (model.Classifier, error) { return nil, nil }))

	for _, name := range []string{"nil", "nil-pointer"} {
		_, err := r.Assemble(registry.Config{Chat: registry.SlotConfig{Impl: name}})
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), fmt.Sprintf(`chat %q: factory returned no implementation`, name))
	}
	_, err := r.Assemble(registry.Config{
		Chat:       registry.SlotConfig{Impl: "basic"},
		Classifier: &registry.SlotConfig{Impl: "nil"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `classifier "nil": factory returned no implementation`)
}

type fakeChatPtr struct{}

func (*fakeChatPtr) Chat(context.Context, model.ChatRequest) (model.ChatResponse, error) {
	return model.ChatResponse{}, nil
}

func TestSecretSources(t *testing.T) {
	ctx := context.Background()
	chat := registry.SlotConfig{Impl: "basic"}

	t.Setenv("BONYAN_TEST_REGISTRY_SECRET", "from-env")
	c, err := newRegistry(t).Assemble(registry.Config{Chat: chat})
	require.NoError(t, err)
	v, err := c.Secrets.Scope("BONYAN_TEST_REGISTRY_SECRET").Resolve(ctx, "BONYAN_TEST_REGISTRY_SECRET")
	require.NoError(t, err)
	assert.Equal(t, "from-env", v.Reveal(), "with no sources configured, the environment is read")

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "BONYAN_TEST_REGISTRY_SECRET"), []byte("from-dir\n"), 0o600))
	opts, err := json.Marshal(map[string]string{"path": dir})
	require.NoError(t, err)
	c, err = newRegistry(t).Assemble(registry.Config{Chat: chat, Secrets: []registry.SlotConfig{
		{Impl: registry.SecretDir, Options: opts},
		{Impl: registry.SecretEnv},
	}})
	require.NoError(t, err)
	v, err = c.Secrets.Scope("BONYAN_TEST_REGISTRY_SECRET").Resolve(ctx, "BONYAN_TEST_REGISTRY_SECRET")
	require.NoError(t, err)
	assert.Equal(t, "from-dir", v.Reveal(), "configured sources are tried in order")

	r := newRegistry(t)
	require.NoError(t, r.RegisterSecretSource("program", func(json.RawMessage) (secret.Source, error) {
		return secret.Dir{Path: dir}, nil
	}))
	_, err = r.Assemble(registry.Config{Chat: chat, Secrets: []registry.SlotConfig{{Impl: "program"}}})
	require.NoError(t, err)

	_, err = r.Assemble(registry.Config{Chat: chat, Secrets: []registry.SlotConfig{{Impl: registry.SecretDir}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `secret "dir": options need a "path"`)

	_, err = r.Assemble(registry.Config{Chat: chat, Secrets: []registry.SlotConfig{{Impl: "remote-store"}}})
	require.ErrorIs(t, err, registry.ErrUnknown)
	assert.Contains(t, err.Error(), `secret "remote-store" (registered: [dir env program])`)
}
