// Package telemetry emits bonyan's spans and metrics through the OpenTelemetry
// API (ADR 0001 §9): a span for each run, loop step, model call and tool call,
// and metrics for the model calls' tokens, duration and cost. The program wires
// the SDK and exporters; bonyan imports the API only.
//
// Names follow the OpenTelemetry semantic conventions, with the GenAI names
// pinned in this package (see names.go). Attributes carry sizes and
// identifiers, never content. Content capture is an explicit option, off by
// default; with it on, the conventions' content attributes carry messages,
// instructions, tool arguments and results after secret scrubbing, and never
// memory.
//
// A nil *Telemetry emits nothing.
package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconvgenai "go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/yaad-index/bonyan/budget"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/eval/score"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/prompt"
	"github.com/yaad-index/bonyan/tool"
)

// scope is the instrumentation scope bonyan's spans and metrics are under.
const scope = "github.com/yaad-index/bonyan"

// Options configures Telemetry.
type Options struct {
	// TracerProvider and MeterProvider are where spans and metrics go; nil
	// means the global ones, as an instrumented library's usually do.
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	// ProviderName is the value of gen_ai.provider.name. bonyan cannot know
	// which provider a configured model is, so empty leaves it out.
	ProviderName string
	// CaptureContent adds the conventions' content attributes to model and
	// tool spans. Memory is never captured.
	CaptureContent bool
}

// Telemetry emits spans and metrics.
type Telemetry struct {
	tracer   trace.Tracer
	duration metric.Float64Histogram
	tokens   metric.Int64Histogram
	cost     metric.Int64Counter
	score    metric.Float64Histogram
	provider string
	capture  bool
}

// New returns Telemetry emitting through opts' providers.
func New(opts Options) (*Telemetry, error) {
	tp, mp := opts.TracerProvider, opts.MeterProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	m := mp.Meter(scope, metric.WithSchemaURL(SchemaURL))
	duration, err := semconvgenai.NewClientOperationDuration(m)
	if err != nil {
		return nil, err
	}
	tokens, err := semconvgenai.NewClientTokenUsage(m)
	if err != nil {
		return nil, err
	}
	cost, err := m.Int64Counter(metricUsageCost, metric.WithUnit("1"),
		metric.WithDescription("What model calls cost, in millionths of the price table's unit."))
	if err != nil {
		return nil, err
	}
	score, err := m.Float64Histogram(metricEvalScore, metric.WithUnit("1"),
		metric.WithDescription("A score an evaluator gave a finished run."))
	if err != nil {
		return nil, err
	}
	return &Telemetry{
		tracer:   tp.Tracer(scope, trace.WithSchemaURL(SchemaURL)),
		duration: duration.Inst(),
		tokens:   tokens.Inst(),
		cost:     cost,
		score:    score,
		provider: opts.ProviderName,
		capture:  opts.CaptureContent,
	}, nil
}

// Run starts a run's span. end takes how the run ended: "cleared", or the
// reason it was not cleared.
func (t *Telemetry) Run(ctx context.Context, agent string) (context.Context, func(outcome string, cleared bool)) {
	if t == nil {
		return ctx, func(string, bool) {}
	}
	name := opInvokeAgent.Value.AsString()
	attrs := []attribute.KeyValue{opInvokeAgent}
	if agent != "" {
		name += " " + agent
		attrs = append(attrs, keyAgentName.String(agent))
	}
	attrs = t.withProvider(attrs)
	ctx, span := t.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	return ctx, func(outcome string, cleared bool) {
		span.SetAttributes(keyRunOutcome.String(outcome))
		if !cleared {
			span.SetAttributes(keyErrorType.String(outcome))
			span.SetStatus(codes.Error, outcome)
		}
		span.End()
	}
}

// Step starts the span of one loop step, numbered from 1.
func (t *Telemetry) Step(ctx context.Context, index int) (context.Context, func()) {
	if t == nil {
		return ctx, func() {}
	}
	ctx, span := t.tracer.Start(ctx, spanStep, trace.WithAttributes(keyStepIndex.Int(index)))
	return ctx, func() { span.End() }
}

// Chat wraps inner so each call it makes has a span and is counted in the
// metrics. price is the model's, for the call's cost. scrub removes resolved
// secrets from captured content; nil captures nothing.
func (t *Telemetry) Chat(inner model.Chat, modelName string, price budget.Price, scrub func(string) string) model.Chat {
	if t == nil {
		return inner
	}
	return &tracedChat{t: t, inner: inner, model: modelName, price: price, scrub: scrub}
}

type tracedChat struct {
	t     *Telemetry
	inner model.Chat
	model string
	price budget.Price
	scrub func(string) string
}

