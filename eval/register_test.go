package eval_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/eval"
	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/registry"
)

func assembled(t *testing.T, evaluators ...registry.SlotConfig) (registry.Components, error) {
	t.Helper()
	r := registry.New()
	require.NoError(t, eval.Register(r))
	require.NoError(t, r.RegisterChat("basic", func(json.RawMessage) (model.Chat, error) {
		return &counted{f: func(model.ChatRequest, int) (model.ChatResponse, error) { return answer("x"), nil }}, nil
	}))
	return r.Assemble(registry.Config{Chat: registry.SlotConfig{Impl: "basic"}, Evaluators: evaluators})
}

// The evaluators bonyan ships are assembled from configuration, in the order
// configured, with their options in force.
func TestEvaluatorsAreASlot(t *testing.T) {
	c, err := assembled(t,
		registry.SlotConfig{Impl: "outcome"},
		registry.SlotConfig{Impl: "loops", Options: json.RawMessage(`{"threshold":2}`)},
		registry.SlotConfig{Impl: "waste", Options: json.RawMessage(`{"prices":{"main":{"input":3,"output":4}}}`)},
	)
	require.NoError(t, err)
	require.Len(t, c.Evaluators, 3)
	var names []string
	for _, e := range c.Evaluators {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"outcome", "loops", "waste"}, names)

	// Two identical calls are a loop at threshold 2, and not at the default.
	twice := record.Run{Calls: []record.Call{{
		Kind: record.KindChat, Model: "main",
		Response: record.Response{
			ToolCalls: []record.ToolCall{{ID: "1", Name: "search"}, {ID: "2", Name: "search"}},
			Usage:     &record.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
		},
	}}}
	ctx := context.Background()
	scores, err := c.Evaluators[1].Evaluate(ctx, score.Subject{Run: twice})
	require.NoError(t, err)
	assert.Equal(t, 1.0, mustScore(t, scores, "", "loop"))
	scores, err = eval.Loops{}.Evaluate(ctx, score.Subject{Run: twice})
	require.NoError(t, err)
	assert.Equal(t, 0.0, mustScore(t, scores, "", "loop"))

	// A million tokens each way at 3 and 4 per million.
	scores, err = c.Evaluators[2].Evaluate(ctx, score.Subject{Run: twice})
	require.NoError(t, err)
	assert.Equal(t, 7.0, mustScore(t, scores, "", "cost"))

	c, err = assembled(t)
	require.NoError(t, err)
	assert.Empty(t, c.Evaluators, "none configured")
}

func TestEvaluatorConfigurationIsChecked(t *testing.T) {
	for name, cfg := range map[string][]registry.SlotConfig{
		"an unknown evaluator":    {{Impl: "groundedness"}},
		"a misspelt option":       {{Impl: "loops", Options: json.RawMessage(`{"treshold":2}`)}},
		"an option outcome lacks": {{Impl: "outcome", Options: json.RawMessage(`{"strict":true}`)}},
		"a wrong option type":     {{Impl: "loops", Options: json.RawMessage(`{"threshold":"two"}`)}},
		"one evaluator twice":     {{Impl: "loops"}, {Impl: "loops", Options: json.RawMessage(`{"threshold":2}`)}},
	} {
		_, err := assembled(t, cfg...)
		require.Error(t, err, name)
	}
	_, err := assembled(t, registry.SlotConfig{Impl: "groundedness"})
	require.ErrorIs(t, err, registry.ErrUnknown)

	r := registry.New()
	require.NoError(t, eval.Register(r))
	require.ErrorIs(t, eval.Register(r), registry.ErrDuplicate, "registering twice")
}
