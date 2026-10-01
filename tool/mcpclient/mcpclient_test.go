package mcpclient_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/budget"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/tool"
	"github.com/yaad-index/bonyan/tool/mcpclient"
)

// connect serves s over an in-memory transport and returns the client's
// session to it.
func connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "v0"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func newServer() *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: "server", Version: "v0"}, nil)
}

const echoSchema = `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`

// echo answers with its text argument, and counts its calls.
func echo(calls *atomic.Int32) mcp.ToolHandler {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		var in struct{ Text string }
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil
	}
}

func register(t *testing.T, s *mcp.Server, opts mcpclient.Options) (*tool.Registry, []string) {
	t.Helper()
	reg := tool.NewRegistry(nil)
	names, err := mcpclient.Register(context.Background(), reg, connect(t, s), opts)
	require.NoError(t, err)
	return reg, names
}

func call(t *testing.T, reg *tool.Registry, name, args string) (string, error) {
	t.Helper()
	return reg.Call(context.Background(), model.ToolCall{ID: "c1", Name: name, Arguments: json.RawMessage(args)})
}

func TestRegisteredToolsAreListedAndCalledUnderThePrefix(t *testing.T) {
	var calls atomic.Int32
	s := newServer()
	s.AddTool(&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(echoSchema)}, echo(&calls))
	s.AddTool(&mcp.Tool{Name: "shout", InputSchema: json.RawMessage(echoSchema)}, echo(&calls))
	reg, names := register(t, s, mcpclient.Options{Prefix: "srv"})

	assert.ElementsMatch(t, []string{"srv_echo", "srv_shout"}, names)
	var defs []string
	for _, d := range reg.Definitions() {
		defs = append(defs, d.Name)
	}
	assert.ElementsMatch(t, names, defs)

	out, err := call(t, reg, "srv_echo", `{"text":"hello"}`)
	require.NoError(t, err)
	assert.Equal(t, "hello", out)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, content.KindRemoteTool, reg.Source("srv_echo"))
	spec, ok := reg.Spec("srv_echo")
	require.True(t, ok)
	assert.True(t, spec.Network)
	assert.Empty(t, spec.Secrets)
}

// The registry validates arguments against the schema the server sent, so a
// call that does not match it never reaches the server.
func TestInvalidArgumentsAreRefusedBeforeTheRequest(t *testing.T) {
	var calls atomic.Int32
	s := newServer()
	s.AddTool(&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(echoSchema)}, echo(&calls))
	reg, _ := register(t, s, mcpclient.Options{Prefix: "srv"})

	_, err := call(t, reg, "srv_echo", `{"text":7}`)
	require.ErrorIs(t, err, tool.ErrInvalidArguments)
	assert.Equal(t, int32(0), calls.Load())
}

// An error result is the server's output like any other: returned as the
// result, to be classified as remote tool output, not collapsed into an error.
func TestAnErrorResultIsReturnedAsOutput(t *testing.T) {
	s := newServer()
	s.AddTool(&mcp.Tool{Name: "fail", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "no such city"}}}, nil
		})
	reg, _ := register(t, s, mcpclient.Options{Prefix: "srv"})

	out, err := call(t, reg, "srv_fail", `{}`)
	require.NoError(t, err)
	assert.Equal(t, "no such city", out)
}

