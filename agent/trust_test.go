package agent_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/trust"
)

// trustAll is a program's policy that declares every source trusted.
type trustAll struct{}

func (trustAll) Classify(context.Context, content.Provenance) (trust.Decision, error) {
	return trust.Decision{Verdict: trust.Trusted}, nil
}

// refuser refuses every request carrying untrusted content.
type refuser struct{ trust.Default }

func (refuser) Handle(context.Context, []trust.Item) (trust.Handling, error) {
	return trust.Handling{Refuse: true}, nil
}

// With the default policy every untrusted part reaches the model marked, and
// each classification is recorded.
func TestTheDefaultPolicyMarksEverythingUntrusted(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
	sink := &eventSink{}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Material = []content.Untrusted{content.From(content.Provenance{Kind: content.KindFetched, ID: "doc"}, "a page")}
	c := withHooks(t, &a, sink, nil, nil)
	a.Recorder = c.Recorder
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)

	for _, msg := range m.reqs[1].Messages[1:] {
		for _, p := range msg.Parts {
			_, ok := p.(content.Marked)
			assert.True(t, ok, "a %T part in a %s message", p, msg.Role)
		}
	}
	var kinds []string
	for _, e := range sink.events {
		if e.Slot == registry.SlotTrust {
			assert.Equal(t, "untrusted", e.Decision)
			assert.Equal(t, trust.DefaultName, e.Name)
			kinds = append(kinds, e.Source)
		}
	}
	assert.Equal(t, []string{"fetched", "user", "tool"}, kinds, "the material, the input, then the tool result")
}

// A policy that declares sources trusted gets trusted text where each entered,
// in its own role and never in the system message.
func TestADeclaredTrustedSourceArrivesAsTrustedText(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Trust = trustAll{}
	a.Material = []content.Untrusted{content.From(content.Provenance{Kind: content.KindFetched, ID: "doc"}, "a page")}
	a.History = []model.Message{earlierTurn("m0", "earlier")}
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)

	msgs := m.reqs[1].Messages
	require.Len(t, msgs, 6, "system, material, history, input, the call, its result")
	assert.Equal(t, []content.Text{content.Instruction("a page")}, msgs[1].Parts)
	assert.Equal(t, []content.Text{content.Instruction("earlier")}, msgs[2].Parts)
	assert.Equal(t, model.Message{Role: model.RoleUser, Parts: []content.Text{content.Instruction("go")}}, msgs[3])
	assert.Equal(t, model.RoleTool, msgs[5].Role)
	assert.Equal(t, []content.Text{content.Instruction("found it")}, msgs[5].Parts)
}

// bonyan's own text in place of a tool result stays untrusted whatever the
// policy says about tool output.
func TestBonyansToolTextsStayUntrusted(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("missing", `{}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Trust = trustAll{}
	a.Tools = &tools{err: map[string]error{"missing": agent.ErrUnknownTool}}
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.Equal(t, "error: unknown tool", mustItem(t, m.reqs[1].Messages[3].Parts[0]).Raw())
}

// A hook's change never raises trust: a changed trusted message or result
// arrives untrusted, with the provenance of what it replaced.
func TestAChangeToTrustedContentIsUntrusted(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Trust = trustAll{}
	msg := act(hook.Action{Text: text("rewritten")})
	res := act(hook.Action{Text: text("trimmed")})
	withHooks(t, &a, nil, map[hook.Point][]string{hook.UserMessage: {"msg"}, hook.AfterTool: {"res"}}, map[string]hook.Hook{"msg": msg, "res": res})
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	require.Len(t, msg.seen, 1)
	assert.True(t, msg.seen[0].Trusted, "the hook is told the policy's decision")
	assert.True(t, res.seen[0].Trusted)

	user := mustItem(t, m.reqs[1].Messages[1].Parts[0])
	assert.Equal(t, "rewritten", user.Raw())
	assert.Equal(t, content.Provenance{Kind: content.KindUser, ID: "m1"}, user.Provenance())
	result := mustItem(t, m.reqs[1].Messages[3].Parts[0])
	assert.Equal(t, "trimmed", result.Raw())
	assert.Equal(t, content.KindTool, result.Provenance().Kind)
}

func TestThePolicysHandlingCanRefuseTheRequest(t *testing.T) {
	m := &scripted{steps: stepsOf(answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Trust = refuser{}
	out, rep, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonTrustRefused, out.Reason())
	assert.ErrorIs(t, rep.Err, registry.ErrRefused)
	assert.Empty(t, m.reqs, "nothing is sent")
}
