// Package tokenize counts the input of a model call for the pre-call budget
// bound (ADR 0001 §11).
//
// A count is used only to refuse calls that could cross the budget, never as a
// charge, so a counter must not undercount. ByteBound, the counter every
// program has, is an upper bound under the assumption stated on it; an exact
// counter for a named encoding lives in a separate module.
package tokenize

import (
	"math"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
)

// Counter counts the input tokens of a chat request: its messages, tool
// definitions and response schema.
type Counter interface {
	Count(req model.ChatRequest) (int64, error)
}

// DefaultPerMessage is ByteBound's allowance for the framing a chat template
// adds around each message (role markers and separators). It is a chosen
// value, not one measured against every template: a program whose model's
// template adds more per message sets ByteBound.PerMessage.
const DefaultPerMessage = 16

// ByteBound counts one token per UTF-8 byte, plus an allowance per message and
// per tool definition (ADR 0001 §11). It rests on the tokenizer emitting at
// most one token per byte of text, which holds for byte-level and
// byte-fallback tokenizers; counting bytes rather than characters is what
// keeps non-ASCII text from being undercounted.
type ByteBound struct {
	// PerMessage is the allowance per message and per tool definition. Zero
	// means DefaultPerMessage.
	PerMessage int64
}

// Count returns the bound for req. It saturates at math.MaxInt64 rather than
// overflowing.
func (b ByteBound) Count(req model.ChatRequest) (int64, error) {
	per := b.PerMessage
	if per <= 0 {
		per = DefaultPerMessage
	}
	var n int64
	add := func(k int64) {
		if k > math.MaxInt64-n {
			n = math.MaxInt64
			return
		}
		n += k
	}
	for _, m := range req.Messages {
		add(per)
		add(int64(len(m.Role)))
		for _, p := range m.Parts {
			add(int64(len(text(p))))
		}
	}
	for _, t := range req.Tools {
		add(per)
		add(int64(len(t.Name) + len(t.Description) + len(t.Parameters)))
	}
	add(int64(len(req.Schema)))
	return n, nil
}

// text returns the characters of a part, whichever kind it is.
func text(p content.Text) string {
	switch v := p.(type) {
	case content.Trusted:
		return v.String()
	case content.Untrusted:
		return v.Raw()
	case content.Section:
		return v.Render()
	}
	return ""
}
