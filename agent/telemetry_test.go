package agent_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/telemetry"
)

// observed is what one run emitted.
type observed struct {
	spans   []sdktrace.ReadOnlySpan
	metrics metricdata.ResourceMetrics
}

func observe(t *testing.T, a agent.Agent, opts telemetry.Options, in content.Untrusted) observed {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	opts.TracerProvider = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	opts.MeterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tel, err := telemetry.New(opts)
	require.NoError(t, err)
	a.Telemetry = tel
	_, _, err = agent.Run(context.Background(), a, in)
	require.NoError(t, err)
	var o observed
	o.spans = spans.Ended()
	require.NoError(t, reader.Collect(context.Background(), &o.metrics))
	return o
}

// attrs is every attribute of every span and metric data point.
func (o observed) attrs() []attribute.KeyValue {
	var out []attribute.KeyValue
	for _, s := range o.spans {
		out = append(out, s.Attributes()...)
	}
	for _, sm := range o.metrics.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					out = append(out, p.Attributes.ToSlice()...)
				}
			case metricdata.Histogram[int64]:
				for _, p := range d.DataPoints {
					out = append(out, p.Attributes.ToSlice()...)
				}
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					out = append(out, p.Attributes.ToSlice()...)
				}
			}
		}
	}
	return out
}

// everything is every value and status text the run emitted, as one string.
func (o observed) everything() string {
	var b strings.Builder
	for _, kv := range o.attrs() {
		fmt.Fprintln(&b, kv.Value.String())
	}
	for _, s := range o.spans {
		fmt.Fprintln(&b, s.Name(), s.Status().Description)
	}
	return b.String()
}

func (o observed) span(t *testing.T, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range o.spans {
		if s.Name() == name {
			return s
		}
	}
	require.Failf(t, "no span", "%q", name)
	return nil
}

