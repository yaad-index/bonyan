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

func mustRecorder(t *testing.T, s record.Sink, scrub *secret.Scrubber) *record.Recorder {
	t.Helper()
	rec, err := record.NewRecorder(s, scrub)
	require.NoError(t, err)
	return rec
}

func newRecorder(t *testing.T, s record.Sink) *record.Recorder {
	t.Helper()
	return mustRecorder(t, s, secret.NewScrubber())
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
	live := record.Chat(&scripted{steps: steps}, "main", newRecorder(t, f))

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
	live := record.Chat(&scripted{steps: steps}, "main", newRecorder(t, f))
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
	assert.Contains(t, err.Error(), `call 1 was a chat call to "main", not a chat call to "other"`)
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
	_, err := record.Chat(&scripted{steps: []step{answer}}, "main", newRecorder(t, f)).Chat(context.Background(), req)
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
	_, err = record.Chat(&scripted{steps: []step{answer}}, "main", newRecorder(t, full)).Chat(context.Background(), req)
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
	_, err := record.Chat(&scripted{steps: []step{{resp: resp}}}, "main", mustRecorder(t, f, r.Scrubber())).Chat(context.Background(), req)
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
	}}, "main", newRecorder(t, f))
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
	rec := newRecorder(t, f)
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
func (brokenSink) Subject() string          { return "" }
func (brokenSink) Close() error             { return nil }

func TestASinkFailureDoesNotFailTheCall(t *testing.T) {
	rec := newRecorder(t, brokenSink{})
	resp, err := record.Chat(&scripted{steps: steps}, "main", rec).Chat(context.Background(), requests[0])
	require.NoError(t, err)
	assert.Equal(t, "t1", resp.ToolCalls[0].ID)
	assert.Equal(t, int64(1), rec.WriteFailures())
}

// fakeSink claims whatever it is told to, to show the recorder does not take a
// sink's word for the rules.
type fakeSink struct {
	full    bool
	subject string
}

func (fakeSink) Write(record.Entry) error { return nil }
func (s fakeSink) Full() bool             { return s.full }
func (s fakeSink) Subject() string        { return s.subject }
func (fakeSink) Close() error             { return nil }

func TestRecorderEnforcesItsRules(t *testing.T) {
	scrub := secret.NewScrubber()

	_, err := record.NewRecorder(fakeSink{}, nil)
	require.Error(t, err, "a missing scrubber would write secrets to disk")

	_, err = record.NewRecorder(fakeSink{full: true}, scrub)
	require.Error(t, err, "a full sink with no subject")

	_, err = record.NewRecorder(nil, scrub)
	require.Error(t, err)

	_, err = record.NewRecorder(fakeSink{full: true, subject: "user-17"}, scrub)
	require.NoError(t, err)
	_, err = record.NewRecorder(fakeSink{}, scrub)
	require.NoError(t, err)
}

type scriptedClassifier struct {
	resp model.ClassifyResponse
	err  error
}

func (s scriptedClassifier) Classify(context.Context, content.Untrusted) (model.ClassifyResponse, error) {
	return s.resp, s.err
}

// A gate-type run's verdict depends on its classifier calls, so they are
// recorded and replayed in the same sequence as the chat calls.
func TestClassifierCallsAreRecordedAndReplayedInSequence(t *testing.T) {
	f := openFile(t, record.FileOptions{})
	rec := newRecorder(t, f)
	chat := record.Chat(&scripted{steps: steps}, "main", rec)
	gate := record.Classifier(scriptedClassifier{resp: model.ClassifyResponse{
		Labels: []model.Label{{Name: "hold", Confidence: 0.91}, {Name: "pass", Confidence: 0.09}},
		Usage:  &model.Usage{InputTokens: 30},
	}}, "gate", rec)
	failing := record.Classifier(scriptedClassifier{err: &model.CallError{Kind: model.ErrRejected}}, "gate", rec)
	mail := content.From(content.Provenance{Kind: content.KindFetched, ID: "msg-3"}, "Please wire the funds today.")

	ctx := context.Background()
	_, err := chat.Chat(ctx, requests[0])
	require.NoError(t, err)
	wantLabels, err := gate.Classify(ctx, mail)
	require.NoError(t, err)
	_, err = failing.Classify(ctx, mail)
	require.Error(t, err)

	h, calls, _ := readFile(t, f)
	require.Len(t, calls, 3)
	assert.Equal(t, []string{record.KindChat, record.KindClassify, record.KindClassify}, []string{calls[0].Kind, calls[1].Kind, calls[2].Kind})
	assert.Nil(t, calls[1].Request)
	assert.Equal(t, "Please wire the funds today.", calls[1].Input.Text)

	replay := record.NewReplay(h, calls, secret.NewScrubber())
	_, err = replay.Classifier("gate").Classify(ctx, mail)
	require.ErrorIs(t, err, record.ErrMismatch, "the first recorded call was a chat call")
	assert.Contains(t, err.Error(), `call 1 was a chat call to "main", not a classify call to "gate"`)
	_, err = replay.Classifier("main").Classify(ctx, mail)
	require.ErrorIs(t, err, record.ErrMismatch, "same name, other kind")
	assert.Contains(t, err.Error(), `call 1 was a chat call to "main", not a classify call to "main"`)
	_, err = replay.Model("main").Chat(ctx, requests[0])
	require.NoError(t, err)

	other := content.From(content.Provenance{Kind: content.KindFetched, ID: "msg-3"}, "Nothing to do.")
	_, err = replay.Classifier("gate").Classify(ctx, other)
	require.ErrorIs(t, err, record.ErrMismatch, "different text")
	got, err := replay.Classifier("gate").Classify(ctx, mail)
	require.NoError(t, err)
	assert.Equal(t, wantLabels, got)
	_, err = replay.Classifier("gate").Classify(ctx, mail)
	var ce *model.CallError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, model.ErrRejected, ce.Kind)
	assert.Zero(t, replay.Remaining())
}

