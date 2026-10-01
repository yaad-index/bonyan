package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/trust"
)

// trustAll is a program's policy that declares every source trusted.
type trustAll struct{}

func (trustAll) Classify(context.Context, content.Provenance) (trust.Decision, error) {
	return trust.Decision{Verdict: trust.Trusted}, nil
}

// claimsDefault is a program's policy that reports the default policy's name
// and still declares every source trusted.
type claimsDefault struct{ trustAll }

func (claimsDefault) Name() string { return trust.DefaultName }

type nullSink struct{}

func (nullSink) Write(record.Entry) error { return nil }
func (nullSink) Full() bool               { return false }
func (nullSink) Subject() string          { return "" }
func (nullSink) Close() error             { return nil }

// assembled returns the trust policy the registry assembles from cfg, the way
// a program hands Components.Trust to an agent.
func assembled(t *testing.T, cfg *registry.SlotConfig) trust.Policy {
	t.Helper()
	r := registry.New()
	require.NoError(t, r.RegisterChat("basic", func(json.RawMessage) (model.Chat, error) {
		return &scripted{steps: stepsOf(answer("unused"))}, nil
	}))
	require.NoError(t, r.RegisterPolicy("program", func(json.RawMessage) (trust.Policy, error) { return trustAll{}, nil }))
	require.NoError(t, r.RegisterPolicy("wraps-default", func(json.RawMessage) (trust.Policy, error) { return &trust.Default{}, nil }))
	c, err := r.Assemble(registry.Config{Chat: registry.SlotConfig{Impl: "basic"}, Trust: cfg}, registry.WithSink(nullSink{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c.Trust
}

func TestTheDefaultTrustPolicyRuns(t *testing.T) {
	for name, policy := range map[string]func(*testing.T) trust.Policy{
		"none":                   func(*testing.T) trust.Policy { return nil },
		"default value":          func(*testing.T) trust.Policy { return trust.Default{} },
		"default pointer":        func(*testing.T) trust.Policy { return &trust.Default{} },
		"registry, unconfigured": func(t *testing.T) trust.Policy { return assembled(t, nil) },
		"registry, default value under another name": func(t *testing.T) trust.Policy {
			return assembled(t, &registry.SlotConfig{Impl: "wraps-default"})
		},
		"registry, named default": func(t *testing.T) trust.Policy { return assembled(t, &registry.SlotConfig{Impl: trust.DefaultName}) },
	} {
		t.Run(name, func(t *testing.T) {
			a := newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(answer("done"))}})
			a.Trust = policy(t)
			out, _, err := agent.Run(context.Background(), a, input("q"))
			require.NoError(t, err)
			assert.True(t, out.Cleared(), out.String())
		})
	}
}

// Until the trust-policy phase applies a policy, any other one is refused
// rather than silently replaced by the default classification.
func TestAnotherTrustPolicyIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		policy func(*testing.T) trust.Policy
		names  string
	}{
		"program policy":               {func(*testing.T) trust.Policy { return trustAll{} }, "agent_test.trustAll"},
		"program policy named default": {func(*testing.T) trust.Policy { return claimsDefault{} }, "agent_test.claimsDefault"},
		"registry, configured":         {func(t *testing.T) trust.Policy { return assembled(t, &registry.SlotConfig{Impl: "program"}) }, `"program"`},
	} {
		t.Run(name, func(t *testing.T) {
			m := &scripted{steps: stepsOf(answer("done"))}
			a := newAgent(agent.Model{Name: "main", Chat: m})
			a.Trust = tc.policy(t)
			_, _, err := agent.Run(context.Background(), a, input("q"))
			require.ErrorIs(t, err, agent.ErrTrustPolicy)
			assert.Contains(t, err.Error(), tc.names)
			assert.Contains(t, err.Error(), "trust-policy phase")
			assert.Empty(t, m.reqs, "refused before any model call")
		})
	}
}
