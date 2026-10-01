package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/secret"
)

// observer records every event it is called with.
type observer struct {
	mu     sync.Mutex
	events []hook.Event
	err    error
}

func (o *observer) Observe(_ context.Context, ev hook.Event) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, ev)
	return o.err
}

func (o *observer) points() []hook.Point {
	o.mu.Lock()
	defer o.mu.Unlock()
	var ps []hook.Point
	for _, ev := range o.events {
		ps = append(ps, ev.Point)
	}
	return ps
}

// interceptor acts through f and records what it saw.
type interceptor struct {
	mu   sync.Mutex
	seen []hook.Event
	f    func(hook.Event) (hook.Action, error)
}

func (i *interceptor) Observe(context.Context, hook.Event) error { return nil }

func (i *interceptor) Intercept(_ context.Context, ev hook.Event) (hook.Action, error) {
	i.mu.Lock()
	i.seen = append(i.seen, ev)
	i.mu.Unlock()
	return i.f(ev)
}

func act(a hook.Action) *interceptor {
	return &interceptor{f: func(hook.Event) (hook.Action, error) { return a, nil }}
}

func text(s string) *string { return &s }

// eventSink keeps the recorded events.
type eventSink struct {
	mu     sync.Mutex
	events []record.Event
}

func (s *eventSink) Write(e record.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Event != nil {
		s.events = append(s.events, *e.Event)
	}
	return nil
}
func (*eventSink) Full() bool      { return false }
func (*eventSink) Subject() string { return "" }
func (*eventSink) Close() error    { return nil }

// withHooks assembles the hooks attached by points through the registry, as a
// program does, and attaches them and the registry's scrubber to a.
func withHooks(t *testing.T, a *agent.Agent, sink *eventSink, points map[hook.Point][]string, impls map[string]hook.Hook) registry.Components {
	t.Helper()
	r := registry.New()
	require.NoError(t, r.RegisterChat("basic", func(json.RawMessage) (model.Chat, error) {
		return &scripted{steps: stepsOf(answer("unused"))}, nil
	}))
	for name, h := range impls {
		require.NoError(t, r.RegisterHook(name, func(json.RawMessage) (hook.Hook, error) { return h, nil }))
	}
	cfg := registry.Config{Chat: registry.SlotConfig{Impl: "basic"}, Hooks: map[hook.Point][]registry.SlotConfig{}}
	for p, names := range points {
		for _, n := range names {
			cfg.Hooks[p] = append(cfg.Hooks[p], registry.SlotConfig{Impl: n})
		}
	}
	if sink == nil {
		sink = &eventSink{}
	}
	c, err := r.Assemble(cfg, registry.WithSink(sink))
	require.NoError(t, err)
	a.Hooks = c.Hooks
	a.Scrubber = c.Secrets.Scrubber()
	return c
}

func every(name string) map[hook.Point][]string {
	m := map[hook.Point][]string{}
	for _, p := range hook.Points() {
		m[p] = []string{name}
	}
	return m
}

func TestHooksAreCalledAtTheirPointsWithTheirPayloads(t *testing.T) {
	o := &observer{}
	a := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(toolCall("search", `{"q":"x"}`), answer("done"))}})
	withHooks(t, &a, nil, every("o"), map[string]hook.Hook{"o": o})
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.Equal(t, []hook.Point{
		hook.RunStart, hook.UserMessage,
		hook.BeforeModel, hook.AfterModel, hook.BeforeTool, hook.AfterTool,
		hook.BeforeModel, hook.AfterModel, hook.Reply,
		hook.RunEnd,
	}, o.points())

	ev := o.events
	assert.Equal(t, "go", ev[1].Message.Raw())
	assert.Equal(t, content.KindUser, ev[1].Message.Provenance().Kind)
	assert.Len(t, ev[2].Messages, 2, "system and user message")
	assert.Equal(t, "search", ev[3].Response.ToolCalls[0].Name)
	assert.Equal(t, "search", ev[4].Call.Name)
	assert.JSONEq(t, `{"q":"x"}`, string(ev[4].Call.Arguments))
	assert.Equal(t, "found it", ev[5].Result.Raw())
	assert.Equal(t, content.KindTool, ev[5].Result.Provenance().Kind)
	assert.Equal(t, "done", ev[8].Reply)
	assert.Equal(t, "cleared", ev[9].Outcome)
}

