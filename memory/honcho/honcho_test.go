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

// ana is the peer of the subject most tests write: the hex of "ana".
var ana = hex.EncodeToString([]byte("ana"))

// onlyWorkspace is the one workspace the fake holds.
func onlyWorkspace(t *testing.T, f *fake) string {
	t.Helper()
	ws := f.workspaces()
	require.Len(t, ws, 1)
	return ws[0]
}

// What the deriver concluded about the user comes back as derived facts:
// untrusted, from the user's messages, with the conclusion's time. What it
// concluded about the program's peer, or another peer about the user, does
// not.
func TestConclusionsAboutTheUserComeBackAsDerivedFacts(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "I take the train", start))
	require.NoError(t, err)
	ws := onlyWorkspace(t, f)
	f.conclude(ws, ana, ana, "commutes by train", start.Add(time.Minute))
	f.conclude(ws, "agent", "agent", "about the program", start.Add(time.Minute))
	f.conclude(ws, "626f", ana, "another peer's view of ana", start.Add(time.Minute))

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

// Deleting a subject erases it from the workspace: its sessions, the
// conclusions it holds or is the subject of, its card and the program's card
// about it, and its metadata. Another subject in the same workspace keeps all
// of its own, and the erased subject's empty peer remains (ADR 0003 §3).
func TestErasingASubjectRemovesEverythingButThePeer(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	bo := hex.EncodeToString([]byte("bo"))
	for _, subject := range []string{"ana", "bo"} {
		r := event("s1", subject+"'s event", start)
		r.Subject = subject
		_, err := b.Write(ctx, r)
		require.NoError(t, err)
		_, err = b.Write(ctx, memory.Record{Layer: memory.LongTerm, Subject: subject, Origin: content.KindUser, Text: subject + "'s fact", At: start})
		require.NoError(t, err)
	}
	ws := onlyWorkspace(t, f)
	for _, peer := range []string{ana, bo} {
		f.conclude(ws, peer, peer, peer+" about itself", start)
		f.conclude(ws, "agent", peer, "the program about "+peer, start)
		f.setCard(ws, peer, "", "a card")
		f.setCard(ws, "agent", peer, "the program's card")
		f.setPeerMeta(ws, peer, map[string]any{"name": "someone"})
	}
	f.conclude(ws, ana, bo, "ana about bo", start)

	require.NoError(t, b.DeleteSubject(ctx, "ana"))

	for _, s := range f.sessions(ws) {
		assert.False(t, strings.HasPrefix(s, ana+"--"), "ana's session %s is gone", s)
	}
	assert.Empty(t, f.conclusionsOf(ws, ana), "no conclusion ana holds or is the subject of")
	assert.Empty(t, f.ws[ws].cards[ana+"|"])
	assert.Empty(t, f.ws[ws].cards["agent|"+ana])
	assert.Empty(t, f.ws[ws].peerMeta[ana])
	assert.True(t, f.ws[ws].peers[ana], "the peer remains: the service cannot delete one")
	assert.Equal(t, []string{ws}, f.workspaces(), "the workspace stays")

	h, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Empty(t, h)
	got, err := b.Recall(ctx, "ana", "", 10, time.Time{})
	require.NoError(t, err)
	assert.Empty(t, got)

	h, err = b.History(ctx, "bo", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"bo's event"}, texts(h), "bo's records stay")
	got, err = b.Recall(ctx, "bo", "", 10, time.Time{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"bo's fact", bo + " about itself"}, texts(got))
	assert.ElementsMatch(t, []string{bo + " about itself", "the program about " + bo}, f.conclusionsOf(ws, bo), "bo's conclusions stay, ana's about bo go")
	assert.Equal(t, []any{"a card"}, f.ws[ws].cards[bo+"|"])
	assert.Equal(t, []any{"the program's card"}, f.ws[ws].cards["agent|"+bo])
	assert.NotEmpty(t, f.ws[ws].peerMeta[bo])

	require.NoError(t, b.DeleteSubject(ctx, "ana"), "erasing again is fine")
	_, err = b.Write(ctx, event("s1", "back again", start))
	require.NoError(t, err, "the empty peer is written to again")
	h, err = b.History(ctx, "ana", "s1", time.Time{})
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
	f.conclude(onlyWorkspace(t, f), ana, ana, "an old conclusion", start)
	f.conclude(onlyWorkspace(t, f), ana, ana, "a new conclusion", start.Add(3*time.Hour))

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
		assert.Equal(t, map[string]any{
			ana:     map[string]any{"observe_me": true, "observe_others": false},
			"agent": map[string]any{"observe_me": false, "observe_others": false},
		}, s.peers, "only the user is observed, and nobody observes another")
	}
	var events, facts []map[string]any
	for _, s := range f.sessions(ws) {
		if strings.HasPrefix(s, ana+"--facts-") {
			facts = append(facts, f.messages(ws, s)...)
		} else {
			events = append(events, f.messages(ws, s)...)
		}
	}
	require.Len(t, events, 2)
	assert.Equal(t, ana, events[0]["peer_id"])
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

