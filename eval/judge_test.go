package eval_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/assemble"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/eval"
	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/inmem"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/prompt"
	"github.com/yaad-index/bonyan/record"
)

const (
	docText    = "DOC-3b9 the capital is Paris"
	resultSeen = "RESULT-3b9 the river is the Seine"
)

// judgeModel answers every request with verdict and keeps the requests.
type judgeModel struct {
	mu      sync.Mutex
	verdict string
	reqs    []model.ChatRequest
}

func (j *judgeModel) Chat(_ context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.reqs = append(j.reqs, req)
	return answer(j.verdict), nil
}

func judgeAgent(m *judgeModel) agent.Agent {
	a := newAgent(nil)
	a.Models = []agent.Model{{Name: "main", Chat: m}}
	a.Instructions, a.Tools = content.Trusted{}, nil
	return a
}

// judgedRun is a recorded run with material and a tool result in its context,
// which answered "saw: " and the tool's result.
func judgedRun(t *testing.T) record.Run {
	t.Helper()
	a := searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: resultSeen})
	a.Material = []content.Untrusted{content.From(content.Provenance{Kind: content.KindFetched, ID: "doc"}, docText)}
	return recorded(t, a, "find x")
}

// seen is one untrusted item a judge was sent, with the section holding it.
type seen struct {
	section string
	from    content.Provenance
	text    string
}

func itemsOf(req model.ChatRequest) []seen {
	var out []seen
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if mk, ok := p.(content.Marked); ok {
				for _, it := range mk.Section().Items() {
					out = append(out, seen{mk.Section().Label(), it.Provenance(), it.Raw()})
				}
			}
		}
	}
	return out
}

func systemText(req model.ChatRequest) string {
	return req.Messages[0].Parts[0].(content.Trusted).String()
}

func evaluate(t *testing.T, run record.Run, e score.Evaluator) ([]score.Score, []string) {
	t.Helper()
	return score.Evaluate(context.Background(), score.Subject{Run: run}, e)
}

// The judge is given the answer as model output, the run's context as the
// material it was, each under an ID it can cite, and the run's message as its
// input; its verdict becomes counts.
func TestGroundednessJudgesTheAnswerAgainstTheContext(t *testing.T) {
	m := &judgeModel{verdict: `{"claims":[{"claim":"river","verdict":"supported","sources":["c2"]},{"claim":"asked","verdict":"supported","sources":["question"]},{"claim":"city","verdict":"unsupported"}],"invented_citations":1}`}
	scores, failed := evaluate(t, judgedRun(t), eval.Groundedness{Judge: judgeAgent(m)})
	require.Empty(t, failed)
	for metric, want := range map[string]float64{"claims": 3, "supported": 2, "unsupported": 1, "unverifiable": 0, "invented_citations": 1} {
		assert.Equal(t, want, mustScore(t, scores, "groundedness", metric), metric)
	}

	require.Len(t, m.reqs, 1)
	req := m.reqs[0]
	assert.True(t, strings.HasPrefix(systemText(req), "You check whether an answer is grounded"), "bonyan's instructions when the judge has none")
	assert.Equal(t, []seen{
		{assemble.SectionMaterial, content.Provenance{Kind: content.KindModel, ID: "answer"}, "saw: " + resultSeen},
		{assemble.SectionMaterial, content.Provenance{Kind: content.KindFetched, ID: "c1"}, docText},
		{assemble.SectionMaterial, content.Provenance{Kind: content.KindTool, ID: "c2"}, resultSeen},
		{agent.SectionUserMessage, content.Provenance{Kind: content.KindUser, ID: "question"}, "find x"},
	}, itemsOf(req), "the answer is model output, never the judge's input")
}

