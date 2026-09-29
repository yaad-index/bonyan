# ADR 0001: The shape of bonyan, a library for building agents

**Status:** Proposed

## Context

Several programs built on language models tend to re-implement the same parts, each slightly
differently: a call to a model provider, prompt and context assembly, retries and timeouts,
memory about users, tool calls, secret handling, logging and cost control. Examples of the kind
of program in mind: a chat bot answering from a corpus of source material, a news digest that
summarises and ranks feed items, and a gate that judges whether an outgoing message is safe to send.

Re-implementing these parts in each program has predictable costs. Fixes do not travel between
programs; each one invents its own memory and its own way of treating fetched text; each one's
tests assert what is *sent* to a model and never what the model *writes*; and none of them can be
measured against a stable set of real cases before a prompt or model change.

bonyan is a Go **library** (no binary of its own) that owns those parts once, so a program built on
it describes *what its agent does* and inherits *how an agent runs*.

## Decision

bonyan is organised as small packages with one interface each. A program uses what it needs; no
package requires another beyond what is stated here.

**Everything is pluggable and switchable by configuration.** Not only what touches a vendor, a store
or the network: models, memory backends, context-pipeline stages, tools, secret sources, the approval
store, recorders, eval scorers and telemetry exporters each sit behind an interface, and every
implementation registers under a name. An agent is assembled from configuration that names the
implementation for each slot, so swapping one (another model provider, another memory backend,
another secret store) is a configuration change, not a code change. Each slot ships at least one
in-repo implementation, and a program can register its own under a new name without forking bonyan.

**Where pluggability stops: the invariants.** The guarantees in this ADR are applied by bonyan
*around* the slots, never by an implementation inside one, so no configuration and no registered
implementation can switch them off. They are: the untrusted-content type (trusted text cannot be
constructed from it outside bonyan, §3), re-marking of recalled facts by source (§4), secret scoping
and scrubbing (§10), exclusion of memory before anything reaches an exporter or recorder, except operator-enabled full
recordings (§4, §9),
the distinct not-cleared result (§7), and the loop's limits (§2, §11). A custom pipeline stage,
exporter or recorder receives only what has already passed through them.

### 1. Model: one interface for different kinds of model

- A `Model` takes a request (messages, tool definitions, an optional output schema, limits) and
  returns a response (content, tool calls, token usage, stop reason).
- **Kinds are first-class, not squeezed into chat:** chat/completion models, embedding models, and
  **classifiers** (text in, labels with confidences out, no prompt). A classifier is not given a chat
  interface it cannot honour.
- Providers are adapters behind the interface: a chat-completions-compatible HTTP adapter (which
  also covers most local servers), plus others as needed. No provider is a dependency of the core packages.
- Routing and fallback are explicit configuration: a primary model, an ordered fallback list, and
  per-call timeouts. A failed call returns a typed error; it is never silently answered by a
  different model without that being recorded.

### 2. The agent loop

- One loop: assemble context → call the model → if the model requests tools, run them and feed
  results back → stop on a final answer, a step limit, a budget limit, or a cancelled context.
- Step limits, budget limits and a deadline always apply. Each has a default; zero or negative is
  invalid, never "unlimited", so a limit cannot be switched off by omission.
- The loop is a plain function over interfaces, so it runs identically in a test with a recorded
  model (§8) and in production.

### 3. Context assembly

Context for each call is built by an explicit, ordered pipeline, not string concatenation spread
through the program:

1. the system instructions (versioned, §10),
2. memory recalled for this user and task (§4),
3. retrieved or fetched material,
4. conversation history (trimmed to budget),
5. tool definitions.

- **A token budget is allocated per section**, and trimming is deterministic and recorded, so it is
  always possible to say what was dropped from a call and why.
- **Fetched content is data, never instructions (§7).** Anything that did not come from the program
  or the operator (mail, web pages, feed items, user uploads, tool output) is a **distinct type** that
  the instruction builder does not accept, so it can only enter context inside a delimited,
  marked section. The mechanism is structural: the pipeline, not each program, owns the boundary.
  **What this guarantees is placement, not behaviour:** a model may still act on instructions inside
  marked data, which is why §7's approval hook and fail-closed mode exist.

### 4. Memory: part of the framework, pluggable

- Two layers behind one interface:
  - **Short-term:** per-session history, stored as events.
  - **Long-term:** facts about a user or subject, extracted from sessions and recalled into context.
