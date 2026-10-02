package eval

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/assemble"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/prompt"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/tool"
)

// Rerun runs a recorded run's input again against live models, Times times
// for each variant, and scores every run with the same evaluators into one
// report, so a flaky answer or a regression can be looked at after the fact
// (ADR 0001 §8).
//
// A re-run repeats no side effect unless told to: tool calls are answered from
// the recording, matched by name and arguments, and a call with no recorded
// result ends that run as agent.ReasonReplayMismatch instead of executing;
// hooks do not run (§12); memory is neither recalled nor written; and an action
// that needs approval is never approved by the re-run itself, so with no hook
// opted in to approve it the run ends not approved. Re-runs spend real budget
// under the agent's limits.
type Rerun struct {
	// Agent is what each run uses: its models, prices, limits, instructions
	// and tools. It must have no Recorder; each run is recorded by the re-run.
	Agent agent.Agent
	// Evaluators score every run.
	Evaluators []score.Evaluator
	// Times is how often each variant runs; zero means once.
	Times int
	// Variants are the grid. None means one variant: the agent as it is.
	Variants []Variant
	// Models are models a variant may name besides the agent's own. Each grid
	// model receives the recorded input, which can be private, so a model is
	// used only when it is the agent's or named here.
	Models []agent.Model
	// LiveTools are the tools whose calls execute for real instead of being
	// answered from the recording.
	LiveTools []string
	// Hooks are the hooks that run, by name; the others do not.
	Hooks []string
}

// Variant is one point of the grid.
type Variant struct {
	// Name names the variant in the report.
	Name string
	// Model, when set, is the one model the variant's runs use, by name.
	Model string
	// Context, when set, replaces the agent's context budgets.
	Context assemble.Budgets
	// Prompt, when set, replaces the agent's instructions or prompt, so a
	// grid can compare prompt versions.
	Prompt *prompt.Prompt
}

// RerunReport is what a re-run found. Each result's Case is the variant's
// name and the run's number, as "name#1".
type RerunReport struct {
	Report
	// MemoryExcluded says the recording left recalled memory out, so the
	// runs had less context than the original.
	MemoryExcluded bool
}

// Run re-runs recorded, a run read from a recording (record.ReadRuns).
func (r Rerun) Run(ctx context.Context, recorded record.Run) (RerunReport, error) {
	if r.Agent.Recorder != nil {
		return RerunReport{}, errors.New("eval: the re-run records each run itself; leave Agent.Recorder nil")
	}
	if err := checkNames(r.Evaluators); err != nil {
		return RerunReport{}, err
	}
	if r.Times < 0 {
		return RerunReport{}, errors.New("eval: Times must not be negative")
	}
	times := max(r.Times, 1)
	input, err := recordedInput(recorded)
	if err != nil {
		return RerunReport{}, err
	}
	variants := r.Variants
	if len(variants) == 0 {
		variants = []Variant{{Name: "as configured"}}
	}
	models := map[string]agent.Model{}
	for _, m := range append(append([]agent.Model{}, r.Agent.Models...), r.Models...) {
		models[m.Name] = m
	}
	names := map[string]bool{}
	for _, v := range variants {
		if v.Name == "" || names[v.Name] {
			return RerunReport{}, fmt.Errorf("eval: a variant has no name, or the name %q twice", v.Name)
		}
		names[v.Name] = true
		if _, ok := models[v.Model]; v.Model != "" && !ok {
			return RerunReport{}, fmt.Errorf("eval: variant %q names model %q, which is neither the agent's nor in Models", v.Name, v.Model)
		}
	}
	scrub := r.Agent.Scrubber
	if scrub == nil {
		scrub = secret.NewScrubber()
	}
	live := map[string]bool{}
	for _, n := range r.LiveTools {
		live[n] = true
	}

	out := RerunReport{MemoryExcluded: excludesMemory(recorded)}
	for _, v := range variants {
		a := r.Agent
		if v.Model != "" {
			a.Models = []agent.Model{models[v.Model]}
		}
		if v.Context != (assemble.Budgets{}) {
			a.Context = v.Context
		}
		if v.Prompt != nil {
			a.Instructions, a.Prompt = content.Trusted{}, v.Prompt
		}
		a.Hooks = a.Hooks.Only(r.Hooks...)
		a.Memory, a.Session, a.Approvals = nil, "", nil
		for i := 1; i <= times; i++ {
			// Every run gets a fresh replay, so each answers the recording
			// from its start.
			a.Tools = newReplayTools(recorded, r.Agent.Tools, live)
			res, err := runRecorded(ctx, a, input, r.Evaluators, scrub)
			if err != nil {
				return RerunReport{}, fmt.Errorf("eval: variant %q, run %d: %w", v.Name, i, err)
			}
			res.Case = fmt.Sprintf("%s#%d", v.Name, i)
			out.Cases = append(out.Cases, res)
		}
	}
	out.Aggregates = aggregate(out.Cases)
	return out, nil
}

