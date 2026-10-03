package honcho

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// errNotFound is a resource the service does not hold.
var errNotFound = errors.New("honcho: not found")

// errConflict is a request the service refuses in the resource's present
// state, such as deleting a workspace that still has sessions.
var errConflict = errors.New("honcho: conflict")

// client calls the service's v3 HTTP API. An error names the operation and
// the status, never a response body, which can hold stored text.
type client struct {
	base  string
	http  *http.Client
	token string
}

// call sends body as JSON to path with method and decodes the answer into
// out, when out is not nil. op names the call in an error.
func (c *client) call(ctx context.Context, method, op, path string, query url.Values, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("honcho: %s: %w", op, err)
		}
		r = bytes.NewReader(b)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return fmt.Errorf("honcho: %s: %w", op, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("honcho: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", errNotFound, op)
	case resp.StatusCode == http.StatusConflict:
		return fmt.Errorf("%w: %s", errConflict, op)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("honcho: %s: status %d", op, resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("honcho: %s: decoding the answer: %w", op, err)
	}
	return nil
}

// page is one page of a list.
type page[T any] struct {
	Items []T `json:"items"`
	Page  int `json:"page"`
	Pages int `json:"pages"`
}

// pageSize is the largest page the service serves.
const pageSize = 100

// list collects every page of a list endpoint.
func list[T any](ctx context.Context, c *client, op, path string, body any) ([]T, error) {
	var out []T
	for n := 1; ; n++ {
		var p page[T]
		q := url.Values{"page": {strconv.Itoa(n)}, "size": {strconv.Itoa(pageSize)}}
		if err := c.call(ctx, http.MethodPost, op, path, q, body, &p); err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if n >= p.Pages {
			return out, nil
		}
	}
}

type workspace struct {
	ID string `json:"id"`
}

type session struct {
	ID string `json:"id"`
}

type message struct {
	ID        string         `json:"id"`
	Content   string         `json:"content"`
	PeerID    string         `json:"peer_id"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt time.Time      `json:"created_at"`
}

type conclusion struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// reasoning is the service's switch for its deriver.
type reasoning struct {
	Enabled            bool   `json:"enabled"`
	CustomInstructions string `json:"custom_instructions,omitempty"`
}

type configuration struct {
	Reasoning reasoning `json:"reasoning"`
}

type newMessage struct {
	Content       string         `json:"content"`
	PeerID        string         `json:"peer_id"`
	Metadata      map[string]any `json:"metadata"`
	CreatedAt     time.Time      `json:"created_at"`
	Configuration *configuration `json:"configuration,omitempty"`
}

func wsPath(ws string) string { return "/v3/workspaces/" + url.PathEscape(ws) }

func sessionPath(ws, s string) string { return wsPath(ws) + "/sessions/" + url.PathEscape(s) }

func (c *client) createWorkspace(ctx context.Context, id, namespace string, cfg configuration) error {
	body := map[string]any{"id": id, "metadata": map[string]any{"bonyan_namespace": namespace}, "configuration": cfg}
	return c.call(ctx, http.MethodPost, "create workspace", "/v3/workspaces", nil, body, nil)
}

func (c *client) listWorkspaces(ctx context.Context) ([]workspace, error) {
	return list[workspace](ctx, c, "list workspaces", "/v3/workspaces/list", map[string]any{})
}

func (c *client) deleteWorkspace(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "delete workspace", wsPath(id), nil, nil, nil)
}

func (c *client) createPeer(ctx context.Context, ws, id string) error {
	return c.call(ctx, http.MethodPost, "create peer", wsPath(ws)+"/peers", nil, map[string]any{"id": id}, nil)
}

// createSession creates a session the user's peer and the program's peer take
// part in, with only the user observed.
func (c *client) createSession(ctx context.Context, ws, id string) error {
	body := map[string]any{"id": id, "peers": map[string]any{
		peerUser:  map[string]bool{"observe_me": true},
		peerAgent: map[string]bool{"observe_me": false},
	}}
	return c.call(ctx, http.MethodPost, "create session", wsPath(ws)+"/sessions", nil, body, nil)
}

func (c *client) listSessions(ctx context.Context, ws string) ([]session, error) {
	return list[session](ctx, c, "list sessions", wsPath(ws)+"/sessions/list", map[string]any{})
}

func (c *client) deleteSession(ctx context.Context, ws, id string) error {
	return c.call(ctx, http.MethodDelete, "delete session", sessionPath(ws, id), nil, nil, nil)
}

func (c *client) addMessages(ctx context.Context, ws, s string, msgs []newMessage) ([]message, error) {
	var out []message
	err := c.call(ctx, http.MethodPost, "add messages", sessionPath(ws, s)+"/messages", nil, map[string]any{"messages": msgs}, &out)
	return out, err
}

func (c *client) listMessages(ctx context.Context, ws, s string) ([]message, error) {
	return list[message](ctx, c, "list messages", sessionPath(ws, s)+"/messages/list", map[string]any{})
}

func (c *client) searchMessages(ctx context.Context, ws, s, query string, limit int) ([]message, error) {
	var out []message
	err := c.call(ctx, http.MethodPost, "search messages", sessionPath(ws, s)+"/search", nil, map[string]any{"query": query, "limit": min(limit, pageSize)}, &out)
	return out, err
}

// selfFilter selects the conclusions the user's peer holds about itself.
var selfFilter = map[string]any{"observer_id": peerUser, "observed_id": peerUser}

func (c *client) listConclusions(ctx context.Context, ws string) ([]conclusion, error) {
	return list[conclusion](ctx, c, "list conclusions", wsPath(ws)+"/conclusions/list", map[string]any{"filters": selfFilter})
}

func (c *client) queryConclusions(ctx context.Context, ws, query string, limit int) ([]conclusion, error) {
	var out []conclusion
	err := c.call(ctx, http.MethodPost, "query conclusions", wsPath(ws)+"/conclusions/query", nil, map[string]any{"query": query, "top_k": min(limit, pageSize), "filters": selfFilter}, &out)
	return out, err
}

func (c *client) deleteConclusion(ctx context.Context, ws, id string) error {
	return c.call(ctx, http.MethodDelete, "delete conclusion", wsPath(ws)+"/conclusions/"+url.PathEscape(id), nil, nil, nil)
}