- **A basic implementation ships in bonyan** (a local store with simple extraction and recall), so
  memory works with no external service.
- **External backends plug into the same interface** (for example a dedicated user-memory service
  run as a separate process). bonyan calls such a service over its API and does not vendor it, which
  also keeps bonyan's licence independent of the backend's.
- Long-term extraction may run asynchronously, and the interface says so: a fact from this turn is
  not promised to be recallable on the next one.
- **Recall re-applies trust by source.** Every record carries its source. A fact extracted from
  untrusted material is recalled as untrusted (§3) and enters context inside the marked section, not
  as trusted memory. This is a rule of the memory interface, not left to each backend, so a fact
  planted by a fetched item cannot come back later as trusted context.
- **Privacy is part of the interface, not an afterthought:** every stored record carries its subject
  and its source; a retention period is configurable; deletion by subject is **required to
  implement** for every backend (bonyan can require and call it; for an external backend it cannot
  verify the deletion happened). A **conformance test suite** in bonyan that every backend must pass (write,
  recall, delete by subject, assert absent, including extracted facts) proves a backend *implements*
  deletion; it does not prove that a given deletion on a live external store happened.
- **Memory in logs, traces and recordings:** by default memory contents never appear in bonyan's own
  logs or traces (§9; a program's own logging is outside bonyan's reach), and recordings (§8) exclude recalled-memory sections. **Trace content capture (§9) always
  excludes memory sections**, because exported spans leave for a trace backend bonyan cannot delete
  from. If an operator enables full recordings, they are local files **indexed by subject**, under the
  same retention rules, and deletion by subject removes them.

### 5. Tools

- A tool registry with typed inputs and outputs (JSON Schema derived from Go types).
- An **MCP client**, so tools served over the Model Context Protocol are registered the same way as
  in-process tools.
- Each tool declares what it may touch: secrets by name, network, filesystem. **Only secrets are
  enforced**, because bonyan is what resolves them (§10). Network and filesystem declarations are
  metadata for review and approval, not a sandbox: in-process Go code can open a socket or a file
  without asking, and a remote tool server reports its own capabilities. Real isolation needs a
  separate process, and that is the program's choice.

### 6. Structured output

- A call may require a schema. The response is validated; an invalid response is retried with the
  validation error, a bounded number of times, then fails with a typed error.
- This is the path a judging or classifying agent uses: it gets a verdict value, not prose to parse.

### 7. Safety boundary

- Untrusted material (§3) is marked, delimited and never concatenated into instruction positions.
- A **human-in-the-loop hook**: an agent can mark an action as needing approval; the loop suspends
  it, emits it to a configured approver, and resumes or cancels on the decision. Suspended actions are
  kept in a pluggable store so they survive a restart; with no durable store configured, a restart
  cancels every pending action (fail-closed), it never drops one silently. An approval that arrives for an action
  the loop no longer holds (cancelled by a restart or a timeout) is rejected as unknown, never applied.
