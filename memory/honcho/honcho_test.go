package honcho_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/honcho"
	"github.com/yaad-index/bonyan/memory/memorytest"
	"github.com/yaad-index/bonyan/trust"
)

var (
	ctx   = context.Background()
	start = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
)

func TestConformance(t *testing.T) {
	memorytest.Run(t, func(t *testing.T) memorytest.Open {
		_, srv := newFake(t)
		return func(namespace string) memory.Backend {
			b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: namespace, DeleteWait: time.Second})
			require.NoError(t, err)
			return b
		}
	})
}

// The suite passes while the service has embedded nothing too, as it can be
// right after a write.
func TestConformanceBeforeTheServiceEmbeds(t *testing.T) {
	memorytest.Run(t, func(t *testing.T) memorytest.Open {
		f, srv := newFake(t)
		f.unembedded = true
		return func(namespace string) memory.Backend {
			b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: namespace, DeleteWait: time.Second})
			require.NoError(t, err)
			return b
		}
	})
}

func open(t *testing.T, f *fake, url string) *honcho.Backend {
	t.Helper()
	b, err := honcho.Open(honcho.Options{URL: url, Namespace: "test", DeleteWait: 2 * time.Second})
	require.NoError(t, err)
	return b
}

func event(session, text string, at time.Time) memory.Record {
	return memory.Record{
		Layer: memory.ShortTerm, Subject: "ana", Session: session, Origin: content.KindUser, Text: text, At: at,
		Decision: memory.Decision{Verdict: trust.Untrusted, Policy: "default"},
	}
}

func texts(recs []memory.Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Text
	}
	return out
}

// onlyWorkspace is the one workspace the fake holds.
func onlyWorkspace(t *testing.T, f *fake) string {
	t.Helper()
	ws := f.workspaces()
	require.Len(t, ws, 1)
	return ws[0]
}

// What the deriver concluded about the user comes back as derived facts:
// untrusted, from the user's messages, with the conclusion's time. What it
// concluded about the program's peer does not.
func TestConclusionsAboutTheUserComeBackAsDerivedFacts(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "I take the train", start))
	require.NoError(t, err)
	ws := onlyWorkspace(t, f)
	f.conclude(ws, "user", "user", "commutes by train", start.Add(time.Minute))
	f.conclude(ws, "agent", "agent", "about the program", start.Add(time.Minute))

	for _, q := range []string{"", "train"} {
		got, err := b.Recall(ctx, "ana", q, 10, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 1, "query %q", q)
		r := got[0]
		assert.Equal(t, "commutes by train", r.Text)
		assert.True(t, r.Derived)
		assert.Equal(t, content.KindUser, r.Origin)
		assert.Equal(t, memory.LongTerm, r.Layer)
		assert.Equal(t, trust.Untrusted, r.Decision.Verdict)
		assert.True(t, r.At.Equal(start.Add(time.Minute)))
	}
}

// Deleting a subject deletes its workspace, waiting while the service still
// holds its sessions.
func TestDeletingASubjectDeletesItsWorkspace(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "hello", start))
	require.NoError(t, err)
	f.busy = 3
	require.NoError(t, b.DeleteSubject(ctx, "ana"))
	assert.Empty(t, f.workspaces())
	require.NoError(t, b.DeleteSubject(ctx, "ana"), "gone already")

	_, err = b.Write(ctx, event("s1", "back again", start))
	require.NoError(t, err, "writing after a delete makes the workspace again")
	h, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"back again"}, texts(h))
}

// A session holding records on both sides of the cut is rewritten: what is
// newer keeps its ID, in one generation, and what is older is gone.
func TestAPurgeRewritesASessionAcrossTheCut(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	for i, text := range []string{"old 1", "old 2", "new 1", "new 2"} {
		_, err := b.Write(ctx, event("s1", text, start.Add(time.Duration(i)*time.Hour)))
		require.NoError(t, err)
	}
	before, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	f.conclude(onlyWorkspace(t, f), "user", "user", "an old conclusion", start)
	f.conclude(onlyWorkspace(t, f), "user", "user", "a new conclusion", start.Add(3*time.Hour))

	require.NoError(t, b.DeleteBefore(ctx, start.Add(2*time.Hour)))
	after, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"new 1", "new 2"}, texts(after))
	assert.Equal(t, []string{before[2].ID, before[3].ID}, []string{after[0].ID, after[1].ID}, "the same records")
	ws := onlyWorkspace(t, f)
	gens := f.sessions(ws)
	require.Len(t, gens, 1, "one generation left")
	for _, m := range f.messages(ws, gens[0]) {
		assert.Equal(t, map[string]any{"reasoning": map[string]any{"enabled": false}}, m["configuration"], "a copy is not new to the deriver")
	}
	facts, err := b.Recall(ctx, "ana", "", 10, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"a new conclusion"}, texts(facts))

	_, err = b.Write(ctx, event("s1", "newest", start.Add(5*time.Hour)))
	require.NoError(t, err)
	after, err = b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"new 1", "new 2", "newest"}, texts(after), "a later write goes to the new generation")
}