// A claim the judge finds unsupported may rest on context the recording left
// out, so it is counted as unverifiable, and what was left out is never sent.
func TestAClaimMayRestOnContextTheRecordingLeftOut(t *testing.T) {
	store, err := memory.NewStore(inmem.New(), memory.Options{Retention: time.Hour})
	require.NoError(t, err)
	require.NoError(t, store.Remember(context.Background(), "ana", content.KindUser, "MEMORY-3b9 find things"))
	a := searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: resultSeen})
	a.Memory, a.Subject = store, "ana"
	run := recorded(t, a, "find x")

	m := &judgeModel{verdict: `{"claims":[{"claim":"a","verdict":"supported","sources":["c1"]},{"claim":"b","verdict":"unsupported"}],"invented_citations":0}`}
	scores, failed := evaluate(t, run, eval.Groundedness{Judge: judgeAgent(m)})
	require.Empty(t, failed)
	assert.Equal(t, 1.0, mustScore(t, scores, "groundedness", "supported"))
	assert.Equal(t, 0.0, mustScore(t, scores, "groundedness", "unsupported"))
	assert.Equal(t, 1.0, mustScore(t, scores, "groundedness", "unverifiable"))
	for _, it := range itemsOf(m.reqs[0]) {
		assert.NotEqual(t, content.KindMemory, it.from.Kind)
	}

	u := &judgeModel{verdict: `{"used":["c1"]}`}
	scores, failed = evaluate(t, run, eval.UnusedContext{Judge: judgeAgent(u)})
	require.Empty(t, failed)
	assert.Equal(t, 1.0, mustScore(t, scores, "unused_context", "unjudged_items"))
	assert.Equal(t, 1.0, mustScore(t, scores, "unused_context", "items"), "the tool result")
}

// A verdict the evaluator cannot read as given fails it, and so does a judge
// that was not shown the whole context or did not answer; it never scores.
func TestAJudgeThatCannotBeTrustedFails(t *testing.T) {
	run := judgedRun(t)
	for name, verdict := range map[string]string{
		"a verdict it does not have":  `{"claims":[{"claim":"a","verdict":"unverifiable"}],"invented_citations":0}`,
		"an item it was not given":    `{"claims":[{"claim":"a","verdict":"supported","sources":["c3"]}],"invented_citations":0}`,
		"the answer as its own proof": `{"claims":[{"claim":"a","verdict":"supported","sources":["answer"]}],"invented_citations":0}`,
		"negative citations":          `{"claims":[],"invented_citations":-1}`,
		"no verdict":                  `this is grounded`,
	} {
		scores, failed := evaluate(t, run, eval.Groundedness{Judge: judgeAgent(&judgeModel{verdict: verdict})})
		assert.Empty(t, scores, name)
		assert.Equal(t, []string{"groundedness"}, failed, name)
	}
	scores, failed := evaluate(t, run, eval.UnusedContext{Judge: judgeAgent(&judgeModel{verdict: `{"used":["c9"]}`})})
	assert.Empty(t, scores)
	assert.Equal(t, []string{"unused_context"}, failed, "an item it was not given")

	ok := `{"claims":[],"invented_citations":0}`
	small := judgeAgent(&judgeModel{verdict: ok})
	small.Context = assemble.DefaultBudgets()
	small.Context.Material = 40
	_, failed = evaluate(t, run, eval.Groundedness{Judge: small})
	assert.Equal(t, []string{"groundedness"}, failed, "not shown the whole context")

	for name, j := range map[string]func(*agent.Agent){
		"its Output set":   func(a *agent.Agent) { a.Output = &agent.Output{} },
		"its Material set": func(a *agent.Agent) { a.Material = []content.Untrusted{input("x")} },
	} {
		a := judgeAgent(&judgeModel{verdict: ok})
		j(&a)
		_, failed := evaluate(t, run, eval.Groundedness{Judge: a})
		assert.Equal(t, []string{"groundedness"}, failed, name)
	}
}

// A judge is the program's agent: its own instructions or prompt replace
// bonyan's.
func TestTheProgramsJudgePromptIsUsed(t *testing.T) {
	ok := `{"claims":[],"invented_citations":0}`
	m := &judgeModel{verdict: ok}
	a := judgeAgent(m)
	a.Prompt = &prompt.Prompt{Name: "mine", Version: "2", Text: "MY-JUDGE-3b9"}
	_, failed := evaluate(t, judgedRun(t), eval.Groundedness{Judge: a})
	require.Empty(t, failed)
	assert.Equal(t, "MY-JUDGE-3b9", systemText(m.reqs[0]))

	m = &judgeModel{verdict: ok}
	a = judgeAgent(m)
	a.Instructions = content.Instruction("MY-INSTRUCTIONS-3b9")
	_, failed = evaluate(t, judgedRun(t), eval.Groundedness{Judge: a})
	require.Empty(t, failed)
	assert.Equal(t, "MY-INSTRUCTIONS-3b9", systemText(m.reqs[0]))
}

