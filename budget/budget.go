// Package budget enforces a run's token and cost ceiling (ADR 0001 §11).
//
// Two mechanisms combine. Before a call, Admit refuses it if its bound (the
// counted input plus the call's required max-output cap) could cross what is
// left; this bound only ever refuses, it is never charged. After a call,
// Charge takes what the model reported, never an estimate; missing usage is an
// error, and crossing the ceiling ends the run with ErrExceeded.
package budget

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sync"

	"github.com/yaad-index/bonyan/model"
)

// ErrExceeded reports that a call would cross, or has crossed, the budget.
var ErrExceeded = errors.New("budget: exceeded")

// ErrNoOutputCap reports a call in a budgeted run without a max-output cap.
var ErrNoOutputCap = errors.New("budget: call has no max-output cap")

// ErrUnpriced reports a model missing from the price table. An unpriced model
// is refused, never counted as free.
var ErrUnpriced = errors.New("budget: model has no price")

// Price is what a model costs, in millionths of the price table's unit per
// million tokens.
type Price struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

// PriceTable holds the price of each model by the name the program calls it
// by. It is configuration.
type PriceTable map[string]Price

// Lookup returns the price of name.
func (t PriceTable) Lookup(name string) (Price, error) {
	p, ok := t[name]
	if !ok {
		return Price{}, fmt.Errorf("%w: %q", ErrUnpriced, name)
	}
	if p.Input < 0 || p.Output < 0 {
		return Price{}, fmt.Errorf("budget: negative price for %q", name)
	}
	return p, nil
}

// costMicros returns tokens × perMillion / 1e6, rounded up, saturating at
// math.MaxInt64.
func costMicros(tokens, perMillion int64) int64 {
	if tokens <= 0 || perMillion <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(tokens), uint64(perMillion))
	const million = 1_000_000
	if hi >= million {
		return math.MaxInt64
	}
	q, r := bits.Div64(hi, lo, million)
	if r != 0 {
		q++
	}
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}

func satAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// Meter tracks what a run has spent against its ceiling. It is safe for
// concurrent use, but Admit reserves nothing: two calls admitted at once can
// together cross the ceiling, which Charge then reports.
type Meter struct {
	mu        sync.Mutex
	maxTokens int64
	maxCost   int64
	prices    PriceTable
	tokens    int64
	cost      int64
}

// NewMeter returns a meter for a ceiling of maxTokens tokens and maxCostMicros
// in millionths of the price table's unit. Both must be positive: a budget
// cannot be switched off by leaving it out.
func NewMeter(maxTokens, maxCostMicros int64, prices PriceTable) (*Meter, error) {
	if maxTokens <= 0 || maxCostMicros <= 0 {
		return nil, fmt.Errorf("budget: ceiling must be positive, got %d tokens and %d cost", maxTokens, maxCostMicros)
	}
	return &Meter{maxTokens: maxTokens, maxCost: maxCostMicros, prices: prices}, nil
}

// Admit reports whether a call to modelName with inputBound counted input
// tokens and a cap of maxOutput output tokens fits in what is left. A call
// without a cap is refused with ErrNoOutputCap, an unpriced model with
// ErrUnpriced, and a call that could cross the ceiling with ErrExceeded.
func (m *Meter) Admit(modelName string, inputBound int64, maxOutput int) error {
	if maxOutput <= 0 {
		return ErrNoOutputCap
	}
	if inputBound < 0 {
		return fmt.Errorf("budget: negative input bound %d", inputBound)
	}
	p, err := m.prices.Lookup(modelName)
	if err != nil {
		return err
	}
	tokens := satAdd(inputBound, int64(maxOutput))
	cost := satAdd(costMicros(inputBound, p.Input), costMicros(int64(maxOutput), p.Output))

	m.mu.Lock()
	defer m.mu.Unlock()
	if satAdd(m.tokens, tokens) > m.maxTokens {
		return fmt.Errorf("%w: a call bounded at %d tokens does not fit in the %d left", ErrExceeded, tokens, m.maxTokens-m.tokens)
	}
	if satAdd(m.cost, cost) > m.maxCost {
		return fmt.Errorf("%w: a call bounded at cost %d does not fit in the %d left", ErrExceeded, cost, m.maxCost-m.cost)
	}
	return nil
}

// Charge adds what a call to modelName reported. Missing usage is
// model.ErrMissingUsage. The usage is charged even when it crosses the
// ceiling, and the crossing is reported as ErrExceeded.
//
// The tokens are charged whether or not the model is priced. For a model
// missing from the price table the cost of the call is unknown and is not
// charged, and Charge returns ErrUnpriced; Admit never admits such a model, so
// this happens only to a call made without Admit.
func (m *Meter) Charge(modelName string, u *model.Usage) error {
	if u == nil {
		return fmt.Errorf("%w: from %q", model.ErrMissingUsage, modelName)
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 {
		return fmt.Errorf("budget: negative usage reported by %q", modelName)
	}
	tokens := satAdd(u.InputTokens, u.OutputTokens)
	p, priceErr := m.prices.Lookup(modelName)
	var cost int64
	if priceErr == nil {
		cost = satAdd(costMicros(u.InputTokens, p.Input), costMicros(u.OutputTokens, p.Output))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = satAdd(m.tokens, tokens)
	m.cost = satAdd(m.cost, cost)
	if priceErr != nil {
		return priceErr
	}
	if m.tokens > m.maxTokens || m.cost > m.maxCost {
		return fmt.Errorf("%w: spent %d of %d tokens and %d of %d cost", ErrExceeded, m.tokens, m.maxTokens, m.cost, m.maxCost)
	}
	return nil
}

// ChargeBound charges a failed call its bound: inputBound input tokens and
// maxOutput output tokens at modelName's price. It is for a call that failed
// after its request may have reached the provider and reported no usage (ADR
// 0001 §11). As with Charge, the tokens are charged even for an unpriced model,
// and crossing the ceiling is reported as ErrExceeded.
func (m *Meter) ChargeBound(modelName string, inputBound int64, maxOutput int) error {
	if inputBound < 0 || maxOutput < 0 {
		return fmt.Errorf("budget: negative bound for %q", modelName)
	}
	return m.Charge(modelName, &model.Usage{InputTokens: inputBound, OutputTokens: int64(maxOutput)})
}

// Spent returns the tokens and cost charged so far.
func (m *Meter) Spent() (tokens, costMicros int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens, m.cost
}
