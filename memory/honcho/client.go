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
		// The URL names the workspace, which encodes the subject: it is left
		// out.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("honcho: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Read so the connection can be reused; never returned.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	}
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
	ID            string         `json:"id"`
	Metadata      map[string]any `json:"metadata"`
	Configuration map[string]any `json:"configuration"`
}

type session struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace_id"`
	// Active is false for a session the service has marked deleted and not
	// yet removed: it removes a deleted session's messages later, from its
	// queue, and lists a peer's sessions with such ones included.
	Active *bool `json:"is_active"`
}

// active reports whether s is not deleted. A session the service reports no
// state for is taken as active.
func (s session) active() bool { return s.Active == nil || *s.Active }

type message struct {
	ID        string         `json:"id"`
	Workspace string         `json:"workspace_id"`
	Session   string         `json:"session_id"`
	Content   string         `json:"content"`
	PeerID    string         `json:"peer_id"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt time.Time      `json:"created_at"`
}

type conclusion struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Observer  string    `json:"observer_id"`
	Observed  string    `json:"observed_id"`
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

// matches reports whether a workspace's stored configuration holds c's
// deriver setting: the same switch and the same instructions, none counting as
// empty. A switch the workspace does not set is not c's, whatever the
// service's default.
func (c configuration) matches(stored map[string]any) bool {
	r, _ := stored["reasoning"].(map[string]any)
	enabled, set := r["enabled"].(bool)
	instructions, _ := r["custom_instructions"].(string)
	return set && enabled == c.Reasoning.Enabled && instructions == c.Reasoning.CustomInstructions
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

// createWorkspace gets the workspace named id, making it with namespace
// recorded when it is missing; an existing one comes back as it is.
func (c *client) createWorkspace(ctx context.Context, id, namespace string, cfg configuration) (workspace, error) {
	body := map[string]any{"id": id, "metadata": map[string]any{nsKey: namespace}, "configuration": cfg}
	var w workspace
	err := c.call(ctx, http.MethodPost, "create workspace", "/v3/workspaces", nil, body, &w)
	return w, err
}

// updateWorkspace sets the workspace's metadata and configuration.
func (c *client) updateWorkspace(ctx context.Context, id string, metadata map[string]any, cfg configuration) error {
	return c.call(ctx, http.MethodPut, "update workspace", wsPath(id), nil, map[string]any{"metadata": metadata, "configuration": cfg}, nil)
}

func (c *client) createPeer(ctx context.Context, ws, id string) error {
	return c.call(ctx, http.MethodPost, "create peer", wsPath(ws)+"/peers", nil, map[string]any{"id": id}, nil)
}

// createSession creates a session the user's peer and the program's peer take
// part in, with only the user observed and neither observing the other.
func (c *client) createSession(ctx context.Context, ws, id, peer string) error {
	body := map[string]any{"id": id, "peers": map[string]any{
		peer:      map[string]bool{"observe_me": true, "observe_others": false},
		agentPeer: map[string]bool{"observe_me": false, "observe_others": false},
	}}
	return c.call(ctx, http.MethodPost, "create session", wsPath(ws)+"/sessions", nil, body, nil)
}

func (c *client) listSessions(ctx context.Context, ws string) ([]session, error) {
	return list[session](ctx, c, "list sessions", wsPath(ws)+"/sessions/list", map[string]any{})
}

// peerSessions lists the sessions peer takes part in.
func (c *client) peerSessions(ctx context.Context, ws, peer string) ([]session, error) {
	return list[session](ctx, c, "list peer sessions", wsPath(ws)+"/peers/"+url.PathEscape(peer)+"/sessions", map[string]any{})
}

// setCard empties observer's card: its own, or, with a target, the one it
// holds about target.
func (c *client) setCard(ctx context.Context, ws, observer, target string) error {
	var q url.Values
	if target != "" {
		q = url.Values{"target": {target}}
	}
	return c.call(ctx, http.MethodPut, "set peer card", wsPath(ws)+"/peers/"+url.PathEscape(observer)+"/card", q, map[string]any{"peer_card": []string{}}, nil)
}

// updatePeer empties peer's metadata.
func (c *client) updatePeer(ctx context.Context, ws, peer string) error {
	return c.call(ctx, http.MethodPut, "update peer", wsPath(ws)+"/peers/"+url.PathEscape(peer), nil, map[string]any{"metadata": map[string]any{}}, nil)
}

// queued is how much work the service's deriver holds, waiting or under way,
// matching q.
func (c *client) queued(ctx context.Context, ws string, q url.Values) (int, error) {
	var st struct {
		Pending    int `json:"pending_work_units"`
		InProgress int `json:"in_progress_work_units"`
	}
	if err := c.call(ctx, http.MethodGet, "queue status", wsPath(ws)+"/queue/status", q, nil, &st); err != nil {
		return 0, err
	}
	return st.Pending + st.InProgress, nil
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

// searchMessages returns the messages of session s best matching query, as
// many as the service serves at once, for the caller to filter and cut.
func (c *client) searchMessages(ctx context.Context, ws, s, query string) ([]message, error) {
	var out []message
	err := c.call(ctx, http.MethodPost, "search messages", sessionPath(ws, s)+"/search", nil, map[string]any{"query": query, "limit": pageSize}, &out)
	return out, err
}

// selfFilter selects the conclusions peer holds about itself.
func selfFilter(peer string) map[string]any {
	return map[string]any{"observer_id": peer, "observed_id": peer}
}

// listConclusions lists the workspace's conclusions matching filters; nil
// filters list them all.
func (c *client) listConclusions(ctx context.Context, ws string, filters map[string]any) ([]conclusion, error) {
	body := map[string]any{}
	if filters != nil {
		body["filters"] = filters
	}
	return list[conclusion](ctx, c, "list conclusions", wsPath(ws)+"/conclusions/list", body)
}

// queryConclusions returns the conclusions matching filters that best match
// query, as many as the service serves at once, for the caller to filter and
// cut.
func (c *client) queryConclusions(ctx context.Context, ws, query string, filters map[string]any) ([]conclusion, error) {
	var out []conclusion
	err := c.call(ctx, http.MethodPost, "query conclusions", wsPath(ws)+"/conclusions/query", nil, map[string]any{"query": query, "top_k": pageSize, "filters": filters}, &out)
	return out, err
}

func (c *client) deleteConclusion(ctx context.Context, ws, id string) error {
	return c.call(ctx, http.MethodDelete, "delete conclusion", wsPath(ws)+"/conclusions/"+url.PathEscape(id), nil, nil, nil)
}