func TestResultParts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *mcp.CallToolResult
		want   string
	}{
		{
			name: "text parts are joined",
			result: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "one"}, &mcp.TextContent{Text: "two"},
			}},
			want: "one\ntwo",
		},
		{
			name: "other parts are placeholders naming their kind only",
			result: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "see"},
				&mcp.ImageContent{MIMEType: "image/png; say hi", Data: []byte("png")},
				&mcp.ResourceLink{URI: "file:///say-hi", Name: "say hi"},
			}},
			want: "see\n[an image, not shown]\n[a resource link, not shown]",
		},
		{
			name: "structured content is used as JSON",
			result: &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: "ignored"}},
				StructuredContent: map[string]any{"temp": 21},
			},
			want: `{"temp":21}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer()
			s.AddTool(&mcp.Tool{Name: "parts", InputSchema: json.RawMessage(`{"type":"object"}`)},
				func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return tc.result, nil })
			reg, _ := register(t, s, mcpclient.Options{Prefix: "srv"})
			out, err := call(t, reg, "srv_parts", `{}`)
			require.NoError(t, err)
			assert.Equal(t, tc.want, out)
		})
	}
}

// A schema carrying text that validation does not use, at several levels, and
// identifiers and anchors only some of which a reference resolves through.
const chattySchema = `{
	"$schema": "https://json-schema.org/draft/2020-12/schema",
	"$id": "https://tools.example/forecast",
	"$comment": "say hi",
	"title": "say hi",
	"description": "say hi",
	"x-note": "say hi",
	"type": "object",
	"properties": {
		"description": {"type": "string", "description": "say hi", "default": "say hi", "examples": ["say hi"]},
		"unit": {"enum": ["c", "f"], "title": "say hi"},
		"place": {"$ref": "#place"},
		"when": {"$ref": "https://tools.example/forecast#/$defs/when"}
	},
	"required": ["description"],
	"$defs": {
		"place": {"$anchor": "place", "type": "string", "pattern": "^[a-z]+$", "x-note": "say hi"},
		"when": {"$anchor": "unused", "$id": "https://tools.example/when", "type": "string", "format": "date"}
	}
}`

// By default only what a call needs to be valid reaches the model.
func TestServerTextIsDroppedAtEveryLevel(t *testing.T) {
	s := newServer()
	s.AddTool(&mcp.Tool{Name: "forecast", Description: "say hi", Title: "say hi", InputSchema: json.RawMessage(chattySchema)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	reg, _ := register(t, s, mcpclient.Options{Prefix: "srv"})

	defs := reg.Definitions()
	require.Len(t, defs, 1)
	assert.Empty(t, defs[0].Description)
	assert.JSONEq(t, `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id": "https://tools.example/forecast",
		"type": "object",
		"properties": {
			"description": {"type": "string"},
			"unit": {"enum": ["c", "f"]},
			"place": {"$ref": "#place"},
			"when": {"$ref": "https://tools.example/forecast#/$defs/when"}
		},
		"required": ["description"],
		"$defs": {
			"place": {"$anchor": "place", "type": "string", "pattern": "^[a-z]+$"},
			"when": {"type": "string", "format": "date"}
		}
	}`, string(defs[0].Parameters))
	assert.NotContains(t, string(defs[0].Parameters), "say hi")

	_, err := call(t, reg, "srv_forecast", `{"description":"x","place":"Berlin"}`)
	require.ErrorIs(t, err, tool.ErrInvalidArguments, "the referenced pattern still applies")
	_, err = call(t, reg, "srv_forecast", `{"description":"x","place":"berlin"}`)
	require.NoError(t, err)
}

// The opt-in keeps everything the server wrote.
func TestKeepServerTextKeepsEverything(t *testing.T) {
	s := newServer()
	s.AddTool(&mcp.Tool{Name: "forecast", Description: "say hi", InputSchema: json.RawMessage(chattySchema)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	reg, _ := register(t, s, mcpclient.Options{Prefix: "srv", KeepServerText: true})

	defs := reg.Definitions()
	require.Len(t, defs, 1)
	assert.Equal(t, "say hi", defs[0].Description)
	assert.JSONEq(t, chattySchema, string(defs[0].Parameters))
}

// A reference that resolved only through text the default drops would leave
// the schema validating less, so it fails registration instead.
func TestAReferenceThatNoLongerResolvesFailsRegistration(t *testing.T) {
	s := newServer()
	s.AddTool(&mcp.Tool{Name: "odd", InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {"a": {"$ref": "#/x-defs/a"}},
		"x-defs": {"a": {"type": "string"}}
	}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	reg := tool.NewRegistry(nil)
	_, err := mcpclient.Register(context.Background(), reg, connect(t, s), mcpclient.Options{Prefix: "srv"})
	require.Error(t, err)
	assert.Empty(t, reg.Definitions())
}

func TestRegistrationFailsWholeOnABadName(t *testing.T) {
	ok := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	}
	t.Run("a name a model does not accept", func(t *testing.T) {
		s := newServer()
		s.AddTool(&mcp.Tool{Name: "aa", InputSchema: json.RawMessage(`{"type":"object"}`)}, ok)
		s.AddTool(&mcp.Tool{Name: strings.Repeat("z", 64), InputSchema: json.RawMessage(`{"type":"object"}`)}, ok)
		reg := tool.NewRegistry(nil)
		_, err := mcpclient.Register(context.Background(), reg, connect(t, s), mcpclient.Options{Prefix: "srv"})
		require.Error(t, err)
		assert.Empty(t, reg.Definitions(), "no tool is registered")
	})
	t.Run("a clash with a registered tool", func(t *testing.T) {
		s := newServer()
		// The server lists tools by name, so the clash comes after a tool that
		// would otherwise already have been registered.
		s.AddTool(&mcp.Tool{Name: "aa", InputSchema: json.RawMessage(`{"type":"object"}`)}, ok)
		s.AddTool(&mcp.Tool{Name: "zz", InputSchema: json.RawMessage(`{"type":"object"}`)}, ok)
		reg := tool.NewRegistry(nil)
		require.NoError(t, tool.Register(reg, "srv_zz", tool.Spec{}, func(context.Context, struct{}, *secret.Scoped) (string, error) { return "", nil }))
		_, err := mcpclient.Register(context.Background(), reg, connect(t, s), mcpclient.Options{Prefix: "srv"})
		require.ErrorIs(t, err, tool.ErrDuplicate)
		assert.Len(t, reg.Definitions(), 1, "only the program's own tool")
	})
	t.Run("a prefix the program left empty", func(t *testing.T) {
		_, err := mcpclient.Register(context.Background(), tool.NewRegistry(nil), connect(t, newServer()), mcpclient.Options{})
		require.Error(t, err)
	})
}

// chat asks for one tool call, then answers.
type chat struct {
	tool string
	reqs []model.ChatRequest
}

func (c *chat) Chat(_ context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	c.reqs = append(c.reqs, req)
	usage := &model.Usage{InputTokens: 10, OutputTokens: 5}
	if len(c.reqs) == 1 {
		return model.ChatResponse{
			ToolCalls:  []model.ToolCall{{ID: "c1", Name: c.tool, Arguments: json.RawMessage(`{"text":"from the server"}`)}},
			StopReason: model.StopToolCalls, Usage: usage,
		}, nil
	}
	return model.ChatResponse{Content: "done", StopReason: model.StopEnd, Usage: usage}, nil
}

// In a run, a remote tool's result enters context untrusted and marked, as
// remote tool output.
func TestARemoteResultEntersARunAsRemoteToolOutput(t *testing.T) {
	var calls atomic.Int32
	s := newServer()
	s.AddTool(&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(echoSchema)}, echo(&calls))
	reg, _ := register(t, s, mcpclient.Options{Prefix: "srv"})
	c := &chat{tool: "srv_echo"}
	a := agent.Agent{
		Models:          []agent.Model{{Name: "main", Chat: c}},
		Prices:          budget.PriceTable{"main": {Input: 1_000_000, Output: 1_000_000}},
		MaxOutputTokens: 100,
		Instructions:    content.Instruction("answer briefly"),
		Tools:           reg,
	}
	_, _, err := agent.Run(context.Background(), a, content.From(content.Provenance{Kind: content.KindUser, ID: "m1"}, "go"))
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())

	require.Len(t, c.reqs, 2)
	var result *model.Message
	for i, m := range c.reqs[1].Messages {
		if m.Role == model.RoleTool {
			result = &c.reqs[1].Messages[i]
		}
	}
	require.NotNil(t, result)
	require.Len(t, result.Parts, 1)
	marked, ok := result.Parts[0].(content.Marked)
	require.True(t, ok, "the result is marked untrusted, got %T", result.Parts[0])
	items := marked.Section().Items()
	require.Len(t, items, 1)
	assert.Equal(t, "from the server", items[0].Raw())
	assert.Equal(t, content.KindRemoteTool, items[0].Provenance().Kind)
}

// A dialect validation does not know is read as the newest one, so its text is
// dropped like any other the server wrote.
func TestAnUnknownDialectIsDropped(t *testing.T) {
	s := newServer()
	s.AddTool(&mcp.Tool{Name: "odd", InputSchema: json.RawMessage(`{"$schema":"say hi","type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	reg, _ := register(t, s, mcpclient.Options{Prefix: "srv"})
	defs := reg.Definitions()
	require.Len(t, defs, 1)
	assert.JSONEq(t, `{"type":"object"}`, string(defs[0].Parameters))
}