- **Fail-closed mode** for gate-type agents: every non-answer is "not cleared", never "cleared by
  default". **"Not cleared" is its own result value, distinct from an error**, so a caller that handles
  errors and verdicts separately cannot read a failure as a pass by forgetting a branch. Non-answers include a failed model or classifier call, structured output still invalid
  after retries (§6), a step or budget limit reached, cancellation or deadline, the fallback list
  exhausted, a detected loop (§8), and an approver who does not answer (the approval hook's timeout means cancel).

### 8. Evaluation and replay

- Every model call can be **recorded** (request, response, usage) to a local file in a stable format.
- A **replay model** serves recorded responses, so a test runs the real loop without a provider.
- An **eval runner** scores an agent against a set of cases (inputs plus expected properties) and
  reports per-case results and aggregates. A prompt, model or threshold change is meant to be judged
  by this report before it ships.
- **Evaluators judge behaviour, not just record it.** Pluggable evaluators run over recordings
  offline and, sampled, over live runs, and report as metrics (§9). **Every evaluator, inline loop detection
  included, is switched on or off by the caller**, per agent or per run; switching one off never
  switches off the step and budget limits, which are invariants. The initial set:
  - **groundedness / hallucination:** claims in an answer checked against the material that was in
    context (sources, tool results, recalled memory); unsupported claims and invented citations are
    counted. When the material is not in the recording (default recordings exclude memory, §4), a
    claim that may rest on it is reported as **unverifiable**, not unsupported;
  - **loops:** repeated tool calls with the same arguments, repeated states, and runs that hit the
    step limit;
  - **waste:** tokens and cost per completed task, redundant or failed calls, retries, and context
    sent but never used (the last is an attribution judgement, so model-based);
  - **task outcome:** did the run reach a valid final answer, and does it meet the case's expected
    properties.
- Some evaluators are deterministic (loops, most of waste); some need a model (groundedness, unused
  context). A model-based
  evaluator is itself a measurement with error, so its agreement with hand labels on a sample is
  reported beside its score, and it never runs as a gate on its own. A model-based evaluator is itself a model
  call reading untrusted context and answers (an answer claiming "this is grounded" is an injection
  aimed at the judge), so it runs inside the same invariants, with its own budget. **Sampled live
  evaluation that sends user context to an evaluator model is opt-in and off by default.**
- Loop detection also runs inline in the agent loop (§2), with a configurable threshold (polling a
  tool repeats legitimately): a detected loop ends the run with a typed error rather than burning the
  step budget, and for a gate-type agent it is one more "not cleared" case (§7).
- Recordings of real traffic may contain private data. A library cannot stop a file being committed,
  so the defaults are what it can control: the recording path defaults outside the working tree, files
  are written with owner-only permissions, and memory sections are excluded unless enabled (§4). Test
  fixtures in the repo are synthetic.

### 9. Observability

- OpenTelemetry built in: a span per loop step, model call and tool call, with model, tokens,
  latency and cost as attributes; metrics for the same.
- **Redaction at the boundary:** span attributes carry sizes and identifiers, not message content,
  memory contents or secrets. Content capture is an explicit, off-by-default option.

### 10. Configuration, prompts and secrets

- **Configuration is data per instance**, not code: which models, which memory backend, budgets,
  thresholds. A program's defaults describe the generic case; an operator's instance is config.
- **Prompts are versioned assets** with an identifier recorded on every call, so a trace or a
  recording says which prompt version produced it.
- **Secrets:**
  - resolved from pluggable sources (environment, files, and external secret stores through
    adapters);
  - resolved by tools at call time and never placed into context by bonyan itself;
  - **scoped by capability on bonyan's resolver:** each tool is handed a resolver that can only reach
    the names its configuration grants, and it has no other path to secrets through bonyan. In-process code can still read the environment
    directly; the scoping covers what goes through bonyan, not the process;
  - **scrubbed:** every value resolved through bonyan is removed, by exact match, from tool output
    before it enters context, and from logs, traces and recordings. Exact match is the limit: an
    encoded or partial secret passes the scrubber;
  - re-readable without a restart, so rotation does not need a redeploy.

### 11. Budget and cost

- Every run has a token and cost ceiling (from configuration, with a default). Usage is known only
  after a call returns, so two mechanisms combine:
  - **a pre-call bound:** every call in a budgeted run must carry a max-output-tokens cap (required,
    not optional). The loop counts the input with the model adapter's tokenizer, or, when the adapter has
    none, a safe upper bound of one token per UTF-8 byte plus a fixed per-message allowance for
    chat-template framing (per-character bounds undercount non-ASCII text), and refuses a call whose bound (input +
    max output) × price would cross the remaining budget. This is a bound, not a charge.
  - **charging from reported usage:** what is spent is taken from the provider's reported usage after
    the call, never estimated. Crossing the ceiling ends the run with a typed error, recorded in the
    trace.
- **Missing usage is an error in a budgeted run**, never counted as zero: some local servers report
  none, and treating that as zero would leave the budget silently unenforced.
- Cost charged to a run is computed from reported provider usage and a price table in
  configuration. The pre-call bound above is the only estimate, and it only ever refuses calls; it
  never stands in for a charge.

## Consequences

- Programs built on bonyan stop owning these concerns and gain them together; the cost is a shared
  dependency whose interfaces must stay stable, so interface changes follow semantic versioning (before 1.0 a breaking change bumps the minor
  version, per the release config) and get their own ADR.
- The first releases will ship interfaces and the basic implementations before any program migrates.
  Each migration is its own piece of work in that program's repository.
- The library stays generic: no instance's topics, names, providers or thresholds appear in code,
  tests or docs. Examples and fixtures are synthetic.

## Out of scope for this ADR

- Package-level API details (method signatures), which follow in the implementation PRs.
- Which external memory backend or secret store adapters are written first.
- Hosting: bonyan is a library; where an agent runs is the program's decision.
