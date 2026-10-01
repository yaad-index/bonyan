// Package record writes model calls to recordings and replays them (ADR 0001
// §8). Chat and classifier calls are recorded; embedding calls are not.
//
// A recording is a JSON Lines file: a header line, then one line per entry.
// The header names the format and its version, so a reader can refuse a
// recording it does not understand. What reaches a sink has already been
// through bonyan's rules: recalled-memory parts are excluded unless the sink
// is a full recording (§4), and every resolved secret is scrubbed (§10). A
// failed call is recorded by its error kind, never its error text.
package record

import (
	"encoding/json"
	"time"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
)

// Format and Version identify the recording format in its header.
const (
	Format  = "bonyan-recording"
	Version = 1
)

// Header is the first line of a recording.
type Header struct {
	Format  string    `json:"format"`
	Version int       `json:"version"`
	Full    bool      `json:"full"`
	Created time.Time `json:"created"`
}

// Entry is one line after the header: a model call or an event. Chat and
// classifier calls share one sequence, so a replay serves them in the order a
// run made them.
type Entry struct {
	Call  *Call  `json:"call,omitempty"`
	Event *Event `json:"event,omitempty"`
}

// The kinds of recorded call.
const (
	KindChat     = "chat"
	KindClassify = "classify"
)

// Call is one recorded model call: a chat call, with Request, or a classifier
// call, with Input. Embedding calls are not recorded.
type Call struct {
	Seq         int64    `json:"seq"`
	Kind        string   `json:"kind"`
	Model       string   `json:"model"`
	Fingerprint string   `json:"fingerprint"`
	Request     *Request `json:"request,omitempty"`
	Input       *Part    `json:"input,omitempty"`
	Response    Response `json:"response"`
	// ErrorKind is set when the call failed, and Response is then empty.
	ErrorKind model.ErrorKind `json:"error_kind,omitempty"`
}

// Request is a recorded chat request.
type Request struct {
	Messages        []Message       `json:"messages"`
	Tools           []Tool          `json:"tools,omitempty"`
	Schema          json.RawMessage `json:"schema,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens"`
}

// Message is a recorded message.
type Message struct {
	Role       model.Role `json:"role"`
	Parts      []Part     `json:"parts"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Part is a recorded message part. Trusted parts carry only their text.
// Untrusted parts carry their provenance too. An excluded part carries its
// provenance and no text.
type Part struct {
	Trusted    bool        `json:"trusted"`
	Text       string      `json:"text,omitempty"`
	Provenance *Provenance `json:"provenance,omitempty"`
	Excluded   bool        `json:"excluded,omitempty"`
	// Section and Items are set for a section of untrusted material: its
	// label and its items, each recorded as an untrusted part.
	Section string `json:"section,omitempty"`
	Items   []Part `json:"items,omitempty"`
}

// Provenance is where an untrusted part came from.
type Provenance struct {
	Kind   content.Kind `json:"kind"`
	Origin content.Kind `json:"origin,omitempty"`
	ID     string       `json:"id,omitempty"`
}

// Tool is a recorded tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is a recorded tool call requested by the model.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Usage is recorded token usage. The format names its own fields rather than
// relying on another package's, so the format changes only when Version does.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Label is a recorded classifier label.
type Label struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
}

// Response is a recorded response: content and tool calls for a chat call,
// labels for a classifier call.
type Response struct {
	Content    string           `json:"content,omitempty"`
	ToolCalls  []ToolCall       `json:"tool_calls,omitempty"`
	StopReason model.StopReason `json:"stop_reason,omitempty"`
	Labels     []Label          `json:"labels,omitempty"`
	Usage      *Usage           `json:"usage,omitempty"`
}

// ErrorOther is the kind recorded for a failure that is not a model.CallError
// and not a context error.
const ErrorOther model.ErrorKind = "other"

// Event is something a wrapper recorded: a trust decision, or a hook or
// policy failure. It never holds content.
type Event struct {
	Slot     string `json:"slot"`
	Name     string `json:"name"`
	Point    string `json:"point,omitempty"`
	Source   string `json:"source,omitempty"`
	Decision string `json:"decision,omitempty"`
	Failure  string `json:"failure,omitempty"`
	// Item and Tokens describe an item left out of a call's context: its
	// source's ID (never for memory) and what it counted.
	Item   string `json:"item,omitempty"`
	Tokens int64  `json:"tokens,omitempty"`
}

func (r Response) usage() *model.Usage {
	if r.Usage == nil {
		return nil
	}
	return &model.Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens}
}

func (r Response) classify() model.ClassifyResponse {
	out := model.ClassifyResponse{Usage: r.usage()}
	for _, l := range r.Labels {
		out.Labels = append(out.Labels, model.Label{Name: l.Name, Confidence: l.Confidence})
	}
	return out
}

func (r Response) model() model.ChatResponse {
	out := model.ChatResponse{Content: r.Content, StopReason: r.StopReason}
	for _, tc := range r.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, model.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
	}
	out.Usage = r.usage()
	return out
}
