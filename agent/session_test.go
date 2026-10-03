package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/model/chatcompat"
)

// A session's earlier answer comes back into the next run as the assistant's
// turn, inside a marked section, as untrusted memory of model output, through
// assembly, the enforcement point and an adapter's wire body.
func TestAnEarlierAnswerComesBackAsTheAssistantsTurn(t *testing.T) {
	first, _, store := withMemory(t)
	first.Models[0].Chat = &scripted{steps: stepsOf(answer("ANSWER-5e2 the folder is blue"))}
	_, _, err := agent.Run(context.Background(), first, input("which folder?"))
	require.NoError(t, err)

	history, err := memory.Messages(context.Background(), store, "ana", "s1")
	require.NoError(t, err)
	require.Len(t, history, 2)

	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	wire, err := chatcompat.New(chatcompat.Options{BaseURL: srv.URL + "/v1/", Model: "wire-model"})
	require.NoError(t, err)
	m := &recording{inner: wire}
	second := newAgent(agent.Model{Name: "main", Chat: m})
	second.History = history
	out, _, err := agent.Run(context.Background(), second, input("and the other one?"))
	require.NoError(t, err)
	require.True(t, out.Cleared(), out.String())

	require.Len(t, m.reqs, 1)
	var reply *model.Message
	for i, msg := range m.reqs[0].Messages {
		if msg.Role == model.RoleAssistant {
			reply = &m.reqs[0].Messages[i]
		}
	}
	require.NotNil(t, reply, "the earlier answer is the assistant's turn")
	require.Len(t, reply.Parts, 1)
	mk, ok := reply.Parts[0].(content.Marked)
	require.True(t, ok, "marked at the enforcement point")
	items := mk.Section().Items()
	require.Len(t, items, 1)
	assert.Equal(t, "ANSWER-5e2 the folder is blue", items[0].Raw())
	assert.Equal(t, content.KindMemory, items[0].Provenance().Kind)
	assert.Equal(t, content.KindModel, items[0].Provenance().Origin)

	require.Len(t, bodies, 1)
	var sent []string
	for _, wm := range bodies[0]["messages"].([]any) {
		w := wm.(map[string]any)
		if w["role"] == "assistant" {
			sent = append(sent, w["content"].(string))
		}
	}
	require.Len(t, sent, 1)
	assert.Equal(t, mk.Text(), sent[0], "sent as marked")
	assert.True(t, strings.Contains(sent[0], "ANSWER-5e2"))
}

// recording passes requests to inner and keeps them.
type recording struct {
	inner model.Chat
	reqs  []model.ChatRequest
}

func (r *recording) Chat(ctx context.Context, req model.ChatRequest) (model.ChatResponse, error) {
	r.reqs = append(r.reqs, req)
	return r.inner.Chat(ctx, req)
}
