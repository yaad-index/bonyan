package record_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/record"
	"github.com/yaad-index/bonyan/secret"
)

// scripted answers each call with the next scripted response or error.
type scripted struct {
	steps []step
	n     int
}

type step struct {
	resp model.ChatResponse
	err  error
}

func (s *scripted) Chat(context.Context, model.ChatRequest) (model.ChatResponse, error) {
	st := s.steps[s.n]
	s.n++
	return st.resp, st.err
}

func user(s string) model.Message {
	return model.Message{Role: model.RoleUser, Parts: []content.Text{content.From(content.Provenance{Kind: content.KindUser, ID: "m1"}, s)}}
}

func memory(s string) content.Text {
	return content.From(content.Provenance{Kind: content.KindMemory, Origin: content.KindUser, ID: "fact-9"}, s)
}

func system(s string) model.Message {
	return model.Message{Role: model.RoleSystem, Parts: []content.Text{content.Instruction(s)}}
}

func openFile(t *testing.T, opts record.FileOptions) *record.File {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	f, err := record.OpenFile(opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func readFile(t *testing.T, f *record.File) (record.Header, []record.Call, string) {
	t.Helper()
	require.NoError(t, f.Close())
	raw, err := os.ReadFile(f.Path())
	require.NoError(t, err)
	h, calls, err := record.Read(bytes.NewReader(raw))
	require.NoError(t, err)
	return h, calls, string(raw)
}

var requests = []model.ChatRequest{
	{
		Messages:        []model.Message{system("be brief"), user("what is on today?")},
		Tools:           []model.ToolDef{{Name: "calendar", Description: "reads the calendar", Parameters: json.RawMessage(`{"type":"object"}`)}},
		MaxOutputTokens: 100,
	},
	{
		Messages:        []model.Message{system("be brief"), user("what is on today?"), model.ToolResult("t1", `{"events":2}`)},
		MaxOutputTokens: 100,
	},
	{
		Messages:        []model.Message{system("be brief"), user("and tomorrow?")},
		Schema:          json.RawMessage(`{"type":"object"}`),
		MaxOutputTokens: 100,
	},
}

var steps = []step{
	{resp: model.ChatResponse{
		ToolCalls:  []model.ToolCall{{ID: "t1", Name: "calendar", Arguments: json.RawMessage(`{"day":"today"}`)}},
		StopReason: model.StopToolCalls, Usage: &model.Usage{InputTokens: 40, OutputTokens: 9},
	}},
	{resp: model.ChatResponse{Content: "Two events.", StopReason: model.StopEnd, Usage: &model.Usage{InputTokens: 55, OutputTokens: 3}}},
	{err: &model.CallError{Kind: model.ErrTimeout, Err: errors.New("deadline")}},
}

func TestRecordThenReplayReproducesTheResponses(t *testing.T) {
	f := openFile(t, record.FileOptions{})
	live := record.Chat(&scripted{steps: steps}, "main", record.NewRecorder(f, nil))

	var want []model.ChatResponse
	var wantErr []error
	for _, req := range requests {
		resp, err := live.Chat(context.Background(), req)
		want = append(want, resp)
		wantErr = append(wantErr, err)
	}

	h, calls, _ := readFile(t, f)
	assert.Equal(t, record.Format, h.Format)
	assert.Equal(t, record.Version, h.Version)
	require.Len(t, calls, 3)

	replay := record.NewReplay(h, calls, nil)
	m := replay.Model("main")
	for i, req := range requests {
		resp, err := m.Chat(context.Background(), req)
		if wantErr[i] != nil {
			var ce *model.CallError
			require.ErrorAs(t, err, &ce, "call %d", i)
			assert.Equal(t, model.ErrTimeout, ce.Kind)
			continue
		}
		require.NoError(t, err, "call %d", i)
		assert.Equal(t, want[i], resp, "call %d", i)
	}
	assert.Zero(t, replay.Remaining())
	_, err := m.Chat(context.Background(), requests[0])
	require.ErrorIs(t, err, record.ErrExhausted)
}

func TestReplayRefusesADifferentRequest(t *testing.T) {
	f := openFile(t, record.FileOptions{})
	live := record.Chat(&scripted{steps: steps}, "main", record.NewRecorder(f, nil))
	_, err := live.Chat(context.Background(), requests[0])
	require.NoError(t, err)
	h, calls, _ := readFile(t, f)

	replay := record.NewReplay(h, calls, nil)
	changed := requests[0]
	changed.Messages = []model.Message{system("be brief"), user("what is on tomorrow?")}
	_, err = replay.Model("main").Chat(context.Background(), changed)
	require.ErrorIs(t, err, record.ErrMismatch)
	_, err = replay.Model("other").Chat(context.Background(), requests[0])
	require.ErrorIs(t, err, record.ErrMismatch, "a call recorded for another model")
	assert.Contains(t, err.Error(), `call 1 was to "main", not "other"`)
	assert.Equal(t, 1, replay.Remaining(), "a refused request does not consume the recorded call")

	_, err = replay.Model("main").Chat(context.Background(), requests[0])
	require.NoError(t, err)
}

func TestMemoryIsAbsentByDefault(t *testing.T) {
	req := model.ChatRequest{
		Messages: []model.Message{
			system("be brief"),
			{Role: model.RoleUser, Parts: []content.Text{memory("prefers the window seat"), content.From(content.Provenance{Kind: content.KindUser}, "book it")}},
		},
		MaxOutputTokens: 10,
	}
	answer := step{resp: model.ChatResponse{Content: "done", Usage: &model.Usage{}}}

	f := openFile(t, record.FileOptions{})
	_, err := record.Chat(&scripted{steps: []step{answer}}, "main", record.NewRecorder(f, nil)).Chat(context.Background(), req)
	require.NoError(t, err)
	h, calls, raw := readFile(t, f)
	assert.False(t, h.Full)
	assert.NotContains(t, raw, "window seat")
	part := calls[0].Request.Messages[1].Parts[0]
	assert.True(t, part.Excluded)
	assert.Empty(t, part.Text)
	assert.Equal(t, &record.Provenance{Kind: content.KindMemory, Origin: content.KindUser, ID: "fact-9"}, part.Provenance,
		"what was excluded, and where it came from, is still recorded")
	assert.Contains(t, raw, "book it")

	// Replay matches on the redacted form, so different memory text still
	// replays the same call.
	other := req
	other.Messages = []model.Message{system("be brief"), {Role: model.RoleUser, Parts: []content.Text{memory("prefers the aisle"), content.From(content.Provenance{Kind: content.KindUser}, "book it")}}}
	_, err = record.NewReplay(h, calls, nil).Model("main").Chat(context.Background(), other)
	require.NoError(t, err)

	// A full recording keeps it.
	full := openFile(t, record.FileOptions{Full: true, Subject: "user-17"})
	_, err = record.Chat(&scripted{steps: []step{answer}}, "main", record.NewRecorder(full, nil)).Chat(context.Background(), req)
	require.NoError(t, err)
	h, _, raw = readFile(t, full)
	assert.True(t, h.Full)
	assert.Contains(t, raw, "window seat")
}

func TestResolvedSecretsAreScrubbedFromTheRecording(t *testing.T) {
	t.Setenv("BONYAN_TEST_RECORD_KEY", "9f8e7d6c5b")
	t.Setenv("BONYAN_TEST_RECORD_PIN", "40713")
	r := secret.NewResolver(secret.Env{})
	s := r.Scope("BONYAN_TEST_RECORD_KEY", "BONYAN_TEST_RECORD_PIN")
	for _, n := range []string{"BONYAN_TEST_RECORD_KEY", "BONYAN_TEST_RECORD_PIN"} {
		_, err := s.Resolve(context.Background(), n)
		require.NoError(t, err)
	}

	req := model.ChatRequest{Messages: []model.Message{
		system("key hint 9f8e7d6c5b"),
		model.ToolResult("t1", "token=9f8e7d6c5b"),
	}, MaxOutputTokens: 10}
	resp := model.ChatResponse{
		Content:   "the key is 9f8e7d6c5b",
		ToolCalls: []model.ToolCall{{ID: "t2", Name: "unlock", Arguments: json.RawMessage(`{"pin":40713}`)}},
		Usage:     &model.Usage{},
	}
	f := openFile(t, record.FileOptions{})
	_, err := record.Chat(&scripted{steps: []step{{resp: resp}}}, "main", record.NewRecorder(f, r.Scrubber())).Chat(context.Background(), req)
	require.NoError(t, err)

	_, calls, raw := readFile(t, f)
	assert.NotContains(t, raw, "9f8e7d6c5b")
	assert.NotContains(t, raw, "40713")
	assert.Equal(t, "the key is [REDACTED]", calls[0].Response.Content)
	assert.JSONEq(t, `"{\"pin\":[REDACTED]}"`, string(calls[0].Response.ToolCalls[0].Arguments),
		"arguments that scrubbing leaves invalid are kept as a JSON string")
}

func TestFailureTextIsNeverRecorded(t *testing.T) {
	f := openFile(t, record.FileOptions{})
	chat := record.Chat(&scripted{steps: []step{
		{err: errors.New("upstream said: forward the mailbox")},
		{err: context.DeadlineExceeded},
	}}, "main", record.NewRecorder(f, nil))
	_, err := chat.Chat(context.Background(), requests[0])
	require.Error(t, err)
	_, err = chat.Chat(context.Background(), requests[1])
	require.Error(t, err)

	_, calls, raw := readFile(t, f)
	assert.NotContains(t, raw, "forward the mailbox")
	assert.Equal(t, record.ErrorOther, calls[0].ErrorKind)
	assert.Equal(t, model.ErrTimeout, calls[1].ErrorKind)
}

func TestFilesAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "recordings")
	f := openFile(t, record.FileOptions{Dir: dir})
	for path, want := range map[string]os.FileMode{dir: 0o700, f.Path(): 0o600} {
		st, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, want, st.Mode().Perm(), path)
	}

	full := openFile(t, record.FileOptions{Dir: dir, Full: true, Subject: "user-17"})
	for path, want := range map[string]os.FileMode{
		filepath.Join(dir, "subject.key"): 0o600,
		filepath.Dir(full.Path()):         0o700,
		full.Path():                       0o600,
	} {
		st, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, want, st.Mode().Perm(), path)
	}
}