func (c *tracedChat) Chat(ctx context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	t := c.t
	// Clipped, so each append below copies rather than writing into a shared
	// array.
	common := slices.Clip(t.withProvider([]attribute.KeyValue{opChat, keyRequestModel.String(c.model)}))
	attrs := append(common, keyRequestMaxTokens.Int(req.MaxOutputTokens))
	if req.Temperature != nil {
		attrs = append(attrs, keyRequestTemperature.Float64(*req.Temperature))
	}
	if ref, ok := prompt.RefOf(ctx); ok {
		if ref.ID != "" {
			attrs = append(attrs, keyPromptID.String(ref.ID))
		}
		attrs = append(attrs, keyPromptHash.String(ref.Hash))
	}
	if t.capture && c.scrub != nil {
		attrs = append(attrs, captureRequest(req, c.scrub)...)
	}
	ctx, span := t.tracer.Start(ctx, opChat.Value.AsString()+" "+c.model,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	defer span.End()

	start := time.Now()
	resp, err := c.inner.Chat(ctx, req)
	elapsed := time.Since(start).Seconds()
	if err != nil {
		et := ErrorType(err)
		span.SetAttributes(keyErrorType.String(et))
		span.SetStatus(codes.Error, et)
		t.duration.Record(ctx, elapsed, metric.WithAttributes(append(common, keyErrorType.String(et))...))
		return resp, err
	}
	t.duration.Record(ctx, elapsed, metric.WithAttributes(common...))
	span.SetAttributes(keyResponseFinish.StringSlice([]string{string(resp.StopReason)}))
	if u := resp.Usage; u != nil {
		cost := c.price.Cost(*u)
		span.SetAttributes(keyUsageInputTokens.Int64(u.InputTokens), keyUsageOutputTokens.Int64(u.OutputTokens), keyUsageCost.Int64(cost))
		t.tokens.Record(ctx, u.InputTokens, metric.WithAttributes(append(common, tokenInput)...))
		t.tokens.Record(ctx, u.OutputTokens, metric.WithAttributes(append(common, tokenOutput)...))
		t.cost.Add(ctx, cost, metric.WithAttributes(common...))
	}
	if t.capture && c.scrub != nil {
		span.SetAttributes(captureResponse(resp, c.scrub))
	}
	return resp, err
}

// Tool starts the span of one tool call. source is the kind of the tool's
// results: a tool on a tool server is an "extension" in the conventions' terms,
// any other a "function". end takes the result the model is given and the
// call's error, if any. scrub is as for Chat.
func (t *Telemetry) Tool(ctx context.Context, call model.ToolCall, source content.Kind, scrub func(string) string) (context.Context, func(result string, err error)) {
	if t == nil {
		return ctx, func(string, error) {}
	}
	toolType := "function"
	if source == content.KindRemoteTool {
		toolType = "extension"
	}
	attrs := []attribute.KeyValue{opExecuteTool, keyToolName.String(call.Name), keyToolCallID.String(call.ID), keyToolType.String(toolType)}
	capture := t.capture && scrub != nil
	if capture {
		attrs = append(attrs, keyToolCallArguments.String(scrub(string(call.Arguments))))
	}
	ctx, span := t.tracer.Start(ctx, opExecuteTool.Value.AsString()+" "+call.Name,
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	return ctx, func(result string, err error) {
		if err != nil {
			et := ErrorType(err)
			span.SetAttributes(keyErrorType.String(et))
			span.SetStatus(codes.Error, et)
		} else if capture {
			span.SetAttributes(keyToolCallResult.String(scrub(result)))
		}
		span.End()
	}
}

func (t *Telemetry) withProvider(attrs []attribute.KeyValue) []attribute.KeyValue {
	if t.provider == "" {
		return attrs
	}
	return append(attrs, keyProviderName.String(t.provider))
}

// ErrorType is the value of error.type for err: a low-cardinality kind, never
// the error's text, which can carry content.
func ErrorType(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, budget.ErrExceeded):
		return "budget_exceeded"
	case errors.Is(err, budget.ErrUnpriced):
		return "unpriced"
	case errors.Is(err, model.ErrMissingUsage):
		return "missing_usage"
	case errors.Is(err, tool.ErrUnknown):
		return "unknown_tool"
	case errors.Is(err, tool.ErrInvalidArguments):
		return "invalid_arguments"
	}
	return errorTypeOther.Value.AsString()
}