// runRecorded runs a on input with a recording of its own, marked as
// evaluation so it never feeds a queue, and scores the run it holds.
func runRecorded(ctx context.Context, a agent.Agent, input content.Untrusted, evaluators []score.Evaluator, scrub *secret.Scrubber) (Result, error) {
	sink := newMemSink()
	rec, err := record.NewRecorder(sink, scrub)
	if err != nil {
		return Result{}, err
	}
	a.Recorder = rec
	out, report, err := agent.Run(record.WithEvaluation(ctx), a, input)
	if err != nil {
		return Result{}, err
	}
	if rec.WriteFailures() > 0 {
		return Result{}, errors.New("the run's recording is incomplete")
	}
	_, runs, err := record.ReadRuns(sink.reader())
	if err != nil {
		return Result{}, err
	}
	if len(runs) != 1 {
		return Result{}, fmt.Errorf("the run's recording holds %d runs", len(runs))
	}
	answer, answered := out.Answer()
	scores, failed := score.Evaluate(ctx, score.Subject{Run: runs[0], Answer: answer, Answered: answered}, evaluators...)
	return Result{Outcome: out, Report: report, Scores: scores, Failed: failed}, nil
}

// recordedInput is the user's message the recorded run started from, as the
// untrusted text it was, with its source.
func recordedInput(run record.Run) (content.Untrusted, error) {
	for _, c := range run.Calls {
		if c.Kind != record.KindChat || c.Request == nil {
			continue
		}
		for _, m := range c.Request.Messages {
			if m.Role != model.RoleUser {
				continue
			}
			for _, p := range m.Parts {
				for _, it := range append([]record.Part{p}, p.Items...) {
					if it.Provenance != nil && it.Provenance.Kind != content.KindMemory && it.Text != "" && !it.Excluded {
						return content.From(content.Provenance{Kind: it.Provenance.Kind, ID: it.Provenance.ID}, it.Text), nil
					}
				}
			}
		}
		break
	}
	return content.Untrusted{}, errors.New("eval: the recorded run holds no user message to run again")
}

// excludesMemory reports whether any recorded request left memory out.
func excludesMemory(run record.Run) bool {
	var walk func([]record.Part) bool
	walk = func(parts []record.Part) bool {
		for _, p := range parts {
			if p.Excluded || walk(p.Items) {
				return true
			}
		}
		return false
	}
	for _, c := range run.Calls {
		if c.Request == nil {
			continue
		}
		for _, m := range c.Request.Messages {
			if walk(m.Parts) {
				return true
			}
		}
	}
	return false
}

// recordedResult is how a recorded tool call was answered.
type recordedResult struct {
	text    string
	kind    content.Kind
	failure string // the recorded failure's kind, or "" for a result
	known   bool   // whether the recording holds an answer for the call
}

// replayTools answers tool calls from a recorded run: each call is matched by
// name and arguments to the recorded calls in the order they were made, and
// answered as that call was. Tools named live execute through the agent's
// own tools.
type replayTools struct {
	live    agent.Tools
	liveSet map[string]bool
	defs    []model.ToolDef
	mu      sync.Mutex
	byKey   map[string][]recordedResult
	kinds   map[string]content.Kind
}

