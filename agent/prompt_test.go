package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/prompt"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/telemetry"
)

const promptText = "PROMPT-TEXT-4c1e answer in one line"

var helperPrompt = prompt.Prompt{Name: "helper", Version: "3", Text: promptText}

// chatPrompts are the prompt refs on every chat call of a run, in order.
func chatPrompts(run record.Run) []*record.PromptRef {
	var out []*record.PromptRef
	for _, c := range run.Calls {
		if c.Kind == record.KindChat {
			out = append(out, c.Prompt)
		}
	}
	return out
}

// A versioned prompt is what the model is instructed with, and every call and
// the run's start record its ID and the hash of its text, never the text.
func TestEveryCallRecordsItsPrompt(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("search", `{"q":"x"}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Instructions, a.Prompt = content.Trusted{}, &helperPrompt
	sink := recorded(t, &a)
	_, _, err := agent.Run(context.Background(), a, input("find x"))
	require.NoError(t, err)

	assert.Equal(t, promptText, m.reqs[0].Messages[0].Parts[0].(content.Trusted).String(), "the model is instructed with the prompt")
	want := &record.PromptRef{ID: "helper@3", Hash: helperPrompt.Ref().Hash}
	run := sink.runs(t)[0]
	assert.Equal(t, []*record.PromptRef{want, want}, chatPrompts(run))
	require.NotNil(t, run.Calls)
	assert.True(t, strings.HasPrefix(want.Hash, "sha256:"))

	// The text is in the request messages, as instructions always are, and
	// in no prompt field.
	raw := sink.raw()
	assert.Equal(t, 2, strings.Count(raw, promptText), "once per request")
	var fields int
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n")[1:] {
		var e record.Entry
		require.NoError(t, json.Unmarshal([]byte(line), &e))
		for _, ref := range []*record.PromptRef{callPrompt(e), startPrompt(e)} {
			if ref != nil {
				fields++
				b, err := json.Marshal(ref)
				require.NoError(t, err)
				assert.NotContains(t, string(b), promptText[:12])
			}
		}
	}
	assert.Equal(t, 3, fields, "the start and both calls")
}

// Instructions given without a version are recorded by their hash, so an
// edit shows; an edit under an unchanged version shows the same way.
func TestAnEditShowsInTheHash(t *testing.T) {
	refs := func(a agent.Agent) []*record.PromptRef {
		sink := recorded(t, &a)
		_, _, err := agent.Run(context.Background(), a, input("x"))
		require.NoError(t, err)
		return chatPrompts(sink.runs(t)[0])
	}
	plain := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(answer("done"))}})
	first := refs(plain)
	require.Len(t, first, 1)
	assert.Empty(t, first[0].ID, "unversioned")
	assert.Equal(t, prompt.Unversioned(plain.Instructions).Hash, first[0].Hash)
	plain.Instructions = content.Instruction("answer briefly, please")
	assert.NotEqual(t, first[0].Hash, refs(plain)[0].Hash)

	versioned := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(answer("done"))}})
	versioned.Instructions = content.Trusted{}
	p := helperPrompt
	versioned.Prompt = &p
	before := refs(versioned)[0]
	p.Text += " and be kind"
	after := refs(versioned)[0]
	assert.Equal(t, before.ID, after.ID, "the version did not change")
	assert.NotEqual(t, before.Hash, after.Hash, "the text did")
}

func TestAPromptIsChecked(t *testing.T) {
	both := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(answer("x"))}})
	both.Prompt = &helperPrompt
	_, _, err := agent.Run(context.Background(), both, input("x"))
	require.Error(t, err, "Instructions and Prompt both set")

	empty := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(answer("x"))}})
	empty.Instructions, empty.Prompt = content.Trusted{}, &prompt.Prompt{Name: "helper"}
	_, _, err = agent.Run(context.Background(), empty, input("x"))
	require.Error(t, err, "a prompt with no version or text")
}

// nesting is a tool that, while the outer run waits on it, runs a judge agent
// with a prompt of its own and makes a classifier call, both recorded to the
// outer run's recorder.
type nesting struct {
	tools
	judge agent.Agent
	cls   model.Classifier
}

func (n *nesting) Call(ctx context.Context, _ model.ToolCall) (string, error) {
	if _, _, err := agent.Run(ctx, n.judge, input("is it grounded?")); err != nil {
		return "", err
	}
	_, err := n.cls.Classify(ctx, content.From(content.Provenance{Kind: content.KindTool}, "x"))
	return "checked", err
}

// A call made for something other than the agent's own prompt records that
// caller's prompt, never the agent's: a judge's calls carry the judge's, and a
// classifier's call carries none.
func TestACallForAnotherCallerRecordsItsOwnPrompt(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("search", `{"q":"x"}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Instructions, a.Prompt, a.Name = content.Trusted{}, &helperPrompt, "helper"
	sink := recorded(t, &a)

	judgePrompt := prompt.Prompt{Name: "judge", Version: "1", Text: "judge the answer"}
	judge := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(answer("grounded"))}})
	judge.Instructions, judge.Prompt, judge.Recorder, judge.Name = content.Trusted{}, &judgePrompt, a.Recorder, "judge"
	cls := record.Classifier(classifierFunc(func() model.ClassifyResponse { return model.ClassifyResponse{} }), "gate", a.Recorder)
	a.Tools = &nesting{tools: tools{out: map[string]string{"search": "x"}}, judge: judge, cls: cls}

	_, _, err := agent.Run(context.Background(), a, input("find x"))
	require.NoError(t, err)
	var agentRun, judgeRun record.Run
	for _, run := range sink.runs(t) {
		switch run.Agent {
		case "judge":
			judgeRun = run
		case "helper":
			agentRun = run
		}
	}
	require.NotEmpty(t, judgeRun.Calls, "the judge ran")
	for _, p := range chatPrompts(judgeRun) {
		assert.Equal(t, "judge@1", p.ID)
	}
	var classify *record.Call
	for i, c := range agentRun.Calls {
		switch c.Kind {
		case record.KindChat:
			assert.Equal(t, "helper@3", c.Prompt.ID)
		case record.KindClassify:
			classify = &agentRun.Calls[i]
		}
	}
	require.NotNil(t, classify, "the classifier was called inside the agent's run")
	assert.Nil(t, classify.Prompt, "a classifier's call carries no prompt")
}

type classifierFunc func() model.ClassifyResponse

func (f classifierFunc) Classify(context.Context, content.Untrusted) (model.ClassifyResponse, error) {
	return f(), nil
}

// A model call's span carries the prompt's ID and hash, never its text.
func TestTheSpanCarriesThePromptIDNotItsText(t *testing.T) {
	a := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(answer("done"))}})
	a.Instructions, a.Prompt = content.Trusted{}, &helperPrompt
	o := observe(t, a, telemetry.Options{}, input("x"))
	span := o.span(t, "chat main")
	id, ok := attr(span, "bonyan.prompt.id")
	require.True(t, ok)
	assert.Equal(t, "helper@3", id.AsString())
	hash, ok := attr(span, "bonyan.prompt.hash")
	require.True(t, ok)
	assert.Equal(t, helperPrompt.Ref().Hash, hash.AsString())
	assert.NotContains(t, o.everything(), promptText[:12])
}

func callPrompt(e record.Entry) *record.PromptRef {
	if e.Call == nil {
		return nil
	}
	return e.Call.Prompt
}

func startPrompt(e record.Entry) *record.PromptRef {
	if e.Start == nil {
		return nil
	}
	return e.Start.Prompt
}