// A subject too long for a peer name, or a subject and session too long for a
// session name at any generation, is refused before any call, not cut; the
// error gives lengths, never the names. So is a namespace too long for the
// workspace's name.
func TestATooLongNameIsRefused(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)

	r := event("s1", "hello", start)
	r.Subject = strings.Repeat("x", 300)
	_, err := b.Write(ctx, r)
	require.ErrorContains(t, err, "too long for a peer name (600 characters, at most 512)")
	assert.NotContains(t, err.Error(), "xxx")
	for _, call := range []func() error{
		func() error { _, err := b.History(ctx, r.Subject, "s1", time.Time{}); return err },
		func() error { _, err := b.Recall(ctx, r.Subject, "", 1, time.Time{}); return err },
		func() error { return b.DeleteSubject(ctx, r.Subject) },
	} {
		require.ErrorContains(t, call(), "too long")
	}

	r = event(strings.Repeat("s", 50), "hello", start)
	r.Subject = strings.Repeat("x", 200)
	_, err = b.Write(ctx, r)
	require.ErrorContains(t, err, "too long for a session name (513 characters, at most 512)")
	r.Session = strings.Repeat("s", 49)
	_, err = b.Write(ctx, r)
	require.NoError(t, err, "one shorter fits, with room for any generation")

	f2, srv2 := newFake(t)
	_, err = honcho.Open(honcho.Options{URL: srv2.URL, Namespace: strings.Repeat("n", 300)})
	require.ErrorContains(t, err, "too long for a workspace name")
	assert.Empty(t, f2.called())
	assert.NotContains(t, strings.Join(f.called(), " "), hex.EncodeToString([]byte(strings.Repeat("x", 300))), "the refused subject was never sent")
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

// Deleting a subject waits only while the deriver has work for it: any other
// failure is returned at once.
func TestDeletingASubjectFailsFastOnAnotherError(t *testing.T) {
	f, srv := newFake(t)
	b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", DeleteWait: 5 * time.Second})
	require.NoError(t, err)
	_, err = b.Write(ctx, event("s1", "hello", start))
	require.NoError(t, err)
	f.queue["sender_id="+ana] = 1000
	f.fail = func(method, path string) bool { return strings.HasSuffix(path, "/queue/status") }
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
		return method == http.MethodPost && strings.Contains(path, "--"+s1+"-g2")
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
	f.conclude(ws, ana, ana, "commutes by train", start.Add(2*time.Hour))
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
	f.conclude(onlyWorkspace(t, f), ana, ana, "commutes by trains", start)
	got, err := b.Recall(ctx, "ana", "trains", 2, time.Time{})
	require.NoError(t, err)
	assert.Contains(t, texts(got), "commutes by trains")
}

