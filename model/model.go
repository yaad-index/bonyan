// Package model defines the interfaces bonyan uses to reach a model, one per
// kind of model. Adapters implement them; nothing here depends on a provider.
//
// Every response reports token usage as a pointer. A nil Usage means the model
// reported none, which is different from reporting zero: a budgeted run treats
// missing usage as an error rather than as free (ADR 0001 §11).
package model

import (
	"context"
	"encoding/json"

	"github.com/yaad-index/bonyan/content"
)

// Role is who a message is from.
type Role string

// The message roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one message of a chat request. Its parts are content.Text, so
// whether each part is trusted, and where untrusted parts came from, survives
// into the request an adapter sends. There is no plain-string field.
type Message struct {
	Role  Role
	Parts []content.Text
	// ToolCalls are the calls a RoleAssistant message requested, so the turn
	// can be sent back in the history that answers them.
	ToolCalls []ToolCall
	// ToolCallID links a RoleTool message to the call it answers.
	ToolCallID string
}

// ToolResult builds the message answering a tool call. The result is
// untrusted, with the tool call as its provenance.
func ToolResult(callID, result string) Message {
	return Message{
		Role:       RoleTool,
		Parts:      []content.Text{content.From(content.Provenance{Kind: content.KindTool, ID: callID}, result)},
		ToolCallID: callID,
	}
}

// ToolDef describes a tool the model may call.
type ToolDef struct {
	Name        string
	Description string
	// Parameters is the JSON Schema of the tool's input.
	Parameters json.RawMessage
}

// ToolCall is a tool invocation the model requested.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Usage is the token usage a model reported for one call.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// StopReason is why a model stopped generating.
type StopReason string

// The stop reasons adapters map to.
const (
	StopEnd       StopReason = "end"        // a final answer
	StopToolCalls StopReason = "tool_calls" // the model requested tools
	StopMaxTokens StopReason = "max_tokens" // the output cap was reached
	StopOther     StopReason = "other"
)

// ChatRequest is one call to a chat model.
type ChatRequest struct {
	Messages []Message
	Tools    []ToolDef
	// Schema, when set, requires the response to be JSON matching it.
	Schema json.RawMessage
	// MaxOutputTokens caps the response. A budgeted run requires it (§11).
	MaxOutputTokens int
	// Temperature, when set, is the sampling temperature. Nil leaves it to
	// the provider's default.
	Temperature *float64
}

// ChatResponse is a chat model's reply.
type ChatResponse struct {
	Content    string
	ToolCalls  []ToolCall
	StopReason StopReason
	Usage      *Usage // nil when the model reported no usage
}

// Chat is a chat or completion model.
type Chat interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// EmbedResponse is an embedding model's reply, one vector per input.
type EmbedResponse struct {
	Vectors [][]float32
	Usage   *Usage // nil when the model reported no usage
}

// Embedder is an embedding model.
//
// It takes plain strings deliberately. An embedding yields vectors, not text a
// model will follow, so the untrusted/trusted distinction does not change what
// it does. A caller that needs provenance for the embedded items keeps it
// alongside, indexed like the inputs.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) (EmbedResponse, error)
}

// Label is one classifier label with its confidence in [0, 1].
type Label struct {
	Name       string
	Confidence float64
}

// ClassifyResponse is a classifier's reply: every label it scores.
type ClassifyResponse struct {
	Labels []Label
	Usage  *Usage // nil when the model reported no usage
}

// Classifier scores text against labels fixed by its configuration. It takes
// no prompt or instruction: the text is data, and a caller cannot change what
// the classifier is asked.
type Classifier interface {
	Classify(ctx context.Context, text content.Untrusted) (ClassifyResponse, error)
}