func TestRunEndCarriesTheNotClearedReason(t *testing.T) {
	o := &observer{}
	a := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(fail(&model.CallError{Kind: model.ErrRejected}))}})
	withHooks(t, &a, nil, map[hook.Point][]string{hook.RunEnd: {"o"}}, map[string]hook.Hook{"o": o})
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	require.Len(t, o.events, 1)
	assert.Equal(t, string(agent.ReasonModelFailed), o.events[0].Outcome)
}

// An observing hook's failure is recorded and changes nothing.
func TestAFailingObserverChangesNothing(t *testing.T) {
	o := &observer{err: errors.New("broken")}
	sink := &eventSink{}
	a := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}})
	withHooks(t, &a, sink, every("o"), map[string]hook.Hook{"o": o})
	out, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	got, ok := out.Answer()
	require.True(t, ok, out.String())
	assert.Equal(t, "done", got)
	require.NotEmpty(t, sink.events)
	for _, e := range sink.events {
		assert.Equal(t, string(registry.FailError), e.Failure)
		assert.Empty(t, e.Decision)
	}
}

// An interceptor that returns the zero Action lets the payload through.
func TestAnInterceptorThatDoesNothingChangesNothing(t *testing.T) {
	sink := &eventSink{}
	m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	points := map[hook.Point][]string{}
	for _, p := range []hook.Point{hook.UserMessage, hook.BeforeModel, hook.BeforeTool, hook.AfterTool, hook.Reply} {
		points[p] = []string{"pass"}
	}
	pass := act(hook.Action{})
	withHooks(t, &a, sink, points, map[string]hook.Hook{"pass": pass})
	out, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	got, ok := out.Answer()
	require.True(t, ok, out.String())
	assert.Equal(t, "done", got)
	assert.Len(t, pass.seen, 6, "one user message, two requests, one call, its result, the reply")
	assert.Empty(t, sink.events, "nothing changed, denied or failed")
}

func TestAChangeKeepsTheProvenanceOfWhatItReplaced(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("search", `{"q":"x"}`), answer("done"))}
	tl := &tools{out: map[string]string{"search": "found it"}}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Tools = tl
	sink := &eventSink{}
	next := &observer{}
	withHooks(t, &a, sink, map[hook.Point][]string{
		hook.UserMessage: {"msg"},
		hook.BeforeTool:  {"args"},
		hook.AfterTool:   {"result", "next"},
		hook.Reply:       {"reply"},
	}, map[string]hook.Hook{
		"msg":    act(hook.Action{Text: text("rewritten")}),
		"args":   act(hook.Action{Arguments: json.RawMessage(`{"q":"y"}`)}),
		"result": act(hook.Action{Text: text("trimmed")}),
		"next":   next,
		"reply":  act(hook.Action{Text: text("redacted")}),
	})
	out, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)

	user, ok := m.reqs[0].Messages[1].Parts[0].(content.Untrusted)
	require.True(t, ok, "a changed message stays untrusted")
	assert.Equal(t, "rewritten", user.Raw())
	assert.Equal(t, content.Provenance{Kind: content.KindUser, ID: "m1"}, user.Provenance())

	require.Len(t, tl.calls, 1)
	assert.JSONEq(t, `{"q":"y"}`, string(tl.calls[0].Arguments), "the tool runs with the changed arguments")

	res, ok := m.reqs[1].Messages[3].Parts[0].(content.Untrusted)
	require.True(t, ok, "a changed result stays untrusted")
	assert.Equal(t, "trimmed", res.Raw())
	assert.Equal(t, content.KindTool, res.Provenance().Kind)

	require.Len(t, next.events, 1)
	assert.Equal(t, "trimmed", next.events[0].Result.Raw(), "the next hook sees the change")
	assert.Equal(t, content.Provenance{Kind: content.KindTool, ID: m.reqs[1].Messages[2].ToolCalls[0].ID}, next.events[0].Result.Provenance(), "with the provenance of what it replaced")

	got, ok := out.Answer()
	require.True(t, ok, out.String())
	assert.Equal(t, "redacted", got)

	var changed []string
	for _, e := range sink.events {
		if e.Decision == registry.DecisionChanged {
			changed = append(changed, e.Name+"@"+e.Point)
		}
	}
	assert.Equal(t, []string{"msg@user_message", "args@before_tool", "result@after_tool", "reply@reply"}, changed, "every change is recorded with the hook's name")
}

