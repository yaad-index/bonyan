package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yaad-index/bonyan/approval"
	"github.com/yaad-index/bonyan/assemble"
	"github.com/yaad-index/bonyan/budget"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/telemetry"
	"github.com/yaad-index/bonyan/tokenize"
	"github.com/yaad-index/bonyan/tool"
	"github.com/yaad-index/bonyan/trust"
)

// Tools is what the loop needs from the tools an agent may call.
// *tool.Registry is bonyan's: it validates arguments against each tool's
// schema and gives each tool only the secrets it declared (ADR 0001 §5).
type Tools interface {
	// Definitions describes the tools to the model.
	Definitions() []model.ToolDef
	// Call runs one tool call and returns its output. An error is reported
	// to the model by kind (tool.ErrUnknown, tool.ErrInvalidArguments with
	// its path and rule, or a failure); it does not end the run.
	Call(ctx context.Context, call model.ToolCall) (string, error)
	// Source is the source kind the trust policy classifies the named tool's
	// results under: content.KindTool, or content.KindRemoteTool for a tool on
	// a tool server (ADR 0001 §3).
	Source(name string) content.Kind
	// NeedsApproval reports whether every call of the named tool waits for
	// approval before it runs (ADR 0001 §7).
	NeedsApproval(name string) bool
}

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
	// Output, when set, requires the final answer to be JSON matching a
	// schema (ADR 0001 §6).
	Output *Output
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
	// Recorder records every call, the run's start and end, each tool call
	// that gave no result and each answer sent back for another try; nil
	// records nothing.
	Recorder *record.Recorder
	// Hooks are the hooks attached to each point (registry.Components.Hooks);
	// nil runs none. A hook may change or deny where ADR 0001 §12 allows.
	Hooks *registry.Hooks
	// Scrubber removes resolved secrets from tool output before it enters
	// context (ADR 0001 §10); nil means a scrubber holding no values. Pass
	// registry.Components.Secrets.Scrubber(), the one the hooks scrub with.
	Scrubber *secret.Scrubber
	// Name names the agent in telemetry; empty leaves it out.
	Name string
	// Telemetry emits a span per run, loop step, model call and tool call,
	// and metrics for the model calls (ADR 0001 §9); nil emits nothing.
	Telemetry *telemetry.Telemetry
	// Approvals holds actions whose approval an approver said is pending,
	// until the program decides them through it; nil holds none, so a
	// pending answer cancels the action at once.
	Approvals approval.Store
	// Memory is the agent's memory (registry.Components.Memory); nil means
	// none. At the start of a run the facts about Subject matching the user's
	// message are recalled into the context, and the message is stored as an
	// event of Session (ADR 0001 §4).
	Memory *memory.Store
	// Subject is who the run is about. It is required with Memory.
	Subject string
	// Session is the session the run belongs to; empty stores no event.
	Session string
	// MemoryLimit is how many facts are recalled; zero means
	// DefaultMemoryLimit.
	MemoryLimit int
}

// DefaultMemoryLimit is how many facts a run recalls when Agent.MemoryLimit is
// zero.
const DefaultMemoryLimit = 10

// Output is the structured answer a run must give.
type Output struct {
	// Schema is what the final answer must match. Every request carries it.
	Schema tool.Schema
	// Retries is how many times an answer that does not match is sent back
	// with the reason before the run ends not cleared. Zero means none;
	// negative is invalid.
	Retries int
}

// OutputFor returns the Output for answers of type T.
func OutputFor[T any](retries int) (*Output, error) {
	s, err := tool.SchemaFor[T]()
	if err != nil {
		return nil, err
	}
	return &Output{Schema: s, Retries: retries}, nil
}

// Decode reads a cleared outcome's structured answer into a T.
func Decode[T any](o Outcome) (T, error) {
	var v T
	answer, ok := o.Answer()
	if !ok {
		return v, fmt.Errorf("agent: no answer: %s", o)
	}
	err := json.Unmarshal([]byte(answer), &v)
	return v, err
}

