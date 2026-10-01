package registry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/trust"
)

// ErrPlacement reports a request with untrusted content outside a section, or
// a section in the system message, at the enforcement point (ADR 0001 §3).
var ErrPlacement = errors.New("registry: untrusted content outside a marked section")

// ErrRefused reports a request the trust policy's handling refused, or whose
// handling failed.
var ErrRefused = errors.New("registry: refused by the trust policy")

// GuardPolicy returns p inside the wrapper Assemble puts around every policy,
// so a policy a program passes directly fails closed and has its decisions
// recorded like a configured one. A nil p is the default policy; a policy
// already wrapped is returned as it is. rec may be nil.
func GuardPolicy(p trust.Policy, rec *record.Recorder) trust.Policy {
	if g, ok := p.(guardedPolicy); ok {
		return g
	}
	var ev events = discard{}
	if rec != nil {
		ev = rec
	}
	if p == nil {
		return guardPolicy(trust.DefaultName, trust.Default{}, ev)
	}
	return guardPolicy("program", p, ev)
}

// Classify applies p to u where it enters a run and builds the typed value
// from the decision: trusted text keeping u's provenance when the policy
// declares the source trusted, u unchanged otherwise. p must come from GuardPolicy or Assemble, which never
// return an error and fail closed.
func Classify(ctx context.Context, p trust.Policy, u content.Untrusted) content.Text {
	d, err := p.Classify(ctx, u.Provenance())
	if err == nil && d.Verdict == trust.Trusted {
		return content.TrustedFrom(u.Provenance(), u.Raw())
	}
	return u
}

// Enforce is the trust enforcement point: the last step before a request
// reaches a model, after every pipeline stage and hook (ADR 0001 §3). It
// refuses untrusted content outside a section and any section in the system
// message, lets the policy's handling drop items or refuse the request, and
// marks every section, with the default marking when the policy's own cannot
// be used. p must come from GuardPolicy or Assemble.
func Enforce(ctx context.Context, p trust.Policy, req model.ChatRequest) (model.ChatRequest, error) {
	g, ok := p.(guardedPolicy)
	if !ok {
		return model.ChatRequest{}, fmt.Errorf("registry: enforce needs a guarded policy, got %T", p)
	}
	var items []trust.Item
	for _, m := range req.Messages {
		for _, part := range m.Parts {
			switch v := part.(type) {
			case content.Trusted:
			case content.Section:
				if m.Role == model.RoleSystem {
					return model.ChatRequest{}, fmt.Errorf("%w: a %q section in the system message", ErrPlacement, v.Label())
				}
				for _, it := range v.Items() {
					items = append(items, trust.Item{Section: v.Label(), Index: len(items), Source: it.Provenance(), Bytes: len(it.Raw())})
				}
			default:
				return model.ChatRequest{}, fmt.Errorf("%w: a %T part in a %s message", ErrPlacement, part, m.Role)
			}
		}
	}

	var drop []int
	if len(items) > 0 {
		h := g.handle(ctx, items)
		if h.Refuse {
			return model.ChatRequest{}, ErrRefused
		}
		drop = h.Drop
	}

	// Drop what the handling dropped, then mark every section under one nonce
	// over the whole request's untrusted text, so that no item anywhere in it
	// can hold a line of any section's marking.
	out := req
	out.Messages = make([]model.Message, len(req.Messages))
	var sections []content.Section
	n := 0
	for i, m := range req.Messages {
		parts := make([]content.Text, len(m.Parts))
		for j, part := range m.Parts {
			s, ok := part.(content.Section)
			if !ok {
				parts[j] = part
				continue
			}
			var kept []content.Untrusted
			for _, it := range s.Items() {
				if !slices.Contains(drop, n) {
					kept = append(kept, it)
				}
				n++
			}
			s = content.NewSection(s.Label(), kept...)
			sections = append(sections, s)
			parts[j] = s
		}
		m.Parts = parts
		out.Messages[i] = m
	}
	nonce := content.Nonce(sections...)
	var texts []string
	for _, s := range sections {
		for _, it := range s.Items() {
			texts = append(texts, it.Raw())
		}
	}
	for i, m := range out.Messages {
		for j, part := range m.Parts {
			if s, ok := part.(content.Section); ok {
				m.Parts[j] = content.NewMarked(s, g.mark(ctx, s, nonce, texts))
			}
		}
		out.Messages[i] = m
	}
	return out, nil
}