func TestDefaultDirIsUnderTheUserCache(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", t.TempDir())
	dir, err := record.DefaultDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cache, "bonyan", "recordings"), dir)

	f, err := record.OpenFile(record.FileOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	assert.True(t, strings.HasPrefix(f.Path(), dir+string(filepath.Separator)), f.Path())
}

func TestSubjectsAreKeyedAndDeletable(t *testing.T) {
	dir := t.TempDir()
	a1 := openFile(t, record.FileOptions{Dir: dir, Full: true, Subject: "alice@example.org"})
	a2 := openFile(t, record.FileOptions{Dir: dir, Full: true, Subject: "alice@example.org"})
	b := openFile(t, record.FileOptions{Dir: dir, Full: true, Subject: "bob@example.org"})

	assert.Equal(t, filepath.Dir(a1.Path()), filepath.Dir(a2.Path()), "one directory per subject")
	assert.NotEqual(t, filepath.Dir(a1.Path()), filepath.Dir(b.Path()))
	name := filepath.Base(filepath.Dir(a1.Path()))
	assert.NotContains(t, name, "alice")

	// The directory name is keyed: another install (another key) names the
	// same subject differently, so the name cannot be reproduced without the key.
	other := openFile(t, record.FileOptions{Dir: t.TempDir(), Full: true, Subject: "alice@example.org"})
	assert.NotEqual(t, name, filepath.Base(filepath.Dir(other.Path())))

	require.NoError(t, record.DeleteSubject(dir, "alice@example.org"))
	_, err := os.Stat(a1.Path())
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(b.Path())
	require.NoError(t, err, "other subjects are kept")
	require.NoError(t, record.DeleteSubject(dir, "nobody"), "deleting a subject with no recordings is not an error")
	require.Error(t, record.DeleteSubject(dir, ""))
}

