// Package trust defines the trust policy: the slot that decides which content
// is untrusted (ADR 0001 §3).
//
// A policy only decides. bonyan applies the decision: it builds the typed value
// from it, and a policy never constructs a trusted value itself. A policy
// failure fails closed, and that rule lives in the registry's wrapper around
// every policy, not in the policies.
package trust

import (
	"context"

	"github.com/yaad-index/bonyan/content"
)

// Verdict is a policy's classification of one source.
type Verdict int

// The verdicts. The zero value is no decision, which counts as untrusted.
const (
	NoDecision Verdict = iota
	Untrusted
	Trusted
)

func (v Verdict) String() string {
	switch v {
	case Untrusted:
		return "untrusted"
	case Trusted:
		return "trusted"
	}
	return "no decision"
}

// Decision is a policy's answer for one source.
type Decision struct {
	Verdict Verdict
}

// Policy classifies content by where it entered bonyan. It sees the source's
// provenance, never the content itself. A program can register further source
// kinds, and a policy can classify by any of them, including declaring a source
// trusted.
type Policy interface {
	Classify(ctx context.Context, source content.Provenance) (Decision, error)
}

// DefaultName is the name the default policy is registered under.
const DefaultName = "default"

// Default is the policy a program gets when it configures none: everything
// that reaches classification is untrusted. Program and operator instructions
// never reach it; they are content.Instruction from the start.
type Default struct{}

// Classify reports every source untrusted.
func (Default) Classify(context.Context, content.Provenance) (Decision, error) {
	return Decision{Verdict: Untrusted}, nil
}

// Marking is how a policy delimits a section of untrusted content: the lines
// before and after it, and the line before each item. bonyan composes the
// section from them and refuses delimiters that are empty or that an item
// contains, using the default marking instead (ADR 0001 §3).
type Marking struct {
	Open, Close string
	Header      func(content.Provenance) string
}

// Marker is a policy that chooses its marking. A policy that does not
// implement it, or whose Mark fails, gets the default marking.
type Marker interface {
	Mark(ctx context.Context, label string) (Marking, error)
}

// Item describes one untrusted item of a request to a Handler: where it came
// from and its size, never its text.
type Item struct {
	// Section is the label of the section holding it.
	Section string
	// Index is its position among the request's untrusted items.
	Index  int
	Source content.Provenance
	Bytes  int
}

// Handling is what a Handler decides for a request.
type Handling struct {
	// Drop lists the Index of each item to leave out of the request.
	Drop []int
	// Refuse ends the run not cleared instead of sending the request.
	Refuse bool
	// RequireApproval makes every tool call the run makes from here on need
	// approval (ADR 0001 §7).
	RequireApproval bool
}

// Handler is a policy that acts on a request carrying untrusted content,
// beyond marking it. It is called at the enforcement point, after every hook.
// A Handler that fails refuses the request.
type Handler interface {
	Handle(ctx context.Context, items []Item) (Handling, error)
}