// After another process deleted the workspace, a write here makes it again
// rather than failing.
func TestAWriteAfterAnotherProcessDeletedTheWorkspace(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "first", start))
	require.NoError(t, err)
	f.mu.Lock()
	clear(f.ws)
	f.mu.Unlock()
	_, err = b.Write(ctx, event("s1", "second", start))
	require.NoError(t, err)
	h, err := b.History(ctx, "ana", "s1", time.Time{})
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

// Every subject of a namespace is kept in one workspace, named by the prefix
// and the namespace, which records the namespace it holds.
func TestOneWorkspaceHoldsTheNamespace(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	for _, subject := range []string{"ana", "bo", "cy"} {
		r := event("s1", "hello", start)
		r.Subject = subject
		_, err := b.Write(ctx, r)
		require.NoError(t, err)
	}
	ws := "bonyan--" + hex.EncodeToString([]byte("test"))
	assert.Equal(t, []string{ws}, f.workspaces())
	assert.Equal(t, "test", f.ws[ws].metadata["bonyan_namespace"])
	assert.Len(t, f.ws[ws].peers, 4, "one peer per subject, and the program's")
}

// A workspace recording another namespace is refused for every operation, and
// nothing is written to it.
func TestAWorkspaceOfAnotherNamespaceIsRefused(t *testing.T) {
	f, srv := newFake(t)
	f.makeWorkspace("shared", map[string]any{"bonyan_namespace": "other"})
	b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", Workspace: "shared"})
	require.NoError(t, err)
	for name, call := range map[string]func() error{
		"write":     func() error { _, err := b.Write(ctx, event("s1", "hello", start)); return err },
		"history":   func() error { _, err := b.History(ctx, "ana", "s1", time.Time{}); return err },
		"recall":    func() error { _, err := b.Recall(ctx, "ana", "q", 1, time.Time{}); return err },
		"delete":    func() error { return b.DeleteSubject(ctx, "ana") },
		"retention": func() error { return b.DeleteBefore(ctx, start) },
	} {
		require.ErrorContains(t, call(), "another namespace", name)
	}
	assert.Empty(t, f.ws["shared"].peers)
	assert.Empty(t, f.ws["shared"].sessions)
	assert.Equal(t, "other", f.ws["shared"].metadata["bonyan_namespace"])
}

// A named workspace an operator made, recording no namespace, is taken for
// the Backend's: the namespace is added to its metadata, which keeps what it
// held, and the deriver is set as configured.
func TestAnOperatorsWorkspaceIsTakenForTheNamespace(t *testing.T) {
	f, srv := newFake(t)
	f.makeWorkspace("ops-made", map[string]any{"owner": "ops"})
	b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", Workspace: "ops-made", Derive: true})
	require.NoError(t, err)
	_, err = b.Write(ctx, event("s1", "hello", start))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"owner": "ops", "bonyan_namespace": "test"}, f.ws["ops-made"].metadata)
	assert.Equal(t, map[string]any{"enabled": true}, f.ws["ops-made"].config["reasoning"])
	assert.Equal(t, []string{"ops-made"}, f.workspaces())

	other, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: "other", Workspace: "ops-made"})
	require.NoError(t, err)
	_, err = other.Write(ctx, event("s1", "hello", start))
	require.ErrorContains(t, err, "another namespace", "taken now")
}

// A prefix and a workspace name are not both given, and a workspace name
// the service would refuse is refused at once.
func TestAWorkspaceNameIsChecked(t *testing.T) {
	_, srv := newFake(t)
	_, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", Prefix: "p", Workspace: "w"})
	require.Error(t, err)
	_, err = honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", Workspace: "a/b"})
	require.Error(t, err)
	_, err = honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", Workspace: strings.Repeat("w", 513)})
	require.Error(t, err)
	b, err := honcho.Factory("test", json.RawMessage(`{"url":"`+srv.URL+`","workspace":"w"}`))
	require.NoError(t, err)
	assert.Equal(t, "test", b.Namespace())
}

