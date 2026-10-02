package eval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/eval"
	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/inmem"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/trust"
)

// liveTools is a tool set that counts its real executions, answering out, or
// failing with err, under kind.
type liveTools struct {
	mu    sync.Mutex
	calls int
	out   string
	err   error
	kind  content.Kind
	needs bool
}

func (*liveTools) Definitions() []model.ToolDef {
	return []model.ToolDef{{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

func (t *liveTools) Call(context.Context, model.ToolCall) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	return t.out, t.err
}

func (t *liveTools) Source(string) content.Kind {
	if t.kind == "" {
		return content.KindTool
	}
	return t.kind
}
func (t *liveTools) NeedsApproval(string) bool { return t.needs }

// searchThenAnswer asks for one search, then answers with what the search
// returned, and records every request it saw.
type searchThenAnswer struct {
	mu   sync.Mutex
	args string
	reqs []model.ChatRequest
}

func (s *searchThenAnswer) Chat(_ context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	last := req.Messages[len(req.Messages)-1]
	if last.Role != model.RoleTool {
		return call("search", s.args, len(req.Messages)), nil
	}
	return answer("saw: " + resultText(last)), nil
}

// resultText is the text of a tool result message.
func resultText(m model.Message) string {
	for _, p := range m.Parts {
		var items []content.Untrusted
		switch v := p.(type) {
		case content.Section:
			items = v.Items()
		case content.Marked:
			items = v.Section().Items()
		case content.Trusted:
			return v.String()
		}
		for _, it := range items {
			return it.Raw()
		}
	}
	return ""
}

// resultKind is the source kind of a tool result message's item.
func resultKind(m model.Message) content.Kind {
	for _, p := range m.Parts {
		switch v := p.(type) {
		case content.Section:
			return v.Items()[0].Provenance().Kind
		case content.Marked:
			return v.Section().Items()[0].Provenance().Kind
		case content.Trusted:
			return v.Provenance().Kind
		}
	}
	return ""
}

// recorded runs a once on in, with tools, and returns the run as read back.
func recorded(t *testing.T, a agent.Agent, in string) record.Run {
	t.Helper()
	_, runs, err := record.ReadRuns(bytes.NewReader(recordRun(t, a, in)))
	require.NoError(t, err)
	require.Len(t, runs, 1)
	return runs[0]
}

func searchAgent(model *searchThenAnswer, tl agent.Tools) agent.Agent {
	a := newAgent(nil)
	a.Models = []agent.Model{{Name: "main", Chat: model}}
	a.Tools = tl
	return a
}

// A re-run answers tool calls from the recording and executes nothing.
func TestARerunAnswersToolsFromTheRecording(t *testing.T) {
	orig := &liveTools{out: "recorded result"}
	run := recorded(t, searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, orig), "find x")

	now := &liveTools{out: "LIVE result"}
	m := &searchThenAnswer{args: `{ "q": "x" }`}
	rep, err := eval.Rerun{Agent: searchAgent(m, now), Evaluators: all, Times: 2}.Run(context.Background(), run)
	require.NoError(t, err)
	require.Len(t, rep.Cases, 2)
	assert.Equal(t, []string{"as configured#1", "as configured#2"}, []string{rep.Cases[0].Case, rep.Cases[1].Case})
	for _, res := range rep.Cases {
		a, ok := res.Outcome.Answer()
		require.True(t, ok, "%s", res.Outcome)
		assert.Equal(t, "saw: recorded result", a, "each run answers from the recording's start")
	}
	assert.Zero(t, now.calls, "nothing executed")
	assert.Equal(t, 2, aggregate(t, rep.Report, "outcome", "cleared").N)
	assert.Equal(t, "find x", userText(m.reqs[0]), "the recorded input")
}

// A call the recording has no answer for ends that run, and executes nothing.
func TestAnUnmatchedCallEndsTheRun(t *testing.T) {
	run := recorded(t, searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "r"}), "find x")
	now := &liveTools{out: "LIVE"}
	rep, err := eval.Rerun{Agent: searchAgent(&searchThenAnswer{args: `{"q":"y"}`}, now), Evaluators: all}.Run(context.Background(), run)
	require.NoError(t, err)
	res := rep.Cases[0]
	assert.Equal(t, agent.ReasonReplayMismatch, res.Outcome.Reason())
	require.ErrorIs(t, res.Report.Err, record.ErrMismatch)
	assert.Zero(t, now.calls)
	assert.Equal(t, 1.0, mustScore(t, res.Scores, "waste", "failed_tool_calls"), "recorded as a tool call with no result")
}

