package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/budget"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/eval"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/tool"
)

// chat answers each request with f.
type chat func(req model.ChatRequest, n int) (model.ChatResponse, error)

// counted calls f with how many requests came before.
type counted struct {
	mu sync.Mutex
	n  int
	f  chat
}

func (c *counted) Chat(_ context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	c.mu.Lock()
	n := c.n
	c.n++
	c.mu.Unlock()
	return c.f(req, n)
}

var usage = &model.Usage{InputTokens: 10, OutputTokens: 5}

func answer(text string) model.ChatResponse {
	return model.ChatResponse{Content: text, StopReason: model.StopEnd, Usage: usage}
}

func call(name, args string, n int) model.ChatResponse {
	return model.ChatResponse{
		ToolCalls:  []model.ToolCall{{ID: fmt.Sprintf("c%d", n), Name: name, Arguments: json.RawMessage(args)}},
		StopReason: model.StopToolCalls, Usage: usage,
	}
}

// userText is the text of the request's user message.
func userText(req model.ChatRequest) string {
	for _, m := range req.Messages {
		if m.Role != model.RoleUser {
			continue
		}
		for _, p := range m.Parts {
			if s, ok := p.(content.Section); ok {
				for _, it := range s.Items() {
					return it.Raw()
				}
			}
			if mk, ok := p.(content.Marked); ok {
				for _, it := range mk.Section().Items() {
					return it.Raw()
				}
			}
		}
	}
	return ""
}

// tools has one tool, search, which fails when fail is set.
type tools struct{ fail bool }