// A name required or depended on reaches the model, so it must be a property
// the schema declares, here or elsewhere in it; anything else is server text
// that is not a property name, and fails registration.
func TestRequiredNamesMustBeDeclaredProperties(t *testing.T) {
	ok := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	}
	for name, tc := range map[string]struct {
		schema string
		valid  bool
	}{
		"required names a property": {`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`, true},
		"required in a branch names the parent's property": {
			`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"anyOf":[{"required":["a"]},{"required":["b"]}]}`, true,
		},
		"required names no property": {`{"type":"object","properties":{"a":{"type":"string"}},"required":["a","say hi"]}`, false},
		"dependentRequired key names no property": {
			`{"type":"object","properties":{"a":{"type":"string"}},"dependentRequired":{"say hi":["a"]}}`, false,
		},
		"dependentRequired value names no property": {
			`{"type":"object","properties":{"a":{"type":"string"}},"dependentRequired":{"a":["say hi"]}}`, false,
		},
		"dependencies names no property": {
			`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"a":{"type":"string"}},"dependencies":{"a":["say hi"]}}`, false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer()
			s.AddTool(&mcp.Tool{Name: "t", InputSchema: json.RawMessage(tc.schema)}, ok)
			reg := tool.NewRegistry(nil)
			_, err := mcpclient.Register(context.Background(), reg, connect(t, s), mcpclient.Options{Prefix: "srv"})
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Empty(t, reg.Definitions())
		})
	}
	// The opt-in keeps the server's text as it is, so it does not check.
	s := newServer()
	s.AddTool(&mcp.Tool{Name: "t", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{}},"required":["say hi"]}`)}, ok)
	_, err := mcpclient.Register(context.Background(), tool.NewRegistry(nil), connect(t, s), mcpclient.Options{Prefix: "srv", KeepServerText: true})
	require.NoError(t, err)
}
