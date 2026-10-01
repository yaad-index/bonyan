// Package hook defines the points where front-ends, policies and integrations
// attach to a run (ADR 0001 §12).
//
// A hook runs inside the invariants: the registry wraps every hook, and the
// wrapper, not the hook, decides what a failure means.
package hook

import (
	"context"
	"encoding/json"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
)

// TODO(phase 12a, memory; phase 14, approvals): the payloads and rights of the
// memory and approval points. Nothing calls them before then.

// Point is a place in a run where hooks are called.
type Point string

// The hook points of every run.
const (
	RunStart     Point = "run_start"
	RunEnd       Point = "run_end"
	UserMessage  Point = "user_message"
	Reply        Point = "reply"
	BeforeModel  Point = "before_model"
	AfterModel   Point = "after_model"
	BeforeTool   Point = "before_tool"
	AfterTool    Point = "after_tool"
	Approval     Point = "approval"
	MemoryWrite  Point = "memory_write"
	MemoryRecall Point = "memory_recall"
)

// Points lists every hook point.
func Points() []Point {
	return []Point{
		RunStart, RunEnd, UserMessage, Reply, BeforeModel, AfterModel,
		BeforeTool, AfterTool, Approval, MemoryWrite, MemoryRecall,
	}
}

// Valid reports whether p is one of the hook points.
func (p Point) Valid() bool {
	for _, q := range Points() {
		if p == q {
			return true
		}
	}
	return false
}

// Event is what a hook is called with: the point and that point's payload.
// Only the fields of the event's point are set. Content in an event has had
// every resolved secret removed (ADR 0001 §10).
type Event struct {
	Point Point
	// Message is the user message, at UserMessage.
	Message content.Untrusted
	// Messages are the request's messages, at BeforeModel.
	Messages []model.Message
	// Response is the model's response, at AfterModel.
	Response model.ChatResponse
	// Call is the tool call, at BeforeTool and AfterTool.
	Call model.ToolCall
	// Result is the tool's result, at AfterTool.
	Result content.Untrusted
	// Trusted is the trust policy's decision for Message or Result: true when
	// it declared the source trusted. A hook's change to either is untrusted
	// whatever this says, since a change never raises trust.
	Trusted bool
	// Reply is the final answer, at Reply.
	Reply string
	// Outcome is how the run ended, at RunEnd: "cleared" or the not-cleared
	// reason.
	Outcome string
}

// Hook is called at the points configuration attaches it to. A hook that only
// implements Hook observes: its error is recorded and changes nothing.
type Hook interface {
	Observe(ctx context.Context, ev Event) error
}

// Interceptor is a hook that may change or deny at the points that allow it
// (Rights). An error, a panic, or an action its point does not allow counts as
// a denial; after a tool call, where denying is not allowed, it withholds the
// result instead (ADR 0001 §12). Observe is not called on an Interceptor.
type Interceptor interface {
	Hook
	Intercept(ctx context.Context, ev Event) (Action, error)
}

// Action is what an Interceptor decides. The zero value lets the payload
// through unchanged. A change is plain data: bonyan puts it back in the type
// and provenance of what it replaces, so a change never raises trust.
type Action struct {
	// Deny ends the point: a denied tool call is reported to the model as
	// denied, and any other denial ends the run not cleared.
	Deny bool
	// Text replaces the message (UserMessage), the result (AfterTool) or the
	// reply (Reply).
	Text *string
	// Arguments replace the tool call's arguments (BeforeTool).
	Arguments json.RawMessage
	// Messages replace the request's messages (BeforeModel). A trusted part
	// must be one the request already held, and an untrusted part must carry a
	// provenance the request already held.
	Messages []model.Message
}

// Right is what a hook may do at a point beyond observing.
type Right uint8

// The rights.
const (
	Change Right = 1 << iota
	Deny
)

// Rights returns what a hook may do at p beyond observing (ADR 0001 §12).
func Rights(p Point) Right {
	switch p {
	case UserMessage, BeforeModel, BeforeTool, Reply:
		return Change | Deny
	case AfterTool:
		return Change
	}
	return 0
}

// Verdict is the outcome of running every hook at a point.
type Verdict struct {
	// Event is the payload as the last hook left it.
	Event Event
	// Changed names the hooks that changed the payload, in order.
	Changed []string
	// Denied names the hook whose denial ended the point; empty when none did.
	Denied string
}