// handle asks the policy's Handler, if it has one. A failure, or a drop of an
// item that does not exist, refuses the request.
func (g guardedPolicy) handle(ctx context.Context, items []trust.Item) trust.Handling {
	h, ok := g.inner.(trust.Handler)
	if !ok {
		return trust.Handling{}
	}
	got, failure := call(ctx, func() (trust.Handling, error) { return h.Handle(ctx, items) })
	if failure == "" {
		for _, i := range got.Drop {
			if i < 0 || i >= len(items) {
				failure = FailNotAllowed
			}
		}
	}
	switch {
	case failure != "":
		got = trust.Handling{Refuse: true}
		g.rec.Event(record.Event{Slot: SlotTrust, Name: g.name, Decision: DecisionRefused, Failure: string(failure)})
	case got.Refuse:
		g.rec.Event(record.Event{Slot: SlotTrust, Name: g.name, Decision: DecisionRefused})
	default:
		for _, i := range got.Drop {
			ev := record.Event{Slot: SlotTrust, Name: g.name, Decision: DecisionDropped, Source: string(items[i].Source.Kind), Item: items[i].Source.ID}
			if items[i].Source.Kind == content.KindMemory {
				ev.Item = "" // a recording never holds a memory item's ID (ADR 0001 §4)
			}
			g.rec.Event(ev)
		}
	}
	return got
}

// mark renders s with the policy's marking when it has one that delimits s
// within the whole request, whose items' texts are texts, and with the default
// marking under nonce otherwise.
func (g guardedPolicy) mark(ctx context.Context, s content.Section, nonce string, texts []string) string {
	open, closing, header := content.DefaultMarking(s.Label(), nonce)
	m, ok := g.inner.(trust.Marker)
	if !ok {
		return compose(s, open, closing, header)
	}
	got, failure := call(ctx, func() (trust.Marking, error) { return m.Mark(ctx, s.Label()) })
	if failure == "" {
		if text, ok := delimits(s, got, texts); ok {
			return text
		}
		failure = FailNotAllowed
	}
	g.rec.Event(record.Event{Slot: SlotTrust, Name: g.name, Source: s.Label(), Decision: DecisionDefaultMarking, Failure: string(failure)})
	return compose(s, open, closing, header)
}

// compose renders s with the default marking, which no item can contain.
func compose(s content.Section, open, closing string, header func(content.Provenance) string) string {
	text, _ := s.Compose(open, closing, header)
	return text
}

// delimits renders s with m and reports whether m delimits it: Compose accepts
// it for the section's own items, and no line of it occurs in any item of the
// request. A Header that panics does not delimit.
func delimits(s content.Section, m trust.Marking, texts []string) (text string, ok bool) {
	defer func() {
		if recover() != nil {
			text, ok = "", false
		}
	}()
	if m.Header == nil {
		return "", false
	}
	// Header is called once per source, so the line checked is the line sent.
	headers := map[content.Provenance]string{}
	header := func(p content.Provenance) string {
		h, ok := headers[p]
		if !ok {
			h = m.Header(p)
			headers[p] = h
		}
		return h
	}
	text, err := s.Compose(m.Open, m.Close, header)
	if err != nil {
		return "", false
	}
	lines := []string{m.Open, m.Close}
	for _, it := range s.Items() {
		lines = append(lines, header(it.Provenance()))
	}
	for _, line := range lines {
		for _, t := range texts {
			if strings.Contains(t, line) {
				return "", false
			}
		}
	}
	return text, true
}
