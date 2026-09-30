# ADR 0002: Stack, conventions and dependencies

**Status:** Accepted, amended (maintainer sign-off recorded by approval of the PR that sets this status and of each PR that amends it; see Amendments)

## Context

ADR 0001 decides what bonyan is. This ADR decides what it is built with: the house Go conventions and where a library departs from them, the third-party modules the public API rests on, and where the heavier ones live. The implementation plan (`docs/implementation-plan.md`) surveys the candidates per slot; this records the choices that are load-bearing.

Adding a module to a library is costlier than adding it to a program. Every module a library's `go.mod` requires is part of the module graph of every program that uses the library, and takes part in its version selection, even when the program never imports the package that needed it. Module counts below are measured, not estimated: for each dependency one named package was imported alone into an empty module on 2026-09-29, the module tidied, and the modules listed by `go list -m all` counted, excluding the empty module itself. The count depends on the package imported, so each figure names its package.

## Decision

### 1. Conventions

The house Go conventions apply, with these departures because bonyan is a library:

- **No binary.** No `cmd/`, no CLI, no container image.
- **The public API is exported packages.** Only helpers go under `internal/`.
- **Configuration is passed in by the program**, as data. bonyan reads no configuration file itself.
- **Formatting and linting:** gofumpt, and goimports with the local prefix `github.com/yaad-index/bonyan`, both also enabled as golangci-lint formatters.
- **Dev tools** are tool dependencies in a separate `tools/go.mod` and run with `go tool -modfile=tools/go.mod`, so none of them enter the library's module graph. No tool is exempt.
- **One verification target**, `make check`, which CI also runs.
- **The `go` directive** is the lowest minor version bonyan needs, not the newest patch release, because it is a requirement placed on every caller.

### 2. Modules the core rests on

| Module | Version at decision | Package imported for the count | Modules added | Used for |
|---|---|---|---|---|
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | `github.com/modelcontextprotocol/go-sdk/mcp` | 13 | the tool-server protocol client (ADR 0001 §5) |
| `github.com/google/jsonschema-go` | v0.4.3 | `github.com/google/jsonschema-go/jsonschema` | 2 | schemas generated from Go types, and validation of structured output (§5, §6) |
| `go.opentelemetry.io/otel` (API packages only) | v1.46.0 | `go.opentelemetry.io/otel/trace` | 10 | spans and metrics (§9); the SDK and exporters are the program's |
| `go.opentelemetry.io/otel/semconv/v1.41.0` | (in otel v1.46.0) | — | — | GenAI attribute names, pinned, re-exported from one bonyan package (§9) |
| `golang.org/x/time` | — | — | — | rate limiting in adapters |
| `github.com/stretchr/testify` | — | — | — | tests only |

The GenAI names are pinned to `semconv/v1.41.0` because later versions of that package no longer carry them: the GenAI conventions moved to a separate specification with no Go package yet. Every GenAI name therefore comes from one bonyan package, so moving to an upstream package later is a change in one place.

The chat-completions-compatible adapter is written on `net/http` rather than on a provider SDK. Its reasons are in the plan (§3); the relevant one here is that it adds no module.

### 3. The basic memory store: SQLite, not the house key-value store

The basic memory backend (ADR 0001 §4) uses `modernc.org/sqlite` (v1.60.0, pure Go, 25 modules, counted by importing `modernc.org/sqlite`), not the house embedded store `go.etcd.io/bbolt` (v1.5.0, 15 modules, counted by importing `go.etcd.io/bbolt`).

The memory interface requires delete by subject, retention by age and recall. With SQLite these are a `DELETE … WHERE subject = ?`, a `DELETE … WHERE created_at < ?`, and a full-text query (FTS5), each a single statement the database keeps consistent. With a key-value store each needs a secondary index kept consistent by hand, and recall needs a text index written from scratch. The conformance suite (ADR 0001 §4) tests either way, but it is the hand-kept indexes that would need it most. The driver builds with cgo off, so the static-binary convention still holds for programs that use it.

### 4. Where the heavier dependencies live

Two packages are optional and heavy: the SQLite backend (25 modules), and the exact token counter `tokenize/tiktoken`, built on `github.com/tiktoken-go/tokenizer` (v0.8.1, 4 modules counted by importing that package, but its vocabularies are compiled in and its codec sources total about 16 MB).

**Each gets its own nested module**: `github.com/yaad-index/bonyan/memory/sqlite` and `github.com/yaad-index/bonyan/tokenize/tiktoken`. The root module keeps only what every agent needs (section 2). A program that uses the in-memory reference backend and the byte-bound counter never sees SQLite or the tokenizer in its module graph.

Deciding this before either package exists is deliberate. Moving a package between modules after release, in either direction, leaves two modules able to provide the same import path at some versions, which callers hit as ambiguous imports. The only move without that hazard is the one made before the first release.

### 5. How a nested module is built, checked and released

- **Building against the root.** A nested module requires the root module at the placeholder version `v0.0.0-00010101000000-000000000000` and replaces it with the repository's root directory (`replace github.com/yaad-index/bonyan => ../..` from `tokenize/tiktoken`). A replace directive applies only when the module is the main module, so no caller ever sees it. A `go.work` file was not used: `go mod tidy` ignores a workspace and resolves the root module at the version required, so with a workspace a change touching the root and a nested module could not be tidied until the root change was published.
- **Releasing.** Because callers do see the requirement, a nested module is released only after a root release, and the pull request that prepares it first raises the requirement from the placeholder to that released root version. The release PR of a nested module is not merged while its `go.mod` still requires the placeholder.
- **Tags.** The root keeps `vX.Y.Z`. A nested module is tagged `<path>/vX.Y.Z` (`tokenize/tiktoken/vX.Y.Z`), the form Go requires for a module in a subdirectory.
- **release-please.** Each nested module is its own package in the release configuration, with its path as component, `/` as tag separator and its own changelog. The root package excludes the nested paths, so a change only inside a nested module does not bump the root. Each package gets its own release PR, so the root can be released on its own before a nested module. The resulting tag format is taken from the release-please documentation and is to be confirmed on the first release PR that includes a nested module, as the first root version is (see `RELEASING.md`).
- **Checks.** There is still one verification target. `make check` in the root runs vet, build, the race tests, lint and the tidiness check in every nested module as well, from a list in the Makefile; formatting already walks the whole tree. CI runs that target, so a nested module is checked on every pull request.
- **Dependency updates.** Each nested module has its own Dependabot entry.
- **Adding a nested module** means adding it to the Makefile list, the release configuration and manifest, the Dependabot configuration and the CI cache paths.

## Consequences

- The root module stays small: the three modules in section 2 and their dependencies.
- Each nested module has its own `go.mod`, its own tags (`memory/sqlite/vX.Y.Z`, `tokenize/tiktoken/vX.Y.Z`) and its own release-please package, and the root `make check` covers it (section 5).
- During development a nested module builds against the root in the same repository through a replace directive; its releases require a released root version, and each is a two-step release: the root first, then the nested module with its requirement raised (section 5).
- A dependency added later that is load-bearing for the public API, or that adds substantially to the module graph, needs its own ADR or an amendment to this one.
- Reversing this after release, in either direction, is a migration with the ambiguous-import hazard above, so it needs its own ADR.

## Amendments

- Section 5 was added, settling how nested modules are built against the root (a replace directive, not a workspace), released (after a root release, with the requirement raised first, from its own release PR), tagged, configured for release-please, checked (by the root `make check`, not a separate run) and kept up to date. The consequences about nested modules were updated to match.
