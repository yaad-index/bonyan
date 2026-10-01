package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/approval"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/trust"
)

// approver answers with f and records what it was asked.
type approver struct {
	mu   sync.Mutex
	seen []hook.Event
	f    func(hook.Event) (hook.Answer, error)
}

func (a *approver) Observe(context.Context, hook.Event) error { return nil }

func (a *approver) Answer(_ context.Context, ev hook.Event) (hook.Answer, error) {
	a.mu.Lock()
	a.seen = append(a.seen, ev)
	a.mu.Unlock()
	return a.f(ev)
}

func answers(ans hook.Answer) *approver {
	return &approver{f: func(hook.Event) (hook.Answer, error) { return ans, nil }}
}

// gated is an agent whose search tool needs approval, with the given hooks at
// the approval point, in order.
func gated(t *testing.T, sink *eventSink, hooks ...hook.Hook) (agent.Agent, *scripted, *tools) {
	t.Helper()
	m := &scripted{steps: stepsOf(toolCall("search", `{"q":"x"}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	tl := &tools{out: map[string]string{"search": "found it"}, needs: map[string]bool{"search": true}}
	a.Tools = tl
	names := make([]string, len(hooks))
	impls := map[string]hook.Hook{}
	for i, h := range hooks {
		names[i] = string(rune('a' + i))
		impls[names[i]] = h
	}
	withHooks(t, &a, sink, map[hook.Point][]string{hook.Approval: names}, impls)
	return a, m, tl
}

// approvals are the outcomes recorded at the approval point, as "name:decision".
func approvals(sink *eventSink) []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var out []string
	for _, e := range sink.events {
		if e.Point == string(hook.Approval) && e.Decision != "" {
			out = append(out, e.Name+":"+e.Decision)
		}
	}
	return out
}

func TestAnApprovedCallRuns(t *testing.T) {
	sink := &eventSink{}
	a, _, tl := gated(t, sink, answers(hook.Approve))
	out, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.True(t, out.Cleared())
	assert.Len(t, tl.calls, 1)
	assert.Equal(t, []string{"a:approved"}, approvals(sink))
}

func TestARejectedCallIsReportedAsDenied(t *testing.T) {
	sink := &eventSink{}
	a, m, tl := gated(t, sink, answers(hook.Reject))
	out, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.True(t, out.Cleared(), "the model is told and the run goes on")
	assert.Empty(t, tl.calls)
	assert.Equal(t, "error: the call was denied", mustItem(t, m.reqs[1].Messages[3].Parts[0]).Raw())
	assert.Equal(t, []string{"a:denied"}, approvals(sink))
}

// Nothing that decides lets an action through: no hook, an observing hook, or
// approvers that abstain cancel it at once, and the run is not cleared.
func TestAnUndecidedActionIsCancelledAtOnce(t *testing.T) {
	for name, hooks := range map[string][]hook.Hook{
		"no hook":          nil,
		"an observer only": {&observer{}},
		"abstentions":      {answers(hook.Abstain), answers(hook.Abstain)},
	} {
		t.Run(name, func(t *testing.T) {
			sink := &eventSink{}
			a, _, tl := gated(t, sink, hooks...)
			a.Approvals = approval.NewMemory()
			start := time.Now()
			out, rep, err := agent.Run(context.Background(), a, input("go"))
			require.NoError(t, err)
			assert.Equal(t, agent.ReasonNotApproved, out.Reason())
			require.Error(t, rep.Err)
			assert.Empty(t, tl.calls)
			assert.Less(t, time.Since(start), time.Second, "cancelled at once, not after a wait")
			assert.Equal(t, []string{":cancelled"}, approvals(sink), "recorded with no hook named")
		})
	}
}

// The first rejection wins over an approval, whatever the order.
func TestARejectionWinsOverAnApproval(t *testing.T) {
	for name, hooks := range map[string][]hook.Hook{
		"approve then reject": {answers(hook.Approve), answers(hook.Reject)},
		"reject then approve": {answers(hook.Reject), answers(hook.Approve)},
	} {
		t.Run(name, func(t *testing.T) {
			a, _, tl := gated(t, nil, hooks...)
			_, _, err := agent.Run(context.Background(), a, input("go"))
			require.NoError(t, err)
			assert.Empty(t, tl.calls)
		})
	}
}

// A failing approver rejects.
func TestAFailingApproverRejects(t *testing.T) {
	sink := &eventSink{}
	failing := &approver{f: func(hook.Event) (hook.Answer, error) { return hook.Approve, errors.New("down") }}
	a, _, tl := gated(t, sink, failing, answers(hook.Approve))
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.Empty(t, tl.calls)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var failed bool
	for _, e := range sink.events {
		if e.Point == string(hook.Approval) && e.Name == "a" && e.Decision == registry.DecisionDenied && e.Failure != "" {
			failed = true
		}
	}
	assert.True(t, failed, "recorded as a rejection with its failure")
}

// The approver gets the call as it will run, scrubbed, with why it needs
// approval.
func TestTheApproverSeesTheCallAsItWillRun(t *testing.T) {
	ap := answers(hook.Approve)
	a, _, tl := gated(t, nil, ap)
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	require.Len(t, ap.seen, 1)
	ev := ap.seen[0]
	assert.Equal(t, hook.Approval, ev.Point)
	assert.Equal(t, "search", ev.Call.Name)
	assert.JSONEq(t, `{"q":"x"}`, string(ev.Call.Arguments))
	assert.Equal(t, hook.ReasonTool, ev.Approval.Reason)
	assert.NotEmpty(t, ev.Approval.ID)
	require.Len(t, tl.calls, 1)
	assert.Equal(t, ev.Call.Arguments, tl.calls[0].Arguments, "what is approved is what runs")
}

// A pending action waits for the program's decision through the store.
func TestAPendingActionWaitsForTheDecision(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(map[bool]string{true: "approved", false: "rejected"}[approve], func(t *testing.T) {
			sink := &eventSink{}
			store := approval.NewMemory()
			a, _, tl := gated(t, sink, answers(hook.Pending))
			a.Approvals = store
			go func() {
				for {
					held, _ := store.List(context.Background())
					if len(held) == 1 {
						assert.Equal(t, "search", held[0].Tool)
						assert.NoError(t, store.Decide(context.Background(), held[0].ID, approve))
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			out, _, err := agent.Run(context.Background(), a, input("go"))
			require.NoError(t, err)
			assert.True(t, out.Cleared())
			if approve {
				assert.Len(t, tl.calls, 1)
				assert.Equal(t, []string{"a:approved"}, approvals(sink))
			} else {
				assert.Empty(t, tl.calls)
				assert.Equal(t, []string{"a:denied"}, approvals(sink))
			}
		})
	}
}

// A pending action that gets no decision in time is cancelled, and a decision
// arriving after that is refused as unknown, never applied.
func TestAPendingActionTimesOut(t *testing.T) {
	sink := &eventSink{}
	store := approval.NewMemory()
	a, _, tl := gated(t, sink, answers(hook.Pending))
	a.Approvals = store
	a.Limits = agent.DefaultLimits()
	a.Limits.ApprovalTimeout = 50 * time.Millisecond
	out, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonNotApproved, out.Reason())
	assert.Empty(t, tl.calls)
	assert.Equal(t, []string{"a:timed out"}, approvals(sink))
	held, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, held, "the action is no longer held")
}

// A decision for an action the store does not hold is refused: a restart
// leaves an in-memory store empty, so every action pending before it is
// cancelled, and a late decision is unknown.
func TestALateDecisionIsUnknown(t *testing.T) {
	before := approval.NewMemory()
	_, err := before.Hold(context.Background(), approval.Pending{ID: "a1", Tool: "search"})
	require.NoError(t, err)
	after := approval.NewMemory() // the process restarted
	require.ErrorIs(t, after.Decide(context.Background(), "a1", true), approval.ErrUnknown)
	require.NoError(t, before.Decide(context.Background(), "a1", true))
	require.ErrorIs(t, before.Decide(context.Background(), "a1", true), approval.ErrUnknown, "decided once only")
}

// With no store, a pending answer cancels the action at once.
func TestPendingWithNoStoreCancels(t *testing.T) {
	sink := &eventSink{}
	a, _, tl := gated(t, sink, answers(hook.Pending))
	out, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonNotApproved, out.Reason())
	assert.Empty(t, tl.calls)
	assert.Equal(t, []string{"a:cancelled"}, approvals(sink))
}

// requireApproval is a policy that requires approval once a request carries
// untrusted content.
type requireApproval struct{ trust.Default }

func (requireApproval) Handle(context.Context, []trust.Item) (trust.Handling, error) {
	return trust.Handling{RequireApproval: true}, nil
}

// The policy's handling can require approval for every tool call from then
// on, for tools that do not ask for it themselves.
func TestThePolicyCanRequireApproval(t *testing.T) {
	ap := answers(hook.Reject)
	m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Trust = requireApproval{}
	withHooks(t, &a, nil, map[hook.Point][]string{hook.Approval: {"a"}}, map[string]hook.Hook{"a": ap})
	tl := a.Tools.(*tools)
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	require.Len(t, ap.seen, 1)
	assert.Equal(t, hook.ReasonPolicy, ap.seen[0].Approval.Reason)
	assert.Empty(t, tl.calls)
}

// An approver can only be attached at the approval point, where it is the
// only kind of hook that can let an action through.
func TestAnApproverOnlyAttachesAtTheApprovalPoint(t *testing.T) {
	r := registry.New()
	require.NoError(t, r.RegisterChat("basic", func(json.RawMessage) (model.Chat, error) { return &scripted{}, nil }))
	require.NoError(t, r.RegisterHook("ap", func(json.RawMessage) (hook.Hook, error) { return answers(hook.Approve), nil }))
	_, err := r.Assemble(registry.Config{
		Chat:  registry.SlotConfig{Impl: "basic"},
		Hooks: map[hook.Point][]registry.SlotConfig{hook.BeforeTool: {{Impl: "ap"}}},
	}, registry.WithSink(&eventSink{}))
	require.ErrorIs(t, err, registry.ErrHookPoint)
}

func TestANegativeApprovalTimeoutIsInvalid(t *testing.T) {
	l := agent.DefaultLimits()
	l.ApprovalTimeout = -time.Second
	require.ErrorIs(t, l.Validate(), agent.ErrInvalidLimits)
}

// The approver sees the call with every resolved secret removed.
func TestTheApproverSeesNoSecret(t *testing.T) {
	t.Setenv("BONYAN_APPROVAL_TEST_KEY", "s3cr3t-approval-value")
	ap := answers(hook.Approve)
	m := &scripted{steps: stepsOf(toolCall("search", `{"key":"s3cr3t-approval-value"}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Tools = &tools{out: map[string]string{"search": "found it"}, needs: map[string]bool{"search": true}}
	c := withHooks(t, &a, nil, map[hook.Point][]string{hook.Approval: {"a"}}, map[string]hook.Hook{"a": ap})
	_, err := c.Secrets.Scope("BONYAN_APPROVAL_TEST_KEY").Resolve(context.Background(), "BONYAN_APPROVAL_TEST_KEY")
	require.NoError(t, err)
	_, _, err = agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	require.Len(t, ap.seen, 1)
	assert.NotContains(t, string(ap.seen[0].Call.Arguments), "s3cr3t-approval-value")
}

// An approval does not let an action through while another approver's answer
// is pending: the action waits for that decision, and runs only if it is not
// a rejection.
func TestAnApprovalWaitsForAPendingAnswer(t *testing.T) {
	for _, approve := range []bool{false, true} {
		t.Run(map[bool]string{true: "then approved", false: "then rejected"}[approve], func(t *testing.T) {
			sink := &eventSink{}
			store := approval.NewMemory()
			a, _, tl := gated(t, sink, answers(hook.Approve), answers(hook.Pending))
			a.Approvals = store
			go func() {
				for {
					held, _ := store.List(context.Background())
					if len(held) == 1 {
						assert.NoError(t, store.Decide(context.Background(), held[0].ID, approve))
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			_, _, err := agent.Run(context.Background(), a, input("go"))
			require.NoError(t, err)
			if approve {
				assert.Len(t, tl.calls, 1)
				assert.Equal(t, []string{"b:approved"}, approvals(sink))
			} else {
				assert.Empty(t, tl.calls, "the earlier approval did not let it through")
				assert.Equal(t, []string{"b:denied"}, approvals(sink))
			}
		})
	}
}