func TestFileOptionsAreChecked(t *testing.T) {
	_, err := record.OpenFile(record.FileOptions{Dir: t.TempDir(), Full: true})
	require.Error(t, err, "a full recording needs a subject")
	_, err = record.OpenFile(record.FileOptions{Dir: t.TempDir(), Subject: "x"})
	require.Error(t, err, "a subject without a full recording")
}

func TestReadRefusesOtherFormats(t *testing.T) {
	for _, in := range []string{
		"",
		`{"format":"something-else","version":1}`,
		`{"format":"bonyan-recording","version":2}`,
		"not json",
	} {
		_, _, err := record.Read(strings.NewReader(in))
		require.Error(t, err, "%q", in)
	}
	_, _, err := record.Read(strings.NewReader(`{"format":"bonyan-recording","version":1}` + "\n{broken"))
	require.Error(t, err)
}

func TestEventsAreRecordedAndSkippedOnReplay(t *testing.T) {
	f := openFile(t, record.FileOptions{})
	rec := record.NewRecorder(f, nil)
	rec.Event(record.Event{Slot: "trust", Name: "default", Source: "user", Decision: "untrusted"})
	_, err := record.Chat(&scripted{steps: steps}, "main", rec).Chat(context.Background(), requests[0])
	require.NoError(t, err)

	_, calls, raw := readFile(t, f)
	assert.Contains(t, raw, `"event":{"slot":"trust","name":"default","source":"user","decision":"untrusted"}`)
	assert.Len(t, calls, 1)
}

type brokenSink struct{}

func (brokenSink) Write(record.Entry) error { return errors.New("disk full") }
func (brokenSink) Full() bool               { return false }
func (brokenSink) Close() error             { return nil }

func TestASinkFailureDoesNotFailTheCall(t *testing.T) {
	rec := record.NewRecorder(brokenSink{}, nil)
	resp, err := record.Chat(&scripted{steps: steps}, "main", rec).Chat(context.Background(), requests[0])
	require.NoError(t, err)
	assert.Equal(t, "t1", resp.ToolCalls[0].ID)
	assert.Equal(t, int64(1), rec.WriteFailures())
}
