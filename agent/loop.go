package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yaad-index/bonyan/budget"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/tokenize"
)

// TODO(phase 10, tools and structured output): replace Tools with the tool
// registry, and add the invalid-output non-answer.
// TODO(phase 14, approvals): add the approval hook and the approver-timeout
// non-answer.

// Tools is what the loop needs from the tools an agent may call.
type Tools interface {
	// Definitions describes the tools to the model.
	Definitions() []model.ToolDef
	// Call runs one tool call and returns its output. An error is reported
	// to the model as a failed call; it does not end the run.
	Call(ctx context.Context, call model.ToolCall) (string, error)
}

// ErrUnknownTool is what Tools.Call returns for a name it does not provide.
var ErrUnknownTool = errors.New("agent: unknown tool")

// Model is a chat model with the name the price table and recordings know it
// by.
type Model struct {
	Name string
	Chat model.Chat
}

// DefaultLoopThreshold is how many identical tool calls count as a loop when
// Agent.LoopThreshold is zero.
const DefaultLoopThreshold = 3

// Agent is everything a run needs. Models, Prices, MaxOutputTokens and
// Instructions are required; the rest is optional.
type Agent struct {
	// Models are tried in order: when a call to one fails, the next is asked
	// the same request. The first is the primary.
	Models []Model
	// Prices hold the price of every model in Models.
	Prices budget.PriceTable
	// Counter bounds each call's input for the budget; nil means ByteBound.
	Counter tokenize.Counter
	// Limits bound the run; the zero value means DefaultLimits.
	Limits Limits
	// MaxOutputTokens caps every model call (ADR 0001 §11 requires a cap).
	MaxOutputTokens int
	// Retry is applied to every call, outside the budget and recording, so
	// each attempt is charged and recorded.
	Retry model.RetryPolicy
	// Instructions are the system instructions.
	Instructions content.Trusted
	// Tools the model may call; nil means none.
	Tools Tools
	// LoopThreshold is how many times the same tool call, by name and
	// arguments, may be requested before the run ends as a loop. Zero means
	// DefaultLoopThreshold; a negative value switches detection off, which
	// leaves the step limit in force.
	LoopThreshold int
	// Recorder records every call; nil records nothing.
	Recorder *record.Recorder
	// Hooks observe the run; nil runs none.
	Hooks HookRunner
	// Scrubber removes resolved secrets from tool output before it enters
	// context (ADR 0001 §10); nil means a scrubber holding no values.
	Scrubber *secret.Scrubber
}

// HookRunner calls the hooks attached to a point.
type HookRunner interface {
	Run(ctx context.Context, ev hook.Event)
}

// Report is what a run did, beside its Outcome.
type Report struct {
	Steps  int
	Tokens int64
	Cost   int64
	// Err is the error behind a not-cleared outcome, when there was one.
	Err error
}

// Run runs the agent on input until it answers or cannot. Every way of not
// answering is a not-cleared Outcome with its reason, never a cleared one
// (ADR 0001 §7). The error is only for an agent that cannot run at all.
func Run(ctx context.Context, a Agent, input content.Untrusted) (Outcome, Report, error) {
	limits := a.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if err := limits.Validate(); err != nil {
		return Outcome{}, Report{}, err
	}
	if len(a.Models) == 0 {
		return Outcome{}, Report{}, errors.New("agent: no model")
	}
	if a.MaxOutputTokens <= 0 {
		return Outcome{}, Report{}, budget.ErrNoOutputCap
	}
	for _, m := range a.Models {
		if m.Chat == nil {
			return Outcome{}, Report{}, fmt.Errorf("agent: model %q has no implementation", m.Name)
		}
		if _, err := a.Prices.Lookup(m.Name); err != nil {
			return Outcome{}, Report{}, err
		}
	}
	meter, err := budget.NewMeter(limits.Budget.MaxTokens, limits.Budget.MaxCostMicros, a.Prices)
	if err != nil {
		return Outcome{}, Report{}, err
	}
	counter := a.Counter
	if counter == nil {
		counter = tokenize.ByteBound{}
	}
	scrub := a.Scrubber
	if scrub == nil {
		scrub = secret.NewScrubber()
	}
	threshold := a.LoopThreshold
	if threshold == 0 {
		threshold = DefaultLoopThreshold
	}
	hooks := a.Hooks
	if hooks == nil {
		hooks = noHooks{}
	}

	models := make([]model.Chat, len(a.Models))
	for i, m := range a.Models {
		c := m.Chat
		if a.Recorder != nil {
			c = record.Chat(c, m.Name, a.Recorder)
		}
		models[i] = model.Retry(budget.Chat(c, m.Name, meter, counter), a.Retry)
	}

	ctx, cancel := context.WithTimeout(ctx, limits.Deadline)
	defer cancel()

	r := run{a: a, models: models, scrub: scrub, hooks: hooks, threshold: threshold, seen: map[string]int{}}
	hooks.Run(ctx, hook.Event{Point: hook.RunStart})
	out, rep := r.loop(ctx, limits.MaxSteps, input)
	rep.Tokens, rep.Cost = meter.Spent()
	hooks.Run(ctx, hook.Event{Point: hook.RunEnd})
	return out, rep, nil
}