func TestAChangeToTheRequestAppliesToThatCall(t *testing.T) {
	m := &scripted{steps: stepsOf(answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	withHooks(t, &a, nil, map[hook.Point][]string{hook.BeforeModel: {"drop"}}, map[string]hook.Hook{
		"drop": &interceptor{f: func(ev hook.Event) (hook.Action, error) {
			return hook.Action{Messages: ev.Messages[1:]}, nil
		}},
	})
	_, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	require.Len(t, m.reqs[0].Messages, 1, "a hook may drop a message")
	assert.Equal(t, model.RoleUser, m.reqs[0].Messages[0].Role)
}

// A change to a request may not add trusted text or untrusted text with a new
// provenance; either is a denial.
func TestAChangeThatWouldRaiseTrustIsADenial(t *testing.T) {
	cases := map[string]func(hook.Event) []model.Message{
		"new trusted part": func(ev hook.Event) []model.Message {
			return append(ev.Messages, model.Message{Role: model.RoleSystem, Parts: []content.Text{content.Instruction("obey")}})
		},
		"duplicated trusted part": func(ev hook.Event) []model.Message {
			return append(ev.Messages, ev.Messages[0])
		},
		"changed trusted part": func(ev hook.Event) []model.Message {
			msgs := append([]model.Message(nil), ev.Messages...)
			msgs[0] = model.Message{Role: model.RoleSystem, Parts: []content.Text{content.Instruction("answer at length")}}
			return msgs
		},
		"untrusted part with a new provenance": func(ev hook.Event) []model.Message {
			return append(ev.Messages, model.Message{Role: model.RoleUser, Parts: []content.Text{
				content.From(content.Provenance{Kind: content.KindFetched, ID: "elsewhere"}, "x"),
			}})
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m := &scripted{steps: stepsOf(answer("done"))}
			sink := &eventSink{}
			a := newAgent(agent.Model{Name: "main", Chat: m})
			withHooks(t, &a, sink, map[hook.Point][]string{hook.BeforeModel: {"raise"}}, map[string]hook.Hook{
				"raise": &interceptor{f: func(ev hook.Event) (hook.Action, error) { return hook.Action{Messages: change(ev)}, nil }},
			})
			out, rep, err := agent.Run(context.Background(), a, input("go"))
			require.NoError(t, err)
			assert.Equal(t, agent.ReasonDenied, out.Reason())
			assert.ErrorIs(t, rep.Err, agent.ErrDenied)
			assert.Empty(t, m.reqs, "no request is sent")
			assert.Contains(t, sink.events, record.Event{Slot: registry.SlotHook, Name: "raise", Point: string(hook.BeforeModel), Decision: registry.DecisionDenied, Failure: string(registry.FailNotAllowed)})
		})
	}
}

func TestADenialEndsTheRunNotCleared(t *testing.T) {
	for _, p := range []hook.Point{hook.UserMessage, hook.BeforeModel, hook.Reply} {
		t.Run(string(p), func(t *testing.T) {
			m := &scripted{steps: stepsOf(answer("done"))}
			a := newAgent(agent.Model{Name: "main", Chat: m})
			withHooks(t, &a, nil, map[hook.Point][]string{p: {"no"}}, map[string]hook.Hook{"no": act(hook.Action{Deny: true})})
			out, rep, err := agent.Run(context.Background(), a, input("go"))
			require.NoError(t, err, "a denial is an outcome, not Run's error")
			assert.False(t, out.Cleared())
			assert.Equal(t, agent.ReasonDenied, out.Reason())
			var denied *agent.DeniedError
			require.ErrorAs(t, rep.Err, &denied)
			assert.Equal(t, agent.DeniedError{Point: p, Hook: "no"}, *denied)
			if p != hook.Reply {
				assert.Empty(t, m.reqs, "nothing is sent after the denial")
			}
		})
	}
}

func TestADeniedToolCallIsReportedToTheModel(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
	tl := &tools{out: map[string]string{"search": "found it"}}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Tools = tl
	after := &observer{}
	withHooks(t, &a, nil, map[hook.Point][]string{hook.BeforeTool: {"no"}, hook.AfterTool: {"after"}}, map[string]hook.Hook{
		"no": act(hook.Action{Deny: true}), "after": after,
	})
	out, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.True(t, out.Cleared(), "the run goes on")
	assert.Empty(t, tl.calls, "the tool is not called")
	assert.Empty(t, after.events, "no after-tool hook for a call that did not run")
	res := m.reqs[1].Messages[3].Parts[0].(content.Untrusted)
	assert.Equal(t, "error: the call was denied", res.Raw())
}

// After a tool call a hook may change but not deny, so a denial, an error or a
// panic there withholds the result.
func TestAFailureAfterAToolWithholdsTheResult(t *testing.T) {
	for name, h := range map[string]*interceptor{
		"deny":  act(hook.Action{Deny: true}),
		"error": {f: func(hook.Event) (hook.Action, error) { return hook.Action{}, errors.New("broken") }},
		"panic": {f: func(hook.Event) (hook.Action, error) { panic("broken") }},
	} {
		t.Run(name, func(t *testing.T) {
			m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
			sink := &eventSink{}
			a := newAgent(agent.Model{Name: "main", Chat: m})
			withHooks(t, &a, sink, map[hook.Point][]string{hook.AfterTool: {"h"}}, map[string]hook.Hook{"h": h})
			out, _, err := agent.Run(context.Background(), a, input("go"))
			require.NoError(t, err)
			assert.True(t, out.Cleared(), out.String())
			res := m.reqs[1].Messages[3].Parts[0].(content.Untrusted)
			assert.Equal(t, "error: the result was withheld", res.Raw())
			require.Len(t, sink.events, 1)
			assert.Equal(t, registry.DecisionDenied, sink.events[0].Decision)
			if name == "deny" {
				assert.Equal(t, string(registry.FailNotAllowed), sink.events[0].Failure, "denying is not allowed after a tool call")
			}
		})
	}
}

// An interceptor that fails where it may deny counts as a denial.
func TestAFailingInterceptorDenies(t *testing.T) {
	for name, h := range map[string]*interceptor{
		"error":          {f: func(hook.Event) (hook.Action, error) { return hook.Action{}, errors.New("broken") }},
		"panic":          {f: func(hook.Event) (hook.Action, error) { panic("broken") }},
		"two changes":    act(hook.Action{Text: text("a"), Arguments: json.RawMessage(`{}`)}),
		"wrong change":   act(hook.Action{Arguments: json.RawMessage(`{}`)}),
		"invalid change": act(hook.Action{Text: nil, Messages: nil, Arguments: json.RawMessage(`{`)}),
	} {
		t.Run(name, func(t *testing.T) {
			point := hook.UserMessage
			if name == "invalid change" {
				point = hook.BeforeTool
			}
			m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
			tl := &tools{out: map[string]string{"search": "found it"}}
			a := newAgent(agent.Model{Name: "main", Chat: m})
			a.Tools = tl
			withHooks(t, &a, nil, map[hook.Point][]string{point: {"h"}}, map[string]hook.Hook{"h": h})
			out, _, err := agent.Run(context.Background(), a, input("go"))
			require.NoError(t, err)
			if point == hook.BeforeTool {
				assert.Empty(t, tl.calls, "a denied call does not run")
				return
			}
			assert.Equal(t, agent.ReasonDenied, out.Reason())
		})
	}
}

// Hooks at a point run in configured order, each seeing the one before it,
// and the first denial ends the point.
func TestHooksAtAPointChainAndTheFirstDenialWins(t *testing.T) {
	first := &interceptor{f: func(ev hook.Event) (hook.Action, error) { return hook.Action{Text: text(ev.Reply + " one")}, nil }}
	second := &interceptor{f: func(ev hook.Event) (hook.Action, error) { return hook.Action{Text: text(ev.Reply + " two")}, nil }}
	m := &scripted{steps: stepsOf(answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	withHooks(t, &a, nil, map[hook.Point][]string{hook.Reply: {"first", "second"}}, map[string]hook.Hook{"first": first, "second": second})
	out, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	got, _ := out.Answer()
	assert.Equal(t, "done one two", got)

	deny := act(hook.Action{Deny: true})
	after := &observer{}
	a = newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(answer("done"))}})
	withHooks(t, &a, nil, map[hook.Point][]string{hook.Reply: {"deny", "after"}}, map[string]hook.Hook{"deny": deny, "after": after})
	out, rep, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.Equal(t, agent.ReasonDenied, out.Reason())
	assert.ErrorIs(t, rep.Err, agent.ErrDenied)
	assert.Empty(t, after.events, "hooks after a denial do not run")
}