func newReplayTools(run record.Run, live agent.Tools, liveSet map[string]bool) *replayTools {
	t := &replayTools{live: live, liveSet: liveSet, byKey: map[string][]recordedResult{}, kinds: map[string]content.Kind{}}
	failures := map[string]string{}
	for _, e := range run.Events {
		if e.Slot == record.SlotTool && e.Call != "" {
			failures[e.Call] = e.Failure
		}
	}
	results := map[string]recordedResult{}
	for _, c := range run.Calls {
		if c.Request == nil {
			continue
		}
		if t.defs == nil {
			for _, d := range c.Request.Tools {
				t.defs = append(t.defs, model.ToolDef{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
			}
		}
		for _, m := range c.Request.Messages {
			if m.Role != model.RoleTool || m.ToolCallID == "" {
				continue
			}
			for _, p := range m.Parts {
				for _, it := range append([]record.Part{p}, p.Items...) {
					if it.Provenance != nil && it.Provenance.ID == m.ToolCallID && !it.Excluded {
						results[m.ToolCallID] = recordedResult{text: it.Text, kind: it.Provenance.Kind, known: true}
					}
				}
			}
		}
	}
	for _, c := range run.Calls {
		for _, tc := range c.Response.ToolCalls {
			res := results[tc.ID]
			if f, ok := failures[tc.ID]; ok {
				res = recordedResult{failure: f, known: true}
			}
			key := agent.CallKey(model.ToolCall{Name: tc.Name, Arguments: tc.Arguments})
			t.byKey[key] = append(t.byKey[key], res)
			if res.kind != "" {
				t.kinds[tc.Name] = res.kind
			}
		}
	}
	return t
}

// Definitions are the agent's tools' definitions, or the recorded ones when
// the agent has no tools.
func (t *replayTools) Definitions() []model.ToolDef {
	if t.live != nil {
		return t.live.Definitions()
	}
	return t.defs
}

// Call answers tc as the recorded call it matches was answered, or executes
// it when its tool is live. A call with no recorded answer, or whose recorded
// answer is not the tool's (a denied or withheld call), is a mismatch.
func (t *replayTools) Call(ctx context.Context, tc model.ToolCall) (string, error) {
	if t.liveSet[tc.Name] && t.live != nil {
		return t.live.Call(ctx, tc)
	}
	key := agent.CallKey(tc)
	t.mu.Lock()
	queue := t.byKey[key]
	var res recordedResult
	if len(queue) > 0 {
		res, t.byKey[key] = queue[0], queue[1:]
	}
	t.mu.Unlock()
	switch {
	case !res.known:
		return "", fmt.Errorf("%w: no recorded answer for tool %q", record.ErrMismatch, tc.Name)
	case res.failure == "":
		return res.text, nil
	case res.failure == record.ToolUnknown:
		return "", tool.ErrUnknown
	case res.failure == record.ToolInvalid:
		return "", tool.ErrInvalidArguments
	case res.failure == record.ToolFailed:
		return "", errors.New("eval: the tool failed when recorded")
	}
	return "", fmt.Errorf("%w: tool %q gave no result when recorded (%s)", record.ErrMismatch, tc.Name, res.failure)
}

// Source is the source kind the tool's results were recorded under, so the
// trust policy classifies a replayed result as it classified the original. A
// live tool's output is new, so it is classified by the kind its tool reports
// now.
func (t *replayTools) Source(name string) content.Kind {
	if t.liveSet[name] && t.live != nil {
		return t.live.Source(name)
	}
	if k, ok := t.kinds[name]; ok {
		return k
	}
	if t.live != nil {
		return t.live.Source(name)
	}
	return content.KindTool
}

// NeedsApproval is the agent's tools' answer: a re-run needs approval wherever
// the original did.
func (t *replayTools) NeedsApproval(name string) bool {
	return t.live != nil && t.live.NeedsApproval(name)
}
