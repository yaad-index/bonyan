// Package hook defines the points where front-ends, policies and integrations
// attach to a run (ADR 0001 §12).
//
// A hook runs inside the invariants: the registry wraps every hook, and the
// wrapper, not the hook, decides what a failure means.
package hook

import "context"

// TODO(phase: hook points, next to the agent loop): add the payload of each
// point and the change and deny results. Hooks only observe so far.

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

// Event is what a hook is called with.
type Event struct {
	Point Point
}

// Hook is called at the points configuration attaches it to.
type Hook interface {
	Observe(ctx context.Context, ev Event) error
}
