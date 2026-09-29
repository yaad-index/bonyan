# bonyan
A generic Go library for building LLM agents: model interface, context assembly, pluggable memory, tools, secrets, evals, OpenTelemetry.

## Development

Requires only the Go version in `go.mod`; the formatters and the linter are built from `tools/go.mod`.

```
make check   # vet, build, race tests, formatting, lint, tidiness: the same as CI
```

See `AGENTS.md` for working on the library.