// A rewrite cut short loses nothing: stopped after copying, before deleting
// the old generation, or in the middle of copying, every newer record is
// still read, once, and the next purge finishes the rewrite.
func TestARewriteCutShortLosesNothing(t *testing.T) {
	for name, at := range map[string]func(method, path string) bool{
		"before deleting the old generation": func(method, path string) bool {
			return method == http.MethodDelete && strings.HasSuffix(path, "-g1")
		},
		"while copying": func(method, path string) bool {
			return method == http.MethodPost && strings.HasSuffix(path, "-g2/messages")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, srv := newFake(t)
			b := open(t, f, srv.URL)
			for i, text := range []string{"old", "new 1", "new 2"} {
				_, err := b.Write(ctx, event("s1", text, start.Add(time.Duration(i)*time.Hour)))
				require.NoError(t, err)
			}
			f.fail = at
			require.Error(t, b.DeleteBefore(ctx, start.Add(30*time.Minute)), "the purge stops")
			f.fail = nil

			got, err := b.History(ctx, "ana", "s1", start.Add(30*time.Minute))
			require.NoError(t, err)
			assert.Equal(t, []string{"new 1", "new 2"}, texts(got), "every newer record, once")

			require.NoError(t, b.DeleteBefore(ctx, start.Add(30*time.Minute)), "the next purge finishes it")
			got, err = b.History(ctx, "ana", "s1", time.Time{})
			require.NoError(t, err)
			assert.Equal(t, []string{"new 1", "new 2"}, texts(got))
			assert.Len(t, f.sessions(onlyWorkspace(t, f)), 1)
		})
	}
}

// A session wholly before the cut is deleted, with no new generation.
func TestAPurgeDeletesASessionWhollyBeforeTheCut(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "old", start))
	require.NoError(t, err)
	_, err = b.Write(ctx, event("s2", "new", start.Add(2*time.Hour)))
	require.NoError(t, err)
	require.NoError(t, b.DeleteBefore(ctx, start.Add(time.Hour)))
	ws := onlyWorkspace(t, f)
	assert.Len(t, f.sessions(ws), 1, "only s2's")
	h, err := b.History(ctx, "ana", "s2", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"new"}, texts(h))
}

// A failure is reported by its operation and status, never with the
// service's answer, which can hold stored text.
func TestAnErrorHoldsNoAnswerText(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	f.fail = func(string, string) bool { return true }
	_, err := b.Write(ctx, event("s1", "hello", start))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
	assert.NotContains(t, err.Error(), "SECRET-BODY")
}

// The model's replies are the program's peer's messages, everything else the
// user's; the deriver is off for a remembered fact and on for an event when
// the backend derives.
func TestMessagesGoToTheirPeerWithTheDeriverSetForThem(t *testing.T) {
	f, srv := newFake(t)
	b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", Derive: true, Instructions: "about the speaker only"})
	require.NoError(t, err)
	answer := event("s1", "an answer", start)
	answer.Origin = content.KindModel
	for _, r := range []memory.Record{event("s1", "a question", start), answer} {
		_, err := b.Write(ctx, r)
		require.NoError(t, err)
	}
	fact := memory.Record{Layer: memory.LongTerm, Subject: "ana", Origin: content.KindUser, Text: "a fact", At: start}
	_, err = b.Write(ctx, fact)
	require.NoError(t, err)

	ws := onlyWorkspace(t, f)
	assert.Equal(t, map[string]any{"enabled": true, "custom_instructions": "about the speaker only"}, f.ws[ws].config["reasoning"])
	for _, s := range f.ws[ws].sessions {
		assert.Equal(t, map[string]any{"user": map[string]any{"observe_me": true}, "agent": map[string]any{"observe_me": false}}, s.peers, "only the user is observed")
	}
	var events, facts []map[string]any
	for _, s := range f.sessions(ws) {
		if strings.HasPrefix(s, "facts-") {
			facts = append(facts, f.messages(ws, s)...)
		} else {
			events = append(events, f.messages(ws, s)...)
		}
	}
	require.Len(t, events, 2)
	assert.Equal(t, "user", events[0]["peer_id"])
	assert.Equal(t, "agent", events[1]["peer_id"])
	assert.Nil(t, events[0]["configuration"], "the workspace's deriver reads an event")
	require.Len(t, facts, 1)
	assert.Equal(t, map[string]any{"reasoning": map[string]any{"enabled": false}}, facts[0]["configuration"])
}

