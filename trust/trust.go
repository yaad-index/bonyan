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

// TODO(phase: trust policy, next to context assembly): add marking and handling
// to Policy. Classification is the only part defined so far.

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
