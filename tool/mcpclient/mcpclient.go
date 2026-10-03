// Package mcpclient registers the tools of a Model Context Protocol server in
// a tool registry, where a run calls them like in-process tools (ADR 0001 §5).
//
// What differs is what the server controls. Results, error results included,
// are remote tool output, a source kind of their own (ADR 0001 §3). Of the
// text the server writes into a tool's definition, only what a call needs to
// be valid reaches the model by default: the tool's name under the program's
// prefix, and the schema keywords validation uses, with their property names,
// enum and const values, patterns, formats and the names of the definitions a
// reference points to. Descriptions, titles, examples, defaults, comments and
// any keyword the server invents are dropped at every level.
//
// A name a schema requires, or makes another property depend on, must be a
// property the schema declares somewhere, since it reaches the model as a
// property name. A schema that requires a field it allows only through
// additionalProperties, without declaring it, fails Register for that reason.
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yaad-index/bonyan/tool"
	"github.com/yaad-index/bonyan/tool/internal/remote"
)

// ErrInputRequired is what a call returns when the server asks the client for
// input before it will answer. This client does not provide it.
var ErrInputRequired = errors.New("mcpclient: the server asked for input")

// Options configures Register.
type Options struct {
	// Prefix is put before each tool's name, joined by an underscore, so tools
	// from different servers cannot collide. It is required, and it is chosen
	// by the program rather than taken from the server. It is also the server's
	// name in the source of its tools' results, which a trust policy can
	// classify by (ADR 0001 §3).
	Prefix string
	// KeepServerText keeps the text Register drops by default: the tool's
	// description and every schema keyword validation does not use. Setting it
	// is the program accepting untrusted text in instruction position, outside
	// any marked section (ADR 0001 §5).
	KeepServerText bool
	// NeedsApproval makes every call of every tool from this server wait for
	// approval before it runs (ADR 0001 §7).
	NeedsApproval bool
}

// validName is what a model accepts as a tool's name.
var validName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Register lists the tools session's server offers and registers each in reg
// under opts.Prefix, returning the names it registered them under. The list is
// read once: tools the server adds or removes later are not seen. A tool whose
// name is not one a model accepts, clashes with a registered tool or has a
// schema that does not parse fails Register before any tool is registered.
// The caller owns session and closes it after the run.
func Register(ctx context.Context, reg *tool.Registry, session *mcp.ClientSession, opts Options) ([]string, error) {
	if reg == nil || session == nil {
		return nil, errors.New("mcpclient: nil registry or session")
	}
	if !validName.MatchString(opts.Prefix) {
		return nil, fmt.Errorf("mcpclient: prefix %q is not a valid tool name", opts.Prefix)
	}
	type pending struct {
		name, remoteName, description string
		schema                        json.RawMessage
	}
	var all []pending
	seen := map[string]bool{}
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcpclient: list tools: %w", err)
		}
		name := opts.Prefix + "_" + t.Name
		if !validName.MatchString(name) {
			return nil, fmt.Errorf("mcpclient: tool name %q is not a valid tool name", name)
		}
		if _, taken := reg.Spec(name); taken || seen[name] {
			return nil, fmt.Errorf("%w: %q", tool.ErrDuplicate, name)
		}
		seen[name] = true
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("mcpclient: tool %q: schema: %w", name, err)
		}
		description := t.Description
		if !opts.KeepServerText {
			if schema, err = keepValidation(schema); err != nil {
				return nil, fmt.Errorf("mcpclient: tool %q: schema: %w", name, err)
			}
			description = ""
		}
		if _, err := tool.ParseSchema(schema); err != nil {
			return nil, fmt.Errorf("mcpclient: tool %q: %w", name, err)
		}
		all = append(all, pending{name: name, remoteName: t.Name, description: description, schema: schema})
	}
	names := make([]string, 0, len(all))
	for _, p := range all {
		remoteName := p.remoteName
		call := func(ctx context.Context, args json.RawMessage) (string, error) {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: remoteName, Arguments: args})
			if err != nil {
				return "", err
			}
			return resultText(res)
		}
		if err := remote.Register(reg, opts.Prefix, p.name, p.description, p.schema, opts.NeedsApproval, call); err != nil {
			return names, err
		}
		names = append(names, p.name)
	}
	return names, nil
}

// resultText is a call's result as the text the model is given. An error
// result is returned the same way, as the server's output: it is remote tool
// output like any other result. Structured content, when present, is used as
// JSON; otherwise text parts are joined, and every other part is a one-line
// placeholder naming its kind, never its bytes or anything the server wrote
// about them.
func resultText(res *mcp.CallToolResult) (string, error) {
	if len(res.InputRequests) > 0 {
		return "", ErrInputRequired
	}
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return "", fmt.Errorf("mcpclient: structured content: %w", err)
		}
		return string(b), nil
	}
	parts := make([]string, 0, len(res.Content))
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, c.Text)
		case *mcp.ImageContent:
			parts = append(parts, "[an image, not shown]")
		case *mcp.AudioContent:
			parts = append(parts, "[audio, not shown]")
		case *mcp.ResourceLink:
			parts = append(parts, "[a resource link, not shown]")
		case *mcp.EmbeddedResource:
			parts = append(parts, "[an embedded resource, not shown]")
		default:
			parts = append(parts, "[content of another kind, not shown]")
		}
	}
	return strings.Join(parts, "\n"), nil
}
