package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/trust"
)

var (
	ctx   = context.Background()
	start = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
)

func open(t *testing.T, path string) *Backend {
	t.Helper()
	b, err := Open(path, "test")
	require.NoError(t, err)
	return b
}

func rec(layer memory.Layer, subject, text string, at time.Time) memory.Record {
	r := memory.Record{
		Layer: layer, Subject: subject, Origin: content.KindUser, Text: text, At: at,
		Decision: memory.Decision{Verdict: trust.Untrusted, Policy: "default"},
	}
	if layer == memory.ShortTerm {
		r.Session = "s1"
	}
	return r
}

func write(t *testing.T, b *Backend, recs ...memory.Record) {
	t.Helper()
	for _, r := range recs {
		_, err := b.Write(ctx, r)
		require.NoError(t, err)
	}
}

// indexed counts the full-text entries matching word, straight from the index.
func indexed(t *testing.T, b *Backend, word string) int {
	t.Helper()
	var n int
	require.NoError(t, b.db.QueryRow(`SELECT count(*) FROM records_text WHERE records_text MATCH ?`, `"`+word+`"`).Scan(&n))
	return n
}

// onDisk is every byte of the database's files.
func onDisk(t *testing.T, path string) string {
	t.Helper()
	var all strings.Builder
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		all.Write(b)
	}
	return all.String()
}

func TestRecordsSurviveReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	b := open(t, path)
	write(t, b, rec(memory.LongTerm, "ana", "prefers mail", start))
	require.NoError(t, b.Close())

	b = open(t, path)
	defer func() { require.NoError(t, b.Close()) }()
	got, err := b.Recall(ctx, "ana", "mail", 10, time.Time{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "prefers mail", got[0].Text)
}

// Nothing in a query is read as full-text query syntax.
func TestAQueryIsOnlyWords(t *testing.T) {
	b := open(t, filepath.Join(t.TempDir(), "memory.db"))
	defer func() { require.NoError(t, b.Close()) }()
	write(t, b, rec(memory.LongTerm, "ana", `use NEAR( or "quoted" text:* here`, start))

	for _, q := range []string{`NEAR(`, `or`, `"quoted"`, `text:*`, `text:* NEAR(`, `*`, `"`} {
		_, err := b.Recall(ctx, "ana", q, 10, time.Time{})
		require.NoError(t, err, "query %q", q)
	}
	got, err := b.Recall(ctx, "ana", `NEAR( "quoted"`, 10, time.Time{})
	require.NoError(t, err)
	assert.Len(t, got, 1, "the words match as words")
}

// The database and its write-ahead log are readable by their owner only.
func TestTheFilesAreOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	b := open(t, path)
	defer func() { require.NoError(t, b.Close()) }()
	write(t, b, rec(memory.LongTerm, "ana", "prefers mail", start))
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		require.NoError(t, err, "%s exists while the database is open", filepath.Base(p))
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), filepath.Base(p))
	}
}

// Deleting removes the text from the index and from every file of the
// database, not only from query results.
func TestDeletedTextIsGoneFromTheFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		del  func(*Backend) error
	}{
		{"by subject", func(b *Backend) error { return b.DeleteSubject(ctx, "ana") }},
		{"by age", func(b *Backend) error { return b.DeleteBefore(ctx, start.Add(time.Hour)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "memory.db")
			b := open(t, path)
			write(t, b,
				rec(memory.LongTerm, "ana", "zebracrossing fact", start),
				rec(memory.ShortTerm, "ana", "okapiwalk event", start),
				rec(memory.LongTerm, "bo", "giraffeneck fact", start.Add(2*time.Hour)),
			)
			require.Equal(t, 1, indexed(t, b, "zebracrossing"))
			// The positive control: before the delete, the text is on disk.
			all := onDisk(t, path)
			require.True(t, strings.Contains(all, "zebracrossing"), "zebracrossing"+" on disk")
			require.True(t, strings.Contains(all, "okapiwalk"), "okapiwalk"+" on disk")

			require.NoError(t, tc.del(b))
			assert.Equal(t, 0, indexed(t, b, "zebracrossing"), "the full-text entry is gone")
			assert.Equal(t, 1, indexed(t, b, "giraffeneck"), "another record's entry stays")
			all = onDisk(t, path)
			assert.False(t, strings.Contains(all, "zebracrossing"), "zebracrossing"+" still on disk")
			assert.False(t, strings.Contains(all, "okapiwalk"), "okapiwalk"+" still on disk")
			require.NoError(t, b.Close())
			all = onDisk(t, path)
			assert.False(t, strings.Contains(all, "zebracrossing"), "zebracrossing"+" still on disk")
			assert.False(t, strings.Contains(all, "okapiwalk"), "okapiwalk"+" still on disk")
			assert.True(t, strings.Contains(all, "giraffeneck"), "giraffeneck"+" on disk")
		})
	}
}