// A tool opted in as live executes for real.
func TestALiveToolExecutes(t *testing.T) {
	run := recorded(t, searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "recorded"}), "find x")
	now := &liveTools{out: "LIVE"}
	rep, err := eval.Rerun{Agent: searchAgent(&searchThenAnswer{args: `{"q":"y"}`}, now), LiveTools: []string{"search"}}.Run(context.Background(), run)
	require.NoError(t, err)
	a, _ := rep.Cases[0].Outcome.Answer()
	assert.Equal(t, "saw: LIVE", a)
	assert.Equal(t, 1, now.calls)
}

// A recorded failure is replayed as the same kind of failure.
func TestARecordedFailureIsReplayedAsTheSameKind(t *testing.T) {
	for name, err := range map[string]error{"failed": errors.New("down"), "unknown_tool": toolErrUnknown} {
		t.Run(name, func(t *testing.T) {
			run := recorded(t, searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{err: err}), "find x")
			m := &searchThenAnswer{args: `{"q":"x"}`}
			rep, rerr := eval.Rerun{Agent: searchAgent(m, &liveTools{out: "LIVE"}), Evaluators: all}.Run(context.Background(), run)
			require.NoError(t, rerr)
			res := rep.Cases[0]
			require.True(t, res.Outcome.Cleared(), "the run goes on, as it did")
			assert.Equal(t, 1.0, mustScore(t, res.Scores, "waste", "failed_tool_calls"))
			assert.True(t, strings.HasPrefix(resultText(m.reqs[1].Messages[len(m.reqs[1].Messages)-1]), "error: "), "the model sees bonyan's failure text")
		})
	}
}

var toolErrUnknown = func() error {
	_, err := tools{}.Call(context.Background(), model.ToolCall{Name: "nothing"})
	return err
}()

// trustOnly trusts the one source kind it names.
type trustOnly content.Kind

func (k trustOnly) Classify(_ context.Context, p content.Provenance) (trust.Decision, error) {
	if p.Kind == content.Kind(k) {
		return trust.Decision{Verdict: trust.Trusted}, nil
	}
	return trust.Decision{Verdict: trust.Untrusted}, nil
}

// A replayed result keeps the source kind it was recorded under, so the
// trust policy classifies it as it did in the original run, whatever the
// agent's tools say now.
func TestAReplayedResultKeepsItsSourceKind(t *testing.T) {
	for _, kind := range []content.Kind{content.KindTool, content.KindRemoteTool} {
		t.Run(string(kind), func(t *testing.T) {
			orig := searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "r", kind: kind})
			orig.Trust = trustOnly(content.KindTool)
			run := recorded(t, orig, "find x")

			other := content.KindRemoteTool
			if kind == content.KindRemoteTool {
				other = content.KindTool
			}
			m := &searchThenAnswer{args: `{"q":"x"}`}
			a := searchAgent(m, &liveTools{out: "LIVE", kind: other})
			a.Trust = trustOnly(content.KindTool)
			_, err := eval.Rerun{Agent: a}.Run(context.Background(), run)
			require.NoError(t, err)
			last := m.reqs[1].Messages[len(m.reqs[1].Messages)-1]
			assert.Equal(t, kind, resultKind(last))
			_, trusted := last.Parts[0].(content.Trusted)
			assert.Equal(t, kind == content.KindTool, trusted, "classified as in the original run")
		})
	}
}

// counter counts the events it observes.
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) Observe(context.Context, hook.Event) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return nil
}

