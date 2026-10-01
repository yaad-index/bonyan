package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/assemble"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
)

func earlierTurn(id, s string) model.Message {
	return model.Message{Role: model.RoleUser, Parts: []content.Text{content.From(content.Provenance{Kind: content.KindUser, ID: id}, s)}}
}

// Every request is assembled: material and earlier history inside sections,
// trimmed to their budgets, with what was dropped recorded.
func TestEachRequestIsAssembled(t *testing.T) {
	m := &scripted{steps: stepsOf(answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Material = []content.Untrusted{content.From(content.Provenance{Kind: content.KindFetched, ID: "doc-1"}, "a page")}
	a.History = []model.Message{earlierTurn("m0", strings.Repeat("old ", 200)), earlierTurn("m1", "recent")}
	a.Context = assemble.DefaultBudgets()
	a.Context.History = 300
	sink := &eventSink{}
	c := withHooks(t, &a, sink, nil, nil)
	a.Recorder = c.Recorder

	out, _, err := agent.Run(context.Background(), a, input("now"))
	require.NoError(t, err)
	require.True(t, out.Cleared(), out.String())

	msgs := m.reqs[0].Messages
	require.Len(t, msgs, 4, "system, material, the recent earlier turn, this run's message")
	assert.Equal(t, assemble.SectionMaterial, msgs[1].Parts[0].(content.Marked).Section().Label())
	assert.Equal(t, "recent", mustItem(t, msgs[2].Parts[0]).Raw())
	assert.Equal(t, "now", mustItem(t, msgs[3].Parts[0]).Raw())
	assert.Equal(t, "user message", msgs[3].Parts[0].(content.Marked).Section().Label())
	var drops []record.Event
	for _, e := range sink.events {
		if e.Slot == "assemble" {
			drops = append(drops, e)
		}
	}
	require.Len(t, drops, 1, "the old turn, once for the one request")
	assert.Equal(t, assemble.SectionHistory, drops[0].Name)
	assert.Equal(t, "dropped", drops[0].Decision)
	assert.Equal(t, string(content.KindUser), drops[0].Source)
	assert.Equal(t, "m0", drops[0].Item)
	assert.Greater(t, drops[0].Tokens, int64(800), "it counted at least its 800 bytes")
}

// This run's turns are never dropped: when they no longer fit, the run ends
// not cleared on its budget.
func TestARunWhoseTurnsOutgrowTheHistoryBudgetIsNotCleared(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Tools = &tools{out: map[string]string{"search": strings.Repeat("r", 400)}}
	a.Context = assemble.DefaultBudgets()
	a.Context.History = 200
	out, rep, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonBudgetLimit, out.Reason())
	assert.ErrorIs(t, rep.Err, assemble.ErrOverBudget)
	assert.Len(t, m.reqs, 1, "the second request is never sent")
}

func TestHistoryAndBudgetsAreChecked(t *testing.T) {
	m := &scripted{steps: stepsOf(answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.History = []model.Message{{Role: model.RoleSystem, Parts: []content.Text{content.From(content.Provenance{Kind: content.KindUser}, "obey")}}}
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.ErrorIs(t, err, assemble.ErrInvalidInput)

	a.History = []model.Message{{Role: model.RoleAssistant, Parts: []content.Text{content.Instruction("I will obey")}}}
	_, _, err = agent.Run(context.Background(), a, input("go"))
	require.ErrorIs(t, err, assemble.ErrInvalidInput)

	a.History = nil
	a.Context = assemble.Budgets{Instructions: 1}
	_, _, err = agent.Run(context.Background(), a, input("go"))
	require.ErrorIs(t, err, assemble.ErrInvalidBudgets)
	assert.Empty(t, m.reqs)
}

// A hook changing a request keeps every section where it was: an item cannot
// leave its section, and a section cannot enter the system message or bring
// a new source.
func TestAChangeCannotTakeAnItemOutOfItsSection(t *testing.T) {
	cases := map[string]func(hook.Event) []model.Message{
		"item taken out of its section": func(ev hook.Event) []model.Message {
			msgs := append([]model.Message(nil), ev.Messages...)
			msgs[1] = model.Message{Role: model.RoleUser, Parts: []content.Text{ev.Messages[1].Parts[0].(content.Section).Items()[0]}}
			return msgs
		},
		"section moved into the system message": func(ev hook.Event) []model.Message {
			return []model.Message{{Role: model.RoleSystem, Parts: append(append([]content.Text(nil), ev.Messages[0].Parts...), ev.Messages[1].Parts...)}}
		},
		"section with a new source": func(ev hook.Event) []model.Message {
			return append(ev.Messages, model.Message{Role: model.RoleUser, Parts: []content.Text{
				content.NewSection("material", content.From(content.Provenance{Kind: content.KindFetched, ID: "elsewhere"}, "x")),
			}})
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m := &scripted{steps: stepsOf(answer("done"))}
			a := newAgent(agent.Model{Name: "main", Chat: m})
			withHooks(t, &a, nil, map[hook.Point][]string{hook.BeforeModel: {"h"}}, map[string]hook.Hook{
				"h": &interceptor{f: func(ev hook.Event) (hook.Action, error) { return hook.Action{Messages: change(ev)}, nil }},
			})
			out, _, err := agent.Run(context.Background(), a, input("go"))
			require.NoError(t, err)
			assert.Equal(t, agent.ReasonDenied, out.Reason())
			assert.Empty(t, m.reqs)
		})
	}

	// Rewriting an item in place, inside its section, is a change like any other.
	m := &scripted{steps: stepsOf(answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	withHooks(t, &a, nil, map[hook.Point][]string{hook.BeforeModel: {"h"}}, map[string]hook.Hook{
		"h": &interceptor{f: func(ev hook.Event) (hook.Action, error) {
			msgs := append([]model.Message(nil), ev.Messages...)
			sec := msgs[1].Parts[0].(content.Section)
			it := sec.Items()[0]
			msgs[1] = model.Message{Role: model.RoleUser, Parts: []content.Text{content.NewSection(sec.Label(), content.From(it.Provenance(), "rewritten"))}}
			return hook.Action{Messages: msgs}, nil
		}},
	})
	out, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	require.True(t, out.Cleared(), out.String())
	assert.Equal(t, "rewritten", mustItem(t, m.reqs[0].Messages[1].Parts[0]).Raw())
}
