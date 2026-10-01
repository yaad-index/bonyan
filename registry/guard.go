package registry

import (
	"context"
	"encoding/json"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/trust"
)

// The guards are where ADR 0001's invariants are applied around each slot. In
// this phase they only delegate; the phases that introduce each invariant add it
// here, so no implementation has to, and none can opt out.

type guardedChat struct{ inner model.Chat }

func guardChat(c model.Chat) model.Chat { return guardedChat{inner: c} }

func (g guardedChat) Chat(ctx context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	return g.inner.Chat(ctx, req)
}

type guardedEmbedder struct{ inner model.Embedder }

func guardEmbedder(e model.Embedder) model.Embedder { return guardedEmbedder{inner: e} }

func (g guardedEmbedder) Embed(ctx context.Context, inputs []string) (model.EmbedResponse, error) {
	return g.inner.Embed(ctx, inputs)
}

type guardedClassifier struct{ inner model.Classifier }

func guardClassifier(c model.Classifier) model.Classifier { return guardedClassifier{inner: c} }

func (g guardedClassifier) Classify(ctx context.Context, text content.Untrusted) (model.ClassifyResponse, error) {
	return g.inner.Classify(ctx, text)
}

// guardedPolicy applies ADR 0001 §3's fail-closed rule around a trust policy:
// an error, a panic, the context ending first or no verdict all leave the
// source untrusted. Every decision is recorded, with the failure if there was
// one.
type guardedPolicy struct {
	name  string
	inner trust.Policy
	rec   events
}

func guardPolicy(name string, p trust.Policy, rec events) trust.Policy {
	return guardedPolicy{name: name, inner: p, rec: rec}
}

// Classify never returns an error: a failed classification is an untrusted one.
func (g guardedPolicy) Classify(ctx context.Context, source content.Provenance) (trust.Decision, error) {
	d, failure := call(ctx, func() (trust.Decision, error) { return g.inner.Classify(ctx, source) })
	if failure == "" && d.Verdict != trust.Untrusted && d.Verdict != trust.Trusted {
		failure = FailNoDecision
	}
	if failure != "" {
		d = trust.Decision{Verdict: trust.Untrusted}
	}
	g.rec.Event(record.Event{
		Slot:     SlotTrust,
		Name:     g.name,
		Source:   string(source.Kind),
		Decision: d.Verdict.String(),
		Failure:  string(failure),
	})
	return d, nil
}

// guardedHook runs one hook inside ADR 0001 §12's rules. An observing hook's
// failure is recorded and changes nothing. An Interceptor's failure, or an
// action its point does not allow, is a denial. A change is put back in the
// type and provenance of what it replaces and scrubbed again, so a hook can
// neither raise trust nor put a resolved secret back. Every change and denial
// is recorded with the hook's name.
type guardedHook struct {
	name  string
	inner hook.Hook
	rec   events
	scrub *secret.Scrubber
}

// run calls the hook on ev and returns the event as the hook left it, whether
// it changed it, and whether it denied.
func (g guardedHook) run(ctx context.Context, ev hook.Event) (hook.Event, bool, bool) {
	ic, ok := g.inner.(hook.Interceptor)
	if !ok {
		_, failure := call(ctx, func() (struct{}, error) { return struct{}{}, g.inner.Observe(ctx, ev) })
		if failure != "" {
			g.event(ev.Point, "", failure)
		}
		return ev, false, false
	}
	act, failure := call(ctx, func() (hook.Action, error) { return ic.Intercept(ctx, ev) })
	if failure != "" {
		g.event(ev.Point, DecisionDenied, failure)
		return ev, false, true
	}
	if act.Deny {
		if hook.Rights(ev.Point)&hook.Deny == 0 {
			g.event(ev.Point, DecisionDenied, FailNotAllowed)
		} else {
			g.event(ev.Point, DecisionDenied, "")
		}
		return ev, false, true
	}
	if act.Text == nil && act.Arguments == nil && act.Messages == nil {
		return ev, false, false
	}
	next, ok := g.apply(ev, act)
	if !ok {
		g.event(ev.Point, DecisionDenied, FailNotAllowed)
		return ev, false, true
	}
	g.event(ev.Point, DecisionChanged, "")
	return next, true, false
}