// ErrInvalidOutput is what Report.Err matches when the final answer did not
// match Output's schema after every retry; the error carries the last
// *tool.ValidationError.
var ErrInvalidOutput = errors.New("agent: the answer does not match the schema")

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
	if a.Memory != nil && a.Subject == "" {
		return Outcome{}, Report{}, errors.New("agent: memory needs a subject")
	}
	if a.MemoryLimit < 0 {
		return Outcome{}, Report{}, errors.New("agent: the memory limit must not be negative")
	}
	if a.Output != nil && (a.Output.Schema.IsZero() || a.Output.Retries < 0) {
		return Outcome{}, Report{}, errors.New("agent: an output needs a schema and zero or more retries")
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
		// Inside the budget, so only a call that is sent has a span.
		price, _ := a.Prices.Lookup(m.Name)
		c = a.Telemetry.Chat(c, m.Name, price, scrub.Scrub)
		models[i] = model.Retry(budget.Chat(c, m.Name, meter, counter), a.Retry)
	}

	ctx, cancel := context.WithTimeout(ctx, limits.Deadline)
	defer cancel()
	ctx, endRun := a.Telemetry.Run(ctx, a.Name)
	ctx = record.WithRun(ctx, newID())
	if a.Recorder != nil {
		a.Recorder.Start(ctx, record.Start{Agent: a.Name})
	}

	r := run{a: a, models: models, scrub: scrub, hooks: a.Hooks, threshold: threshold, seen: map[string]int{}, budgets: budgets, counter: counter, approvalTimeout: limits.ApprovalTimeout}
	r.policy = registry.GuardPolicy(a.Trust, a.Recorder)
	r.hooks.Run(ctx, hook.Event{Point: hook.RunStart})
	out, rep := r.loop(ctx, limits.MaxSteps, input)
	rep.Tokens, rep.Cost = meter.Spent()
	ended := "cleared"
	if !out.Cleared() {
		ended = string(out.Reason())
	}
	r.hooks.Run(ctx, hook.Event{Point: hook.RunEnd, Outcome: ended})
	endRun(ended, out.Cleared())
	if a.Recorder != nil {
		a.Recorder.End(ctx, record.End{Outcome: ended, Steps: rep.Steps, Tokens: rep.Tokens, Cost: rep.Cost})
	}
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
	// approveAll is set once the policy's handling required approval: every
	// tool call from then on needs it.
	approveAll      bool
	approvalTimeout time.Duration
	memory          []content.Text
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
	r.useMemory(ctx, input.Provenance(), textOf(message))
	current := []model.Message{{Role: model.RoleUser, Parts: []content.Text{inSection(labelUser, message)}}}
	var tools []model.ToolDef
	if r.a.Tools != nil {
		tools = r.a.Tools.Definitions()
	}

	retries := 0
	endStep := func() {}
	defer func() { endStep() }()
	runCtx := ctx
	for step := 1; step <= maxSteps; step++ {
		endStep()
		var ctx context.Context
		ctx, endStep = r.a.Telemetry.Step(runCtx, step)
		rep.Steps = step
		if err := ctx.Err(); err != nil {
			rep.Err = err
			return NotCleared(ReasonDeadline), rep
		}
		req, dropped, err := assemble.Build(assemble.Input{
			Instructions: r.a.Instructions,
			Memory:       r.memory,
			Material:     r.material,
			Earlier:      r.history,
			Current:      current,
			Tools:        tools,
		}, r.budgets, r.counter)
		if err != nil {
			rep.Err = err
			return NotCleared(ReasonBudgetLimit), rep
		}
		r.recordDropped(ctx, dropped)
		req.MaxOutputTokens = r.a.MaxOutputTokens
		if r.a.Output != nil {
			req.Schema = r.a.Output.Schema.JSON()
		}
		resp, reason, err := r.call(ctx, req)
		if err != nil {
			rep.Err = err
			return NotCleared(reason), rep
		}
		if len(resp.ToolCalls) == 0 {
			answer := resp.Content
			if err := r.checkOutput(answer); err != nil {
				if retries >= r.a.Output.Retries {
					rep.Err = err
					return NotCleared(ReasonInvalidOutput), rep
				}
				retries++
				r.record(ctx, record.Event{Slot: record.SlotOutput, Decision: record.DecisionRetry})
				current = append(current, retryMessage(err))
				continue
			}
			v := r.hooks.Run(ctx, hook.Event{Point: hook.Reply, Reply: answer})
			if v.Denied != "" {
				rep.Err = &DeniedError{Point: hook.Reply, Hook: v.Denied}
				return NotCleared(ReasonDenied), rep
			}
			if len(v.Changed) > 0 {
				answer = v.Event.Reply
				if err := r.checkOutput(answer); err != nil {
					rep.Err = fmt.Errorf("%w (after hook %q)", err, v.Changed[len(v.Changed)-1])
					return NotCleared(ReasonInvalidOutput), rep
				}
			}
			return Answered(answer), rep
		}

		current = append(current, model.Message{Role: model.RoleAssistant, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			if r.looping(tc) {
				rep.Err = fmt.Errorf("agent: tool call %q repeated %d times", tc.Name, r.threshold)
				return NotCleared(ReasonLoopDetected), rep
			}
			msg, approved := r.runTool(ctx, tc)
			if !approved {
				rep.Err = fmt.Errorf("agent: tool call %q got no approval", tc.Name)
				return NotCleared(ReasonNotApproved), rep
			}
			current = append(current, msg)
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
		sent, approveAll, err := registry.EnforceRequest(ctx, r.policy, sent)
		if approveAll {
			r.approveAll = true
		}
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

// record records ev, when the run is recorded.
func (r *run) record(ctx context.Context, ev record.Event) {
	if r.a.Recorder != nil {
		r.a.Recorder.Event(ctx, ev)
	}
}

// recordDropped records every item left out of a call's context: its section,
// source and size. assemble never reports a memory item's ID.
func (r *run) recordDropped(ctx context.Context, dropped []assemble.Dropped) {
	for _, d := range dropped {
		r.record(ctx, record.Event{
			Slot: "assemble", Name: d.Section, Decision: "dropped",
			Source: string(d.Source.Kind), Item: d.Source.ID, Tokens: d.Tokens,
		})
	}
}

// checkOutput checks answer against the run's Output schema, if it has one.
// The error names a schema path and a rule, never a value from the answer.
func (r *run) checkOutput(answer string) error {
	if r.a.Output == nil {
		return nil
	}
	if err := r.a.Output.Schema.Validate([]byte(answer)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidOutput, err)
	}
	return nil
}

// retryMessage is bonyan's own request for another answer. It reports where
// the answer failed and which rule it broke, and quotes nothing of it, so it
// is trusted text.
func retryMessage(err error) model.Message {
	reason := "it does not match the schema"
	var ve *tool.ValidationError
	if errors.As(err, &ve) {
		reason = ve.Error()
	}
	return model.Message{Role: model.RoleUser, Parts: []content.Text{content.Instruction(
		"Your answer did not match the required schema (" + reason + "). Answer again with JSON that matches it.",
	)}}
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

// useMemory recalls the facts about the run's subject that match message
// into the run's context, then stores message as an event of the session,
// each through its hook point. A denial or a failing hook at either leaves
// nothing recalled or nothing stored, and the run goes on; so does a failure
// of the memory itself, which is recorded.
func (r *run) useMemory(ctx context.Context, from content.Provenance, message string) {
	if r.a.Memory == nil {
		return
	}
	limit := r.a.MemoryLimit
	if limit == 0 {
		limit = DefaultMemoryLimit
	}
	recalled, err := r.a.Memory.Recall(ctx, r.a.Subject, message, limit)
	if err != nil {
		r.memoryFailed(ctx, "recall")
	} else if len(recalled) > 0 {
		v := r.hooks.Run(ctx, hook.Event{Point: hook.MemoryRecall, Memory: recalled})
		switch {
		case v.Denied != "":
		case len(v.Changed) > 0:
			r.memory = v.Event.Memory
		default:
			r.memory = recalled
		}
	}
	if r.a.Session == "" {
		return
	}
	v := r.hooks.Run(ctx, hook.Event{Point: hook.MemoryWrite, Message: content.From(from, message)})
	if v.Denied != "" {
		return
	}
	if len(v.Changed) > 0 {
		message = v.Event.Message.Raw()
	}
	if err := r.a.Memory.Append(ctx, r.a.Subject, r.a.Session, from.Kind, message); err != nil {
		r.memoryFailed(ctx, "write")
	}
}

// memoryFailed records that memory failed at op. The error's text is not
// recorded, since it can carry content.
func (r *run) memoryFailed(ctx context.Context, op string) {
	r.record(ctx, record.Event{Slot: "memory", Name: op, Failure: string(registry.FailError)})
}

// textOf is the text of a user message, trusted or not.
func textOf(t content.Text) string {
	switch v := t.(type) {
	case content.Untrusted:
		return v.Raw()
	case content.Trusted:
		return v.String()
	}
	return ""
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
	resultInvalid  = "error: invalid arguments"
	resultFailed   = "error: the tool failed"
	resultDenied   = "error: the call was denied"
	resultWithheld = "error: the result was withheld"
)

// runTool runs one call and returns the message carrying its result. The
// result is untrusted and scrubbed of resolved secrets, and a failure is
// reported, to the model and in the recording, by kind only, since an error's
// text can carry content. Hooks before the call may change its arguments or
// deny it; hooks after it may change the result, and a failing one withholds
// it.
func (r *run) runTool(ctx context.Context, tc model.ToolCall) (model.Message, bool) {
	v := r.hooks.Run(ctx, hook.Event{Point: hook.BeforeTool, Call: tc})
	if v.Denied != "" {
		r.toolFailed(ctx, tc, record.ToolDenied)
		return toolMessage(tc.ID, toolText(tc.ID, resultDenied)), true
	}
	call := tc
	if len(v.Changed) > 0 {
		call.Arguments = v.Event.Call.Arguments
	}
	// Approval comes after the before-tool hooks, and nothing changes the
	// call between it and the tool: what is approved is what runs.
	if reason, needed := r.needsApproval(call); needed {
		switch r.approve(ctx, call, reason) {
		case hook.Reject:
			r.toolFailed(ctx, call, record.ToolDenied)
			return toolMessage(tc.ID, toolText(tc.ID, resultDenied)), true
		case hook.Abstain:
			return model.Message{}, false
		}
	}
	text, failure := r.callTool(ctx, call)
	ran := failure == ""
	if !ran {
		r.toolFailed(ctx, call, failure)
	}
	kind := content.KindTool
	if ran {
		kind = r.a.Tools.Source(call.Name)
	}
	result := content.From(content.Provenance{Kind: kind, ID: tc.ID}, text)
	var out content.Text = result
	if ran {
		out = registry.Classify(ctx, r.policy, result)
	}
	v = r.hooks.Run(ctx, hook.Event{Point: hook.AfterTool, Call: call, Result: result, Trusted: out.Trusted()})
	switch {
	case v.Denied != "":
		r.toolFailed(ctx, call, record.ToolWithheld)
		out = toolText(tc.ID, resultWithheld)
	case len(v.Changed) > 0:
		out = v.Event.Result
	}
	return toolMessage(tc.ID, out), true
}

// needsApproval reports whether call needs approval, and why.
func (r *run) needsApproval(call model.ToolCall) (hook.ApprovalReason, bool) {
	switch {
	case r.a.Tools != nil && r.a.Tools.NeedsApproval(call.Name):
		return hook.ReasonTool, true
	case r.approveAll:
		return hook.ReasonPolicy, true
	}
	return "", false
}

// approve asks the approval point about call and returns the decision:
// Approve, Reject, or Abstain for an action cancelled because nothing decided
// it in time (ADR 0001 §7, §12).
func (r *run) approve(ctx context.Context, call model.ToolCall, reason hook.ApprovalReason) hook.Answer {
	id := newID()
	v := r.hooks.Approve(ctx, hook.Event{Call: call, Approval: hook.ApprovalRequest{ID: id, Reason: reason}})
	if v.Answer != hook.Pending {
		return v.Answer
	}
	if r.a.Approvals == nil {
		r.hooks.RecordApproval(ctx, v.By, registry.DecisionCancelled)
		return hook.Abstain
	}
	decided, err := r.a.Approvals.Hold(ctx, approval.Pending{ID: id, Tool: call.Name})
	if err != nil {
		r.hooks.RecordApproval(ctx, v.By, registry.DecisionCancelled)
		return hook.Abstain
	}
	wait := ctx
	if r.approvalTimeout > 0 {
		var cancel context.CancelFunc
		wait, cancel = context.WithTimeout(ctx, r.approvalTimeout)
		defer cancel()
	}
	select {
	case ok := <-decided:
		if ok {
			r.hooks.RecordApproval(ctx, v.By, registry.DecisionApproved)
			return hook.Approve
		}
		r.hooks.RecordApproval(ctx, v.By, registry.DecisionDenied)
		return hook.Reject
	case <-wait.Done():
		// Dropped before anything else, so a decision arriving now is refused
		// as unknown rather than applied to an action that will not run.
		_ = r.a.Approvals.Drop(context.WithoutCancel(ctx), id)
		r.hooks.RecordApproval(ctx, v.By, registry.DecisionTimedOut)
		return hook.Abstain
	}
}

// newID names a run, or an action awaiting approval.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// toolFailed records that call gave the model no result, and why.
func (r *run) toolFailed(ctx context.Context, call model.ToolCall, failure string) {
	r.record(ctx, record.Event{Slot: record.SlotTool, Name: call.Name, Failure: failure})
}

// callTool runs tc and returns its scrubbed output, or bonyan's own text for a
// call that produced none with the failure's kind.
func (r *run) callTool(ctx context.Context, tc model.ToolCall) (string, string) {
	if r.a.Tools == nil {
		return resultUnknown, record.ToolUnknown
	}
	ctx, end := r.a.Telemetry.Tool(ctx, tc, r.a.Tools.Source(tc.Name), r.scrub.Scrub)
	out, err := r.a.Tools.Call(ctx, tc)
	end(r.scrub.Scrub(out), err)
	switch {
	case errors.Is(err, tool.ErrUnknown):
		return resultUnknown, record.ToolUnknown
	case errors.Is(err, tool.ErrInvalidArguments):
		// A validation error names a schema path and a rule, never a value.
		var ve *tool.ValidationError
		if errors.As(err, &ve) {
			return resultInvalid + ": " + ve.Error(), record.ToolInvalid
		}
		return resultInvalid, record.ToolInvalid
	case err != nil:
		return resultFailed, record.ToolFailed
	}
	return r.scrub.Scrub(out), ""
}