// Recall and history keep users apart even when the service does not: with
// every filter ignored and every session listed as anyone's, a user still
// reads only messages from their own sessions, sent by them or the program,
// and conclusions their peer holds about itself.
func TestReadsCheckEveryResultAgain(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	bo := hex.EncodeToString([]byte("bo"))
	for _, subject := range []string{"ana", "bo"} {
		r := event("s1", subject+" rides trains", start)
		r.Subject = subject
		_, err := b.Write(ctx, r)
		require.NoError(t, err)
		_, err = b.Write(ctx, memory.Record{Layer: memory.LongTerm, Subject: subject, Origin: content.KindUser, Text: subject + " likes trains", At: start})
		require.NoError(t, err)
	}
	ws := onlyWorkspace(t, f)
	f.conclude(ws, ana, ana, "ana concluded trains", start)
	f.conclude(ws, bo, bo, "bo concluded trains", start)
	f.conclude(ws, bo, ana, "bo about ana trains", start)
	f.conclude(ws, ana, bo, "ana about bo trains", start)
	var anaEvents string
	for _, s := range f.sessions(ws) {
		if strings.HasPrefix(s, ana+"--") && !strings.Contains(s, "--facts-") {
			anaEvents = s
		}
	}
	f.plant(ws, anaEvents, map[string]any{
		"content": "planted by bo trains", "peer_id": bo, "created_at": start.Format(time.RFC3339),
		"metadata": map[string]any{"bonyan": map[string]any{"layer": "short-term", "session": "s1", "origin": "user", "verdict": 2}},
	})
	_, err := b.Recall(ctx, "ana", "", 10, time.Time{})
	require.NoError(t, err)
	_, err = b.Recall(ctx, "ana", "trains", 10, time.Time{})
	require.NoError(t, err)
	for _, filters := range f.filters {
		assert.Equal(t, map[string]any{"observer_id": ana, "observed_id": ana}, filters, "the service is asked for the user's own conclusions only")
	}
	f.leaky = true

	h, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"ana rides trains"}, texts(h))
	for _, q := range []string{"", "trains"} {
		got, err := b.Recall(ctx, "ana", q, 10, time.Time{})
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"ana likes trains", "ana concluded trains"}, texts(got), "query %q", q)
	}
}

// An erase waits for the deriver to finish the user's work and deletes the
// conclusions again, so a fact derived while it ran does not remain.
func TestAnEraseDeletesWhatTheDeriverFormedWhileItRan(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "I take the train", start))
	require.NoError(t, err)
	ws := onlyWorkspace(t, f)
	f.late = func(w, peer string) {
		f.ws[w].conclusions = append(f.ws[w].conclusions, fakeConclusion{ID: "late", Content: "formed late", Observer: peer, Observed: peer, At: start})
	}
	for _, c := range []struct{ filter, busyAs string }{
		{"sender_id=" + ana, "pending"},
		{"observer_id=" + ana, "pending"},
		{"sender_id=" + ana, "in_progress"},
	} {
		f.mu.Lock()
		f.queue[c.filter], f.busyAs = 3, c.busyAs
		f.mu.Unlock()
		require.NoError(t, b.DeleteSubject(ctx, "ana"))
		f.mu.Lock()
		left := f.queue[c.filter]
		f.mu.Unlock()
		assert.Zero(t, left, "%s %s: the queue was read until it drained", c.filter, c.busyAs)
		assert.Empty(t, f.conclusionsOf(ws, ana), "%s %s", c.filter, c.busyAs)
	}
}

// An erase the deriver keeps busy past the wait fails and says so; deleting
// again once the deriver is done finishes it.
func TestAnEraseFailsWhileTheDeriverHoldsWork(t *testing.T) {
	f, srv := newFake(t)
	b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", DeleteWait: 200 * time.Millisecond})
	require.NoError(t, err)
	_, err = b.Write(ctx, event("s1", "hello", start))
	require.NoError(t, err)
	f.queue["observer_id="+ana] = 1 << 30
	require.ErrorContains(t, b.DeleteSubject(ctx, "ana"), "finished its deriver work")
	f.mu.Lock()
	f.queue["observer_id="+ana] = 0
	f.mu.Unlock()
	require.NoError(t, b.DeleteSubject(ctx, "ana"))
}

