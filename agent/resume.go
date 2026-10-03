package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/runstore"
)

// The errors Resume returns for a run it cannot take, each distinct from a
// run that ran and did not clear.
var (
	// ErrUnknownRun is a run the run store does not hold: it was never
	// saved, it ended, or it was deleted.
	ErrUnknownRun = errors.New("agent: no saved run has the id")
	// ErrRunClaimed is a run another process resumed first.
	ErrRunClaimed = errors.New("agent: the run is resumed elsewhere")
	// ErrActionStarted is a run whose pending action was decided and went
	// ahead before the process stopped: resuming it could run the action
	// twice, so it is refused.
	ErrActionStarted = errors.New("agent: the run's action was decided already")
	// ErrChanged is a run resumed under different instructions, tool
	// definitions, material or history than it started with.
	ErrChanged = errors.New("agent: the agent changed since the run was saved")
)

// savedVersion is the version of the saved state's format.
const savedVersion = 1

// suspension is what a run knows about its saved state.
type suspension struct {
	// run is the run's identifier and deadline its end.
	run      string
	deadline time.Time
	// token is the run store claim the run holds: empty for the process
	// that ran it from its start, and after it saves again.
	token string
	// saved says the store holds the run's state.
	saved bool
	// err is why the run gave up its action, when it did.
	err error
}

// pending is the action a saved run waits on.
type pending struct {
	// Approval is the action's identifier in the approval store.
	Approval string `json:"approval"`
	// Index is the action's place among its step's tool calls.
	Index int `json:"index"`
	// Call is the action as approval was asked for it: what runs.
	Call call `json:"call"`
	// By is the hook that said the decision is pending.
	By string `json:"by,omitempty"`
	// Until is when the approval timeout ends; zero for none.
	Until time.Time `json:"until"`
}

type call struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func savedCall(c model.ToolCall) call { return call{ID: c.ID, Name: c.Name, Arguments: c.Arguments} }

func (c call) toolCall() model.ToolCall {
	return model.ToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments}
}

// saved is a run's state as the run store keeps it (ADR 0001 §7). It holds no
// recalled memory, and every text in it is scrubbed of resolved secrets.
type saved struct {
	Version int    `json:"version"`
	Run     string `json:"run"`
	// Hashes of the instructions, the tool definitions, the material and the
	// history the run started with.
	Instructions string `json:"instructions"`
	Tools        string `json:"tools"`
	Material     string `json:"material"`
	History      string `json:"history"`
	// Message is the user's message as the run took it, to recall memory
	// with again.
	Message    string         `json:"message"`
	Current    []message      `json:"current"`
	Step       int            `json:"step"`
	Retries    int            `json:"retries"`
	Seen       map[string]int `json:"seen"`
	ApproveAll bool           `json:"approve_all"`
	Trimmed    bool           `json:"trimmed"`
	Tokens     int64          `json:"tokens"`
	Cost       int64          `json:"cost"`
	Pending    pending        `json:"pending"`
}

