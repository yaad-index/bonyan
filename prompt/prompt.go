// Package prompt holds versioned prompts (ADR 0001 §10): a prompt is an asset
// with a name and a version, and the identifier of the prompt a model call was
// made with is recorded on that call, so a recording or a trace says which
// version produced it.
//
// What is recorded is a Ref: the prompt's identifier and a hash of its text,
// never the text. The hash shows a text edited without its version changing,
// and it is kept for unversioned instructions too, so an edit to those shows
// as well.
package prompt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/yaad-index/bonyan/content"
)

// Prompt is a versioned prompt.
type Prompt struct {
	Name    string
	Version string
	Text    string
}

// Validate refuses a prompt with no name, version or text.
func (p Prompt) Validate() error {
	if p.Name == "" || p.Version == "" || p.Text == "" {
		return errors.New("prompt: a prompt needs a name, a version and a text")
	}
	return nil
}

// Instruction returns the prompt's text as trusted instructions.
func (p Prompt) Instruction() content.Trusted { return content.Instruction(p.Text) }

// ID identifies the prompt and its version, as "name@version".
func (p Prompt) ID() string { return p.Name + "@" + p.Version }

// Ref is what is recorded of the prompt.
func (p Prompt) Ref() Ref { return Ref{ID: p.ID(), Hash: hash(p.Text)} }

// Ref identifies the prompt a call was made with: its ID, empty for
// unversioned instructions, and the hash of its text.
type Ref struct {
	ID   string
	Hash string
}

// Unversioned is the Ref of instructions given without a version.
func Unversioned(instructions content.Trusted) Ref {
	return Ref{Hash: hash(instructions.String())}
}

func hash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type refKey struct{}

// WithRef returns ctx carrying r: a call made with the returned context, or
// one derived from it, was made with that prompt. Set it only around the
// calls the prompt is for, so a call made for something else, such as a
// classifier, does not carry it.
func WithRef(ctx context.Context, r Ref) context.Context {
	return context.WithValue(ctx, refKey{}, r)
}

// RefOf returns the Ref ctx carries, and whether it carries one.
func RefOf(ctx context.Context) (Ref, bool) {
	r, ok := ctx.Value(refKey{}).(Ref)
	return r, ok
}
