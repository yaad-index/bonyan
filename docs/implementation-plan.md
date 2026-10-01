# Implementation plan

**Status:** proposed, for discussion. This is a plan, not a decision record, and it is kept open as the reference while the phases land rather than merged. It turns [ADR 0001](../adr/0001-shape-of-the-framework.md) into a sequence of pull requests, and says which existing libraries each part is built on. Nothing here is implemented yet.

Library versions below were read from the Go module proxy on 2026-09-29. Re-check them when each phase starts, because pinning happens in that phase's PR, not here.

## 1. Conventions this plan follows

The house Go conventions ("Go Project Bootstrap", revision of 2026-09-29) apply as written, with the following deviations because bonyan is a library rather than a program:

| House convention | For bonyan | Why |
|---|---|---|
| Thin `cmd/<binary>/` with a Kong CLI | not applicable | there is no binary |
| All behaviour under `internal/` | the public API is **exported packages**; only helpers live in `internal/` | a library's callers can only import exported packages |
| Dockerfile, image publish on tags | not applicable | nothing to run |
| `config.example.yaml` | replaced by a documented example of the agent configuration, in `docs/` | configuration is data a program passes in, not a file bonyan reads |
| `gofmt` in CI | `gofumpt` plus `goimports -local github.com/yaad-index/bonyan`, also as golangci-lint formatters | house rule |
| Dev tools installed on the machine | **every** dev tool tracked with `go get -tool` in a separate `tools/go.mod` and run as `go tool -modfile=tools/go.mod <tool>` | house rule ("always use go tool"); a separate module keeps tool dependencies out of `go.mod`, which a library's callers resolve |
| CI runs individual steps | CI runs `make check`, the same target a contributor runs | one command, identical locally and in CI |

No tool is exempt. golangci-lint was built and run through `tools/go.mod` at the pinned version (v2.11.4) with the main `go.mod` unchanged, including after `go mod tidy`. Its maintainers do not officially support source builds, so the version stays pinned to the one used elsewhere.

A dependency that is load-bearing for the public API is recorded in an ADR, per the same conventions (phase 1).

## 2. Build, not wrap

bonyan owns the agent loop and context assembly itself, on small libraries. It does not wrap an existing Go agent framework.

The invariants in ADR 0001 are enforced in exactly the places an agent framework owns: the loop that assembles context, calls the model and runs tools. Three existing frameworks were assessed against the invariants, and in each:

- messages inside the framework's loop are plain strings, so an untrusted-content type cannot survive the round trip;
- user-supplied callbacks or plugins can bypass the framework's own hooks, so a guarantee applied there can be switched off;
- trace spans carry message or tool content by default, or through a switch outside bonyan's configuration;
- none provides record and replay, a pre-call budget bound, secret scoping, per-section trimming, a classifier model kind or an eval runner.

Pieces of those ecosystems that sit behind one of bonyan's interfaces, such as a provider adapter kept in a separate module outside the core, remain possible later. They are not part of this plan. The detailed comparison is kept with the maintainers' notes rather than in the repository.

## 3. Libraries per slot