func (c *counter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// approving approves every action.
type approving struct{ counter }

func (*approving) Answer(context.Context, hook.Event) (hook.Answer, error) { return hook.Approve, nil }

func withRegistryHooks(t *testing.T, a *agent.Agent, points map[hook.Point][]string, impls map[string]hook.Hook) {
	t.Helper()
	r := registry.New()
	require.NoError(t, r.RegisterChat("basic", func(json.RawMessage) (model.Chat, error) { return &counted{}, nil }))
	cfg := registry.Config{Chat: registry.SlotConfig{Impl: "basic"}, Hooks: map[hook.Point][]registry.SlotConfig{}}
	for name, h := range impls {
		require.NoError(t, r.RegisterHook(name, func(json.RawMessage) (hook.Hook, error) { return h, nil }))
	}
	for p, names := range points {
		for _, n := range names {
			cfg.Hooks[p] = append(cfg.Hooks[p], registry.SlotConfig{Impl: n})
		}
	}
	c, err := r.Assemble(cfg, registry.WithSink(&discard{}))
	require.NoError(t, err)
	a.Hooks, a.Scrubber = c.Hooks, c.Secrets.Scrubber()
}

// Hooks do not run in a re-run unless opted in by name.
func TestHooksRunInAReRunOnlyWhenOptedIn(t *testing.T) {
	run := recorded(t, searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "r"}), "find x")
	off, on := &counter{}, &counter{}
	a := searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "LIVE"})
	every := map[hook.Point][]string{}
	for _, p := range hook.Points() {
		if p != hook.Approval {
			every[p] = []string{"off", "on"}
		}
	}
	withRegistryHooks(t, &a, every, map[string]hook.Hook{"off": off, "on": on})

	_, err := eval.Rerun{Agent: a, Hooks: []string{"on"}}.Run(context.Background(), run)
	require.NoError(t, err)
	assert.Zero(t, off.count(), "a hook not opted in never fires")
	assert.Positive(t, on.count(), "an opted-in hook fires")
}

// An action that needs approval is not approved by the re-run: with no
// approver opted in, the run ends not approved; an opted-in approver decides.
func TestAReRunNeverApprovesItself(t *testing.T) {
	gated := &liveTools{out: "r", needs: true}
	orig := searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, gated)
	approver := &approving{}
	withRegistryHooks(t, &orig, map[hook.Point][]string{hook.Approval: {"approver"}}, map[string]hook.Hook{"approver": approver})
	run := recorded(t, orig, "find x")

	a := searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "LIVE", needs: true})
	a.Hooks, a.Scrubber = orig.Hooks, orig.Scrubber
	rep, err := eval.Rerun{Agent: a}.Run(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonNotApproved, rep.Cases[0].Outcome.Reason())

	rep, err = eval.Rerun{Agent: a, Hooks: []string{"approver"}}.Run(context.Background(), run)
	require.NoError(t, err)
	assert.True(t, rep.Cases[0].Outcome.Cleared(), "%s", rep.Cases[0].Outcome)
}

// A grid runs every variant Times times; a variant may only name a model the
// agent has or one named for the re-run.
func TestTheGrid(t *testing.T) {
	run := recorded(t, searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "r"}), "find x")
	backup := &searchThenAnswer{args: `{"q":"x"}`}
	extra := &searchThenAnswer{args: `{"q":"x"}`}
	a := searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{})
	a.Models = append(a.Models, agent.Model{Name: "backup", Chat: backup})
	a.Prices = maps.Clone(a.Prices)
	a.Prices["extra"] = a.Prices["backup"]
	rr := eval.Rerun{Agent: a, Evaluators: all, Times: 2, Variants: []eval.Variant{
		{Name: "primary"}, {Name: "fallback", Model: "backup"}, {Name: "named", Model: "extra"},
	}, Models: []agent.Model{{Name: "extra", Chat: extra}}}
	rep, err := rr.Run(context.Background(), run)
	require.NoError(t, err)
	var names []string
	for _, c := range rep.Cases {
		names = append(names, c.Case)
		assert.True(t, c.Outcome.Cleared(), c.Case)
	}
	assert.Equal(t, []string{"primary#1", "primary#2", "fallback#1", "fallback#2", "named#1", "named#2"}, names)
	assert.Len(t, backup.reqs, 4, "the fallback variant used only the backup model")
	assert.Len(t, extra.reqs, 4)
	assert.Equal(t, 6, aggregate(t, rep.Report, "outcome", "cleared").N)

	rr.Models = nil
	_, err = rr.Run(context.Background(), run)
	require.Error(t, err, "a model neither the agent's nor named")
}