func attr(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// A run with distinctive text everywhere it carries content: the instructions,
// the input, material, a memory item, the tool's arguments and result, the
// answer and a resolved secret.
const (
	textInstructions = "INSTRUCTIONS-7f3"
	textInput        = "INPUT-7f3"
	textMaterial     = "MATERIAL-7f3"
	textMemory       = "MEMORY-7f3"
	textArgument     = "ARGUMENT-7f3"
	textResult       = "RESULT-7f3"
	textAnswer       = "ANSWER-7f3"
	textSecret       = "SECRET-7f3"
)

// source is a secret source holding fixed values.
type source map[string]string

func (s source) Lookup(_ context.Context, name string) (string, error) {
	v, ok := s[name]
	if !ok {
		return "", secret.ErrNotFound
	}
	return v, nil
}

func contentRun(t *testing.T) agent.Agent {
	t.Helper()
	res := secret.NewResolver(source{"key": textSecret})
	_, err := res.Scope("key").Resolve(context.Background(), "key")
	require.NoError(t, err)
	m := &scripted{steps: stepsOf(toolCall("search", `{"q":"`+textArgument+` `+textSecret+`"}`), answer(textAnswer))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Name = "helper"
	a.Instructions = content.Instruction(textInstructions)
	a.Material = []content.Untrusted{
		content.From(content.Provenance{Kind: content.KindFetched, ID: "doc"}, textMaterial),
		content.From(content.Provenance{Kind: content.KindMemory, Origin: content.KindUser}, textMemory),
	}
	a.Tools = &tools{out: map[string]string{"search": textResult + " " + textSecret}}
	a.Scrubber = res.Scrubber()
	return a
}

// The run's span holds a span per step, and each step its model and tool
// calls, with tokens and cost on the model call.
func TestTelemetrySpanTree(t *testing.T) {
	o := observe(t, contentRun(t), telemetry.Options{}, input(textInput))

	run := o.span(t, "invoke_agent helper")
	v, _ := attr(run, "bonyan.run.outcome")
	assert.Equal(t, "cleared", v.AsString())
	var steps []sdktrace.ReadOnlySpan
	for _, s := range o.spans {
		if s.Name() == "bonyan.step" {
			assert.Equal(t, run.SpanContext().SpanID(), s.Parent().SpanID())
			steps = append(steps, s)
		}
	}
	require.Len(t, steps, 2)
	tool := o.span(t, "execute_tool search")
	assert.Equal(t, steps[0].SpanContext().SpanID(), tool.Parent().SpanID())
	var chats int
	for _, s := range o.spans {
		if s.Name() != "chat main" {
			continue
		}
		chats++
		assert.True(t, slices.ContainsFunc(steps, func(st sdktrace.ReadOnlySpan) bool { return st.SpanContext().SpanID() == s.Parent().SpanID() }))
		in, _ := attr(s, "gen_ai.usage.input_tokens")
		out, _ := attr(s, "gen_ai.usage.output_tokens")
		cost, _ := attr(s, "bonyan.usage.cost")
		assert.Equal(t, int64(10), in.AsInt64())
		assert.Equal(t, int64(5), out.AsInt64())
		assert.Equal(t, int64(15), cost.AsInt64(), "10 and 5 tokens at one unit per token")
	}
	assert.Equal(t, 2, chats)

	var costSum int64
	for _, sm := range o.metrics.ScopeMetrics {
		for _, m := range sm.Metrics {
			if s, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "bonyan.usage.cost" {
				for _, p := range s.DataPoints {
					costSum += p.Value
				}
			}
		}
	}
	assert.Equal(t, int64(30), costSum)
}

// By default nothing the run carried as content is emitted.
func TestTelemetryCarriesNoContentByDefault(t *testing.T) {
	all := observe(t, contentRun(t), telemetry.Options{}, input(textInput)).everything()
	for _, text := range []string{textInstructions, textInput, textMaterial, textMemory, textArgument, textResult, textAnswer, textSecret} {
		assert.NotContains(t, all, text)
	}
}

// With capture on, content is emitted, but never memory and never a resolved
// secret.
func TestCapturedContentLeavesOutMemoryAndSecrets(t *testing.T) {
	all := observe(t, contentRun(t), telemetry.Options{CaptureContent: true}, input(textInput)).everything()
	for _, text := range []string{textInstructions, textInput, textMaterial, textArgument, textResult, textAnswer} {
		assert.Contains(t, all, text, "captured")
	}
	assert.NotContains(t, all, textMemory)
	assert.NotContains(t, all, textSecret)
}

// A failed call records the kind of error, never its text.
func TestAFailedCallRecordsItsKindNotItsText(t *testing.T) {
	m := &scripted{steps: stepsOf(fail(errors.New("upstream said " + textSecret)))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	o := observe(t, a, telemetry.Options{}, input("go"))

	chat := o.span(t, "chat main")
	v, ok := attr(chat, "error.type")
	require.True(t, ok)
	assert.Equal(t, "_OTHER", v.AsString())
	run := o.span(t, "invoke_agent")
	v, _ = attr(run, "error.type")
	assert.Equal(t, string(agent.ReasonModelFailed), v.AsString())
	assert.NotContains(t, o.everything(), textSecret)
}

// Every name bonyan emits, exactly, span by span and metric by metric. A change
// to one, from an edit here or from the conventions under a new pin, fails
// this test rather than renaming what dashboards and queries read.
func TestTelemetryNames(t *testing.T) {
	opts := telemetry.Options{CaptureContent: true, ProviderName: "a-provider"}
	ok := observe(t, contentRun(t), opts, input(textInput))
	failed := observe(t, newAgent(agent.Model{Name: "main", Chat: &scripted{steps: stepsOf(fail(errors.New("x")))}}), opts, input("go"))

	// The keys seen under each span name and each metric, with the values of
	// the two enumerations bonyan sets.
	got := map[string]map[string]bool{}
	see := func(group string, kvs []attribute.KeyValue) {
		if got[group] == nil {
			got[group] = map[string]bool{}
		}
		for _, kv := range kvs {
			k := string(kv.Key)
			if kv.Key == "gen_ai.operation.name" || kv.Key == "gen_ai.token.type" {
				k += "=" + kv.Value.AsString()
			}
			got[group][k] = true
		}
	}
	for _, o := range []observed{ok, failed} {
		for _, s := range o.spans {
			see("span "+s.Name(), s.Attributes())
		}
		for _, sm := range o.metrics.ScopeMetrics {
			for _, m := range sm.Metrics {
				var kvs []attribute.KeyValue
				switch d := m.Data.(type) {
				case metricdata.Histogram[float64]:
					for _, p := range d.DataPoints {
						kvs = append(kvs, p.Attributes.ToSlice()...)
					}
				case metricdata.Histogram[int64]:
					for _, p := range d.DataPoints {
						kvs = append(kvs, p.Attributes.ToSlice()...)
					}
				case metricdata.Sum[int64]:
					for _, p := range d.DataPoints {
						kvs = append(kvs, p.Attributes.ToSlice()...)
					}
				}
				see("metric "+m.Name, kvs)
			}
		}
	}
	run := []string{"gen_ai.operation.name=invoke_agent", "gen_ai.provider.name", "bonyan.run.outcome"}
	chat := []string{
		"gen_ai.operation.name=chat", "gen_ai.provider.name", "gen_ai.request.model", "gen_ai.request.max_tokens",
		"gen_ai.system_instructions", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.response.finish_reasons",
		"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "bonyan.usage.cost", "error.type",
	}
	want := map[string][]string{
		"span invoke_agent helper": append(slices.Clone(run), "gen_ai.agent.name"),
		"span invoke_agent":        append(slices.Clone(run), "error.type"),
		"span bonyan.step":         {"bonyan.step.index"},
		"span chat main":           chat,
		"span execute_tool search": {
			"gen_ai.operation.name=execute_tool", "gen_ai.tool.name", "gen_ai.tool.call.id", "gen_ai.tool.type",
			"gen_ai.tool.call.arguments", "gen_ai.tool.call.result",
		},
		"metric gen_ai.client.operation.duration": {"gen_ai.operation.name=chat", "gen_ai.provider.name", "gen_ai.request.model", "error.type"},
		"metric gen_ai.client.token.usage": {
			"gen_ai.operation.name=chat", "gen_ai.provider.name", "gen_ai.request.model",
			"gen_ai.token.type=input", "gen_ai.token.type=output",
		},
		"metric bonyan.usage.cost": {"gen_ai.operation.name=chat", "gen_ai.provider.name", "gen_ai.request.model"},
	}
	assert.ElementsMatch(t, keysOf(toSet(want)), keysOf(got), "span names and metric names")
	for group, keys := range want {
		assert.ElementsMatch(t, keys, keysOf(got[group]), group)
	}
}

func toSet(m map[string][]string) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

// A failed tool call records the kind of error, never its text.
func TestAFailedToolCallRecordsItsKindNotItsText(t *testing.T) {
	m := &scripted{steps: stepsOf(toolCall("search", `{}`), answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	a.Tools = &tools{err: map[string]error{"search": errors.New("tool said " + textSecret)}}
	o := observe(t, a, telemetry.Options{CaptureContent: true}, input("go"))
	v, ok := attr(o.span(t, "execute_tool search"), "error.type")
	require.True(t, ok)
	assert.Equal(t, "_OTHER", v.AsString())
	assert.NotContains(t, o.everything(), textSecret)
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// With no telemetry, a run emits nothing and runs as before.
func TestNoTelemetryIsNoOp(t *testing.T) {
	m := &scripted{steps: stepsOf(answer("done"))}
	a := newAgent(agent.Model{Name: "main", Chat: m})
	out, _, err := agent.Run(context.Background(), a, input("go"))
	require.NoError(t, err)
	assert.True(t, out.Cleared())
	assert.Nil(t, a.Telemetry)
}

// Memory the policy declared trusted is still memory, and capture still
// leaves it out.
func TestCapturedContentLeavesOutTrustedMemory(t *testing.T) {
	const trustedFact = "TRUSTED-MEMORY-7f3"
	a := contentRun(t)
	a.Trust = trustAll{}
	a.Material = append(a.Material, content.From(content.Provenance{Kind: content.KindMemory, Origin: content.KindUser}, trustedFact))
	all := observe(t, a, telemetry.Options{CaptureContent: true}, input(textInput)).everything()
	assert.Contains(t, all, textMaterial, "trusted material that is not memory is captured")
	assert.NotContains(t, all, trustedFact)
	assert.NotContains(t, all, textMemory)
}