// A token scoped to the namespace's workspace is enough for every operation:
// no call names another workspace or lists them.
func TestATokenScopedToTheWorkspaceIsEnough(t *testing.T) {
	f, srv := newFake(t)
	f.scope = "bonyan--" + hex.EncodeToString([]byte("test"))
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "old", start))
	require.NoError(t, err)
	_, err = b.Write(ctx, event("s1", "new", start.Add(2*time.Hour)))
	require.NoError(t, err)
	_, err = b.Recall(ctx, "ana", "q", 10, time.Time{})
	require.NoError(t, err)
	require.NoError(t, b.DeleteBefore(ctx, start.Add(time.Hour)))
	h, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"new"}, texts(h))
	require.NoError(t, b.DeleteSubject(ctx, "ana"))
	for _, c := range f.called() {
		assert.NotContains(t, c, "/v3/workspaces/list")
	}
}

// The conformance suite passes with listing workspaces refused, as a scoped
// token refuses it.
func TestConformanceWithoutListingWorkspaces(t *testing.T) {
	memorytest.Run(t, func(t *testing.T) memorytest.Open {
		f, srv := newFake(t)
		f.fail = func(_, path string) bool { return path == "/v3/workspaces/list" }
		return func(namespace string) memory.Backend {
			b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: namespace, DeleteWait: time.Second})
			require.NoError(t, err)
			return b
		}
	})
}

// With the service listing every session as anyone's, a user's write still
// goes to the user's own first generation, whatever generations another user
// has reached.
func TestAWriteCountsOnlyTheUsersOwnGenerations(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	for i, text := range []string{"old", "new"} {
		r := event("s1", text, start.Add(time.Duration(i)*2*time.Hour))
		r.Subject = "bo"
		_, err := b.Write(ctx, r)
		require.NoError(t, err)
	}
	require.NoError(t, b.DeleteBefore(ctx, start.Add(time.Hour)), "bo's s1 is at its second generation")
	f.leaky = true
	_, err := b.Write(ctx, event("s1", "ana's first", start))
	require.NoError(t, err)
	s1 := hex.EncodeToString([]byte("s1"))
	assert.Contains(t, f.sessions(onlyWorkspace(t, f)), ana+"--"+s1+"-g1")
}

// An erase deletes only the user's conclusions even when the service ignores
// the filter it is given.
func TestAnEraseDeletesOnlyTheUsersConclusions(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	bo := hex.EncodeToString([]byte("bo"))
	_, err := b.Write(ctx, event("s1", "hello", start))
	require.NoError(t, err)
	ws := onlyWorkspace(t, f)
	f.conclude(ws, ana, ana, "about ana", start)
	f.conclude(ws, bo, bo, "about bo", start)
	f.leaky = true
	require.NoError(t, b.DeleteSubject(ctx, "ana"))
	assert.Empty(t, f.conclusionsOf(ws, ana))
	assert.Equal(t, []string{"about bo"}, f.conclusionsOf(ws, bo))
}

// A rewrite copies only the user's own conversation: a message another peer
// put in the user's session is not carried into the new generation.
func TestARewriteCopiesOnlyTheUsersMessages(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	for i, text := range []string{"old", "new"} {
		_, err := b.Write(ctx, event("s1", text, start.Add(time.Duration(i)*2*time.Hour)))
		require.NoError(t, err)
	}
	ws := onlyWorkspace(t, f)
	f.plant(ws, f.sessions(ws)[0], map[string]any{
		"content": "planted", "peer_id": hex.EncodeToString([]byte("bo")), "created_at": start.Add(3 * time.Hour).Format(time.RFC3339),
		"metadata": map[string]any{"bonyan": map[string]any{"layer": "short-term", "session": "s1", "origin": "user", "verdict": 2}},
	})
	require.NoError(t, b.DeleteBefore(ctx, start.Add(time.Hour)))
	gens := f.sessions(ws)
	require.Len(t, gens, 1)
	for _, m := range f.messages(ws, gens[0]) {
		assert.NotEqual(t, "planted", m["content"])
	}
}

