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
package requires another beyond what is stated here. Everything that touches a vendor, a store or
the network sits behind an interface with at least one in-repo implementation.

### 1. Model: one interface for different kinds of model

- A `Model` takes a request (messages, tool definitions, an optional output schema, limits) and
  returns a response (content, tool calls, token usage, stop reason).
- **Kinds are first-class, not squeezed into chat:** chat/completion models, embedding models, and
  **classifiers** (text in, labels with confidences out, no prompt). A classifier is not given a chat
  interface it cannot honour.
- Providers are adapters behind the interface: an OpenAI-compatible HTTP adapter (which also covers
  most local servers), plus others as needed. No provider is a dependency of the core packages.
- Routing and fallback are explicit configuration: a primary model, an ordered fallback list, and
  per-call timeouts. A failed call returns a typed error; it is never silently answered by a
  different model without that being recorded.

### 2. The agent loop

- One loop: assemble context → call the model → if the model requests tools, run them and feed
  results back → stop on a final answer, a step limit, a budget limit, or a cancelled context.
- Step limits, budget limits and cancellation are mandatory parameters with defaults, never optional
  behaviours a program can forget to add.
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
  or the operator (mail, web pages, feed items, user uploads, tool output) enters context only
  through a wrapper that marks it as untrusted material. The pipeline, not each program, owns that
  boundary.

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
- **Privacy is part of the interface, not an afterthought:** every stored record carries its subject
  and its source; deletion by subject is required of every backend; a retention period is
  configurable; and memory contents never appear in logs or traces (§9).

### 5. Tools

- A tool registry with typed inputs and outputs (JSON Schema derived from Go types).
- An **MCP client**, so tools served over the Model Context Protocol are registered the same way as
  in-process tools.
- Each tool declares what it may touch (network, secrets by name, filesystem), and the loop refuses a
  call outside that declaration.

### 6. Structured output

- A call may require a schema. The response is validated; an invalid response is retried with the
  validation error, a bounded number of times, then fails with a typed error.
- This is the path a judging or classifying agent uses: it gets a verdict value, not prose to parse.

### 7. Safety boundary

- Untrusted material (§3) is marked, delimited and never concatenated into instruction positions.
- A **human-in-the-loop hook**: an agent can mark an action as needing approval; the loop suspends
  it, emits it to a configured approver, and resumes or cancels on the decision.
- **Fail-closed mode** for gate-type agents: when a model or classifier call fails, the result is
  "not cleared", never "cleared by default".

### 8. Evaluation and replay

- Every model call can be **recorded** (request, response, usage) to a local file in a stable format.
- A **replay model** serves recorded responses, so a test runs the real loop without a provider.
- An **eval runner** scores an agent against a set of cases (inputs plus expected properties) and
  reports per-case results and aggregates. A prompt, model or threshold change is meant to be judged
  by this report before it ships.
- Recordings of real traffic may contain private data; the tooling defaults to keeping them local
  and never committed, and test fixtures in the repo are synthetic.

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
  - **resolved by tools at call time, never placed in model context, memory, logs or traces**;
  - scoped: an agent's configuration names which secrets each tool may use, and nothing else is
    readable by it;
  - re-readable without a restart, so rotation does not need a redeploy.

### 11. Budget and cost

- Every run has a token and cost ceiling (from configuration, with a default). Crossing it ends the
  run with a typed error, recorded in the trace.
- Cost is computed from provider usage and a price table in configuration, never estimated after the
  fact.

## Consequences

- Programs built on bonyan stop owning these concerns and gain them together; the cost is a shared
  dependency whose interfaces must stay stable, so interface changes follow semantic versioning and
  get their own ADR.
- The first releases will ship interfaces and the basic implementations before any program migrates.
  Each migration is its own piece of work in that program's repository.
- The library stays generic: no instance's topics, names, providers or thresholds appear in code,
  tests or docs. Examples and fixtures are synthetic.

## Out of scope for this ADR

- Package-level API details (method signatures), which follow in the implementation PRs.
- Which external memory backend or secret store adapters are written first.
- Hosting: bonyan is a library; where an agent runs is the program's decision.