| Slot (ADR section) | Decision | Module | Version checked | Notes |
|---|---|---|---|---|
| Chat-completions-compatible adapter (§1) | **write a thin client** on `net/http` | — | — | The wire subset needed (messages, tools, response schema, max output tokens, usage) is small. Recording (§8) and "missing usage is an error" (§11) both need the raw response anyway. The most widely used SDK for this wire format (checked at v3.66.0) reads the base URL and API key from environment variables by default, and retries twice by default. Both can be switched off by options, so neither is disqualifying on its own, but both would have to be switched off deliberately on every construction to keep credentials on the secret resolver (§10) and retries visible to the loop. |
| Tool-server protocol client (§5) | **use**, behind the tool registry | `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | v1 with a written compatibility promise; stdio and streamable HTTP transports. Already the house choice. |
| JSON Schema from Go types, and validation of structured output (§5, §6) | **use**, pinned | `github.com/google/jsonschema-go` | v0.4.3 | Generates and validates (drafts 2020-12 and 07); the protocol client above already depends on it. Known gaps to test around: `format` is not enforced, `omitempty` fields are left out of `required`, slices generate as null-or-array. `github.com/santhosh-tekuri/jsonschema/v6` is the replacement validator if format checks become necessary. |
| Tracing and metrics (§9) | **use** | `go.opentelemetry.io/otel` | v1.46.0 | Core packages import the API only; the SDK and exporters are wired by the program. |
| GenAI attribute names (§9) | **use, pinned, behind one package** | `go.opentelemetry.io/otel/semconv/v1.41.0` | (in otel v1.46.0) | `v1.41.0` carries the `gen_ai.*` keys (58 distinct names, marked "Development" stability). `v1.42.0` and later removed them because the GenAI conventions moved to a separate specification, not yet released for Go. So every GenAI name lives in one bonyan package, pinned to `v1.41.0`, and names added upstream later are defined there by bonyan until an upstream Go package exists. |
| Basic memory store (§4) | **use a pure-Go SQLite driver** (proposed; decided in the phase 1 ADR) | `modernc.org/sqlite` | v1.60.0 | Delete-by-subject and retention are single SQL statements, and FTS5 gives keyword recall with no external service. It builds with cgo off. The house embedded store (`go.etcd.io/bbolt`, v1.5.0) would mean writing subject, time and text indexes by hand. This is the one place the plan leaves the house set, which is why it goes to an ADR. |
| Token counting (§11) | **use** for tiktoken-compatible encodings, in an **optional** package; everything else uses the byte bound | `github.com/tiktoken-go/tokenizer` | v0.8.1 | Vocabularies are compiled in, with no network fetch at runtime. That makes the package large (its codec sources total about 16 MB), so it lives in `tokenize/tiktoken`, imported only by programs that want exact counts; core relies on the byte bound. The encoding is named in configuration, never guessed from a model name, because a wrong guess can undercount. |
| Rate limiting | **use** | `golang.org/x/time/rate` | — | For per-endpoint limits in adapters. |
| Retries | **write** | — | — | Every retry must be recorded, counted against the budget and end in a typed error, which general-purpose retry libraries do not do. |
| Tests | **use** | `github.com/stretchr/testify` | — | House choice. |

## 4. Package layout

Exported packages, one concern each, with interfaces where the ADR names a pluggable slot:

| Package | Holds |
|---|---|
| `content` | the trusted and untrusted text types. Untrusted text cannot be turned into trusted text outside bonyan (ADR 0001 §3). |
| `model` | the model interfaces per kind (chat, embedding, classifier), request, response, usage, typed errors |
| `model/chatcompat` | the chat-completions-compatible HTTP adapter |
| `record` | the recording format, the recorder slot and the replay model (§8) |
| `agent` | the loop: limits, the not-cleared result, inline loop detection, tool dispatch |
| `assemble` | the ordered context pipeline, per-section budgets and recorded trimming |
| `tool` and `tool/mcpclient` | the tool registry, schemas and the protocol client adapter |
| `memory`, `memory/memorytest`, `memory/sqlite` | the interface, the backend conformance suite (§4) and the basic backend |
| `secret` | sources, the scoped resolver handed to tools, and the scrubber |
| `approval` | the approval hook and the durable pending-action store |
| `budget`, `tokenize` and `tokenize/tiktoken` | the price table, the pre-call bound, the byte-bound counter, and the optional exact counter |
| `eval` | the runner, deterministic and model-based evaluators, the asynchronous hand-off and re-runs |
| `telemetry` | span and metric emission, redaction, and the one place GenAI names are defined |
| `registry` | named implementations per slot, and assembling an agent from configuration (the invariants wrap the slots here) |

The invariants are applied in `agent`, `assemble` and `registry`, around the slots, never inside a slot implementation.

## 5. Phases

Each phase is one pull request, unless it says otherwise. Every phase lands with the tests named in it. A phase that adds a load-bearing dependency adds it in that PR, never earlier.

### Phase 0: tooling alignment
`tools/go.mod` with gofumpt, goimports and golangci-lint. The Makefile targets `fmt`, `fmt-check`, `lint`, `vet`, `test`, `build`, `tidy-check`, `githook-check`, `check` and an opt-in `install-hooks`. CI runs `make check`. `.golangci.yml` gets gofumpt and goimports as formatters, plus the house misspell ignore rule. `AGENTS.md` maps the repository for contributors. The release configuration is compared against the house template and aligned where it differs.
*Done when:* `make check` passes locally and in CI, and a deliberately misformatted file fails `fmt-check`.

### Phase 1: stack-and-conventions ADR (maintainer approval required)
`adr/0000` (why ADRs) and an ADR recording the stack: the conventions and deviations in §1, and the dependencies in §3 that are load-bearing for the public API, including the SQLite driver over bbolt. It also decides **where the heavier dependencies live**: the SQLite driver and the optional tokenizer are needed only by the packages that use them, but a dependency in the root `go.mod` is part of the module graph of every caller. The ADR decides between keeping them in the root module and giving `memory/sqlite` and `tokenize/tiktoken` their own nested modules, and records why. The same reasoning is what put the dev tools in `tools/go.mod` (§1). Docs only.

### Phase 2: core types
`content`, `model` interfaces per kind, typed errors, the not-cleared result, and limit types where zero is invalid. No third-party dependencies.
*Tests:* untrusted cannot be converted to trusted from outside the package (a compile-time check in an external test package); a zero or negative limit fails validation; not-cleared is distinct from every error.

### Phase 3: registry and assembly from configuration
Named implementations per slot, and an agent assembled from configuration. The invariant wrappers are present as pass-through points that the following phases fill.
*Tests:* an unknown implementation name fails assembly; a program-registered implementation is selectable by name; a slot cannot be assembled without its wrapper.

### Phase 4: secrets
Sources (environment, file), the scoped resolver a tool receives, and exact-match scrubbing, re-readable without a restart.
*Tests:* a tool cannot resolve a name outside its grant; a resolved value is scrubbed from tool output and from log output; rotation is picked up.

### Phase 5: budget and token counting
The price table, the pre-call bound (input counted by an encoding or by bytes, plus the required max-output cap), charging from reported usage, and missing usage as an error. The optional exact counter, and its dependency, is added in `tokenize/tiktoken` only.
*Tests:* a call whose bound crosses the ceiling is refused before it is sent; multi-byte text is not undercounted by the byte bound; missing usage fails a budgeted run.

### Phase 6: recording and replay
The stable recording format, the recorder slot (memory sections excluded unless full recordings are enabled; files owner-only; default path outside the working tree) and the replay model.
*Tests:* record then replay reproduces the same responses; memory sections are absent by default.

### Phase 7: the chat-completions-compatible adapter
The thin HTTP client: messages, tool calls, response schema, max output tokens, usage, and recorded retries under the budget. Adds the rate-limit dependency.
*Tests:* against a local test server only: tool-call round trip, schema request, missing usage surfaced as an error, a retry recorded and charged.

### Phase 8: the agent loop
Context → model → tools → stop, with step, budget and deadline limits, inline loop detection with a configurable threshold, and the not-cleared result for gate agents. Built and tested on the replay model.
*Tests:* every non-answer listed in ADR 0001 §7 yields not-cleared; the step limit ends a looping run even with loop detection off.

### Phase 8a: hook points
The payload of each hook point, the change and deny results, and the loop acting on them (ADR 0001 §12). A denied tool call is reported to the model as denied; a denied model call, message or reply ends the run with a typed error, which for a gate agent is not-cleared. The change and deny interfaces land together with the loop's handling of them, so no configured hook can ask for a change the loop ignores.
*Tests:* each point receives its payload as typed values after secret scrubbing; a hook given untrusted content can return only untrusted content; with several hooks at a point, any denial wins; a failing hook that may change or deny counts as a denial, and an observing hook's failure is recorded and changes nothing.

### Phase 9: context assembly
The ordered pipeline with per-section token budgets, deterministic recorded trimming, and the untrusted section wrapper.
*Tests:* trimming is identical across runs and says what was dropped; untrusted material only ever appears inside the marked section.

### Phase 9a: the trust policy in the run
Classification through the configured policy where content enters the run (the input and each tool result), every decision recorded with the policy's name, the policy's marking of untrusted sections, and the enforcement point as the last step before every model request (ADR 0001 §3). The loop's refusal of a policy other than the default is lifted here. Handling in this phase can drop an item or refuse the call. Handling that requires approval uses phase 14's hook once it exists; recalled memory is classified through the same policy in phase 12; sending the call to a different configured model is the later item below.
*Tests:* a policy declaring a source trusted changes the value's type and the decision is recorded; a failing policy leaves content untrusted and marked as the default policy marks it; a request with untrusted content outside a marked section is refused at the enforcement point; a hook or pipeline stage cannot send a request around it.

### Later: routing as trust handling
A trust policy's handling may send a call carrying untrusted content to a different configured model (ADR 0001 §3). It needs a way to name the models a policy may route to, and it comes after phase 9a.

### Phase 10: tools and structured output
The registry with schemas generated from Go types, per-tool secret grants, and structured-output validation with bounded retries. Adds the JSON Schema dependency.
*Tests:* an invalid structured response is retried with the validation error and then fails typed; the known generator gaps (§3) have a test each.

### Phase 11: the tool-server protocol client
The adapter registering remote tools like in-process ones, from a tool list read once at registration, under a prefix the program chooses. Adds the protocol SDK. The tool registry gains registration from a JSON Schema, with the same argument validation and secret scoping as a Go-typed tool. Results, error results included, are remote tool output (ADR 0001 §3), a source kind only this adapter sets. Of the text a server writes into a tool's definition, only what a call needs to be valid is kept by default, at every level: the schema keywords validation uses, the tool's name, property names, enum and const values, patterns, formats, referenced definition names, and the identifiers and anchors a reference resolves through. Keeping the rest is a per-server opt-in (ADR 0001 §5).
*Tests:* against the SDK's in-memory server: list, call, error result; remote tool output enters context as untrusted, under its own source kind; a keyword validation does not use, an invented one included, is absent at every level and kept with the opt-in; invalid arguments are refused before the request is sent; a clashing name fails registration.

### Later: trust per tool server
A policy that trusts one tool server and not another. A policy sees the source kind, not the server, so it needs a way to name servers to a policy, and it comes after phase 11.

### Phase 12: memory interface and conformance suite
Short-term and long-term layers, subject and source on every record, recall re-marking by source, retention, and delete by subject. A conformance suite every backend must pass.
*Tests:* the suite itself, run against an in-memory reference backend.

### Phase 12a: memory in the run
Recall into the context and writing to memory from the run, the memory write and recall hook points with the rights at each stated in ADR 0001 (an amendment, maintainer approval required), and deletion by subject removing a subject's full recordings.
*Tests:* recalled memory enters the run inside the marked section unless both decisions trusted it; each memory hook point receives its payload and can do only what its rights allow; deletion by subject leaves no full recording of the subject.

### Phase 13: the basic memory backend
The SQLite-backed implementation, passing the conformance suite. Adds the driver. Depends on phase 1's decision.

### Phase 14: approvals
The approval hook, the durable pending-action store, restart cancelling pending actions when no durable store is configured, and late approvals for actions no longer held rejected as unknown.
*Tests:* each of those behaviours, including a restart.

### Later: 14b, approvals that survive a restart
A durable approval store, and resuming a run whose action was approved after a restart. Resuming needs the run's state (its messages, the budget spent, the loop's counts) stored beside the pending action. That state can hold untrusted content and recalled memory, so before it is built: which parts are stored, whether memory sections are excluded as in recordings, and how deletion by subject and retention reach the stored state. It comes after phase 14.

### Later: a decision per approver
A pending action is decided once, through the approval store, and that decision answers for every approver that said pending. Letting each pending approver decide on its own, with the action running only when none of them rejects, needs the store to hold a decision per approver. It comes after phase 14.

### Later: the model's replies in session history
Storing the model's replies as session events needs a source kind for model output, which ADR 0001 §3 does not name; adding one is an amendment (maintainer approval required). Until then phase 12a stores the user's messages only, and history the run reads stays program-supplied.

### Phase 15: telemetry
Spans and metrics per loop step, model call and tool call, using the pinned GenAI names from one package. Content capture is off by default and never includes memory sections.
*Tests:* no message content appears in spans by default; memory never appears even with capture on; attribute names match the pinned conventions.

### Phase 16: evaluation, deterministic
The eval runner, cases, reports, and the deterministic evaluators (loops, most of waste, task outcome). Evaluators can be switched on or off per agent and per run.
*Tests:* a case set produces per-case results and aggregates; a recorded run with a repeated identical tool call is reported as a loop; a disabled evaluator produces no score and switching it off leaves the step and budget limits in force.

### Phase 17: evaluation, model-based and asynchronous
The groundedness and unused-context evaluators under the invariants, with their own budget, and agreement with hand labels reported beside the score. "Unverifiable" when the material is not in the recording. The opt-in asynchronous hand-off, where a queued item is a recording.
*Tests:* a claim resting on memory absent from the recording is reported unverifiable, not unsupported; an evaluator call is charged to its own budget, not the evaluated run's; an answer containing an instruction aimed at the judge stays inside the untrusted section; live evaluation is off unless enabled; delete by subject removes queued items.

### Phase 18: re-runs
Repeated and parameter-grid re-runs of a recorded input. Tools are answered from the recording by default, and the grid is limited to configured models.
*Tests:* a re-run executes no live tool unless that tool is opted in; a tool call with no recorded match fails that cell instead of executing; an action needing approval is not auto-approved; a grid naming an unconfigured model is refused unless the operator names it explicitly.

## 6. Order and parallelism

Phases 0 and 1 come first, in that order. Phases 2 and 3 are the foundation for everything else. After them, 4 to 7 can proceed in parallel; 8 needs 5 and 6; 9 needs 8; 8a needs 8, and 9a needs 9; 10 and 11 need 8; 12 is independent of the loop, and 13 needs 12 and the phase 1 decision; 14 needs 8; 15 can start once 8 exists; 16 to 18 come last. No program migrates to bonyan within this plan.

## 7. Open points

- **Storage driver:** SQLite versus the house embedded store is decided in the phase 1 ADR, not here.
- **GenAI names:** the upstream Go package for the separated GenAI conventions does not exist yet. Revisit the pin when it does.
- **Tokenizers beyond tiktoken-compatible encodings:** no mature pure-Go library was found. The byte bound covers them until one exists.
