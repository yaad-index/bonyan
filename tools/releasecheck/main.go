// Command releasecheck refuses a nested module whose release version is set
// while it still requires the root module at the placeholder version (ADR
// 0002, section 5). A nested module's version in the release manifest leaves
// 0.0.0 on the release PR that would tag it, so this fails before a tag
// exists.
//
// Usage: releasecheck <repo root> <nested module dir>...
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/mod/modfile"
)

const (
	rootModule  = "github.com/yaad-index/bonyan"
	placeholder = "v0.0.0-00010101000000-000000000000"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: releasecheck <repo root> <nested module dir>...")
		os.Exit(2)
	}
	if err := check(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "release-check:", err)
		os.Exit(1)
	}
}

func check(root string, nested []string) error {
	raw, err := os.ReadFile(filepath.Join(root, ".release-please-manifest.json"))
	if err != nil {
		return err
	}
	var manifest map[string]string
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("release manifest: %w", err)
	}
	for _, dir := range nested {
		version, ok := manifest[dir]
		if !ok {
			return fmt.Errorf("%s is not in .release-please-manifest.json", dir)
		}
		required, err := requiredRoot(filepath.Join(root, dir, "go.mod"))
		if err != nil {
			return err
		}
		if version != "0.0.0" && required == placeholder {
			return fmt.Errorf("%s is at %s in the release manifest but still requires %s %s; raise it to a released root version first (RELEASING.md)",
				dir, version, rootModule, placeholder)
		}
	}
	return nil
}

// requiredRoot returns the version of the root module that a go.mod requires.
func requiredRoot(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	f, err := modfile.ParseLax(path, raw, nil)
	if err != nil {
		return "", err
	}
	for _, r := range f.Require {
		if r.Mod.Path == rootModule {
			return r.Mod.Version, nil
		}
	}
	return "", fmt.Errorf("%s does not require %s", path, rootModule)
}