func (tools) Definitions() []model.ToolDef {
	return []model.ToolDef{{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

func (t tools) Call(_ context.Context, tc model.ToolCall) (string, error) {
	if tc.Name != "search" {
		return "", tool.ErrUnknown
	}
	if t.fail {
		return "", errors.New("search is down")
	}
	return "found it", nil
}
func (tools) Source(string) content.Kind { return content.KindTool }
func (tools) NeedsApproval(string) bool  { return false }

var prices = budget.PriceTable{"main": {Input: 1_000_000, Output: 2_000_000}, "backup": {Input: 1_000_000, Output: 1_000_000}}

func newAgent(f chat) agent.Agent {
	return agent.Agent{
		Models:          []agent.Model{{Name: "main", Chat: &counted{f: f}}},
		Prices:          prices,
		MaxOutputTokens: 100,
		Instructions:    content.Instruction("answer briefly"),
		Tools:           tools{},
	}
}

func input(s string) content.Untrusted {
	return content.From(content.Provenance{Kind: content.KindUser, ID: "m1"}, s)
}

// score is the value of a metric among scores, and whether it is there.
func score(scores []eval.Score, evaluator, metric string) (float64, bool) {
	for _, s := range scores {
		if s.Evaluator == evaluator && s.Metric == metric {
			return s.Value, true
		}
	}
	return 0, false
}

func mustScore(t *testing.T, scores []eval.Score, evaluator, metric string) float64 {
	t.Helper()
	v, ok := score(scores, evaluator, metric)
	require.True(t, ok, "no %s %s in %v", evaluator, metric, scores)
	return v
}

func aggregate(t *testing.T, rep eval.Report, evaluator, metric string) eval.Aggregate {
	t.Helper()
	for _, a := range rep.Aggregates {
		if a.Evaluator == evaluator && a.Metric == metric {
			return a
		}
	}
	require.Fail(t, "no aggregate", "%s %s", evaluator, metric)
	return eval.Aggregate{}
}

var all = []eval.Evaluator{eval.Loops{}, eval.Waste{}, eval.Outcome{}}

// A case set produces a result per case, with its scores, and aggregates
// over the cases.
func TestACaseSetProducesResultsAndAggregates(t *testing.T) {
	a := newAgent(func(req model.ChatRequest, n int) (model.ChatResponse, error) {
		switch {
		case strings.Contains(userText(req), "capital"):
			return answer("It is Paris."), nil
		case strings.Contains(userText(req), "forever"):
			return call("search", `{"q":"again"}`, n), nil
		case len(req.Messages) < 4:
			return call("search", `{"q":"answer"}`, n), nil
		}
		return answer("41"), nil
	})
	r := eval.Runner{Agent: a, Evaluators: all}
	rep, err := r.Run(context.Background(), []eval.Case{
		{Name: "capital", Input: input("what is the capital of France?"), Expect: []eval.Property{eval.Contains("Paris")}},
		{Name: "answer", Input: input("what is the answer?"), Expect: []eval.Property{eval.Equals("42"), eval.Contains("4")}},
		{Name: "forever", Input: input("search forever")},
	}, nil)
	require.NoError(t, err)
	require.Len(t, rep.Cases, 3)

	first, second := rep.Cases[0], rep.Cases[1]
	assert.Equal(t, "capital", first.Case)
	assert.True(t, first.Outcome.Cleared())
	assert.Equal(t, 1.0, mustScore(t, first.Scores, "outcome", `property: contains "Paris"`))
	assert.Equal(t, 1.0, mustScore(t, first.Scores, "outcome", "expected"))
	assert.Equal(t, 1.0, mustScore(t, first.Scores, "outcome", "cleared"))

	assert.Equal(t, "answer", second.Case)
	assert.Equal(t, 0.0, mustScore(t, second.Scores, "outcome", `property: equals "42"`))
	assert.Equal(t, 1.0, mustScore(t, second.Scores, "outcome", `property: contains "4"`))
	assert.Equal(t, 0.0, mustScore(t, second.Scores, "outcome", "expected"))
	assert.Equal(t, float64(second.Report.Tokens), mustScore(t, second.Scores, "waste", "tokens"))
	assert.Equal(t, float64(second.Report.Cost), mustScore(t, second.Scores, "waste", "cost"))

	third := rep.Cases[2]
	assert.Equal(t, agent.ReasonLoopDetected, third.Outcome.Reason())
	assert.Equal(t, 0.0, mustScore(t, third.Scores, "outcome", "cleared"))
	assert.Equal(t, 0.0, mustScore(t, third.Scores, "outcome", "expected"), "no answer, even with nothing expected")

	expected := aggregate(t, rep, "outcome", "expected")
	assert.Equal(t, eval.Aggregate{Evaluator: "outcome", Metric: "expected", N: 3, Sum: 1, Mean: 1.0 / 3, Min: 0, Max: 1}, expected)
	tokens := aggregate(t, rep, "waste", "tokens")
	assert.Equal(t, 3, tokens.N)
	assert.Equal(t, float64(first.Report.Tokens+second.Report.Tokens+third.Report.Tokens), tokens.Sum)
	assert.Equal(t, float64(min(first.Report.Tokens, second.Report.Tokens, third.Report.Tokens)), tokens.Min)
	assert.Equal(t, float64(max(first.Report.Tokens, second.Report.Tokens, third.Report.Tokens)), tokens.Max)

	per, ok := rep.PerCleared("waste", "tokens")
	require.True(t, ok)
	assert.Equal(t, tokens.Sum/2, per, "over the two cases that answered, the third's tokens included")
	_, ok = rep.PerCleared("waste", "nothing")
	assert.False(t, ok)
}

// record runs a on in and returns the recording's bytes.
func recordRun(t *testing.T, a agent.Agent, in string) []byte {
	t.Helper()
	f, err := record.OpenFile(record.FileOptions{Dir: t.TempDir()})
	require.NoError(t, err)
	rec, err := record.NewRecorder(f, secret.NewScrubber())
	require.NoError(t, err)
	a.Recorder = rec
	_, _, err = agent.Run(context.Background(), a, input(in))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	raw, err := os.ReadFile(f.Path())
	require.NoError(t, err)
	return raw
}

func evaluateRecording(t *testing.T, raw []byte, evaluators ...eval.Evaluator) []eval.Score {
	t.Helper()
	runs, err := eval.EvaluateRecording(strings.NewReader(string(raw)), evaluators...)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	return runs[0].Scores
}

// A recorded run that requested the same tool call again and again is
// reported as a loop, even when inline detection was off and the run
// answered; a run whose calls all differ is not.
func TestARecordedRunWithARepeatedToolCallIsALoop(t *testing.T) {
	repeating := newAgent(func(_ model.ChatRequest, n int) (model.ChatResponse, error) {
		if n < 3 {
			return call("search", `{"q":"x","page":1}`, n), nil
		}
		return answer("done"), nil
	})
	repeating.LoopThreshold = -1
	scores := evaluateRecording(t, recordRun(t, repeating, "find x"), eval.Loops{})
	assert.Equal(t, 1.0, mustScore(t, scores, "loops", "loop"))
	assert.Equal(t, 2.0, mustScore(t, scores, "loops", "repeated_tool_calls"))
	assert.Equal(t, 3.0, mustScore(t, scores, "loops", "max_identical_tool_calls"))
	assert.Equal(t, 0.0, mustScore(t, scores, "loops", "hit_step_limit"), "the run answered")

	changing := newAgent(func(_ model.ChatRequest, n int) (model.ChatResponse, error) {
		if n < 3 {
			return call("search", fmt.Sprintf(`{"page":%d}`, n), n), nil
		}
		return answer("done"), nil
	})
	scores = evaluateRecording(t, recordRun(t, changing, "find x"), eval.Loops{})
	assert.Equal(t, 0.0, mustScore(t, scores, "loops", "loop"))
	assert.Equal(t, 0.0, mustScore(t, scores, "loops", "repeated_tool_calls"))
	assert.Equal(t, 1.0, mustScore(t, scores, "loops", "max_identical_tool_calls"))
}

// The same call with its arguments' keys in another order is the same call.
func TestArgumentOrderDoesNotHideARepeat(t *testing.T) {
	a := newAgent(func(_ model.ChatRequest, n int) (model.ChatResponse, error) {
		switch n {
		case 0:
			return call("search", `{"q":"x","page":1}`, n), nil
		case 1:
			return call("search", `{ "page": 1, "q": "x" }`, n), nil
		}
		return answer("done"), nil
	})
	a.LoopThreshold = -1
	scores := evaluateRecording(t, recordRun(t, a, "find x"), eval.Loops{Threshold: 2})
	assert.Equal(t, 1.0, mustScore(t, scores, "loops", "repeated_tool_calls"))
	assert.Equal(t, 1.0, mustScore(t, scores, "loops", "loop"))
}

func TestLoopsThreshold(t *testing.T) {
	a := newAgent(func(_ model.ChatRequest, n int) (model.ChatResponse, error) {
		if n < 2 {
			return call("search", `{"q":"x"}`, n), nil
		}
		return answer("done"), nil
	})
	a.LoopThreshold = -1
	raw := recordRun(t, a, "find x")
	for _, tc := range []struct {
		threshold int
		loop      float64
	}{{0, 0}, {2, 1}, {3, 0}, {-1, 0}} {
		scores := evaluateRecording(t, raw, eval.Loops{Threshold: tc.threshold})
		assert.Equal(t, tc.loop, mustScore(t, scores, "loops", "loop"), "threshold %d", tc.threshold)
	}
}

// A run that ended at its step limit, or by inline loop detection, is a loop.
func TestARunEndedByItsLimitsIsALoop(t *testing.T) {
	steps := newAgent(func(_ model.ChatRequest, n int) (model.ChatResponse, error) {
		return call("search", fmt.Sprintf(`{"page":%d}`, n), n), nil
	})
	steps.Limits = agent.DefaultLimits()
	steps.Limits.MaxSteps = 3
	scores := evaluateRecording(t, recordRun(t, steps, "find x"), eval.Loops{})
	assert.Equal(t, 1.0, mustScore(t, scores, "loops", "hit_step_limit"))
	assert.Equal(t, 1.0, mustScore(t, scores, "loops", "loop"))
	assert.Equal(t, 0.0, mustScore(t, scores, "loops", "repeated_tool_calls"))

	inline := newAgent(func(_ model.ChatRequest, n int) (model.ChatResponse, error) {
		return call("search", `{"q":"x"}`, n), nil
	})
	scores = evaluateRecording(t, recordRun(t, inline, "find x"), eval.Loops{Threshold: -1})
	assert.Equal(t, 0.0, mustScore(t, scores, "loops", "hit_step_limit"))
	assert.Equal(t, 1.0, mustScore(t, scores, "loops", "loop"), "ended by inline loop detection")
}

// A request identical to one already answered is a repeated state; one sent
// again after a failed call is a retry.
func TestRepeatedStates(t *testing.T) {
	answered := record.Call{Kind: record.KindChat, Model: "main", Fingerprint: "f1"}
	failed := record.Call{Kind: record.KindChat, Model: "main", Fingerprint: "f1", ErrorKind: model.ErrTimeout}
	classify := record.Call{Kind: record.KindClassify, Model: "main", Fingerprint: "f1"}
	for name, tc := range map[string]struct {
		calls []record.Call
		want  float64
	}{
		"answered, then again": {[]record.Call{answered, answered}, 1},
		"failed, then again":   {[]record.Call{failed, answered}, 0},
		"classifier calls":     {[]record.Call{classify, classify}, 0},
		"different requests":   {[]record.Call{answered, {Kind: record.KindChat, Model: "main", Fingerprint: "f2"}}, 0},
	} {
		scores := eval.Evaluate(eval.Subject{Run: record.Run{Calls: tc.calls}}, eval.Loops{})
		assert.Equal(t, tc.want, mustScore(t, scores, "loops", "repeated_states"), name)
		assert.Equal(t, tc.want, mustScore(t, scores, "loops", "loop"), name)
	}
}

// Waste counts the calls that gave nothing and the answers sent back.
func TestWasteCountsWhatWasSpentForNothing(t *testing.T) {
	a := newAgent(func(req model.ChatRequest, n int) (model.ChatResponse, error) {
		switch n {
		case 0:
			return call("search", `{"q":"x"}`, n), nil
		case 1:
			return call("search", `{"q":"x"}`, n), nil
		case 2:
			return answer("not json"), nil
		}
		return answer(`{"ok":true}`), nil
	})
	a.Tools = tools{fail: true}
	out, err := agent.OutputFor[struct {
		OK bool `json:"ok"`
	}](2)
	require.NoError(t, err)
	a.Output = out
	// The first call fails on the primary and the backup answers it; both
	// answer from one script.
	primary := a.Models[0].Chat
	backup := &counted{f: func(req model.ChatRequest, _ int) (model.ChatResponse, error) {
		return primary.Chat(context.Background(), req)
	}}
	failing := &counted{f: func(req model.ChatRequest, n int) (model.ChatResponse, error) {
		if n == 0 {
			return model.ChatResponse{}, &model.CallError{Kind: model.ErrTransport, Err: errors.New("unavailable")}
		}
		return primary.Chat(context.Background(), req)
	}}
	a.Models = []agent.Model{{Name: "main", Chat: failing}, {Name: "backup", Chat: backup}}

	r := eval.Runner{Agent: a, Evaluators: []eval.Evaluator{eval.Waste{}}}
	rep, err := r.Run(context.Background(), []eval.Case{{Name: "c", Input: input("find x")}}, nil)
	require.NoError(t, err)
	res := rep.Cases[0]
	require.True(t, res.Outcome.Cleared(), "%s", res.Outcome)
	assert.Equal(t, 2.0, mustScore(t, res.Scores, "waste", "failed_tool_calls"))
	assert.Equal(t, 1.0, mustScore(t, res.Scores, "waste", "redundant_tool_calls"))
	assert.Equal(t, 1.0, mustScore(t, res.Scores, "waste", "failed_model_calls"))
	assert.Equal(t, 1.0, mustScore(t, res.Scores, "waste", "output_retries"))
	assert.Equal(t, float64(res.Report.Tokens), mustScore(t, res.Scores, "waste", "tokens"))
	assert.Equal(t, float64(res.Report.Cost), mustScore(t, res.Scores, "waste", "cost"))
}

// A version 1 recording holds no end: waste sums its usage and prices it only
// when every model has a price, and nothing scores what only an end holds.
func TestAVersion1RecordingIsScoredFromItsCalls(t *testing.T) {
	raw, err := os.ReadFile("../record/testdata/v1.jsonl")
	require.NoError(t, err)

	scores := evaluateRecording(t, raw, eval.Loops{}, eval.Waste{Prices: prices}, eval.Outcome{})
	assert.Equal(t, 45.0, mustScore(t, scores, "waste", "tokens"), "three calls of 10 in and 5 out")
	assert.Equal(t, 60.0, mustScore(t, scores, "waste", "cost"), "30 input tokens at 1 and 15 output tokens at 2 per million, in millionths")
	assert.Equal(t, 0.0, mustScore(t, scores, "waste", "failed_tool_calls"), "version 1 has no tool events")
	for _, metric := range []string{"hit_step_limit"} {
		_, ok := score(scores, "loops", metric)
		assert.False(t, ok, metric)
	}
	_, ok := score(scores, "outcome", "cleared")
	assert.False(t, ok, "a version 1 recording holds no outcome")

	scores = evaluateRecording(t, raw, eval.Waste{})
	_, ok = score(scores, "waste", "cost")
	assert.False(t, ok, "no prices, no cost")
	scores = evaluateRecording(t, raw, eval.Waste{Prices: budget.PriceTable{"backup": {Input: 1}}})
	_, ok = score(scores, "waste", "cost")
	assert.False(t, ok, "a model without a price, no cost")
}

// A recording read on its own has an outcome but no case, so nothing scores
// properties.
func TestARecordingAloneIsScoredWithoutACase(t *testing.T) {
	a := newAgent(func(model.ChatRequest, int) (model.ChatResponse, error) { return answer("done"), nil })
	scores := evaluateRecording(t, recordRun(t, a, "x"), eval.Outcome{})
	assert.Equal(t, []eval.Score{{Evaluator: "outcome", Metric: "cleared", Value: 1}}, scores)
}

// A run that did not answer has none of its case's properties.
func TestARunThatDidNotAnswerHasNoProperty(t *testing.T) {
	a := newAgent(func(_ model.ChatRequest, n int) (model.ChatResponse, error) {
		return call("search", fmt.Sprintf(`{"page":%d}`, n), n), nil
	})
	a.Limits = agent.DefaultLimits()
	a.Limits.MaxSteps = 2
	always := eval.Property{Name: "anything", Check: func(string) error { return nil }}
	rep, err := eval.Runner{Agent: a, Evaluators: all}.Run(context.Background(), []eval.Case{{Name: "c", Input: input("x"), Expect: []eval.Property{always}}}, nil)
	require.NoError(t, err)
	scores := rep.Cases[0].Scores
	assert.Equal(t, 0.0, mustScore(t, scores, "outcome", "cleared"))
	assert.Equal(t, 0.0, mustScore(t, scores, "outcome", "property: anything"))
	assert.Equal(t, 0.0, mustScore(t, scores, "outcome", "expected"))
	_, ok := rep.PerCleared("waste", "tokens")
	assert.False(t, ok, "no case answered")
}

func TestValidAgainst(t *testing.T) {
	schema, err := tool.SchemaFor[struct {
		OK bool `json:"ok"`
	}]()
	require.NoError(t, err)
	p := eval.ValidAgainst("verdict", schema)
	assert.Equal(t, "valid against verdict", p.Name)
	require.NoError(t, p.Check(`{"ok":true}`))
	require.Error(t, p.Check(`{"ok":"yes"}`))
	require.Error(t, p.Check(`not json`))
}

// Switching an evaluator off for a run leaves out its scores and nothing
// else: the agent's step and budget limits still end the run.
func TestASwitchedOffEvaluatorGivesNoScoreAndTheLimitsStay(t *testing.T) {
	repeating := newAgent(func(_ model.ChatRequest, n int) (model.ChatResponse, error) {
		return call("search", `{"q":"x"}`, n), nil
	})
	repeating.LoopThreshold = -1
	repeating.Limits = agent.DefaultLimits()
	repeating.Limits.MaxSteps = 3

	r := eval.Runner{Agent: repeating, Evaluators: all}
	cases := []eval.Case{{Name: "c", Input: input("find x")}}
	rep, err := r.Run(context.Background(), cases, eval.Switch{"loops": false, "waste": false})
	require.NoError(t, err)
	res := rep.Cases[0]
	assert.Equal(t, agent.ReasonStepLimit, res.Outcome.Reason(), "the step limit still ends the run")
	assert.Equal(t, 3, res.Report.Steps)
	for _, s := range res.Scores {
		assert.Equal(t, "outcome", s.Evaluator)
	}
	assert.NotEmpty(t, res.Scores)
	for _, a := range rep.Aggregates {
		assert.Equal(t, "outcome", a.Evaluator)
	}

	rep, err = r.Run(context.Background(), cases, eval.Switch{"loops": true})
	require.NoError(t, err)
	assert.Equal(t, 1.0, mustScore(t, rep.Cases[0].Scores, "loops", "loop"), "switched on, it scores")

	budgeted := repeating
	budgeted.Limits.MaxSteps = 50
	budgeted.Limits.Budget.MaxTokens = 40
	rep, err = eval.Runner{Agent: budgeted, Evaluators: all}.Run(context.Background(), cases, eval.Switch{"loops": false, "waste": false, "outcome": false})
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonBudgetLimit, rep.Cases[0].Outcome.Reason(), "the budget still ends the run")
	assert.Empty(t, rep.Cases[0].Scores)
	assert.Empty(t, rep.Aggregates)
}

func TestTheRunnerRefusesWhatItCannotRun(t *testing.T) {
	a := newAgent(func(model.ChatRequest, int) (model.ChatResponse, error) { return answer("x"), nil })
	ok := []eval.Case{{Name: "c", Input: input("x")}}
	rec, err := record.NewRecorder(&discard{}, secret.NewScrubber())
	require.NoError(t, err)
	withRecorder := a
	withRecorder.Recorder = rec
	noModel := a
	noModel.Models = nil

	for name, tc := range map[string]struct {
		r     eval.Runner
		cases []eval.Case
		sw    eval.Switch
	}{
		"an agent with a recorder": {eval.Runner{Agent: withRecorder, Evaluators: all}, ok, nil},
		"an unknown switch":        {eval.Runner{Agent: a, Evaluators: all}, ok, eval.Switch{"groundedness": false}},
		"two evaluators, one name": {eval.Runner{Agent: a, Evaluators: []eval.Evaluator{eval.Loops{}, eval.Loops{Threshold: 2}}}, ok, nil},
		"a nil evaluator":          {eval.Runner{Agent: a, Evaluators: []eval.Evaluator{nil}}, ok, nil},
		"a case with no name":      {eval.Runner{Agent: a, Evaluators: all}, []eval.Case{{Input: input("x")}}, nil},
		"two cases, one name":      {eval.Runner{Agent: a, Evaluators: all}, []eval.Case{ok[0], ok[0]}, nil},
		"a property with no check": {eval.Runner{Agent: a, Evaluators: all}, []eval.Case{{Name: "c", Input: input("x"), Expect: []eval.Property{{Name: "p"}}}}, nil},
		"two properties, one name": {eval.Runner{Agent: a, Evaluators: all}, []eval.Case{{Name: "c", Input: input("x"), Expect: []eval.Property{eval.Equals("x"), eval.Equals("x")}}}, nil},
		"an agent that cannot run": {eval.Runner{Agent: noModel, Evaluators: all}, ok, nil},
	} {
		_, err := tc.r.Run(context.Background(), tc.cases, tc.sw)
		require.Error(t, err, name)
	}
	_, err = eval.EvaluateRecording(strings.NewReader(""), eval.Loops{})
	require.Error(t, err, "an empty recording")
	_, err = eval.EvaluateRecording(strings.NewReader(`{"format":"bonyan-recording","version":2}`), eval.Loops{}, eval.Loops{})
	require.Error(t, err, "two evaluators, one name")
}

type discard struct{}

func (discard) Write(record.Entry) error { return nil }
func (discard) Full() bool               { return false }
func (discard) Subject() string          { return "" }
func (discard) Close() error             { return nil }