// The service marks a deleted session at once and removes it later. Reads
// never see a marked session, so what retention or an erase deleted is gone
// for the reader at once, however long the service takes to remove it.
func TestReadsSkipASessionTheServiceHasNotRemovedYet(t *testing.T) {
	f, srv := newFake(t)
	f.lag = -1
	b := open(t, f, srv.URL)
	for i, text := range []string{"old", "new"} {
		_, err := b.Write(ctx, event("s1", text, start.Add(time.Duration(i)*2*time.Hour)))
		require.NoError(t, err)
	}
	_, err := b.Write(ctx, event("s2", "old in s2", start))
	require.NoError(t, err)
	_, err = b.Write(ctx, memory.Record{Layer: memory.LongTerm, Subject: "ana", Origin: content.KindUser, Text: "old fact", At: start})
	require.NoError(t, err)

	require.NoError(t, b.DeleteBefore(ctx, start.Add(time.Hour)))
	h, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"new"}, texts(h))
	h, err = b.History(ctx, "ana", "s2", time.Time{})
	require.NoError(t, err)
	assert.Empty(t, h)
	got, err := b.Recall(ctx, "ana", "", 10, time.Time{})
	require.NoError(t, err)
	assert.Empty(t, got)
	got, err = b.Recall(ctx, "ana", "old fact", 10, time.Time{})
	require.NoError(t, err)
	assert.Empty(t, got)
}

// A write to a session retention emptied goes to a generation past the
// deleted one, whose name the service refuses until it has removed it.
func TestAWriteAfterRetentionEmptiedASession(t *testing.T) {
	f, srv := newFake(t)
	f.lag = -1
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "old", start))
	require.NoError(t, err)
	require.NoError(t, b.DeleteBefore(ctx, start.Add(time.Hour)))
	_, err = b.Write(ctx, event("s1", "new", start.Add(2*time.Hour)))
	require.NoError(t, err)
	h, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"new"}, texts(h))
	s1 := hex.EncodeToString([]byte("s1"))
	assert.Equal(t, []string{ana + "--" + s1 + "-g2"}, f.sessions(onlyWorkspace(t, f)))
}

// An erase returns only once the service has removed the subject's sessions,
// and fails, saying so, when it has not within the wait.
func TestAnEraseWaitsUntilTheServiceRemovesTheSessions(t *testing.T) {
	f, srv := newFake(t)
	f.lag = 20
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "hello", start))
	require.NoError(t, err)
	ws := onlyWorkspace(t, f)
	require.NoError(t, b.DeleteSubject(ctx, "ana"))
	assert.True(t, f.removed(ws, ana+"--"), "every session removed, not only marked")

	f2, srv2 := newFake(t)
	f2.lag = -1
	b2, err := honcho.Open(honcho.Options{URL: srv2.URL, Namespace: "test", DeleteWait: 200 * time.Millisecond})
	require.NoError(t, err)
	_, err = b2.Write(ctx, event("s1", "hello", start))
	require.NoError(t, err)
	require.ErrorContains(t, b2.DeleteSubject(ctx, "ana"), "had not removed the subject's sessions")
	h, err := b2.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Empty(t, h, "the reader sees nothing all the same")
}

