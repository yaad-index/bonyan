// Package tool is the tool registry (ADR 0001 §5): tools with typed inputs and
// outputs, the input's JSON Schema derived from its Go type, the model's
// arguments validated against it before a tool runs, and each tool handed only
// the secrets it declared.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/tool/internal/remote"
)

// ErrUnknown is what Call returns for a name no tool is registered under.
var ErrUnknown = errors.New("tool: unknown tool")

// ErrInvalidArguments is what Call returns for arguments that do not match the
// tool's input schema. The tool does not run.
var ErrInvalidArguments = errors.New("tool: invalid arguments")

// ErrDuplicate is what Register returns for a name already taken.
var ErrDuplicate = errors.New("tool: name already registered")

// Spec describes a tool and what it may touch. Only Secrets is enforced,
// because bonyan is what resolves them; Network and Filesystem are declarations
// for review and approval, not a sandbox (ADR 0001 §5).
type Spec struct {
	Description string
	// Secrets are the names the tool may resolve. Its scope resolves no other.
	Secrets    []string
	Network    bool
	Filesystem bool
}

// Func is a tool's implementation. secrets resolves only the names in its Spec.
type Func[In, Out any] func(ctx context.Context, in In, secrets *secret.Scoped) (Out, error)

// RawFunc is the implementation of a tool whose input schema is given rather
// than derived from a Go type. It receives arguments already validated against
// that schema and returns its output as text.
type RawFunc func(ctx context.Context, args json.RawMessage, secrets *secret.Scoped) (string, error)

// Registry holds the tools a run may call.
type Registry struct {
	mu       sync.RWMutex
	tools    map[string]*entry
	order    []string
	resolver *secret.Resolver
}

type entry struct {
	def    model.ToolDef
	spec   Spec
	schema Schema
	source content.Kind
	call   func(ctx context.Context, args []byte, secrets *secret.Scoped) (string, error)
}

func init() { remote.Register = registerRemote }

// NewRegistry returns an empty registry whose tools resolve secrets through
// resolver. A nil resolver resolves nothing.
func NewRegistry(resolver *secret.Resolver) *Registry {
	if resolver == nil {
		resolver = secret.NewResolver()
	}
	return &Registry{tools: map[string]*entry{}, resolver: resolver}
}

// Register adds a tool under name, with its input schema derived from In.
func Register[In, Out any](r *Registry, name string, spec Spec, fn Func[In, Out]) error {
	if name == "" {
		return errors.New("tool: empty name")
	}
	if fn == nil {
		return fmt.Errorf("tool %q: nil function", name)
	}
	schema, err := SchemaFor[In]()
	if err != nil {
		return fmt.Errorf("tool %q: %w", name, err)
	}
	e := &entry{
		def:    model.ToolDef{Name: name, Description: spec.Description, Parameters: schema.JSON()},
		spec:   spec,
		schema: schema,
		source: content.KindTool,
		call: func(ctx context.Context, args []byte, secrets *secret.Scoped) (string, error) {
			var in In
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("%w: %w", ErrInvalidArguments, &ValidationError{})
			}
			out, err := fn(ctx, in, secrets)
			if err != nil {
				return "", err
			}
			b, err := json.Marshal(out)
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
	}
	return r.add(name, e)
}

// RegisterSchema adds a tool under name whose input schema is given rather
// than derived from a Go type. Its arguments are validated against schema
// before fn runs, and fn resolves only the secrets in spec, as with Register.
func RegisterSchema(r *Registry, name string, spec Spec, schema Schema, fn RawFunc) error {
	if name == "" {
		return errors.New("tool: empty name")
	}
	if fn == nil {
		return fmt.Errorf("tool %q: nil function", name)
	}
	if schema.IsZero() {
		return fmt.Errorf("tool %q: no schema", name)
	}
	return r.add(name, &entry{
		def:    model.ToolDef{Name: name, Description: spec.Description, Parameters: schema.JSON()},
		spec:   spec,
		schema: schema,
		source: content.KindTool,
		call: func(ctx context.Context, args []byte, secrets *secret.Scoped) (string, error) {
			return fn(ctx, args, secrets)
		},
	})
}

// registerRemote adds a tool served by a tool server: its results are remote
// tool output, it resolves no secrets and it may reach the network.
func registerRemote(reg any, name, description string, schema json.RawMessage, call remote.Call) error {
	r, ok := reg.(*Registry)
	if !ok || r == nil {
		return errors.New("tool: not a registry")
	}
	if name == "" {
		return errors.New("tool: empty name")
	}
	if call == nil {
		return fmt.Errorf("tool %q: nil function", name)
	}
	s, err := ParseSchema(schema)
	if err != nil {
		return fmt.Errorf("tool %q: %w", name, err)
	}
	spec := Spec{Description: description, Network: true}
	return r.add(name, &entry{
		def:    model.ToolDef{Name: name, Description: description, Parameters: s.JSON()},
		spec:   spec,
		schema: s,
		source: content.KindRemoteTool,
		call: func(ctx context.Context, args []byte, _ *secret.Scoped) (string, error) {
			return call(ctx, args)
		},
	})
}

