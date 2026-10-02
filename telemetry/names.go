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
//     with bonyan.eval.evaluator and bonyan.eval.metric naming it.
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
)