type noHooks struct{}

func (noHooks) Run(context.Context, hook.Event) {}

type run struct {
	a         Agent
	models    []model.Chat
	scrub     *secret.Scrubber
	hooks     HookRunner
	threshold int
	seen      map[string]int
}

func (r *run) loop(ctx context.Context, maxSteps int, input content.Untrusted) (Outcome, Report) {
	msgs := []model.Message{
		{Role: model.RoleSystem, Parts: []content.Text{r.a.Instructions}},
		{Role: model.RoleUser, Parts: []content.Text{input}},
	}
	r.hooks.Run(ctx, hook.Event{Point: hook.UserMessage})
	var tools []model.ToolDef
	if r.a.Tools != nil {
		tools = r.a.Tools.Definitions()
	}

	var rep Report
	for step := 1; step <= maxSteps; step++ {
		rep.Steps = step
		if err := ctx.Err(); err != nil {
			rep.Err = err
			return NotCleared(ReasonDeadline), rep
		}
		req := model.ChatRequest{Messages: msgs, Tools: tools, MaxOutputTokens: r.a.MaxOutputTokens}
		resp, reason, err := r.call(ctx, req)
		if err != nil {
			rep.Err = err
			return NotCleared(reason), rep
		}
		if len(resp.ToolCalls) == 0 {
			r.hooks.Run(ctx, hook.Event{Point: hook.Reply})
			return Answered(resp.Content), rep
		}

		msgs = append(msgs, model.Message{Role: model.RoleAssistant, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			if r.looping(tc) {
				rep.Err = fmt.Errorf("agent: tool call %q repeated %d times", tc.Name, r.threshold)
				return NotCleared(ReasonLoopDetected), rep
			}
			msgs = append(msgs, r.runTool(ctx, tc))
		}
	}
	return NotCleared(ReasonStepLimit), rep
}

// call asks each model in turn until one answers. A budget crossing or the
// deadline ends the run at once rather than moving to the next model.
func (r *run) call(ctx context.Context, req model.ChatRequest) (model.ChatResponse, Reason, error) {
	var errs []error
	for _, m := range r.models {
		r.hooks.Run(ctx, hook.Event{Point: hook.BeforeModel})
		resp, err := m.Chat(ctx, req)
		r.hooks.Run(ctx, hook.Event{Point: hook.AfterModel})
		if err == nil {
			return resp, "", nil
		}
		switch {
		case errors.Is(err, budget.ErrExceeded), errors.Is(err, budget.ErrUnpriced), errors.Is(err, budget.ErrNoOutputCap):
			return model.ChatResponse{}, ReasonBudgetLimit, err
		case ctx.Err() != nil:
			return model.ChatResponse{}, ReasonDeadline, err
		}
		errs = append(errs, err)
	}
	if len(r.models) == 1 {
		return model.ChatResponse{}, ReasonModelFailed, errs[0]
	}
	return model.ChatResponse{}, ReasonFallbackExhausted, errors.Join(errs...)
}

// looping counts a tool call and reports whether it has now been requested
// threshold times.
func (r *run) looping(tc model.ToolCall) bool {
	if r.threshold < 0 {
		return false
	}
	key := callKey(tc)
	r.seen[key]++
	return r.seen[key] >= r.threshold
}

// callKey identifies a tool call by name and arguments, with the arguments'
// JSON in a canonical form so that key order and spacing do not hide a repeat.
func callKey(tc model.ToolCall) string {
	args := []byte(tc.Arguments)
	var v any
	if json.Unmarshal(tc.Arguments, &v) == nil {
		if b, err := json.Marshal(v); err == nil {
			args = b
		}
	}
	sum := sha256.Sum256(append([]byte(tc.Name+"\x00"), args...))
	return hex.EncodeToString(sum[:])
}

// runTool runs one call and returns the message carrying its result. The
// result is untrusted, scrubbed of resolved secrets, and a failure is reported
// by kind only, since an error's text can carry content.
func (r *run) runTool(ctx context.Context, tc model.ToolCall) model.Message {
	r.hooks.Run(ctx, hook.Event{Point: hook.BeforeTool})
	defer r.hooks.Run(ctx, hook.Event{Point: hook.AfterTool})
	if r.a.Tools == nil {
		return model.ToolResult(tc.ID, "error: unknown tool")
	}
	out, err := r.a.Tools.Call(ctx, tc)
	switch {
	case errors.Is(err, ErrUnknownTool):
		return model.ToolResult(tc.ID, "error: unknown tool")
	case err != nil:
		return model.ToolResult(tc.ID, "error: the tool failed")
	}
	return model.ToolResult(tc.ID, r.scrub.Scrub(out))
}