// A hook sees content with resolved secrets removed, and cannot put one back.
func TestHooksNeverHoldASecret(t *testing.T) {
	const value = "s3cret-value-42"
	t.Setenv("HOOK_TEST_SECRET", value)
	m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Tools = &tools{out: map[string]string{"search": "token " + value}}
	h := &interceptor{f: func(hook.Event) (hook.Action, error) { return hook.Action{Text: text("put back " + value)}, nil }}
	c := withHooks(t, &a, nil, map[hook.Point][]string{hook.AfterTool: {"h"}}, map[string]hook.Hook{"h": h})
	_, err := c.Secrets.Scope("HOOK_TEST_SECRET").Resolve(context.Background(), "HOOK_TEST_SECRET")
	require.NoError(t, err)

	_, _, err = agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	require.Len(t, h.seen, 1)
	assert.NotContains(t, h.seen[0].Result.Raw(), value, "scrubbed before the hook")
	res := m.reqs[1].Messages[3].Parts[0].(content.Untrusted)
	assert.Equal(t, "put back [REDACTED]", res.Raw(), "scrubbed again after it")
}

func TestAnInterceptorWhereItCannotActIsRefused(t *testing.T) {
	for _, p := range []hook.Point{hook.RunStart, hook.RunEnd, hook.AfterModel, hook.Approval, hook.MemoryWrite, hook.MemoryRecall} {
		t.Run(string(p), func(t *testing.T) {
			r := registry.New()
			require.NoError(t, r.RegisterChat("basic", func(json.RawMessage) (model.Chat, error) { return &scripted{steps: stepsOf(answer("x"))}, nil }))
			require.NoError(t, r.RegisterHook("h", func(json.RawMessage) (hook.Hook, error) { return act(hook.Action{}), nil }))
			_, err := r.Assemble(registry.Config{
				Chat:  registry.SlotConfig{Impl: "basic"},
				Hooks: map[hook.Point][]registry.SlotConfig{p: {{Impl: "h"}}},
			}, registry.WithSink(&eventSink{}))
			require.ErrorIs(t, err, registry.ErrHookPoint)
		})
	}
}

