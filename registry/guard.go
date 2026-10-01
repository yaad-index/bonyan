package registry

import (
	"context"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/hook"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
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

// Name is the name the policy was configured under.
func (g guardedPolicy) Name() string { return g.name }

// IsDefaultPolicy reports whether p is the registry's wrapper around the
// default policy. It decides on the wrapped value, not on a name, and a program
// cannot construct the wrapper.
func IsDefaultPolicy(p trust.Policy) bool {
	g, ok := p.(guardedPolicy)
	if !ok {
		return false
	}
	switch g.inner.(type) {
	case trust.Default, *trust.Default:
		return true
	}
	return false
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

// guardedHook runs one hook. Hooks only observe so far, and an observing
// hook's failure is recorded and does not change the run (ADR 0001 §12).
type guardedHook struct {
	name  string
	inner hook.Hook
	rec   events
}

// Observe never returns an error; a failure is recorded instead.
func (g guardedHook) Observe(ctx context.Context, ev hook.Event) error {
	_, failure := call(ctx, func() (struct{}, error) { return struct{}{}, g.inner.Observe(ctx, ev) })
	if failure != "" {
		g.rec.Event(record.Event{Slot: SlotHook, Name: g.name, Point: string(ev.Point), Failure: string(failure)})
	}
	return nil
}
