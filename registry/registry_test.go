package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/registry"
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

// Every assembled part is the registry's wrapper, never the registered
// implementation: its concrete type is declared in the registry package, and
// the calls still reach the implementation through it.
func TestEveryPartIsWrapped(t *testing.T) {
	c, err := newRegistry(t).Assemble(registry.Config{
		Chat:       registry.SlotConfig{Impl: "basic"},
		Embedder:   &registry.SlotConfig{Impl: "basic"},
		Classifier: &registry.SlotConfig{Impl: "basic"},
	})
	require.NoError(t, err)

	registryPkg := reflect.TypeOf(registry.Registry{}).PkgPath()
	parts := reflect.ValueOf(c)
	for i := 0; i < parts.NumField(); i++ {
		field := parts.Type().Field(i).Name
		v := parts.Field(i)
		require.False(t, v.IsNil(), "%s not assembled", field)
		assert.Equal(t, registryPkg, v.Elem().Type().PkgPath(), "%s is not the registry's wrapper", field)
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
