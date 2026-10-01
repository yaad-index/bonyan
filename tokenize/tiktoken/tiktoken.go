// Package tiktoken counts tokens exactly for byte-pair encodings in the
// tiktoken format, for the pre-call budget bound (ADR 0001 §11).
//
// It is a separate module so that its dependency, whose vocabularies are
// compiled in, reaches only the programs that import it (ADR 0002). A program
// that does not need exact counts uses tokenize.ByteBound.
//
// The encoding is named in configuration and never guessed from a model name:
// a wrong guess can undercount, and an undercount is the one error the budget
// bound must not make.
package tiktoken

import (
	"fmt"
	"math"

	"github.com/tiktoken-go/tokenizer"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/tokenize"
)

// Counter counts the text of a request with one encoding, plus the same
// per-message allowance tokenize.ByteBound uses for a chat template's framing,
// which no encoding of the text itself includes.
type Counter struct {
	codec      tokenizer.Codec
	perMessage int64
}

// New returns a counter for the named encoding, such as "cl100k_base" or
// "o200k_base". An unknown name is an error. A perMessage of zero means
// tokenize.DefaultPerMessage.
func New(encoding string, perMessage int64) (*Counter, error) {
	codec, err := tokenizer.Get(tokenizer.Encoding(encoding))
	if err != nil {
		return nil, fmt.Errorf("tiktoken: encoding %q: %w", encoding, err)
	}
	if perMessage <= 0 {
		perMessage = tokenize.DefaultPerMessage
	}
	return &Counter{codec: codec, perMessage: perMessage}, nil
}

var _ tokenize.Counter = (*Counter)(nil)

// Count returns the tokens of every message part, role, tool definition and
// the response schema, plus the allowances. It saturates at math.MaxInt64.
func (c *Counter) Count(req model.ChatRequest) (int64, error) {
	var n int64
	add := func(k int64) {
		if k > math.MaxInt64-n {
			n = math.MaxInt64
			return
		}
		n += k
	}
	count := func(s string) error {
		if s == "" {
			return nil
		}
		k, err := c.codec.Count(s)
		if err != nil {
			return fmt.Errorf("tiktoken: count: %w", err)
		}
		add(int64(k))
		return nil
	}

	for _, m := range req.Messages {
		add(c.perMessage)
		if err := count(string(m.Role)); err != nil {
			return 0, err
		}
		for _, p := range m.Parts {
			if err := count(text(p)); err != nil {
				return 0, err
			}
		}
	}
	for _, t := range req.Tools {
		add(c.perMessage)
		for _, s := range []string{t.Name, t.Description, string(t.Parameters)} {
			if err := count(s); err != nil {
				return 0, err
			}
		}
	}
	if err := count(string(req.Schema)); err != nil {
		return 0, err
	}
	return n, nil
}

func text(p content.Text) string {
	switch v := p.(type) {
	case content.Trusted:
		return v.String()
	case content.Untrusted:
		return v.Raw()
	case content.Section:
		return v.Render()
	case content.Marked:
		return v.Text()
	}
	return ""
}