// The index's own secure-delete option is on. The file test above cannot show
// it, because merging the index on every delete already writes the deleted
// entries out; the option is what removes them should that merge ever go.
func TestTheIndexDeletesSecurely(t *testing.T) {
	b := open(t, filepath.Join(t.TempDir(), "memory.db"))
	defer func() { require.NoError(t, b.Close()) }()
	var v int
	require.NoError(t, b.db.QueryRow(`SELECT v FROM records_text_config WHERE k = 'secure-delete'`).Scan(&v))
	assert.Equal(t, 1, v)
}

// A reader holding the write-ahead log keeps a delete from emptying it. The
// delete then says so rather than reporting success, and once the reader lets
// go, a later delete empties the log and the text is gone.
func TestADeleteThatCannotEmptyTheLogSaysSo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	b := open(t, path)
	defer func() { require.NoError(t, b.Close()) }()
	write(t, b, rec(memory.LongTerm, "ana", "zebracrossing fact", start), rec(memory.LongTerm, "bo", "giraffeneck fact", start))

	reader, err := b.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	var n int
	require.NoError(t, reader.QueryRow(`SELECT count(*) FROM records`).Scan(&n), "the read holds a snapshot")

	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	err = b.DeleteSubject(short, "ana")
	require.ErrorIs(t, err, ErrLogNotEmptied)
	got, err := b.Recall(ctx, "ana", "", 10, time.Time{})
	require.NoError(t, err)
	assert.Empty(t, got, "the records are deleted all the same")
	assert.True(t, strings.Contains(onDisk(t, path), "zebracrossing"), "the positive control: the text is still in the log")

	require.NoError(t, reader.Rollback())
	require.NoError(t, b.DeleteSubject(ctx, "ana"), "with the reader gone the log empties")
	assert.False(t, strings.Contains(onDisk(t, path), "zebracrossing"), "zebracrossing still on disk")
}

// Emptying the log turns SQLite's own wait off on the connection it uses; the
// connection goes back to the pool with its wait restored.
func TestTheLogCheckpointRestoresTheConnectionsWait(t *testing.T) {
	b := open(t, filepath.Join(t.TempDir(), "memory.db"))
	defer func() { require.NoError(t, b.Close()) }()
	b.db.SetMaxOpenConns(1) // so the next query gets the connection the delete used
	write(t, b, rec(memory.LongTerm, "ana", "a fact", start))
	require.NoError(t, b.DeleteSubject(ctx, "ana"))
	var wait int
	require.NoError(t, b.db.QueryRow(`PRAGMA busy_timeout`).Scan(&wait))
	assert.Equal(t, 5000, wait)
}

// A database made before records named their server, or had namespaces,
// opens with both columns added. Its records have no server, so a policy
// trusting a named server does not trust them, and they are in the empty
// namespace, where a backend opened with another namespace never sees them.
// New records keep their server, and opening again changes nothing.
func TestADatabaseFromBeforeServersAndNamespacesOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	old := strings.Replace(schema, "\tserver  TEXT    NOT NULL DEFAULT '',\n", "", 1)
	require.NotEqual(t, schema, old, "the old schema has no server column")
	require.NotContains(t, old, "namespace", "nor a namespace column")
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = db.Exec(old)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO records (layer, subject, session, origin, text, at, verdict, policy) VALUES ('long-term', 'ana', '', 'remote-tool', 'from before', ?, 2, 'default')`, start.UnixNano())
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO records_text (rowid, text) VALUES (last_insert_rowid(), 'from before')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	for range 2 {
		b, err := Open(path, "")
		require.NoError(t, err)
		_, err = b.Write(ctx, memory.Record{Layer: memory.LongTerm, Subject: "ana", Origin: content.KindRemoteTool, Server: "docs", Text: "from docs", At: start})
		require.NoError(t, err)
		got, err := b.Recall(ctx, "ana", "", 10, time.Time{})
		require.NoError(t, err)
		servers := map[string]string{}
		for _, r := range got {
			servers[r.Text] = r.Server
		}
		assert.Equal(t, "", servers["from before"])
		assert.Equal(t, "docs", servers["from docs"])
		require.NoError(t, b.Close())

		other, err := Open(path, "test")
		require.NoError(t, err)
		got, err = other.Recall(ctx, "ana", "before", 10, time.Time{})
		require.NoError(t, err)
		assert.Empty(t, got, "the old records are in the empty namespace")
		require.NoError(t, other.Close())
	}
}
