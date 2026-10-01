package secret

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/yaad-index/bonyan/content"
)

// Scrubber removes resolved secret values from text by exact match, replacing
// each with Redacted. It keeps every value ever resolved, including values
// since rotated away, because an old value can still turn up in output.
//
// There is no minimum length. A short value, such as a PIN, is redacted
// wherever it occurs, including in text that has nothing to do with the
// secret. Over-redaction is the safe direction; a minimum length would let a
// short secret through. The empty value is never added.
type Scrubber struct {
	mu     sync.RWMutex
	values map[string]struct{}
	r      *strings.Replacer
}

// NewScrubber returns a scrubber holding no values.
func NewScrubber() *Scrubber {
	return &Scrubber{values: map[string]struct{}{}}
}

func (s *Scrubber) add(v string) {
	if v == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.values[v]; ok {
		return
	}
	s.values[v] = struct{}{}

	// Longest first: the replacer tries the old strings in argument order, so a
	// value that contains another is replaced whole.
	vals := make([]string, 0, len(s.values))
	for k := range s.values {
		vals = append(vals, k)
	}
	slices.SortFunc(vals, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	pairs := make([]string, 0, 2*len(vals))
	for _, k := range vals {
		pairs = append(pairs, k, Redacted)
	}
	s.r = strings.NewReplacer(pairs...)
}

// Scrub returns in with every resolved value replaced by Redacted.
func (s *Scrubber) Scrub(in string) string {
	s.mu.RLock()
	r := s.r
	s.mu.RUnlock()
	if r == nil {
		return in
	}
	return r.Replace(in)
}

// ScrubText scrubs a piece of content and keeps its type: trusted text stays
// trusted, and untrusted text stays untrusted with its provenance.
func (s *Scrubber) ScrubText(t content.Text) content.Text {
	switch v := t.(type) {
	case content.Trusted:
		return content.Instruction(s.Scrub(v.String()))
	case content.Untrusted:
		return content.From(v.Provenance(), s.Scrub(v.Raw()))
	case content.Section:
		items := v.Items()
		for i, it := range items {
			items[i] = content.From(it.Provenance(), s.Scrub(it.Raw()))
		}
		return content.NewSection(v.Label(), items...)
	case content.Marked:
		return content.NewMarked(s.ScrubText(v.Section()).(content.Section), s.Scrub(v.Text()))
	}
	return t
}

// Handler wraps h so that every record it handles is scrubbed: the message and
// every attribute, including attributes bound with WithAttrs before a value
// was resolved.
func (s *Scrubber) Handler(h slog.Handler) slog.Handler {
	return scrubHandler{s: s, inner: h}
}

// scrubHandler keeps attributes and groups bound through it and applies them
// to the inner handler only when a record is handled, scrubbed with the values
// known at that moment.
type scrubHandler struct {
	s     *Scrubber
	inner slog.Handler
	ops   []op
}

// op is one WithAttrs (attrs) or WithGroup (group) call.
type op struct {
	group string
	attrs []slog.Attr
}

func (h scrubHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h scrubHandler) Handle(ctx context.Context, r slog.Record) error {
	inner := h.inner
	for _, o := range h.ops {
		if o.attrs != nil {
			inner = inner.WithAttrs(h.scrubAttrs(o.attrs))
		} else {
			inner = inner.WithGroup(o.group)
		}
	}
	out := slog.NewRecord(r.Time, r.Level, h.s.Scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.scrubAttr(a))
		return true
	})
	return inner.Handle(ctx, out)
}

func (h scrubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.with(op{attrs: slices.Clone(attrs)})
}

func (h scrubHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(op{group: name})
}

func (h scrubHandler) with(o op) scrubHandler {
	h.ops = append(slices.Clip(h.ops), o)
	return h
}

func (h scrubHandler) scrubAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = h.scrubAttr(a)
	}
	return out
}

// scrubAttr scrubs one attribute. A group is scrubbed attribute by attribute.
// Any other value, of whatever kind (a number, a time, an error), is rendered
// as text; if the rendering contains a resolved value, the attribute is
// replaced by the scrubbed rendering, otherwise it is kept as it was.
func (h scrubHandler) scrubAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(h.scrubAttrs(v.Group())...)}
	}
	rendered := v.String()
	if scrubbed := h.s.Scrub(rendered); scrubbed != rendered {
		return slog.String(a.Key, scrubbed)
	}
	return slog.Attr{Key: a.Key, Value: v}
}