type message struct {
	Role       model.Role `json:"role"`
	Parts      []part     `json:"parts,omitempty"`
	ToolCalls  []call     `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// part is a saved message part: bonyan's own text, kept as bonyan's, or text
// from a source, kept with its provenance and classified again on resume.
// Label is the section it was in.
type part struct {
	Bonyan bool         `json:"bonyan,omitempty"`
	Label  string       `json:"label,omitempty"`
	Kind   content.Kind `json:"kind,omitempty"`
	Origin content.Kind `json:"origin,omitempty"`
	Server string       `json:"server,omitempty"`
	ID     string       `json:"id,omitempty"`
	Text   string       `json:"text"`
}

// errSecretInAction is why a run is not saved when its pending action, or a
// call of its step after it, holds a resolved secret: those run from the
// saved state, which holds no secret, so they would not run as asked.
var errSecretInAction = errors.New("agent: an action still to run holds a secret")

// save keeps the run's state in the run store while p waits. A run that
// cannot be saved goes on waiting in this process; the failure is recorded.
func (r *run) save(ctx context.Context, p pending) {
	if r.a.RunStore == nil {
		return
	}
	data, err := r.encode(p)
	if err == nil {
		err = r.a.RunStore.Save(ctx, runstore.State{
			Run: r.suspend.run, Subject: r.a.Subject, Saved: time.Now(), Deadline: r.suspend.deadline, Data: data,
		}, r.suspend.token)
	}
	if err != nil {
		r.record(ctx, record.Event{Slot: "runstore", Name: "save", Failure: string(registry.FailError)})
		// An earlier action's state is stale now: removed, so this action
		// waits unsaved in this process and nothing resumes the old one.
		if r.suspend.saved {
			_ = r.a.RunStore.Delete(context.WithoutCancel(ctx), r.suspend.run, r.suspend.token)
			r.suspend.saved = false
		}
		return
	}
	r.suspend.token, r.suspend.saved = "", true
}

// proceed marks the saved run's action decided and going ahead, before
// anything else happens, so a resume can no longer take it. A run that was
// not saved has nothing to mark.
func (r *run) proceed(ctx context.Context) error {
	if !r.suspend.saved {
		return nil
	}
	err := r.a.RunStore.Proceed(context.WithoutCancel(ctx), r.suspend.run, r.suspend.token)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runstore.ErrClaimed):
		r.suspend.err = ErrRunClaimed
	case errors.Is(err, runstore.ErrProceeding):
		r.suspend.err = ErrActionStarted
	default:
		r.suspend.err = fmt.Errorf("agent: the run store: %w", err)
	}
	r.record(ctx, record.Event{Slot: "runstore", Name: "proceed", Failure: string(registry.FailError)})
	return err
}

// finish removes the run's saved state when the run ends. A run another
// process resumed is that process's to remove.
func (r *run) finish(ctx context.Context) {
	if r.suspend.saved {
		_ = r.a.RunStore.Delete(context.WithoutCancel(ctx), r.suspend.run, r.suspend.token)
	}
}

// encode writes the run's state for p, scrubbed.
func (r *run) encode(p pending) ([]byte, error) {
	toRun := []model.ToolCall{p.Call.toolCall()}
	if calls := lastCalls(r.current); p.Index < len(calls) {
		toRun = append(toRun, calls[p.Index+1:]...)
	}
	for _, c := range toRun {
		if r.scrub.Scrub(string(c.Arguments)) != string(c.Arguments) {
			return nil, errSecretInAction
		}
	}
	tokens, cost := r.meter.Spent()
	s := saved{
		Version: savedVersion, Run: r.suspend.run,
		Instructions: r.prompt.Hash, Tools: r.toolsHash, Material: r.materialHash, History: r.historyHash,
		Message: r.scrub.Scrub(r.message), Step: r.step, Retries: r.retries, Seen: r.seen,
		ApproveAll: r.approveAll, Trimmed: r.trimmed, Tokens: tokens, Cost: cost, Pending: p,
	}
	for _, m := range r.current {
		sm := message{Role: m.Role, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			c := savedCall(tc)
			c.Arguments = json.RawMessage(r.scrub.Scrub(string(tc.Arguments)))
			sm.ToolCalls = append(sm.ToolCalls, c)
		}
		for _, p := range m.Parts {
			sm.Parts = append(sm.Parts, r.saveParts("", p)...)
		}
		s.Current = append(s.Current, sm)
	}
	return json.Marshal(s)
}

// saveParts is t as saved parts, in section label.
func (r *run) saveParts(label string, t content.Text) []part {
	switch v := t.(type) {
	case content.Section:
		var out []part
		for _, it := range v.Items() {
			out = append(out, r.saveParts(v.Label(), it)...)
		}
		return out
	case content.Marked:
		return r.saveParts(label, v.Section())
	case content.Untrusted:
		return []part{r.sourced(label, v.Provenance(), v.Raw())}
	case content.Trusted:
		if v.Provenance() == (content.Provenance{}) {
			return []part{{Bonyan: true, Label: label, Text: r.scrub.Scrub(v.String())}}
		}
		return []part{r.sourced(label, v.Provenance(), v.String())}
	}
	return nil
}

func (r *run) sourced(label string, from content.Provenance, text string) part {
	return part{Label: label, Kind: from.Kind, Origin: from.Origin, Server: from.Server, ID: from.ID, Text: r.scrub.Scrub(text)}
}

// restore puts the saved turns back, each part from a source classified
// again under the policy as it is now.
func (r *run) restore(ctx context.Context, msgs []message) {
	r.current = nil
	for _, sm := range msgs {
		m := model.Message{Role: sm.Role, ToolCallID: sm.ToolCallID}
		for _, c := range sm.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, c.toolCall())
		}
		for _, p := range sm.Parts {
			if p.Bonyan {
				m.Parts = append(m.Parts, content.Instruction(p.Text))
				continue
			}
			u := content.From(content.Provenance{Kind: p.Kind, Origin: p.Origin, Server: p.Server, ID: p.ID}, p.Text)
			t := registry.Classify(ctx, r.policy, u)
			if p.Label != "" {
				t = inSection(p.Label, t)
			}
			m.Parts = append(m.Parts, t)
		}
		r.current = append(r.current, m)
	}
}

// hashOf is the hash of v's JSON.
func hashOf(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// contentHashes are the hashes a saved run is compared by: the tool
// definitions, the material and the history.
func contentHashes(a Agent) (tools, material, history string) {
	var defs []model.ToolDef
	if a.Tools != nil {
		defs = a.Tools.Definitions()
	}
	type item struct {
		Kind, Origin    content.Kind
		Server, ID, Txt string
	}
	items := func(ts []content.Text) []item {
		var out []item
		for _, t := range ts {
			switch v := t.(type) {
			case content.Untrusted:
				p := v.Provenance()
				out = append(out, item{p.Kind, p.Origin, p.Server, p.ID, v.Raw()})
			case content.Trusted:
				p := v.Provenance()
				out = append(out, item{p.Kind, p.Origin, p.Server, p.ID, v.String()})
			}
		}
		return out
	}
	mat := make([]content.Text, len(a.Material))
	for i, u := range a.Material {
		mat[i] = u
	}
	type msg struct {
		Role  model.Role
		Items []item
		Calls []model.ToolCall
		ID    string
	}
	var hist []msg
	for _, m := range a.History {
		hist = append(hist, msg{m.Role, items(m.Parts), m.ToolCalls, m.ToolCallID})
	}
	return hashOf(defs), hashOf(items(mat)), hashOf(hist)
}

// Resume takes the run saved under id from a.RunStore and goes on with it
// where it was suspended, waiting on its pending action again with the time
// left on the approval timeout (ADR 0001 §7). a must be the agent the run
// started as: when its instructions, tool definitions, material or history
// differ, Resume returns ErrChanged and leaves the run saved. The run keeps
// its identifier and its deadline, and recalls memory again; the hooks at
// run start and at the approval point are not run again.
//
// A run the store does not hold is ErrUnknownRun, one another process
// resumed is ErrRunClaimed, and one whose action was decided already is
// ErrActionStarted: each is an error, never a run started afresh.
//
// The run is marked before its decided action goes ahead, approved or
// denied, so a process that stops after that point leaves a run Resume
// refuses with ErrActionStarted: whether an approved action ran before the
// stop is for the program to find out, and bonyan never runs it a second
// time. The run's calls after the action are not run again either.
func Resume(ctx context.Context, a Agent, id string) (Outcome, Report, error) {
	if a.RunStore == nil || a.Approvals == nil {
		return Outcome{}, Report{}, errors.New("agent: resuming needs a run store and an approval store")
	}
	s, err := prepare(a)
	if err != nil {
		return Outcome{}, Report{}, err
	}
	token := newID()
	state, err := a.RunStore.Claim(ctx, id, token, time.Now())
	switch {
	case errors.Is(err, runstore.ErrUnknown):
		return Outcome{}, Report{}, fmt.Errorf("%w: %q", ErrUnknownRun, id)
	case errors.Is(err, runstore.ErrClaimed):
		return Outcome{}, Report{}, fmt.Errorf("%w: %q", ErrRunClaimed, id)
	case errors.Is(err, runstore.ErrProceeding):
		return Outcome{}, Report{}, fmt.Errorf("%w: %q", ErrActionStarted, id)
	case err != nil:
		return Outcome{}, Report{}, err
	}
	var sv saved
	if err := json.Unmarshal(state.Data, &sv); err != nil || sv.Version != savedVersion || sv.Run != id {
		return Outcome{}, Report{}, errors.Join(errors.New("agent: the saved run cannot be read"), err, a.RunStore.Save(ctx, state, token))
	}
	tools, material, history := contentHashes(a)
	if sv.Instructions != s.ref.Hash || sv.Tools != tools || sv.Material != material || sv.History != history {
		// Saved again unclaimed, so the agent it started as can resume it.
		return Outcome{}, Report{}, errors.Join(ErrChanged, a.RunStore.Save(ctx, state, token))
	}
	if err := s.meter.Carry(sv.Tokens, sv.Cost); err != nil {
		// Saved again unclaimed, so an agent with room in its budget can.
		return Outcome{}, Report{}, errors.Join(err, a.RunStore.Save(ctx, state, token))
	}

	ctx, cancel := context.WithDeadline(ctx, state.Deadline)
	defer cancel()
	r := s.start(ctx, id, state.Deadline, true)
	r.suspend.token, r.suspend.saved = token, true
	r.step, r.retries, r.seen, r.approveAll, r.trimmed = sv.Step, sv.Retries, sv.Seen, sv.ApproveAll, sv.Trimmed
	if r.seen == nil {
		r.seen = map[string]int{}
	}
	r.message = sv.Message
	if a.Tools != nil {
		r.tools = a.Tools.Definitions()
	}
	out, rep := r.end(r.resume(r.ctx, s.limits.MaxSteps, sv))
	return out, rep, nil
}

// resume goes on with a restored run from its pending action.
func (r *run) resume(ctx context.Context, maxSteps int, sv saved) (Outcome, Report) {
	rep := Report{Steps: r.step}
	r.classifyContext(ctx)
	r.recall(ctx, r.message)
	r.restore(ctx, sv.Current)
	calls := lastCalls(r.current)
	p := sv.Pending
	if p.Index < 0 || p.Index >= len(calls) || calls[p.Index].ID != p.Call.ID {
		rep.Err = errors.New("agent: the saved run's pending action is not in its turns")
		return NotCleared(ReasonNotApproved), rep
	}
	stepCtx, endStep := r.a.Telemetry.Step(ctx, r.step)
	tc, call := calls[p.Index], p.Call.toolCall()
	msg, reason := r.decided(stepCtx, tc, call, r.await(stepCtx, call, p, func() {}))
	if reason := r.ended(tc, reason, &rep); reason != ReasonUnset {
		endStep()
		return NotCleared(reason), rep
	}
	r.current = append(r.current, msg)
	reason = r.calls(stepCtx, calls, p.Index+1, &rep)
	endStep()
	if reason != ReasonUnset {
		return NotCleared(reason), rep
	}
	return r.steps(ctx, maxSteps, r.step+1, rep)
}

// lastCalls are the tool calls of the last assistant turn.
func lastCalls(msgs []model.Message) []model.ToolCall {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant {
			return msgs[i].ToolCalls
		}
	}
	return nil
}
