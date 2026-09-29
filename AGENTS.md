# Working on bonyan

This file is for people and agents changing bonyan. `README.md` is for programs that use it.

## What this is

A Go library for building LLM agents. There is no binary. The shape, the pluggable slots and the invariants that wrap them are decided in [ADR 0001](adr/0001-shape-of-the-framework.md); read it before changing an area it governs. Later decisions are further numbered files in `adr/`.

## Before pushing

```
make check          # vet, build, race tests, formatting, lint, tidiness: what CI runs
make fmt            # apply gofumpt and goimports, tidy both modules
make install-hooks  # optional: run the fast subset of make check on every commit
```

Nothing needs installing besides Go. The formatters and the linter are tool dependencies in `tools/go.mod`, a separate module, and run as `go tool -modfile=tools/go.mod <tool>`. Keeping them in their own module keeps them out of `go.mod`, which every program using bonyan resolves. Add a new dev tool the same way:

```
go get -modfile=tools/go.mod -tool <package>@<version>
```

## Conventions

- Formatting is gofumpt, with imports grouped by goimports under the local prefix `github.com/yaad-index/bonyan`.
- Tests use testify (`require` / `assert`) and always run with `-race`.
- Pull request titles are Conventional Commits: squash merges use the title as the commit subject, and releases are computed from it.
- A dependency that is load-bearing for the public API is decided in an ADR before it is added.
