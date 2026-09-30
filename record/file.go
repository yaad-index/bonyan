package record

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sink receives recorded entries. It is the pluggable slot; what it receives
// has already been redacted by a Recorder.
type Sink interface {
	Write(e Entry) error
	// Full reports whether the sink is a full recording, which may hold
	// recalled memory.
	Full() bool
	// Subject is who a full recording is about, so it can be deleted by
	// subject. A full sink must report one; NewRecorder refuses it otherwise.
	Subject() string
	Close() error
}

// DefaultDir returns the directory recordings are written to when a program
// names none: under the user's cache directory, outside any working tree, so a
// recording of real traffic is not committed by accident.
func DefaultDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("record: no default directory: %w", err)
	}
	return filepath.Join(base, "bonyan", "recordings"), nil
}

// FileOptions configure a File sink.
type FileOptions struct {
	// Dir is where recordings are written. Empty means DefaultDir.
	Dir string
	// Full makes the recording a full one, which keeps recalled memory. It
	// requires Subject.
	Full bool
	// Subject is who a full recording is about. Full recordings are kept per
	// subject so they can be deleted by subject (DeleteSubject).
	Subject string
}

// File writes one recording to a new file. Directories are created owner-only
// (0700) and the file owner-only (0600). A file is never overwritten.
type File struct {
	mu      sync.Mutex
	f       *os.File
	enc     *json.Encoder
	full    bool
	subject string
	path    string
}

// OpenFile creates a new recording file and writes its header.
func OpenFile(opts FileOptions) (*File, error) {
	dir := opts.Dir
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	if opts.Full && opts.Subject == "" {
		return nil, errors.New("record: a full recording needs a subject")
	}
	if !opts.Full && opts.Subject != "" {
		return nil, errors.New("record: a subject is only for full recordings")
	}
	if opts.Full {
		sub, err := subjectDir(dir, opts.Subject)
		if err != nil {
			return nil, err
		}
		dir = sub
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	now := time.Now().UTC()
	name := fmt.Sprintf("%s-%s.jsonl", now.Format("20060102T150405Z"), hex.EncodeToString(suffix[:]))
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	s := &File{f: f, enc: json.NewEncoder(f), full: opts.Full, subject: opts.Subject, path: path}
	if err := s.enc.Encode(Header{Format: Format, Version: Version, Full: opts.Full, Created: now}); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("record: %w", err)
	}
	return s, nil
}

// Path returns the file's path.
func (s *File) Path() string { return s.path }

// Full reports whether this is a full recording.
func (s *File) Full() bool { return s.full }

// Subject returns the subject of a full recording, and "" otherwise.
func (s *File) Subject() string { return s.subject }

// Write appends one entry.
func (s *File) Write(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return errors.New("record: write to a closed recording")
	}
	return s.enc.Encode(e)
}

// Close closes the file.
func (s *File) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// DeleteSubject removes every full recording kept for subject under dir (empty
// means DefaultDir). Removing a subject with no recordings is not an error.
func DeleteSubject(dir, subject string) error {
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			return err
		}
		dir = d
	}
	if subject == "" {
		return errors.New("record: no subject given")
	}
	sub, err := subjectDir(dir, subject)
	if err != nil {
		return err
	}
	return os.RemoveAll(sub)
}

// subjectDir returns the directory holding subject's full recordings. It is
// named by an HMAC of the subject under a key kept in dir, so a directory
// listing alone does not let anyone confirm a guessed subject (a user id, an
// address) by hashing it. Whoever can read the key file can still test guesses;
// the key is owner-only for that reason.
func subjectDir(dir, subject string) (string, error) {
	key, err := subjectKey(dir)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(subject))
	return filepath.Join(dir, "subjects", hex.EncodeToString(mac.Sum(nil))), nil
}

const keyFile = "subject.key"

// subjectKey reads the per-directory key, creating it on first use.
func subjectKey(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	path := filepath.Join(dir, keyFile)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		_, werr := f.Write(key)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			return nil, fmt.Errorf("record: write subject key: %w", errors.Join(werr, cerr))
		}
		return key, nil
	case errors.Is(err, fs.ErrExist):
		existing, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("record: %w", err)
		}
		if len(existing) != len(key) {
			return nil, fmt.Errorf("record: subject key %s has the wrong length", path)
		}
		return existing, nil
	default:
		return nil, fmt.Errorf("record: %w", err)
	}
}
