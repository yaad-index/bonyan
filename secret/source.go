package secret

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Source looks secrets up by name. A source reads at every lookup and keeps no
// copy, so a rotated value is returned by the next lookup. It returns
// ErrNotFound for a name it does not hold.
type Source interface {
	Lookup(ctx context.Context, name string) (string, error)
}

// Env reads secrets from the process environment.
type Env struct{}

// Lookup returns the environment variable name.
func (Env) Lookup(_ context.Context, name string) (string, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return v, nil
}

// Dir reads secrets from a directory holding one file per secret, named after
// it. One trailing newline is removed from the file's contents. A name that is
// not a plain file name is refused, and the file is opened within the directory
// so that no link inside it can reach outside.
type Dir struct {
	Path string
}

// Lookup returns the contents of the file name in the directory.
func (d Dir) Lookup(_ context.Context, name string) (string, error) {
	if !validFileName(name) {
		return "", fmt.Errorf("secret: %q is not a valid name for a directory source", name)
	}
	root, err := os.OpenRoot(d.Path)
	if err != nil {
		return "", fmt.Errorf("secret: open directory source: %w", err)
	}
	defer func() { _ = root.Close() }()

	b, err := root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if err != nil {
		return "", fmt.Errorf("secret: read %q: %w", name, err)
	}
	s := string(b)
	if t, ok := strings.CutSuffix(s, "\n"); ok {
		s = strings.TrimSuffix(t, "\r")
	}
	return s, nil
}

func validFileName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`+"\x00")
}