// The factory reads its options, refusing an unknown one, and opens the
// backend with the namespace it is given.
func TestTheFactory(t *testing.T) {
	_, srv := newFake(t)
	b, err := honcho.Factory("grove", json.RawMessage(`{"url":"`+srv.URL+`","derive":true}`))
	require.NoError(t, err)
	assert.Equal(t, "grove", b.Namespace())
	_, err = honcho.Factory("grove", json.RawMessage(`{"url":"`+srv.URL+`","token":"x"}`))
	require.Error(t, err, "unknown option")
	_, err = honcho.Factory("grove", json.RawMessage(`{}`))
	require.Error(t, err, "no URL")
}

// A namespace and subject too long for a workspace name are refused, not cut.
func TestATooLongNameIsRefused(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	r := event("s1", "hello", start)
	r.Subject = strings.Repeat("x", 300)
	_, err := b.Write(ctx, r)
	require.ErrorContains(t, err, "too long")
	assert.Empty(t, f.workspaces())
}

// A session's events come back oldest first by their time, whatever the order
// they were written in.
func TestHistoryIsOldestFirstByTime(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	for _, r := range []memory.Record{event("s1", "second", start.Add(time.Minute)), event("s1", "first", start)} {
		_, err := b.Write(ctx, r)
		require.NoError(t, err)
	}
	h, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second"}, texts(h))
}

// A purge finds a rewrite an earlier one left half done and finishes it, even
// when nothing is older than its own cut.
func TestAPurgeFinishesAHalfDoneRewrite(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	for i, text := range []string{"old", "new"} {
		_, err := b.Write(ctx, event("s1", text, start.Add(time.Duration(i)*time.Hour)))
		require.NoError(t, err)
	}
	f.fail = func(method, path string) bool { return method == http.MethodDelete && strings.HasSuffix(path, "-g1") }
	require.Error(t, b.DeleteBefore(ctx, start.Add(30*time.Minute)))
	f.fail = nil
	ws := onlyWorkspace(t, f)
	require.Len(t, f.sessions(ws), 2, "left half done")

	require.NoError(t, b.DeleteBefore(ctx, start.Add(-time.Hour)), "a cut before everything")
	assert.Len(t, f.sessions(ws), 1, "finished all the same")
	h, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"old", "new"}, texts(h), "nothing older than this cut, so nothing removed")
}

// Deleting a subject waits only while the service holds its sessions: any
// other failure is returned at once.
func TestDeletingASubjectFailsFastOnAnotherError(t *testing.T) {
	f, srv := newFake(t)
	b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", DeleteWait: 5 * time.Second})
	require.NoError(t, err)
	_, err = b.Write(ctx, event("s1", "hello", start))
	require.NoError(t, err)
	f.fail = func(method, path string) bool {
		return method == http.MethodDelete && !strings.Contains(path, "/sessions/")
	}
	began := time.Now()
	require.Error(t, b.DeleteSubject(ctx, "ana"))
	assert.Less(t, time.Since(began), time.Second)
}

// A fact the service has not embedded yet, so its search does not find it,
// is still recalled by a word it shares with the query; one sharing none is
// not.
func TestAFactNotYetEmbeddedIsFoundByItsWords(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	for _, text := range []string{"prefers mail over calls", "lives in a small town"} {
		_, err := b.Write(ctx, memory.Record{Layer: memory.LongTerm, Subject: "ana", Origin: content.KindUser, Text: text, At: start})
		require.NoError(t, err)
	}
	f.unembedded = true
	got, err := b.Recall(ctx, "ana", "how should we contact her, by mail?", 10, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"prefers mail over calls"}, texts(got))
}

// A session holding more newer records than the service takes in one call is
// rewritten in batches, keeping every one.
func TestARewriteOfManyRecordsGoesInBatches(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "old", start))
	require.NoError(t, err)
	for i := range 150 {
		_, err := b.Write(ctx, event("s1", "new "+strconv.Itoa(i), start.Add(time.Hour+time.Duration(i)*time.Second)))
		require.NoError(t, err)
	}
	require.NoError(t, b.DeleteBefore(ctx, start.Add(30*time.Minute)))
	h, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	require.Len(t, h, 150)
	assert.Equal(t, "new 0", h[0].Text)
	assert.Equal(t, "new 149", h[149].Text)
	assert.Len(t, f.sessions(onlyWorkspace(t, f)), 1)
}