// Content the loop does not scrub itself is scrubbed before a hook sees it, and
// what a hook puts in a message or request is scrubbed again.
func TestHooksNeverHoldASecretFromTheRunsOwnContent(t *testing.T) {
	const value = "s3cret-value-43"
	t.Setenv("HOOK_TEST_SECRET2", value)
	m := &scripted{steps: stepsOf(answer("echo " + value))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	o := &observer{}
	msg := &interceptor{f: func(hook.Event) (hook.Action, error) { return hook.Action{Text: text("again " + value)}, nil }}
	req := &interceptor{f: func(ev hook.Event) (hook.Action, error) {
		msgs := append([]model.Message(nil), ev.Messages...)
		u := msgs[1].Parts[0].(content.Untrusted)
		msgs[1] = model.Message{Role: model.RoleUser, Parts: []content.Text{content.From(u.Provenance(), u.Raw()+" and "+value)}}
		return hook.Action{Messages: msgs}, nil
	}}
	c := withHooks(t, &a, nil, map[hook.Point][]string{
		hook.UserMessage: {"o", "msg"},
		hook.BeforeModel: {"req"},
		hook.AfterModel:  {"o"},
		hook.Reply:       {"o"},
	}, map[string]hook.Hook{"o": o, "msg": msg, "req": req})
	_, err := c.Secrets.Scope("HOOK_TEST_SECRET2").Resolve(context.Background(), "HOOK_TEST_SECRET2")
	require.NoError(t, err)

	_, _, err = agent.Run(context.Background(), a, input("my "+value))
	require.NoError(t, err)
	require.Len(t, o.events, 3)
	assert.Equal(t, "my [REDACTED]", o.events[0].Message.Raw(), "the input")
	assert.Equal(t, "echo [REDACTED]", o.events[1].Response.Content, "the model's response")
	assert.Equal(t, "echo [REDACTED]", o.events[2].Reply, "the reply")
	user := m.reqs[0].Messages[1].Parts[0].(content.Untrusted)
	assert.Equal(t, "again [REDACTED] and [REDACTED]", user.Raw(), "both changes scrubbed again")
}

// A changed message is scrubbed again on its own, with no later hook to do it.
func TestAChangedMessageIsScrubbedAgain(t *testing.T) {
	const value = "s3cret-value-44"
	t.Setenv("HOOK_TEST_SECRET3", value)
	m := &scripted{steps: stepsOf(answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	c := withHooks(t, &a, nil, map[hook.Point][]string{hook.UserMessage: {"msg"}}, map[string]hook.Hook{
		"msg": act(hook.Action{Text: text("again " + value)}),
	})
	_, err := c.Secrets.Scope("HOOK_TEST_SECRET3").Resolve(context.Background(), "HOOK_TEST_SECRET3")
	require.NoError(t, err)
	_, _, err = agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	user := m.reqs[0].Messages[1].Parts[0].(content.Untrusted)
	assert.Equal(t, "again [REDACTED]", user.Raw())
}

// The hooks scrub with the registry's scrubber whatever scrubber the agent was
// given, so a tool result reaches a hook scrubbed even when the agent's own
// scrubber holds nothing.
func TestHooksScrubWithTheRegistrysScrubber(t *testing.T) {
	const value = "s3cret-value-45"
	t.Setenv("HOOK_TEST_SECRET4", value)
	m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Tools = &tools{out: map[string]string{"search": "token " + value}}
	o := &observer{}
	c := withHooks(t, &a, nil, map[hook.Point][]string{hook.AfterTool: {"o"}}, map[string]hook.Hook{"o": o})
	a.Scrubber = secret.NewScrubber()
	_, err := c.Secrets.Scope("HOOK_TEST_SECRET4").Resolve(context.Background(), "HOOK_TEST_SECRET4")
	require.NoError(t, err)
	_, _, err = agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	require.Len(t, o.events, 1)
	assert.Equal(t, "token [REDACTED]", o.events[0].Result.Raw())
}