// The message shapes of the conventions' content attributes.
type (
	message struct {
		Role         string `json:"role"`
		Parts        []part `json:"parts"`
		FinishReason string `json:"finish_reason,omitempty"`
	}
	part struct {
		Type      string          `json:"type"`
		Content   string          `json:"content,omitempty"`
		ID        string          `json:"id,omitempty"`
		Name      string          `json:"name,omitempty"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
		Response  string          `json:"response,omitempty"`
	}
)

// captureRequest is a request's content attributes: its system instructions
// and its other messages, scrubbed, with every memory item left out.
func captureRequest(req model.ChatRequest, scrub func(string) string) []attribute.KeyValue {
	var system []part
	var msgs []message
	for _, m := range req.Messages {
		var parts []part
		for _, t := range texts(m.Parts) {
			parts = append(parts, part{Type: "text", Content: scrub(t)})
		}
		if m.Role == model.RoleSystem {
			system = append(system, parts...)
			continue
		}
		for _, tc := range m.ToolCalls {
			parts = append(parts, part{Type: "tool_call", ID: tc.ID, Name: tc.Name, Arguments: json.RawMessage(scrub(string(tc.Arguments)))})
		}
		if m.Role == model.RoleTool {
			for i := range parts {
				parts[i] = part{Type: "tool_call_response", ID: m.ToolCallID, Response: parts[i].Content}
			}
		}
		msgs = append(msgs, message{Role: string(m.Role), Parts: parts})
	}
	var out []attribute.KeyValue
	if len(system) > 0 {
		out = append(out, keySystemInstructions.String(marshal(system)))
	}
	return append(out, keyInputMessages.String(marshal(msgs)))
}

func captureResponse(resp model.ChatResponse, scrub func(string) string) attribute.KeyValue {
	var parts []part
	if resp.Content != "" {
		parts = append(parts, part{Type: "text", Content: scrub(resp.Content)})
	}
	for _, tc := range resp.ToolCalls {
		parts = append(parts, part{Type: "tool_call", ID: tc.ID, Name: tc.Name, Arguments: json.RawMessage(scrub(string(tc.Arguments)))})
	}
	return keyOutputMessages.String(marshal([]message{{Role: string(model.RoleAssistant), Parts: parts, FinishReason: string(resp.StopReason)}}))
}

// texts is the text of each part, with every memory item left out, trusted or
// not.
func texts(parts []content.Text) []string {
	var out []string
	untrusted := func(items []content.Untrusted) {
		for _, u := range items {
			if u.Provenance().Kind != content.KindMemory {
				out = append(out, u.Raw())
			}
		}
	}
	for _, p := range parts {
		switch v := p.(type) {
		case content.Trusted:
			if v.Provenance().Kind != content.KindMemory {
				out = append(out, v.String())
			}
		case content.Marked:
			untrusted(v.Section().Items())
		case content.Section:
			untrusted(v.Items())
		case content.Untrusted:
			untrusted([]content.Untrusted{v})
		}
	}
	return out
}

func marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// SpanOf returns the trace and span IDs of the span in ctx, as hex, and empty
// strings when ctx holds no valid span.
func SpanOf(ctx context.Context) (traceID, spanID string) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}

// EvaluatedRun identifies a finished run whose scores are emitted: its ID, its
// agent's name, and its span's trace and span IDs as recorded at its start.
type EvaluatedRun struct {
	ID, Agent, Trace, Span string
}

// Scores emits a span for the evaluation of run, linked to the run's span
// when its IDs are known, and records each score on the bonyan.eval.score
// histogram under that span. The run's ID is an attribute of the span only,
// so the metric stays low in cardinality. failed names the evaluators that
// could not score the run.
func (t *Telemetry) Scores(ctx context.Context, run EvaluatedRun, scores []score.Score, failed []string) {
	if t == nil {
		return
	}
	opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(keyRunID.String(run.ID))}
	if link, ok := spanContext(run.Trace, run.Span); ok {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: link}))
	}
	if run.Agent != "" {
		opts = append(opts, trace.WithAttributes(keyAgentName.String(run.Agent)))
	}
	ctx, span := t.tracer.Start(ctx, spanEval, opts...)
	defer span.End()
	if len(failed) > 0 {
		span.SetAttributes(keyEvalFailed.StringSlice(failed))
		span.SetStatus(codes.Error, "an evaluator failed")
	}
	for _, s := range scores {
		attrs := []attribute.KeyValue{keyEvalEvaluator.String(s.Evaluator), keyEvalMetric.String(s.Metric)}
		if run.Agent != "" {
			attrs = append(attrs, keyAgentName.String(run.Agent))
		}
		t.score.Record(ctx, s.Value, metric.WithAttributes(attrs...))
	}
}

// spanContext is the remote span context traceID and spanID name, when both
// are valid hex IDs.
func spanContext(traceID, spanID string) (trace.SpanContext, bool) {
	tid, err := trace.TraceIDFromHex(traceID)
	if err != nil {
		return trace.SpanContext{}, false
	}
	sid, err := trace.SpanIDFromHex(spanID)
	if err != nil {
		return trace.SpanContext{}, false
	}
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, Remote: true}), true
}
