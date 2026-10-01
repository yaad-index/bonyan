package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yaad-index/bonyan/assemble"
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
	// History is the conversation before this run, oldest first. It holds no
	// system message and no trusted text, and it is trimmed to the history
	// budget a whole exchange at a time, oldest first.
	History []model.Message
	// Material is retrieved or fetched material for the run, most relevant
	// first; what does not fit its budget is dropped from the end.
	Material []content.Untrusted
	// Context holds the token budget of each section of a call's context; the
	// zero value means assemble.DefaultBudgets.
	Context assemble.Budgets
	// Tools the model may call; nil means none.
	Tools Tools
	// LoopThreshold is how many times the same tool call, by name and
	// arguments, may be requested before the run ends as a loop. Zero means
	// DefaultLoopThreshold; a negative value switches detection off, which
	// leaves the step limit in force.
	LoopThreshold int
	// Trust is the trust policy (registry.Components.Trust); nil means the
	// default. It classifies the input, each tool result, the history and the
	// material where they enter the run, and marks and handles every request
	// at the enforcement point (ADR 0001 §3).
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
	budgets := a.Context
	if budgets == (assemble.Budgets{}) {
		budgets = assemble.DefaultBudgets()
	}
	if err := budgets.Validate(); err != nil {
		return Outcome{}, Report{}, err
	}
	if err := validHistory(a.History); err != nil {
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

	r := run{a: a, models: models, scrub: scrub, hooks: a.Hooks, threshold: threshold, seen: map[string]int{}, budgets: budgets, counter: counter}
	r.policy = registry.GuardPolicy(a.Trust, a.Recorder)
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

type run struct {
	a         Agent
	models    []model.Chat
	scrub     *secret.Scrubber
	hooks     *registry.Hooks
	threshold int
	seen      map[string]int
	budgets   assemble.Budgets
	counter   tokenize.Counter
	policy    trust.Policy
	history   []model.Message
	material  []content.Text
}

// validHistory refuses history the pipeline cannot place.
func validHistory(msgs []model.Message) error {
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			return fmt.Errorf("%w: a system message in the history", assemble.ErrInvalidInput)
		}
		for _, p := range m.Parts {
			if p.Trusted() {
				return fmt.Errorf("%w: trusted text in the history", assemble.ErrInvalidInput)
			}
		}
	}
	return nil
}

func (r *run) loop(ctx context.Context, maxSteps int, input content.Untrusted) (Outcome, Report) {
	var rep Report
	r.classifyContext(ctx)
	message := registry.Classify(ctx, r.policy, input)
	v := r.hooks.Run(ctx, hook.Event{Point: hook.UserMessage, Message: input, Trusted: message.Trusted()})
	if v.Denied != "" {
		rep.Err = &DeniedError{Point: hook.UserMessage, Hook: v.Denied}
		return NotCleared(ReasonDenied), rep
	}
	if len(v.Changed) > 0 {
		message = v.Event.Message
	}
	current := []model.Message{{Role: model.RoleUser, Parts: []content.Text{inSection(labelUser, message)}}}
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
		req, dropped, err := assemble.Build(assemble.Input{
			Instructions: r.a.Instructions,
			Material:     r.material,
			Earlier:      r.history,
			Current:      current,
			Tools:        tools,
		}, r.budgets, r.counter)
		if err != nil {
			rep.Err = err
			return NotCleared(ReasonBudgetLimit), rep
		}
		r.recordDropped(dropped)
		req.MaxOutputTokens = r.a.MaxOutputTokens
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

		current = append(current, model.Message{Role: model.RoleAssistant, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			if r.looping(tc) {
				rep.Err = fmt.Errorf("agent: tool call %q repeated %d times", tc.Name, r.threshold)
				return NotCleared(ReasonLoopDetected), rep
			}
			current = append(current, r.runTool(ctx, tc))
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
		sent, err := registry.Enforce(ctx, r.policy, sent)
		switch {
		case errors.Is(err, registry.ErrPlacement):
			return model.ChatResponse{}, ReasonPlacement, err
		case err != nil:
			return model.ChatResponse{}, ReasonTrustRefused, err
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

// The labels of the sections the loop builds.
const (
	labelUser   = "user message"
	labelResult = "tool result"
)

// recordDropped records every item left out of a call's context: its section,
// source and size. assemble never reports a memory item's ID.
func (r *run) recordDropped(dropped []assemble.Dropped) {
	if r.a.Recorder == nil {
		return
	}
	for _, d := range dropped {
		r.a.Recorder.Event(record.Event{
			Slot: "assemble", Name: d.Section, Decision: "dropped",
			Source: string(d.Source.Kind), Item: d.Source.ID, Tokens: d.Tokens,
		})
	}
}

// classifyContext classifies every untrusted part of the history and every
// material item where they enter the run.
func (r *run) classifyContext(ctx context.Context) {
	r.history = make([]model.Message, len(r.a.History))
	for i, m := range r.a.History {
		parts := make([]content.Text, len(m.Parts))
		for j, p := range m.Parts {
			parts[j] = p
			if u, ok := p.(content.Untrusted); ok {
				parts[j] = registry.Classify(ctx, r.policy, u)
			}
		}
		m.Parts = parts
		r.history[i] = m
	}
	r.material = make([]content.Text, len(r.a.Material))
	for i, u := range r.a.Material {
		r.material[i] = registry.Classify(ctx, r.policy, u)
	}
}

// inSection returns t as a message part: untrusted text inside a section
// labelled label, trusted text as it is.
func inSection(label string, t content.Text) content.Text {
	if u, ok := t.(content.Untrusted); ok {
		return content.NewSection(label, u)
	}
	return t
}

// toolMessage is the message answering call id with t.
func toolMessage(id string, t content.Text) model.Message {
	return model.Message{Role: model.RoleTool, ToolCallID: id, Parts: []content.Text{inSection(labelResult, t)}}
}

// toolText is bonyan's own text in place of a tool's result: untrusted, from
// the call, like the result it stands in for.
func toolText(id, text string) content.Text {
	return content.From(content.Provenance{Kind: content.KindTool, ID: id}, text)
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
		return toolMessage(tc.ID, toolText(tc.ID, resultDenied))
	}
	call := tc
	if len(v.Changed) > 0 {
		call.Arguments = v.Event.Call.Arguments
	}
	text, ran := r.callTool(ctx, call)
	result := content.From(content.Provenance{Kind: content.KindTool, ID: tc.ID}, text)
	var out content.Text = result
	if ran {
		out = registry.Classify(ctx, r.policy, result)
	}
	v = r.hooks.Run(ctx, hook.Event{Point: hook.AfterTool, Call: call, Result: result, Trusted: out.Trusted()})
	switch {
	case v.Denied != "":
		out = toolText(tc.ID, resultWithheld)
	case len(v.Changed) > 0:
		out = v.Event.Result
	}
	return toolMessage(tc.ID, out)
}

// callTool runs tc and returns its scrubbed output and true, or bonyan's own
// text for a call that produced none and false.
func (r *run) callTool(ctx context.Context, tc model.ToolCall) (string, bool) {
	if r.a.Tools == nil {
		return resultUnknown, false
	}
	out, err := r.a.Tools.Call(ctx, tc)
	switch {
	case errors.Is(err, ErrUnknownTool):
		return resultUnknown, false
	case err != nil:
		return resultFailed, false
	}
	return r.scrub.Scrub(out), true
}