func (r *Registry) add(name string, e *entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicate, name)
	}
	r.tools[name] = e
	r.order = append(r.order, name)
	return nil
}

// Definitions describes every tool to the model, in registration order.
func (r *Registry) Definitions() []model.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]model.ToolDef, 0, len(r.order))
	for _, n := range r.order {
		defs = append(defs, r.tools[n].def)
	}
	return defs
}

// Spec returns what the tool registered under name declared.
func (r *Registry) Spec(name string) (Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.tools[name]
	if !ok {
		return Spec{}, false
	}
	return e.spec, true
}

// Source is the source kind of the results of the tool registered under name:
// content.KindRemoteTool for a tool on a tool server, content.KindTool for any
// other, and for a name no tool is registered under.
func (r *Registry) Source(name string) content.Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.tools[name]; ok {
		return e.source
	}
	return content.KindTool
}

// Call validates the call's arguments against the tool's input schema and runs
// it with a secret scope limited to its declared names. An absent argument
// object counts as an empty one.
func (r *Registry) Call(ctx context.Context, call model.ToolCall) (string, error) {
	r.mu.RLock()
	e, ok := r.tools[call.Name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknown, call.Name)
	}
	args := []byte(call.Arguments)
	if len(args) == 0 {
		args = []byte("{}")
	}
	if err := e.schema.Validate(args); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	return e.call(ctx, args, r.resolver.Scope(e.spec.Secrets...))
}

// Schema is a JSON Schema ready to validate against.
type Schema struct {
	raw      json.RawMessage
	resolved *jsonschema.Resolved
}

// SchemaFor derives the schema of T from its Go type (see the package
// jsonschema-go for the rules and their known gaps: format is not enforced,
// omitempty fields are never required, and a slice also accepts null).
func SchemaFor[T any]() (Schema, error) {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		return Schema{}, err
	}
	return newSchema(s)
}

// ParseSchema reads a schema written as JSON.
func ParseSchema(raw json.RawMessage) (Schema, error) {
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return Schema{}, fmt.Errorf("tool: schema: %w", err)
	}
	return newSchema(&s)
}

func newSchema(s *jsonschema.Schema) (Schema, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return Schema{}, err
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return Schema{}, fmt.Errorf("tool: schema: %w", err)
	}
	return Schema{raw: raw, resolved: resolved}, nil
}

// JSON is the schema as sent to a model.
func (s Schema) JSON() json.RawMessage { return append(json.RawMessage(nil), s.raw...) }

// IsZero reports whether s holds no schema.
func (s Schema) IsZero() bool { return s.resolved == nil }

// ValidationError says where data failed a schema and which rule it broke:
// the schema path and the rule's keyword, never a value from the data, so the
// message can go back to a model without echoing what it wrote.
type ValidationError struct {
	// Path is the schema location of the failure, such as
	// "/properties/items/items"; empty when it could not be told.
	Path string
	// Rule is the schema keyword that failed, such as "required" or "type",
	// or "json" for data that is not JSON.
	Rule string
}

func (e *ValidationError) Error() string {
	switch {
	case e.Rule == "json":
		return "not valid JSON"
	case e.Path == "" || e.Rule == "":
		return "does not match the schema"
	}
	return e.Path + ": " + e.Rule
}

// Validate checks that data, a JSON text, matches the schema. A failure is a
// *ValidationError.
func (s Schema) Validate(data []byte) error {
	if s.resolved == nil {
		return errors.New("tool: no schema")
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return &ValidationError{Rule: "json"}
	}
	if err := s.resolved.Validate(v); err != nil {
		return describe(err.Error())
	}
	return nil
}

// describe reduces a validator message to the deepest schema path it names and
// the keyword that failed there. The message itself is never kept: past the
// keyword it can quote the data.
func describe(msg string) *ValidationError {
	const step = "validating "
	rest := msg
	path := ""
	for strings.HasPrefix(rest, step) {
		end := strings.Index(rest, ": ")
		if end < 0 {
			return &ValidationError{}
		}
		path = rest[len(step):end]
		rest = rest[end+2:]
	}
	if path == "root" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return &ValidationError{}
	}
	for phrase, rule := range phrases {
		if strings.HasPrefix(rest, phrase) {
			return &ValidationError{Path: path, Rule: rule}
		}
	}
	end := strings.Index(rest, ":")
	if end <= 0 || !isKeyword(rest[:end]) {
		return &ValidationError{Path: path}
	}
	return &ValidationError{Path: path, Rule: rest[:end]}
}

// phrases are the validator's messages that name no keyword, by the keyword
// they report.
var phrases = map[string]string{
	"unexpected additional properties": "additionalProperties",
}

// isKeyword reports whether k looks like a JSON Schema keyword: letters only.
func isKeyword(k string) bool {
	for _, r := range k {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return k != ""
}
