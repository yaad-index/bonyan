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
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/tokenize"
	"github.com/yaad-index/bonyan/trust"
)

// TODO(phase 10, tools and structured output): replace Tools with the tool
// registry, and add the invalid-output non-answer.
// TODO(phase 14, approvals): add the approval hook and the approver-timeout
// non-answer.
// TODO(phase: trust policy, next to context assembly): classify the input and
// every tool result through Agent.Trust, record each decision, add the
// enforcement point before every model request, and drop ErrTrustPolicy.

// ErrTrustPolicy is what Run returns for a trust policy other than the
// default. The loop classifies as the default policy does, and refuses another
// policy rather than ignore it until the trust-policy phase applies it.
var ErrTrustPolicy = errors.New("agent: only the default trust policy is supported until the trust-policy phase")

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
	// Trust is the trust policy; nil means the default. Run refuses any other
	// (ErrTrustPolicy).
	Trust trust.Policy
	// Recorder records every call; nil records nothing.
	Recorder *record.Recorder
	// Hooks are the hooks attached to each point (registry.Components.Hooks);
	// nil runs none. A hook may change or deny where ADR 0001 §12 allows.
	Hooks *registry.Hooks
	// Scrubber removes resolved secrets from tool output before it enters
	// context (ADR 0001 §10); nil means a scrubber holding no values. Pass
	// registry.Components.Secrets.Scrubber(), the one the hooks scrub with.
	Scrubber *secret.Scrubber
}

// ErrDenied is what Report.Err matches when a hook's denial ended the run;
// the error is a *DeniedError.
var ErrDenied = errors.New("agent: denied by a hook")

// DeniedError says which hook ended the run, and at which point.
type DeniedError struct {
	Point hook.Point
	Hook  string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("agent: hook %q denied at %s", e.Hook, e.Point)
}

// Is reports a match for ErrDenied.
func (e *DeniedError) Is(target error) bool { return target == ErrDenied }

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
	if err := defaultPolicy(a.Trust); err != nil {
		return Outcome{}, Report{}, err
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

	r := run{a: a, models: models, scrub: scrub, hooks: a.Hooks, threshold: threshold, seen: map[string]int{}}
	r.hooks.Run(ctx, hook.Event{Point: hook.RunStart})
	out, rep := r.loop(ctx, limits.MaxSteps, input)
	rep.Tokens, rep.Cost = meter.Spent()
	ended := "cleared"
	if !out.Cleared() {
		ended = string(out.Reason())
	}
	r.hooks.Run(ctx, hook.Event{Point: hook.RunEnd, Outcome: ended})
	return out, rep, nil
}

// defaultPolicy accepts no policy, the default one, or the registry's wrapper
// around the default one. The decision rests on the value, never on a name a
// policy reports.
func defaultPolicy(p trust.Policy) error {
	switch p.(type) {
	case nil, trust.Default, *trust.Default:
		return nil
	}
	if registry.IsDefaultPolicy(p) {
		return nil
	}
	if n, ok := p.(interface{ Name() string }); ok {
		return fmt.Errorf("%w: got %q (%T)", ErrTrustPolicy, n.Name(), p)
	}
	return fmt.Errorf("%w: got %T", ErrTrustPolicy, p)
}

type run struct {
	a         Agent
	models    []model.Chat
	scrub     *secret.Scrubber
	hooks     *registry.Hooks
	threshold int
	seen      map[string]int
}

func (r *run) loop(ctx context.Context, maxSteps int, input content.Untrusted) (Outcome, Report) {
	var rep Report
	v := r.hooks.Run(ctx, hook.Event{Point: hook.UserMessage, Message: input})
	if v.Denied != "" {
		rep.Err = &DeniedError{Point: hook.UserMessage, Hook: v.Denied}
		return NotCleared(ReasonDenied), rep
	}
	if len(v.Changed) > 0 {
		input = v.Event.Message
	}
	msgs := []model.Message{
		{Role: model.RoleSystem, Parts: []content.Text{r.a.Instructions}},
		{Role: model.RoleUser, Parts: []content.Text{input}},
	}
	var tools []model.ToolDef
	if r.a.Tools != nil {
		tools = r.a.Tools.Definitions()
	}

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
			answer := resp.Content
			v := r.hooks.Run(ctx, hook.Event{Point: hook.Reply, Reply: answer})
			if v.Denied != "" {
				rep.Err = &DeniedError{Point: hook.Reply, Hook: v.Denied}
				return NotCleared(ReasonDenied), rep
			}
			if len(v.Changed) > 0 {
				answer = v.Event.Reply
			}
			return Answered(answer), rep
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

// call asks each model in turn until one answers. A budget crossing, the
// deadline or a denial ends the run at once rather than moving to the next
// model. A hook's change to the request applies to that call only; the run's
// history keeps what the loop built.
func (r *run) call(ctx context.Context, req model.ChatRequest) (model.ChatResponse, Reason, error) {
	var errs []error
	for _, m := range r.models {
		sent := req
		v := r.hooks.Run(ctx, hook.Event{Point: hook.BeforeModel, Messages: req.Messages})
		if v.Denied != "" {
			return model.ChatResponse{}, ReasonDenied, &DeniedError{Point: hook.BeforeModel, Hook: v.Denied}
		}
		if len(v.Changed) > 0 {
			sent.Messages = v.Event.Messages
		}
		resp, err := m.Chat(ctx, sent)
		if err == nil {
			r.hooks.Run(ctx, hook.Event{Point: hook.AfterModel, Response: resp})
			return resp, "", nil
		}
		r.hooks.Run(ctx, hook.Event{Point: hook.AfterModel})
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

// The texts the model is given in place of a tool's result.
const (
	resultUnknown  = "error: unknown tool"
	resultFailed   = "error: the tool failed"
	resultDenied   = "error: the call was denied"
	resultWithheld = "error: the result was withheld"
)

// runTool runs one call and returns the message carrying its result. The
// result is untrusted and scrubbed of resolved secrets, and a failure is
// reported by kind only, since an error's text can carry content. Hooks before
// the call may change its arguments or deny it; hooks after it may change the
// result, and a failing one withholds it.
func (r *run) runTool(ctx context.Context, tc model.ToolCall) model.Message {
	v := r.hooks.Run(ctx, hook.Event{Point: hook.BeforeTool, Call: tc})
	if v.Denied != "" {
		return model.ToolResult(tc.ID, resultDenied)
	}
	call := tc
	if len(v.Changed) > 0 {
		call.Arguments = v.Event.Call.Arguments
	}
	text := r.callTool(ctx, call)
	result := content.From(content.Provenance{Kind: content.KindTool, ID: tc.ID}, text)
	v = r.hooks.Run(ctx, hook.Event{Point: hook.AfterTool, Call: call, Result: result})
	switch {
	case v.Denied != "":
		text = resultWithheld
	case len(v.Changed) > 0:
		text = v.Event.Result.Raw()
	}
	return model.ToolResult(tc.ID, text)
}

func (r *run) callTool(ctx context.Context, tc model.ToolCall) string {
	if r.a.Tools == nil {
		return resultUnknown
	}
	out, err := r.a.Tools.Call(ctx, tc)
	switch {
	case errors.Is(err, ErrUnknownTool):
		return resultUnknown
	case err != nil:
		return resultFailed
	}
	return r.scrub.Scrub(out)
}