// A run with no answer is not judged.
func TestARunWithNoAnswerIsNotJudged(t *testing.T) {
	run := judgedRun(t)
	run.End.Outcome = "budget_limit"
	for _, e := range []func(agent.Agent) score.Evaluator{
		func(a agent.Agent) score.Evaluator { return eval.Groundedness{Judge: a} },
		func(a agent.Agent) score.Evaluator { return eval.UnusedContext{Judge: a} },
	} {
		m := &judgeModel{verdict: `{}`}
		scores, failed := evaluate(t, run, e(judgeAgent(m)))
		assert.Empty(t, scores)
		assert.Empty(t, failed)
		assert.Empty(t, m.reqs, "the judge was not asked")
	}
}

// The context the answer did not use is counted, by item and by size.
func TestUnusedContextCountsWhatTheAnswerDidNotUse(t *testing.T) {
	m := &judgeModel{verdict: `{"used":["c2","c2"]}`}
	scores, failed := evaluate(t, judgedRun(t), eval.UnusedContext{Judge: judgeAgent(m)})
	require.Empty(t, failed)
	assert.Equal(t, 2.0, mustScore(t, scores, "unused_context", "items"))
	assert.Equal(t, 1.0, mustScore(t, scores, "unused_context", "unused_items"))
	assert.Equal(t, float64(len(docText)), mustScore(t, scores, "unused_context", "unused_bytes"))
	assert.Equal(t, 0.0, mustScore(t, scores, "unused_context", "unjudged_items"))
	assert.True(t, strings.HasPrefix(systemText(m.reqs[0]), "You find which items"))

	// A run whose context holds nothing but its message is scored without
	// asking.
	plain := &judgeModel{}
	scores, failed = evaluate(t, recorded(t, newAgent(func(model.ChatRequest, int) (model.ChatResponse, error) { return answer("done"), nil }), "hi"), eval.UnusedContext{Judge: judgeAgent(plain)})
	require.Empty(t, failed)
	assert.Equal(t, 0.0, mustScore(t, scores, "unused_context", "items"))
	assert.Empty(t, plain.reqs)
}

// constant scores one metric with a fixed value.
type constant struct {
	name  string
	value float64
}

func (c constant) Name() string { return c.name }
func (c constant) Evaluate(context.Context, score.Subject) ([]score.Score, error) {
	return []score.Score{{Metric: "m", Value: c.value}}, nil
}

// Each labelled metric's agreement with its labels is reported beside the
// scores; a label for an evaluator that did not run is left out, and one that
// was not scored is counted.
func TestAgreementWithHandLabels(t *testing.T) {
	r := eval.Runner{
		Agent:      newAgent(func(model.ChatRequest, int) (model.ChatResponse, error) { return answer("done"), nil }),
		Evaluators: []score.Evaluator{constant{"judge", 1}, failingEvaluator{}, constant{"off", 0}},
	}
	label := func(e string, v float64) score.Label { return score.Label{Evaluator: e, Metric: "m", Value: v} }
	cases := []score.Case{
		{Name: "a", Input: input("a"), Labels: []score.Label{label("judge", 1), label("failing", 1), label("off", 1)}},
		{Name: "b", Input: input("b"), Labels: []score.Label{label("judge", 0)}},
		{Name: "c", Input: input("c"), Labels: []score.Label{label("judge", 3)}},
		{Name: "d", Input: input("d")},
	}
	rep, err := r.Run(context.Background(), cases, eval.Switch{"off": false})
	require.NoError(t, err)
	assert.Equal(t, []eval.Agreement{
		{Evaluator: "judge", Metric: "m", N: 3, Exact: 1.0 / 3, MAE: 1},
		{Evaluator: "failing", Metric: "m", Unscored: 1},
	}, rep.Agreement)

	for name, labels := range map[string][]score.Label{
		"no such evaluator": {label("nobody", 1)},
		"no metric":         {{Evaluator: "judge"}},
		"twice":             {label("judge", 1), label("judge", 0)},
	} {
		_, err := r.Run(context.Background(), []score.Case{{Name: "a", Input: input("a"), Labels: labels}}, nil)
		assert.Error(t, err, name)
	}
}
