package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// The semantic conventions bonyan's names come from. The GenAI names were
// removed from the general conventions after v1.41.0, when they moved to a
// specification of their own that has no Go package yet, so they are pinned
// here and nowhere else (ADR 0001 §9).
const SchemaURL = semconv.SchemaURL

// The GenAI conventions' names bonyan uses.
const (
	keyOperationName      = semconv.GenAIOperationNameKey
	keyAgentName          = semconv.GenAIAgentNameKey
	keyProviderName       = semconv.GenAIProviderNameKey
	keyRequestModel       = semconv.GenAIRequestModelKey
	keyRequestMaxTokens   = semconv.GenAIRequestMaxTokensKey
	keyRequestTemperature = semconv.GenAIRequestTemperatureKey
	keyResponseFinish     = semconv.GenAIResponseFinishReasonsKey
	keyUsageInputTokens   = semconv.GenAIUsageInputTokensKey
	keyUsageOutputTokens  = semconv.GenAIUsageOutputTokensKey
	keyTokenType          = semconv.GenAITokenTypeKey
	keyToolName           = semconv.GenAIToolNameKey
	keyToolCallID         = semconv.GenAIToolCallIDKey
	keyToolType           = semconv.GenAIToolTypeKey
	keySystemInstructions = semconv.GenAISystemInstructionsKey
	keyInputMessages      = semconv.GenAIInputMessagesKey
	keyOutputMessages     = semconv.GenAIOutputMessagesKey
	keyToolCallArguments  = semconv.GenAIToolCallArgumentsKey
	keyToolCallResult     = semconv.GenAIToolCallResultKey
	keyErrorType          = semconv.ErrorTypeKey
)

// The values of gen_ai.operation.name and gen_ai.token.type bonyan emits.
var (
	opInvokeAgent = semconv.GenAIOperationNameInvokeAgent
	opChat        = semconv.GenAIOperationNameChat
	opExecuteTool = semconv.GenAIOperationNameExecuteTool
	tokenInput    = semconv.GenAITokenTypeInput
	tokenOutput   = semconv.GenAITokenTypeOutput
	// errorTypeOther is error.type for an error of no kind bonyan names.
	errorTypeOther = semconv.ErrorTypeOther
)

// The names the conventions do not cover. Each is bonyan's own, and this is
// the one place they are defined:
//
//   - span bonyan.step: one step of the agent loop, a child of the run's span
//     and the parent of that step's model and tool calls;
//   - bonyan.step.index: the step's number, from 1;
//   - bonyan.run.outcome: how a run ended: "cleared", or the reason it was not
//     cleared;
//   - bonyan.usage.cost: what a model call cost, in millionths of the price
//     table's unit, as a span attribute and as a counter;
//   - span bonyan.eval: the evaluation of one finished run, linked to the
//     run's span;
//   - bonyan.run.id: the identifier of the run evaluated;
//   - bonyan.eval.failed: the evaluators that could not score the run;
//   - bonyan.eval.score: a score an evaluator gave a run, as a histogram,
//     with bonyan.eval.evaluator and bonyan.eval.metric naming it;
//   - bonyan.prompt.id and bonyan.prompt.hash: on a model call's span, the
//     versioned prompt the call was made with and the hash of its text, the
//     ID absent for unversioned instructions; never the text (ADR 0001 §10).
const (
	spanStep         = "bonyan.step"
	keyStepIndex     = attribute.Key("bonyan.step.index")
	keyRunOutcome    = attribute.Key("bonyan.run.outcome")
	keyUsageCost     = attribute.Key("bonyan.usage.cost")
	metricUsageCost  = "bonyan.usage.cost"
	spanEval         = "bonyan.eval"
	keyRunID         = attribute.Key("bonyan.run.id")
	keyEvalFailed    = attribute.Key("bonyan.eval.failed")
	keyEvalEvaluator = attribute.Key("bonyan.eval.evaluator")
	keyEvalMetric    = attribute.Key("bonyan.eval.metric")
	metricEvalScore  = "bonyan.eval.score"
	keyPromptID      = attribute.Key("bonyan.prompt.id")
	keyPromptHash    = attribute.Key("bonyan.prompt.hash")
)

// The bucket boundaries of bonyan's histograms, set when each is created so
// any meter provider records them. Without them the SDK's defaults (0, 5, 10,
// 25 ...) apply, which are sized for milliseconds.
var (
	// durationBuckets are the GenAI conventions' advised boundaries for
	// gen_ai.client.operation.duration, in seconds: 10ms to about 82s.
	durationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	// tokenBuckets are the advised boundaries for gen_ai.client.token.usage.
	tokenBuckets = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
	// scoreBuckets serve both kinds of score (score.Score.Value): a 0/1 flag
	// falls at or below 0.5 or 1, and a count into doubling buckets up to
	// 2^26.
	scoreBuckets = append([]float64{0.5}, doubling(1, 1<<26)...)
)

// doubling is from, 2*from, 4*from ... up to and including to.
func doubling(from, to float64) []float64 {
	var out []float64
	for v := from; v <= to; v *= 2 {
		out = append(out, v)
	}
	return out
}