// A re-run neither recalls nor writes memory, and says when the recording
// left memory out.
func TestAReRunLeavesMemoryAlone(t *testing.T) {
	store, err := memory.NewStore(inmem.New(), memory.Options{Retention: time.Hour})
	require.NoError(t, err)
	require.NoError(t, store.Remember(context.Background(), "ana", content.KindUser, "find things quickly"))
	orig := searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "r"})
	orig.Memory, orig.Subject, orig.Session = store, "ana", "s1"
	run := recorded(t, orig, "find x")
	before, err := store.History(context.Background(), "ana", "s1")
	require.NoError(t, err)

	m := &searchThenAnswer{args: `{"q":"x"}`}
	a := searchAgent(m, &liveTools{})
	a.Memory, a.Subject, a.Session = store, "ana", "s1"
	rep, err := eval.Rerun{Agent: a}.Run(context.Background(), run)
	require.NoError(t, err)
	assert.True(t, rep.MemoryExcluded)
	after, err := store.History(context.Background(), "ana", "s1")
	require.NoError(t, err)
	assert.Len(t, after, len(before), "nothing written")
	for _, req := range m.reqs {
		for _, msg := range req.Messages {
			for _, p := range msg.Parts {
				assert.NotContains(t, resultText(model.Message{Parts: []content.Text{p}}), "quickly", "nothing recalled")
			}
		}
	}
}

func TestTheReRunRefusesWhatItCannotRun(t *testing.T) {
	run := recorded(t, searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "r"}), "find x")
	a := searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{})
	rec, err := record.NewRecorder(&discard{}, secret.NewScrubber())
	require.NoError(t, err)
	withRecorder := a
	withRecorder.Recorder = rec
	for name, tc := range map[string]struct {
		rr  eval.Rerun
		run record.Run
	}{
		"an agent with a recorder": {eval.Rerun{Agent: withRecorder}, run},
		"negative times":           {eval.Rerun{Agent: a, Times: -1}, run},
		"two variants, one name":   {eval.Rerun{Agent: a, Variants: []eval.Variant{{Name: "v"}, {Name: "v"}}}, run},
		"a variant with no name":   {eval.Rerun{Agent: a, Variants: []eval.Variant{{}}}, run},
		"no user message":          {eval.Rerun{Agent: a}, record.Run{}},
		"two evaluators, one name": {eval.Rerun{Agent: a, Evaluators: []score.Evaluator{eval.Loops{}, eval.Loops{}}}, run},
	} {
		_, err := tc.rr.Run(context.Background(), tc.run)
		require.Error(t, err, name)
	}
}

// sequence answers each call with the next of outs.
type sequence struct {
	liveTools
	outs []string
}

func (s *sequence) Call(context.Context, model.ToolCall) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.outs[s.calls]
	s.calls++
	return out, nil
}

// twice asks for the same search twice, then answers with both results.
func twice(reqs *[]model.ChatRequest) *counted {
	return &counted{f: func(req model.ChatRequest, n int) (model.ChatResponse, error) {
		*reqs = append(*reqs, req)
		if n < 2 {
			return call("search", `{"q":"x"}`, n), nil
		}
		var got []string
		for _, m := range req.Messages {
			if m.Role == model.RoleTool {
				got = append(got, resultText(m))
			}
		}
		return answer(strings.Join(got, ",")), nil
	}}
}