// apply puts act's change into ev, or reports that the point does not allow
// it or that it would raise trust. Each kind of change is accepted only at the
// point Rights gives a change to.
func (g guardedHook) apply(ev hook.Event, act hook.Action) (hook.Event, bool) {
	set := 0
	for _, b := range []bool{act.Text != nil, act.Arguments != nil, act.Messages != nil} {
		if b {
			set++
		}
	}
	if set != 1 {
		return ev, false
	}
	switch {
	case act.Text != nil && ev.Point == hook.UserMessage:
		ev.Message = content.From(ev.Message.Provenance(), g.scrub.Scrub(*act.Text))
	case act.Text != nil && ev.Point == hook.AfterTool:
		ev.Result = content.From(ev.Result.Provenance(), g.scrub.Scrub(*act.Text))
	case act.Text != nil && ev.Point == hook.Reply:
		ev.Reply = g.scrub.Scrub(*act.Text)
	case act.Arguments != nil && ev.Point == hook.BeforeTool:
		args := []byte(g.scrub.Scrub(string(act.Arguments)))
		if !json.Valid(args) {
			return ev, false
		}
		ev.Call.Arguments = args
	case act.Messages != nil && ev.Point == hook.BeforeModel:
		msgs, ok := keepTrust(ev.Messages, act.Messages, g.scrub)
		if !ok {
			return ev, false
		}
		ev.Messages = msgs
	default:
		return ev, false
	}
	return ev, true
}

// keepTrust checks that changed raises no trust and moves none: every trusted
// part must be one the original held in a message of the same role, every
// untrusted item must carry a provenance the original held, no untrusted part
// or section may sit in a system message, and an item may leave a section only
// if the original already held it bare. It returns changed with its untrusted
// parts scrubbed.
func keepTrust(original, changed []model.Message, scrub *secret.Scrubber) ([]model.Message, bool) {
	type placed struct {
		role model.Role
		text string
	}
	trusted := map[placed]int{}
	provs := map[content.Provenance]bool{}
	bare := map[content.Provenance]bool{}
	for _, m := range original {
		for _, p := range m.Parts {
			switch v := p.(type) {
			case content.Trusted:
				trusted[placed{m.Role, v.String()}]++
			case content.Untrusted:
				provs[v.Provenance()] = true
				bare[v.Provenance()] = true
			case content.Section:
				for _, it := range v.Items() {
					provs[it.Provenance()] = true
				}
			}
		}
	}
	out := make([]model.Message, len(changed))
	for i, m := range changed {
		parts := make([]content.Text, len(m.Parts))
		for j, p := range m.Parts {
			switch v := p.(type) {
			case content.Trusted:
				k := placed{m.Role, v.String()}
				if trusted[k] == 0 {
					return nil, false
				}
				trusted[k]--
				parts[j] = v
			case content.Untrusted:
				if m.Role == model.RoleSystem || !bare[v.Provenance()] {
					return nil, false
				}
				parts[j] = scrub.ScrubText(v)
			case content.Section:
				if m.Role == model.RoleSystem {
					return nil, false
				}
				for _, it := range v.Items() {
					if !provs[it.Provenance()] {
						return nil, false
					}
				}
				parts[j] = scrub.ScrubText(v)
			default:
				return nil, false
			}
		}
		m.Parts = parts
		out[i] = m
	}
	return out, true
}

func (g guardedHook) event(p hook.Point, decision string, failure Failure) {
	g.rec.Event(record.Event{Slot: SlotHook, Name: g.name, Point: string(p), Decision: decision, Failure: string(failure)})
}
