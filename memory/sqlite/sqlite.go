// Package sqlite is bonyan's basic memory backend: one SQLite file, with
// full-text recall, and no external service (ADR 0001 §4, ADR 0002 §3).
//
// Deleting removes the text from the file, not only from query results: the
// database overwrites deleted content, the full-text index removes a deleted
// record's entries and is merged so no old copy of them stays in the file, and
// the write-ahead log is emptied after every delete, so a deleted record's text
// does not survive in an older log frame. A reader holding the log can keep it
// from emptying; the delete then returns ErrLogNotEmptied.
//
// The file and its write-ahead log are created readable by their owner only.
// Keep the file outside the working tree.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite" // the driver

	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/trust"
)

// Backend is a memory.Backend over one SQLite file.
type Backend struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS records (
	id      INTEGER PRIMARY KEY,
	layer   TEXT    NOT NULL,
	subject TEXT    NOT NULL,
	session TEXT    NOT NULL,
	origin  TEXT    NOT NULL,
	text    TEXT    NOT NULL,
	at      INTEGER NOT NULL,
	verdict INTEGER NOT NULL,
	policy  TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS records_session ON records (subject, layer, session, at);
CREATE INDEX IF NOT EXISTS records_at ON records (at);
CREATE VIRTUAL TABLE IF NOT EXISTS records_text USING fts5 (text, content='', contentless_delete=1);
INSERT INTO records_text (records_text, rank) VALUES ('secure-delete', 1);
`

// Open opens the database at path, creating it owner-only if it does not
// exist. The caller closes it.
func Open(path string) (*Backend, error) {
	if path == "" {
		return nil, errors.New("sqlite: empty path")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	q := url.Values{}
	for _, p := range []string{"secure_delete(1)", "journal_mode(WAL)", "busy_timeout(5000)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: schema: %w", err)
	}
	return &Backend{db: db}, nil
}

// Close closes the database.
func (b *Backend) Close() error { return b.db.Close() }

// Write stores r.
func (b *Backend) Write(ctx context.Context, r memory.Record) (string, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO records (layer, subject, session, origin, text, at, verdict, policy) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(r.Layer), r.Subject, r.Session, string(r.Origin), r.Text, r.At.UnixNano(), int(r.Decision.Verdict), r.Decision.Policy)
	if err != nil {
		return "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	if r.Layer == memory.LongTerm {
		if _, err := tx.ExecContext(ctx, `INSERT INTO records_text (rowid, text) VALUES (?, ?)`, id, r.Text); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return strconv.FormatInt(id, 10), nil
}

const columns = `r.id, r.layer, r.subject, r.session, r.origin, r.text, r.at, r.verdict, r.policy`

// History returns a session's events in the order they were written.
func (b *Backend) History(ctx context.Context, subject, session string, since time.Time) ([]memory.Record, error) {
	return b.query(ctx, `SELECT `+columns+` FROM records r
		WHERE r.subject = ? AND r.layer = ? AND r.session = ? AND r.at >= ? ORDER BY r.at, r.id`,
		subject, string(memory.ShortTerm), session, since.UnixNano())
}

// Recall returns the facts matching every word of query, best match first;
// an empty query returns the newest facts.
func (b *Backend) Recall(ctx context.Context, subject, query string, limit int, since time.Time) ([]memory.Record, error) {
	match := matchQuery(query)
	if match == "" {
		return b.query(ctx, `SELECT `+columns+` FROM records r
			WHERE r.subject = ? AND r.layer = ? AND r.at >= ? ORDER BY r.at DESC, r.id DESC LIMIT ?`,
			subject, string(memory.LongTerm), since.UnixNano(), limit)
	}
	return b.query(ctx, `SELECT `+columns+` FROM records_text f JOIN records r ON r.id = f.rowid
		WHERE records_text MATCH ? AND r.subject = ? AND r.layer = ? AND r.at >= ?
		ORDER BY bm25(records_text), r.at DESC, r.id DESC LIMIT ?`,
		match, subject, string(memory.LongTerm), since.UnixNano(), limit)
}

// matchQuery turns query into a full-text query matching every word, each
// quoted, so nothing in it is read as query syntax. Words with no letter or
// digit are left out.
func matchQuery(query string) string {
	var terms []string
	for _, w := range strings.Fields(query) {
		if !strings.ContainsFunc(w, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " ")
}

func (b *Backend) query(ctx context.Context, q string, args ...any) ([]memory.Record, error) {
	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []memory.Record
	for rows.Next() {
		var (
			r                     memory.Record
			id, at                int64
			verdict               int
			layer, origin, policy string
		)
		if err := rows.Scan(&id, &layer, &r.Subject, &r.Session, &origin, &r.Text, &at, &verdict, &policy); err != nil {
			return nil, err
		}
		r.ID = strconv.FormatInt(id, 10)
		r.Layer, r.Origin = memory.Layer(layer), content.Kind(origin)
		r.At = time.Unix(0, at).UTC()
		r.Decision = memory.Decision{Verdict: trust.Verdict(verdict), Policy: policy}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteSubject deletes every record of subject.
func (b *Backend) DeleteSubject(ctx context.Context, subject string) error {
	return b.delete(ctx, `subject = ?`, subject)
}

// DeleteBefore deletes every record written before t.
func (b *Backend) DeleteBefore(ctx context.Context, t time.Time) error {
	return b.delete(ctx, `at < ?`, t.UnixNano())
}

// delete removes the matching records and their full-text entries in one
// transaction, merges the index, then empties the write-ahead log so no older
// frame keeps the deleted text.
func (b *Backend) delete(ctx context.Context, where string, arg any) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM records_text WHERE rowid IN (SELECT id FROM records WHERE `+where+`)`, arg); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE `+where, arg); err != nil {
		return err
	}
	// Removing an entry from the index rewrites its segment, and the old copy
	// of the segment can stay in the file; merging the index writes it out
	// afresh. It costs time in the size of the index, on a delete, which is
	// rare.
	if _, err := tx.ExecContext(ctx, `INSERT INTO records_text (records_text) VALUES ('optimize')`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return b.emptyLog(ctx)
}

// ErrLogNotEmptied is what a delete returns when the records are deleted but
// the write-ahead log could not be emptied, because a reader held it for as
// long as the delete could wait. The deleted text can remain in the log until
// a later delete, or closing the database, empties it.
var ErrLogNotEmptied = errors.New("sqlite: deleted, but the write-ahead log could not be emptied")

// logWait bounds how long a delete waits for readers to let the log go.
const logWait = 5 * time.Second

// emptyLog checkpoints the write-ahead log and truncates it, retrying while a
// reader holds it, until ctx ends or logWait has passed. The checkpoint runs on
// its own connection with SQLite's busy wait off, so this loop alone decides
// how long to wait and reads every answer.
func (b *Backend) emptyLog(ctx context.Context) (err error) {
	ctx, cancel := context.WithTimeout(ctx, logWait)
	defer cancel()
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrLogNotEmptied, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		return fmt.Errorf("%w: %w", ErrLogNotEmptied, err)
	}
	// The connection goes back to the pool, so it gets its wait back.
	defer func() {
		if _, rerr := conn.ExecContext(context.Background(), `PRAGMA busy_timeout = 5000`); rerr != nil && err == nil {
			err = rerr
		}
	}()
	pause := 10 * time.Millisecond
	for {
		var busy, frames, checkpointed int
		qerr := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed)
		if qerr == nil && busy == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			if qerr != nil {
				return fmt.Errorf("%w: %w", ErrLogNotEmptied, qerr)
			}
			return ErrLogNotEmptied
		case <-time.After(pause):
		}
		pause = min(2*pause, 200*time.Millisecond)
	}
}