// The same call made twice is answered as each recorded call was, in order.
func TestRepeatedCallsAreAnsweredInOrder(t *testing.T) {
	var reqs []model.ChatRequest
	orig := newAgent(nil)
	orig.Models = []agent.Model{{Name: "main", Chat: twice(&reqs)}}
	orig.Tools = &sequence{outs: []string{"first", "second"}}
	orig.LoopThreshold = -1
	run := recorded(t, orig, "find x")

	a := newAgent(nil)
	a.Models = []agent.Model{{Name: "main", Chat: twice(&reqs)}}
	a.Tools = &liveTools{out: "LIVE"}
	a.LoopThreshold = -1
	rep, err := eval.Rerun{Agent: a}.Run(context.Background(), run)
	require.NoError(t, err)
	got, ok := rep.Cases[0].Outcome.Answer()
	require.True(t, ok, "%s", rep.Cases[0].Outcome)
	assert.Equal(t, "first,second", got)
}

// failing is a model that always fails.
type failing struct{ calls int }

func (f *failing) Chat(context.Context, model.ChatRequest) (model.ChatResponse, error) {
	f.calls++
	return model.ChatResponse{}, &model.CallError{Kind: model.ErrTransport, Err: errors.New("down")}
}

// A variant's model is the only model its runs use: when it fails, no other
// model answers in its place.
func TestAVariantUsesOnlyItsModel(t *testing.T) {
	run := recorded(t, searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "r"}), "find x")
	primary, down := &searchThenAnswer{args: `{"q":"x"}`}, &failing{}
	a := searchAgent(primary, &liveTools{})
	a.Models = append(a.Models, agent.Model{Name: "backup", Chat: down})
	rep, err := eval.Rerun{Agent: a, Variants: []eval.Variant{{Name: "backup only", Model: "backup"}}}.Run(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonModelFailed, rep.Cases[0].Outcome.Reason())
	assert.Positive(t, down.calls)
	assert.Empty(t, primary.reqs, "the agent's other model never answered")
}

// A grid naming a model it may not use is refused before any run spends.
func TestABadGridIsRefusedBeforeAnyRun(t *testing.T) {
	run := recorded(t, searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "r"}), "find x")
	primary := &searchThenAnswer{args: `{"q":"x"}`}
	a := searchAgent(primary, &liveTools{})
	_, err := eval.Rerun{Agent: a, Variants: []eval.Variant{{Name: "fine"}, {Name: "bad", Model: "elsewhere"}}}.Run(context.Background(), run)
	require.Error(t, err)
	assert.Empty(t, primary.reqs, "no variant ran")
}

// evaluationMark scores whether the run it reads was recorded as evaluation.
type evaluationMark struct{}

func (evaluationMark) Name() string { return "mark" }
func (evaluationMark) Evaluate(_ context.Context, s score.Subject) ([]score.Score, error) {
	v := 0.0
	if s.Run.Evaluation {
		v = 1
	}
	return []score.Score{{Metric: "evaluation", Value: v}}, nil
}

// Every re-run is recorded as evaluation, so none can feed a live queue.
func TestReRunsAreRecordedAsEvaluation(t *testing.T) {
	run := recorded(t, searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "r"}), "find x")
	rep, err := eval.Rerun{Agent: searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{}), Evaluators: []score.Evaluator{evaluationMark{}}}.Run(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, 1.0, mustScore(t, rep.Cases[0].Scores, "mark", "evaluation"))
}

// A live tool's output is classified by the kind its tool reports now, not
// the recorded kind: a tool recorded as the program's own that now reports a
// tool server is not trusted live.
func TestALiveToolsOutputIsClassifiedByItsKindNow(t *testing.T) {
	orig := searchAgent(&searchThenAnswer{args: `{"q":"x"}`}, &liveTools{out: "r", kind: content.KindTool})
	orig.Trust = trustOnly(content.KindTool)
	run := recorded(t, orig, "find x")

	m := &searchThenAnswer{args: `{"q":"x"}`}
	a := searchAgent(m, &liveTools{out: "LIVE", kind: content.KindRemoteTool})
	a.Trust = trustOnly(content.KindTool)
	_, err := eval.Rerun{Agent: a, LiveTools: []string{"search"}}.Run(context.Background(), run)
	require.NoError(t, err)
	last := m.reqs[1].Messages[len(m.reqs[1].Messages)-1]
	assert.Equal(t, "LIVE", resultText(last))
	assert.Equal(t, content.KindRemoteTool, resultKind(last))
	_, trusted := last.Parts[0].(content.Trusted)
	assert.False(t, trusted)
}