// When the service keeps fewer copies than it was sent, the old generation is
// not deleted: nothing newer is lost.
func TestARewriteKeepsTheOldGenerationUntilEveryCopyIsIn(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	for i, text := range []string{"old", "new 1", "new 2"} {
		_, err := b.Write(ctx, event("s1", text, start.Add(time.Duration(i)*time.Hour)))
		require.NoError(t, err)
	}
	f.short = true
	require.ErrorContains(t, b.DeleteBefore(ctx, start.Add(30*time.Minute)), "copied")
	f.short = false
	assert.Len(t, f.sessions(onlyWorkspace(t, f)), 2, "the old generation kept")
	h, err := b.History(ctx, "ana", "s1", start.Add(30*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, []string{"new 1", "new 2"}, texts(h))
}

// A session that cannot be rewritten does not stop the others' purge.
func TestOneSessionFailingDoesNotStopTheOthers(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "old in s1", start))
	require.NoError(t, err)
	_, err = b.Write(ctx, event("s1", "new in s1", start.Add(2*time.Hour)))
	require.NoError(t, err)
	_, err = b.Write(ctx, event("s2", "old in s2", start))
	require.NoError(t, err)
	s1 := hex.EncodeToString([]byte("s1"))
	f.fail = func(method, path string) bool {
		return method == http.MethodPost && strings.Contains(path, "/sessions/"+s1+"-g2")
	}
	require.Error(t, b.DeleteBefore(ctx, start.Add(time.Hour)))
	f.fail = nil
	h, err := b.History(ctx, "ana", "s2", time.Time{})
	require.NoError(t, err)
	assert.Empty(t, h, "s2 purged all the same")
}

// A query recalls the user's conclusions when no session is left.
func TestAQueryRecallsConclusionsWithNoSessionLeft(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "old", start))
	require.NoError(t, err)
	ws := onlyWorkspace(t, f)
	f.conclude(ws, "user", "user", "commutes by train", start.Add(2*time.Hour))
	require.NoError(t, b.DeleteBefore(ctx, start.Add(time.Hour)))
	require.Empty(t, f.sessions(ws))
	got, err := b.Recall(ctx, "ana", "train", 10, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"commutes by train"}, texts(got))
}

// Remembered facts do not crowd the deriver's conclusions out of a recall.
func TestConclusionsAreNotCrowdedOut(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	for _, text := range []string{"likes trains", "rides trains daily", "trains on weekends"} {
		_, err := b.Write(ctx, memory.Record{Layer: memory.LongTerm, Subject: "ana", Origin: content.KindUser, Text: text, At: start})
		require.NoError(t, err)
	}
	f.conclude(onlyWorkspace(t, f), "user", "user", "commutes by trains", start)
	got, err := b.Recall(ctx, "ana", "trains", 2, time.Time{})
	require.NoError(t, err)
	assert.Contains(t, texts(got), "commutes by trains")
}

// After another process deleted the subject, a write here makes the workspace
// again rather than failing.
func TestAWriteAfterAnotherProcessDeletedTheSubject(t *testing.T) {
	f, srv := newFake(t)
	here, there := open(t, f, srv.URL), open(t, f, srv.URL)
	_, err := here.Write(ctx, event("s1", "first", start))
	require.NoError(t, err)
	require.NoError(t, there.DeleteSubject(ctx, "ana"))
	_, err = here.Write(ctx, event("s1", "second", start))
	require.NoError(t, err)
	h, err := here.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"second"}, texts(h))
}

// A message whose bonyan fields hold a value bonyan never writes is not read
// as a record.
func TestAMessageWithFieldsBonyanNeverWritesIsNotARecord(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "real", start))
	require.NoError(t, err)
	ws := onlyWorkspace(t, f)
	s := f.sessions(ws)[0]
	for _, meta := range []map[string]any{
		{"layer": "short-term", "session": "s1", "origin": "user", "verdict": 99},
		{"layer": "other", "session": "s1", "origin": "user", "verdict": 2},
		{"layer": "short-term", "session": "s1", "origin": "memory", "verdict": 2},
		{"layer": "short-term", "session": "s1", "origin": "user", "server": "docs", "verdict": 2},
		{"layer": "short-term", "origin": "user", "verdict": 2},
	} {
		f.plant(ws, s, map[string]any{"content": "planted", "peer_id": "user", "metadata": map[string]any{"bonyan": meta}, "created_at": start.Format(time.RFC3339)})
	}
	h, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"real"}, texts(h))
}

// An error from the HTTP client does not carry the URL, which names the
// workspace and so encodes the subject.
func TestAnErrorDoesNotNameTheWorkspace(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	srv.Close()
	_, err := b.Write(ctx, event("s1", "hello", start))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), hex.EncodeToString([]byte("ana")))
	assert.NotContains(t, err.Error(), "/v3/workspaces/")
}
