// Package chatcompat is a chat model adapter for servers that speak the
// widely implemented chat-completions wire format.
//
// It is a thin client on net/http: it sends messages, tool definitions, a
// response schema and the max-output cap, and returns the reply, the requested
// tool calls and the reported usage. It does not retry; model.Retry does,
// stacked outside the budget and recording wrappers so every attempt is
// charged and recorded. The API key is resolved through a scoped secret
// resolver on every call, so a rotated key is picked up, and it is never part
// of configuration.
package chatcompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"

	"golang.org/x/time/rate"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/secret"
)

// DefaultMaxResponseBytes caps how much of a response body is read.
const DefaultMaxResponseBytes = 16 << 20

// Options configure a Client.
type Options struct {
	// BaseURL is the server's API root; "/chat/completions" is appended.
	BaseURL string
	// Model is the model name sent on the wire.
	Model string
	// Secrets and KeyName locate the API key. When KeyName is empty no key is
	// sent. The key is resolved on every call.
	Secrets *secret.Scoped
	KeyName string
	// RequestsPerSecond limits the call rate; zero means no limit.
	RequestsPerSecond float64
	// MaxResponseBytes caps the response body; zero means
	// DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// HTTPClient is used for requests; nil means http.DefaultClient.
	HTTPClient *http.Client
}

// Client is a model.Chat over the chat-completions wire format.
type Client struct {
	opts    Options
	url     string
	limiter *rate.Limiter
}

// New returns a client. BaseURL and Model are required, and a KeyName needs
// Secrets.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" || opts.Model == "" {
		return nil, errors.New("chatcompat: BaseURL and Model are required")
	}
	if opts.KeyName != "" && opts.Secrets == nil {
		return nil, errors.New("chatcompat: KeyName needs Secrets")
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	c := &Client{opts: opts, url: strings.TrimSuffix(opts.BaseURL, "/") + "/chat/completions"}
	if opts.RequestsPerSecond > 0 {
		c.limiter = rate.NewLimiter(rate.Limit(opts.RequestsPerSecond), 1)
	}
	return c, nil
}

var _ model.Chat = (*Client)(nil)