// A session or message the service names another workspace for is not the
// Backend's, whatever lookup returned it: a write does not count the session's
// generation, and a read drops the message.
func TestAResultFromAnotherWorkspaceIsDropped(t *testing.T) {
	f, srv := newFake(t)
	b := open(t, f, srv.URL)
	_, err := b.Write(ctx, event("s1", "mine", start))
	require.NoError(t, err)
	ws := onlyWorkspace(t, f)
	s1 := hex.EncodeToString([]byte("s1"))
	f.plant(ws, ana+"--"+s1+"-g1", map[string]any{
		"content": "from elsewhere", "peer_id": ana, "workspace_id": "another", "created_at": start.Format(time.RFC3339),
		"metadata": map[string]any{"bonyan": map[string]any{"layer": "short-term", "session": "s1", "origin": "user", "verdict": 2}},
	})
	f.foreign = ana + "--" + s1 + "-g9"
	_, err = b.Write(ctx, event("s1", "mine too", start.Add(time.Minute)))
	require.NoError(t, err)
	h, err := b.History(ctx, "ana", "s1", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, []string{"mine", "mine too"}, texts(h))
	assert.Equal(t, []string{ana + "--" + s1 + "-g1"}, f.sessions(ws), "the other workspace's generation is not counted")
}

// An erase waits for the subject's own sessions to be removed, not for a
// session the service names another workspace for, which this Backend could
// never remove.
func TestAnEraseDoesNotWaitForAnotherWorkspacesSession(t *testing.T) {
	f, srv := newFake(t)
	b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", DeleteWait: 300 * time.Millisecond})
	require.NoError(t, err)
	_, err = b.Write(ctx, event("s1", "hello", start))
	require.NoError(t, err)
	f.foreign = ana + "--" + hex.EncodeToString([]byte("s1")) + "-g9"
	require.NoError(t, b.DeleteSubject(ctx, "ana"))
}

// A workspace that exists keeps the deriver setting it was made with unless
// it is written again: opening it with another setting, the deriver turned on
// or off or its instructions changed, updates it, and opening it with the
// same setting writes nothing.
func TestAWorkspacesDeriverSettingFollowsTheBackend(t *testing.T) {
	f, srv := newFake(t)
	ws := "bonyan--" + hex.EncodeToString([]byte("test"))
	reopen := func(derive bool, instructions string) {
		t.Helper()
		b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", Derive: derive, Instructions: instructions})
		require.NoError(t, err)
		_, err = b.Write(ctx, event("s1", "hello", start))
		require.NoError(t, err)
	}
	puts := func() int {
		n := 0
		for _, c := range f.called() {
			if c == "PUT /v3/workspaces/"+ws {
				n++
			}
		}
		return n
	}

	reopen(false, "")
	assert.Equal(t, map[string]any{"enabled": false}, f.ws[ws].config["reasoning"])
	assert.Zero(t, puts(), "made with the setting, nothing to write")

	reopen(true, "about the speaker")
	assert.Equal(t, map[string]any{"enabled": true, "custom_instructions": "about the speaker"}, f.ws[ws].config["reasoning"], "turned on")
	assert.Equal(t, 1, puts())
	assert.Equal(t, "test", f.ws[ws].metadata["bonyan_namespace"], "the namespace stays recorded")

	reopen(true, "about the speaker")
	assert.Equal(t, 1, puts(), "the same setting writes nothing")

	reopen(true, "")
	assert.Equal(t, map[string]any{"enabled": true}, f.ws[ws].config["reasoning"], "instructions removed")

	reopen(false, "")
	assert.Equal(t, map[string]any{"enabled": false}, f.ws[ws].config["reasoning"], "turned off")
	assert.Equal(t, 3, puts())
}

// A workspace an operator made with no deriver setting is given the Backend's,
// rather than left to the service's default.
func TestAWorkspaceWithNoDeriverSettingIsGivenOne(t *testing.T) {
	f, srv := newFake(t)
	f.makeWorkspace("ops-made", map[string]any{"bonyan_namespace": "test"})
	b, err := honcho.Open(honcho.Options{URL: srv.URL, Namespace: "test", Workspace: "ops-made"})
	require.NoError(t, err)
	_, err = b.Write(ctx, event("s1", "hello", start))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"enabled": false}, f.ws["ops-made"].config["reasoning"])
}
