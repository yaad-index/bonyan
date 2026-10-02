// Package agent holds the agent loop's types. This file defines what a run
// returns.
package agent

import "fmt"

// Reason says why a run ended without clearing (ADR 0001 §7).
type Reason string

// The reasons a run is not cleared. ReasonUnset is the zero value, so an
// Outcome nobody filled in is not cleared either.
const (
	ReasonUnset             Reason = ""
	ReasonModelFailed       Reason = "model_failed"       // a model or classifier call failed
	ReasonInvalidOutput     Reason = "invalid_output"     // structured output still invalid after retries
	ReasonStepLimit         Reason = "step_limit"         // the step limit was reached
	ReasonBudgetLimit       Reason = "budget_limit"       // the token or cost ceiling was reached
	ReasonDeadline          Reason = "deadline"           // the deadline passed or the run was canceled
	ReasonFallbackExhausted Reason = "fallback_exhausted" // every configured model failed
	ReasonLoopDetected      Reason = "loop_detected"      // the loop repeated itself past its threshold
	ReasonNotApproved       Reason = "not_approved"       // an action needing approval got no decision: none was made, or it timed out
	ReasonDenied            Reason = "denied"             // a hook denied the message, a model call or the reply
	ReasonPlacement         Reason = "placement"          // a request held untrusted content outside a marked section
	ReasonTrustRefused      Reason = "trust_refused"      // the trust policy's handling refused a request
	ReasonReplayMismatch    Reason = "replay_mismatch"    // a re-run's tool call has no recorded result, so nothing is executed
)

// Outcome is what a run returns: either an answer, or not cleared with a
// reason. It is a value, not an error, so a caller that handles errors and
// verdicts separately cannot mistake a failure for a pass by missing a branch.
//
// The zero Outcome is not cleared. Only Answered produces a cleared outcome.
type Outcome struct {
	cleared bool
	answer  string
	reason  Reason
}

// Answered returns a cleared outcome carrying the run's answer.
func Answered(answer string) Outcome { return Outcome{cleared: true, answer: answer} }

// NotCleared returns an outcome that did not clear, for the given reason.
func NotCleared(reason Reason) Outcome { return Outcome{reason: reason} }

// Cleared reports whether the run produced an answer.
func (o Outcome) Cleared() bool { return o.cleared }

// Answer returns the answer, and whether there is one.
func (o Outcome) Answer() (string, bool) { return o.answer, o.cleared }

// Reason returns why the run was not cleared. It is ReasonUnset for a cleared
// outcome, and also for a zero Outcome nobody filled in.
func (o Outcome) Reason() Reason { return o.reason }

func (o Outcome) String() string {
	if o.cleared {
		return "cleared"
	}
	if o.reason == ReasonUnset {
		return "not cleared (no reason recorded)"
	}
	return fmt.Sprintf("not cleared (%s)", o.reason)
}