// Chat sends one request.
func (c *Client) Chat(ctx context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return model.ChatResponse{}, &model.CallError{Kind: ctxKind(ctx), NotSent: true, Err: err}
		}
	}
	body, err := json.Marshal(c.wireRequest(req))
	if err != nil {
		return model.ChatResponse{}, &model.CallError{Kind: model.ErrInvalid, NotSent: true, Err: err}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return model.ChatResponse{}, &model.CallError{Kind: model.ErrInvalid, NotSent: true, Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.opts.KeyName != "" {
		key, err := c.opts.Secrets.Resolve(ctx, c.opts.KeyName)
		if err != nil {
			return model.ChatResponse{}, &model.CallError{Kind: model.ErrRejected, NotSent: true, Err: err}
		}
		httpReq.Header.Set("Authorization", "Bearer "+key.Reveal())
	}

	// A request counts as not sent only if no connection was ever obtained:
	// the dial, the name lookup or the TLS handshake failed. Once a connection
	// is in hand, bytes may have been written, so the call counts as sent
	// (ADR 0001 §11).
	var gotConn atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { gotConn.Store(true) }}
	httpReq = httpReq.WithContext(httptrace.WithClientTrace(ctx, trace))

	resp, err := c.opts.HTTPClient.Do(httpReq)
	if err != nil {
		kind := model.ErrTransport
		if ctx.Err() != nil {
			kind = ctxKind(ctx)
		}
		return model.ChatResponse{}, &model.CallError{Kind: kind, Retryable: kind == model.ErrTransport, NotSent: !gotConn.Load(), Err: scrubURLError(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxResponseBytes+1))
	if err != nil {
		kind := model.ErrTransport
		if ctx.Err() != nil {
			kind = ctxKind(ctx)
		}
		return model.ChatResponse{}, &model.CallError{Kind: kind, Retryable: kind == model.ErrTransport, Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		// The body is not copied into the error: it can echo the request.
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return model.ChatResponse{}, &model.CallError{Kind: model.ErrRejected, Retryable: retry, Err: fmt.Errorf("status %d", resp.StatusCode)}
	}
	if int64(len(raw)) > c.opts.MaxResponseBytes {
		return model.ChatResponse{}, &model.CallError{Kind: model.ErrInvalid, Err: fmt.Errorf("response over %d bytes", c.opts.MaxResponseBytes)}
	}
	return parseResponse(raw)
}

func ctxKind(ctx context.Context) model.ErrorKind {
	if errors.Is(ctx.Err(), context.Canceled) {
		return model.ErrCanceled
	}
	return model.ErrTimeout
}

// scrubURLError drops the URL from a transport error, keeping the operation
// and the cause.
func scrubURLError(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string          `json:"type"`
	Function wireToolDetails `json:"function"`
}

type wireToolDetails struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireResponseFormat struct {
	Type       string         `json:"type"`
	JSONSchema wireJSONSchema `json:"json_schema"`
}

type wireJSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type wireRequest struct {
	Model          string              `json:"model"`
	Messages       []wireMessage       `json:"messages"`
	Tools          []wireTool          `json:"tools,omitempty"`
	ResponseFormat *wireResponseFormat `json:"response_format,omitempty"`
	MaxTokens      int                 `json:"max_tokens,omitempty"`
}

// wireRequest builds the request body. A request from the agent loop has passed
// the trust enforcement point, so its untrusted content arrives as marked
// sections and is sent as marked. A section that did not pass it is sent with
// the default marking; a bare untrusted part, which only a program calling the
// adapter directly can send, is sent as it is.
func (c *Client) wireRequest(req model.ChatRequest) wireRequest {
	out := wireRequest{Model: c.opts.Model, MaxTokens: req.MaxOutputTokens}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
		if len(m.Parts) > 0 || len(m.ToolCalls) == 0 {
			text := joinParts(m.Parts)
			wm.Content = &text
		}
		for _, tc := range m.ToolCalls {
			args := string(tc.Arguments)
			if args == "" {
				args = "{}"
			}
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{ID: tc.ID, Type: "function", Function: wireFunction{Name: tc.Name, Arguments: args}})
		}
		out.Messages = append(out.Messages, wm)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, wireTool{Type: "function", Function: wireToolDetails{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	if len(req.Schema) > 0 {
		out.ResponseFormat = &wireResponseFormat{Type: "json_schema", JSONSchema: wireJSONSchema{Name: "response", Schema: req.Schema, Strict: true}}
	}
	return out
}

func joinParts(parts []content.Text) string {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		switch v := p.(type) {
		case content.Trusted:
			texts = append(texts, v.String())
		case content.Untrusted:
			texts = append(texts, v.Raw())
		case content.Section:
			texts = append(texts, v.Render())
		case content.Marked:
			texts = append(texts, v.Text())
		}
	}
	return strings.Join(texts, "\n\n")
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Content   *string        `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
	} `json:"usage"`
}

func parseResponse(raw []byte) (model.ChatResponse, error) {
	var wr wireResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return model.ChatResponse{}, &model.CallError{Kind: model.ErrInvalid, Err: errors.New("response is not valid JSON")}
	}
	if len(wr.Choices) == 0 {
		return model.ChatResponse{}, &model.CallError{Kind: model.ErrInvalid, Err: errors.New("response has no choices")}
	}
	ch := wr.Choices[0]
	out := model.ChatResponse{StopReason: stopReason(ch.FinishReason)}
	if ch.Message.Content != nil {
		out.Content = *ch.Message.Content
	}
	for _, tc := range ch.Message.ToolCalls {
		args := json.RawMessage(tc.Function.Arguments)
		if !json.Valid(args) {
			return model.ChatResponse{}, &model.CallError{Kind: model.ErrInvalid, Err: fmt.Errorf("tool call %q has arguments that are not JSON", tc.ID)}
		}
		out.ToolCalls = append(out.ToolCalls, model.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: args})
	}
	// Usage is reported only when both counts are present; otherwise it is
	// missing, which a budgeted run treats as an error, never as zero.
	if u := wr.Usage; u != nil && u.PromptTokens != nil && u.CompletionTokens != nil {
		out.Usage = &model.Usage{InputTokens: *u.PromptTokens, OutputTokens: *u.CompletionTokens}
	}
	return out, nil
}

func stopReason(finish string) model.StopReason {
	switch finish {
	case "stop":
		return model.StopEnd
	case "tool_calls", "function_call":
		return model.StopToolCalls
	case "length":
		return model.StopMaxTokens
	}
	return model.StopOther
}