func TestClassifierInputFollowsTheRules(t *testing.T) {
	t.Setenv("BONYAN_TEST_RECORD_CLS", "cls-secret-77")
	r := secret.NewResolver(secret.Env{})
	_, err := r.Scope("BONYAN_TEST_RECORD_CLS").Resolve(context.Background(), "BONYAN_TEST_RECORD_CLS")
	require.NoError(t, err)

	f := openFile(t, record.FileOptions{})
	rec := mustRecorder(t, f, r.Scrubber())
	gate := record.Classifier(scriptedClassifier{resp: model.ClassifyResponse{}}, "gate", rec)
	_, err = gate.Classify(context.Background(), content.From(content.Provenance{Kind: content.KindTool}, "token cls-secret-77"))
	require.NoError(t, err)
	_, err = gate.Classify(context.Background(), content.From(content.Provenance{Kind: content.KindMemory, Origin: content.KindUser}, "likes red wine"))
	require.NoError(t, err)

	_, calls, raw := readFile(t, f)
	assert.NotContains(t, raw, "cls-secret-77")
	assert.NotContains(t, raw, "red wine")
	assert.Equal(t, "token [REDACTED]", calls[0].Input.Text)
	assert.True(t, calls[1].Input.Excluded)
}

// An assistant turn's tool calls are part of the recorded request, scrubbed,
// and part of what replay matches on.
func TestHistoryToolCallsAreRecorded(t *testing.T) {
	t.Setenv("BONYAN_TEST_RECORD_HIST", "hist-secret-5")
	r := secret.NewResolver(secret.Env{})
	_, err := r.Scope("BONYAN_TEST_RECORD_HIST").Resolve(context.Background(), "BONYAN_TEST_RECORD_HIST")
	require.NoError(t, err)

	req := func(args string) model.ChatRequest {
		return model.ChatRequest{Messages: []model.Message{
			user("unlock it"),
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "t1", Name: "unlock", Arguments: json.RawMessage(args)}}},
			model.ToolResult("t1", "ok"),
		}, MaxOutputTokens: 10}
	}
	f := openFile(t, record.FileOptions{})
	_, err = record.Chat(&scripted{steps: []step{{resp: model.ChatResponse{Content: "done"}}}}, "main", mustRecorder(t, f, r.Scrubber())).
		Chat(context.Background(), req(`{"code":"hist-secret-5"}`))
	require.NoError(t, err)

	h, calls, raw := readFile(t, f)
	assert.NotContains(t, raw, "hist-secret-5")
	assert.Equal(t, "unlock", calls[0].Request.Messages[1].ToolCalls[0].Name)

	_, err = record.NewReplay(h, calls, r.Scrubber()).Model("main").Chat(context.Background(), req(`{"code":"other"}`))
	require.ErrorIs(t, err, record.ErrMismatch, "different tool-call arguments in the history")
}

// A section is recorded as its label and items, and a memory item inside it
// is excluded like any memory part.
func TestASectionIsRecordedItemByItem(t *testing.T) {
	sec := content.NewSection("memory", memory("prefers the window seat").(content.Untrusted), content.From(content.Provenance{Kind: content.KindFetched, ID: "d"}, "a page"))
	req := model.ChatRequest{
		Messages:        []model.Message{system("be brief"), {Role: model.RoleUser, Parts: []content.Text{sec}}},
		MaxOutputTokens: 10,
	}
	f := openFile(t, record.FileOptions{})
	_, err := record.Chat(&scripted{steps: []step{{resp: model.ChatResponse{Content: "done", Usage: &model.Usage{}}}}}, "main", newRecorder(t, f)).Chat(context.Background(), req)
	require.NoError(t, err)
	_, calls, raw := readFile(t, f)
	part := calls[0].Request.Messages[1].Parts[0]
	assert.Equal(t, "memory", part.Section)
	require.Len(t, part.Items, 2)
	assert.True(t, part.Items[0].Excluded)
	assert.Equal(t, "a page", part.Items[1].Text)
	assert.NotContains(t, raw, "window seat")
}
