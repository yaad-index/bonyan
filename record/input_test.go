package record_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/secret"
)

// recordCall records one chat call of req with ctx and returns it.
func recordCall(t *testing.T, ctx context.Context, req model.ChatRequest) record.Call {
	t.Helper()
	s := &memSink{}
	_, err := record.Chat(&scripted{steps: []step{{resp: model.ChatResponse{Content: "ok"}}}}, "main", mustRecorder(t, s, secret.NewScrubber())).Chat(ctx, req)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
	require.NotNil(t, s.entries[0].Call)
	return *s.entries[0].Call
}

// marked returns the place of each part req marks as the run's input, as
// message index and part index.
func marked(req *record.Request) [][2]int {
	var out [][2]int
	for i, m := range req.Messages {
		for j, p := range m.Parts {
			if p.Input {
				out = append(out, [2]int{i, j})
			}
		}
	}
	return out
}

// A call marks the run's input where the agent says it is, counted from the
// end of the request, and only while it is still there and still the same
// text from the same source: a hook that moved or changed it leaves no
// marker.
func TestACallMarksTheInputWhereTheAgentSaysItIs(t *testing.T) {
	from := content.Provenance{Kind: content.KindUser, ID: "m1"}
	in := content.NewSection("user message", content.From(from, "find x"))
	trusted := content.TrustedFrom(from, "find x")
	msg := func(p content.Text) model.Message {
		return model.Message{Role: model.RoleUser, Parts: []content.Text{p}}
	}
	sys := system("answer")
	for _, tc := range []struct {
		name    string
		input   content.Text
		fromEnd int
		msgs    []model.Message
		want    [][2]int
	}{
		{"where it was put", in, 1, []model.Message{sys, msg(in)}, [][2]int{{1, 0}}},
		{"before the turns after it", in, 3, []model.Message{sys, msg(in), {Role: model.RoleAssistant}, msg(content.From(content.Provenance{Kind: content.KindTool, ID: "c1"}, "r"))}, [][2]int{{1, 0}}},
		{"marked by the trust policy", in, 1, []model.Message{sys, msg(content.NewMarked(in, "<<find x>>"))}, [][2]int{{1, 0}}},
		{"trusted, with its source", trusted, 1, []model.Message{sys, msg(trusted)}, [][2]int{{1, 0}}},
		{"after a hook dropped a message before it", in, 1, []model.Message{msg(in)}, [][2]int{{0, 0}}},
		{"after a hook added a message after it", in, 1, []model.Message{sys, msg(in), msg(content.From(from, "find x"))}, nil},
		{"after a hook changed its text", in, 1, []model.Message{sys, msg(content.NewSection("user message", content.From(from, "find y")))}, nil},
		{"from another source", in, 1, []model.Message{sys, msg(content.NewSection("user message", content.From(content.Provenance{Kind: content.KindUser, ID: "m2"}, "find x")))}, nil},
		{"in another section", in, 1, []model.Message{sys, msg(content.NewSection("history", content.From(from, "find x")))}, nil},
		{"trusted, from another source", trusted, 1, []model.Message{sys, msg(content.TrustedFrom(content.Provenance{Kind: content.KindUser, ID: "m2"}, "find x"))}, nil},
		{"trusted where it was untrusted", in, 1, []model.Message{sys, msg(trusted)}, nil},
		{"placed past the request's start", in, 3, []model.Message{sys, msg(in)}, nil},
		{"placed nowhere", in, 0, []model.Message{sys, msg(in)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := record.WithInput(context.Background(), tc.input, tc.fromEnd)
			c := recordCall(t, ctx, model.ChatRequest{Messages: tc.msgs})
			assert.Equal(t, tc.want, marked(c.Request))
		})
	}
	c := recordCall(t, context.Background(), model.ChatRequest{Messages: []model.Message{sys, msg(in)}})
	assert.Empty(t, marked(c.Request), "a call that names no input")
}

// The marker takes no part in a call's fingerprint, so a replay, which
// knows no input, serves a marked call, and recordings made before the
// marker keep their fingerprints.
func TestTheMarkerTakesNoPartInTheFingerprint(t *testing.T) {
	in := content.NewSection("user message", content.From(content.Provenance{Kind: content.KindUser, ID: "m1"}, "find x"))
	req := model.ChatRequest{Messages: []model.Message{system("answer"), {Role: model.RoleUser, Parts: []content.Text{in}}}}
	withMarker := recordCall(t, record.WithInput(context.Background(), in, 1), req)
	without := recordCall(t, context.Background(), req)
	require.Equal(t, [][2]int{{1, 0}}, marked(withMarker.Request))
	assert.Equal(t, without.Fingerprint, withMarker.Fingerprint)

	replay := record.NewReplay(record.Header{Format: record.Format, Version: record.Version}, []record.Call{withMarker}, secret.NewScrubber())
	resp, err := replay.Model("main").Chat(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
}
