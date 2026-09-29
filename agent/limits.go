package agent

import (
	"errors"
	"fmt"
	"time"
)

// Budget is a run's token and cost ceiling (ADR 0001 §11). Cost is counted in
// millionths of the unit the price table is written in.
type Budget struct {
	MaxTokens     int64
	MaxCostMicros int64
}

// Limits bound every run (ADR 0001 §2). All of them always apply: zero or
// negative is invalid, never "unlimited", so a limit cannot be switched off by
// leaving it out.
type Limits struct {
	MaxSteps int
	Deadline time.Duration
	Budget   Budget
}

// DefaultLimits returns the limits a run gets when the program sets none.
// They are deliberately modest; a program raises them for its own agents.
func DefaultLimits() Limits {
	return Limits{
		MaxSteps: 20,
		Deadline: 5 * time.Minute,
		Budget: Budget{
			MaxTokens:     200_000,
			MaxCostMicros: 1_000_000,
		},
	}
}

// ErrInvalidLimits reports limits that would leave a run unbounded.
var ErrInvalidLimits = errors.New("agent: invalid limits")

// Validate reports an error for any limit that is zero or negative.
func (l Limits) Validate() error {
	var errs []error
	if l.MaxSteps <= 0 {
		errs = append(errs, fmt.Errorf("max steps must be positive, got %d", l.MaxSteps))
	}
	if l.Deadline <= 0 {
		errs = append(errs, fmt.Errorf("deadline must be positive, got %s", l.Deadline))
	}
	if l.Budget.MaxTokens <= 0 {
		errs = append(errs, fmt.Errorf("token budget must be positive, got %d", l.Budget.MaxTokens))
	}
	if l.Budget.MaxCostMicros <= 0 {
		errs = append(errs, fmt.Errorf("cost budget must be positive, got %d", l.Budget.MaxCostMicros))
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalidLimits, errors.Join(errs...))
}
