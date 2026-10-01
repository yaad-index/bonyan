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
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/tool"
)

type verdict struct {
	Cleared bool   `json:"cleared"`
	Reason  string `json:"reason"`
}

const marker = "MARKER-51c2"

func withOutput(t *testing.T, a *agent.Agent, retries int) {
	t.Helper()
	out, err := agent.OutputFor[verdict](retries)
	require.NoError(t, err)
	a.Output = out
}

// trusted returns the text of a trusted part.
func trusted(t *testing.T, p content.Text) string {
	t.Helper()
	tr, ok := p.(content.Trusted)
	require.True(t, ok, "got %T", p)
	return tr.String()
}

func TestAStructuredAnswerIsDecoded(t *testing.T) {
	m := &scripted{steps: stepsOf(answer(`{"cleared":true,"reason":"fine"}`))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	withOutput(t, &a, 1)
	out, _, err := agent.Run(context.Background(), a, input("judge"))
	require.NoError(t, err)
	v, err := agent.Decode[verdict](out)
	require.NoError(t, err)
	assert.Equal(t, verdict{Cleared: true, Reason: "fine"}, v)
	assert.JSONEq(t, string(a.Output.Schema.JSON()), string(m.reqs[0].Schema), "the request carries the schema")
}

// An answer that does not match is sent back with where it failed and which
// rule it broke, quoting nothing of it, and the next answer is checked again.
func TestAnInvalidAnswerIsRetriedWithTheReason(t *testing.T) {
	m := &scripted{steps: stepsOf(answer(`{"cleared":"`+marker+`"}`), answer(`{"cleared":false,"reason":"spam"}`))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	withOutput(t, &a, 1)
	out, rep, err := agent.Run(context.Background(), a, input("judge"))
	require.NoError(t, err)
	require.True(t, out.Cleared(), out.String())
	assert.Equal(t, 2, rep.Steps, "a retry is a step")

	second := m.reqs[1].Messages
	last := second[len(second)-1]
	assert.Equal(t, model.RoleUser, last.Role)
	text := trusted(t, last.Parts[0])
	assert.Contains(t, text, "(/properties/cleared: type)")
	assert.NotContains(t, text, marker)
	for _, msg := range second {
		for _, p := range msg.Parts {
			if m, ok := p.(content.Marked); ok {
				assert.NotContains(t, m.Text(), marker, "the invalid answer is not sent back")
			}
		}
	}
}

func TestAnAnswerThatNeverMatchesEndsNotCleared(t *testing.T) {
	for _, retries := range []int{0, 2} {
		m := &scripted{steps: stepsOf(answer(`{"cleared":"` + marker + `","reason":"x"}`))}
		a := newAgent(agent.Model{Name: "main", Chat: m})
		withOutput(t, &a, retries)
		out, rep, err := agent.Run(context.Background(), a, input("judge"))
		require.NoError(t, err)
		assert.Equal(t, agent.ReasonInvalidOutput, out.Reason())
		require.ErrorIs(t, rep.Err, agent.ErrInvalidOutput)
		var ve *tool.ValidationError
		require.ErrorAs(t, rep.Err, &ve)
		assert.Equal(t, "/properties/cleared: type", ve.Error())
		assert.NotContains(t, rep.Err.Error(), marker)
		assert.Equal(t, retries+1, m.n, "the first answer and each retry")
		_, err = agent.Decode[verdict](out)
		assert.ErrorContains(t, err, "no answer")
	}
}

// A reply hook's change is checked too: a structured answer cannot leave the
// run in a shape the schema refuses.
func TestAReplyHookCannotBreakTheShape(t *testing.T) {
	m := &scripted{steps: stepsOf(answer(`{"cleared":true,"reason":"fine"}`))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	withOutput(t, &a, 3)
	withHooks(t, &a, nil, map[hook.Point][]string{hook.Reply: {"redact"}}, map[string]hook.Hook{"redact": act(hook.Action{Text: text("[redacted]")})})
	out, rep, err := agent.Run(context.Background(), a, input("judge"))
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonInvalidOutput, out.Reason())
	assert.ErrorIs(t, rep.Err, agent.ErrInvalidOutput)
	assert.Contains(t, rep.Err.Error(), `after hook "redact"`)
	assert.Equal(t, 1, m.n, "a hook's change is not retried")
}

func TestAnOutputMustBeUsable(t *testing.T) {
	a := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(answer("x"))}})
	a.Output = &agent.Output{}
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.Error(t, err)
	withOutput(t, &a, -1)
	_, _, err = agent.Run(context.Background(), a, input("go"))
	require.Error(t, err)
}

type query struct {
	Q string `json:"q"`
}

// The registry's checks reach the model as bonyan's own text: invalid
// arguments by path and rule, and the tool never runs.
func TestARegistryToolWithInvalidArguments(t *testing.T) {
	r := tool.NewRegistry(nil)
	ran := 0
	require.NoError(t, tool.Register(r, "search", tool.Spec{}, func(context.Context, query, *secret.Scoped) (string, error) {
		ran++
		return "found", nil
	}))
	m := &scripted{steps: stepsOf(toolCall("search", `{"q":1,"`+marker+`":2}`), toolCall("search", `{"q":"x"}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Tools = r
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.Equal(t, 1, ran, "only the valid call runs")
	bad := mustItem(t, m.reqs[1].Messages[3].Parts[0]).Raw()
	assert.Equal(t, "error: invalid arguments: /properties/q: type", bad)
	assert.NotContains(t, bad, marker)
	assert.Equal(t, `"found"`, mustItem(t, m.reqs[2].Messages[5].Parts[0]).Raw())
}
