# ADR 0001: The shape of bonyan, a library for building agent harnesses

**Status:** Accepted, amended (maintainer sign-off recorded by approval of the PR that sets this status and of each PR that amends it, per ADR 0000; see Amendments)

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

bonyan is a Go **library** (no binary of its own) for building a complete agent **harness** with
minimal code: the loop, model calls, context, memory, tools, approvals, the trust boundary, evaluation
and telemetry that surround a model, each of them pluggable. It owns those parts once, so a program
built on it describes *what its agent does* and inherits *how an agent runs*.

bonyan has **no user interface**. A front-end (a chat window, a messaging bot, a command line, a web
page) and a policy (what may be said, which actions need a person) are separate code that attaches
to a run through hooks (§12). bonyan provides the hooks, not the front-ends.

## Decision

bonyan is organised as small packages with one interface each. A program uses what it needs; no
package requires another beyond what is stated here.

**Everything is pluggable and switchable by configuration.** Not only what touches a vendor, a store
or the network: models, memory backends, context-pipeline stages, tools, secret sources, the trust policy (§3),
hooks (§12), the approval store, recorders, eval scorers and telemetry exporters each sit behind an
interface, and every
implementation registers under a name. An agent is assembled from configuration that names the
implementation for each slot, so swapping one (another model provider, another memory backend,
another secret store) is a configuration change, not a code change. Each slot ships at least one
in-repo implementation, and a program can register its own under a new name without forking bonyan.

**Where pluggability stops: the invariants.** The guarantees below are applied by bonyan *around*
the slots and hooks, never by an implementation inside one, so no configuration, no registered
implementation and no hook can switch them off. A custom pipeline stage or hook receives content
already classified by the trust policy and already scrubbed of secrets, and the trust enforcement
point runs after all of them, so nothing they produce reaches a model without passing it. An
exporter or recorder receives only what has passed every guarantee. Each is fixed for a stated
reason:

- **The trust enforcement point (§3).** Every request passes one point inside bonyan, after every
  pipeline stage and hook, that applies the configured trust policy. *Why:* a policy that some path
  can skip is advice, and the placement guarantee holds only if every request crosses the same point.
- **No conversion from untrusted to trusted (§3, §4, §12).** Once content is classified untrusted,
  no later stage, hook, policy step or memory round trip turns it into trusted content through
  bonyan's API. *Why:* if any plugin could do it, every plugin would be a way to launder fetched text
  into instructions.
- **Untrusted content is delimited and never in an instruction position (§3, §7).** The trust policy
  chooses how it is marked, not whether. *Why:* this placement is the one guarantee the boundary
  gives at the model; without it the untrusted type protects nothing once the request is built.
  Where this line sits, and what moving it would cost, is set out in §3.
- **Provenance and the policy's decisions are recorded (§3, §4).** *Why:* a pluggable policy can be
  wrong, and a decision that leaves no record cannot be audited or corrected.
- **Secret scoping and scrubbing (§10)**, applied before content reaches context, a hook, a log, a
  trace or a recording. *Why:* a leaked secret cannot be taken back.
- **Memory is excluded before anything reaches bonyan's exporters or recorders**, except
  operator-enabled full recordings (§4, §9). *Why:* exported data lands where bonyan cannot delete by
  subject.
- **The distinct not-cleared result (§7).** *Why:* a gate that can read a failure as a pass fails
  open.
- **What is approved is what runs (§7, §12).** *Why:* otherwise an approval covers a different
  action from the one executed.
- **The loop's limits (§2, §11).** *Why:* a limit a plugin can switch off is not a limit.

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
- **Untrusted content is data, never instructions (§7).** Untrusted content is a **distinct type**
  that the instruction builder does not accept, so it can only enter context inside a delimited,
  marked section. The mechanism is structural: bonyan, not each program, owns the boundary.
  **What this guarantees is placement, not behaviour:** a model may still act on instructions inside
  marked data, which is why §7's approval hook and fail-closed mode exist.

**Trust is a pluggable policy, enforced by bonyan.** What counts as untrusted, how it is marked and
how it is handled are decided by a **trust policy**, a slot like any other. The point where the policy
is applied is not a slot.

- **The policy decides three things:**
  - **classification:** which content is untrusted, by its source. The sources bonyan names are
    program and operator instructions, user messages, uploads, fetched material, tool output, remote
    tool output (returned by a tool served over the Model Context Protocol, §5), model output read
    back into a request (§4, §8) and recalled memory;
    a program can register further source kinds, and a policy can classify by any of them. Remote
    tool output is a source of its own so that a policy which trusts the program's own tools does
    not trust every tool server along with them. Model output is a source of its own so that a
    model's reply read back into a request is never taken for the user's message. Elsewhere in this ADR, "tool output" and "tool
    result" include it;
  - **marking:** how untrusted content is delimited and presented inside its section (the delimiter
    format, labels, an encoding of the text);
  - **handling:** what else happens when untrusted content is present, for example requiring
    approval (§7) for tool calls in a run that has read untrusted content, refusing content over a
    size, dropping a source, or sending the call to a different configured model.
- **Classification happens once, where content enters bonyan**: a user message through the run's
  API, a fetched item through the pipeline, a tool result as it returns, a fact as it is recalled.
  The policy returns a decision; bonyan builds the typed value from it. A policy never constructs a
  trusted value itself.
- **The enforcement point** is the last step before a request reaches a model adapter, after every
  pipeline stage and hook. Every request passes it, and it applies the policy's marking and handling
  and checks placement: untrusted content only inside marked sections. No configuration, stage, hook
  or registered implementation can send a request around it.
- **A policy failure fails closed.** A policy that errors, panics, exceeds the run's deadline or
  returns no decision leaves the content untrusted, marked as the default policy marks it.
- **Every decision is recorded** in the trace and the recording: the source kind, the decision and
  the policy's name, never the content (§9).
- **The default policy** classifies everything that did not come from the program or the operator
  (user messages, uploads, fetched material, tool output, remote tool output, model output read
  back into a request, memory extracted from any of those) as
  untrusted, marks it with a delimited, labelled section, and adds no further handling. A program
  that configures no policy gets exactly this.

**Where the line between policy and mechanism sits, and what moving it costs.** Classification,
marking format and handling are pluggable, and three things are fixed: the enforcement point cannot
be bypassed; once content is classified untrusted nothing converts it to trusted; and untrusted
content is always delimited and never placed in an instruction position. The fixed part is the
mechanism that makes any policy mean something; everything that decides *what* is protected is
pluggable.

- **Decided: classification is fully pluggable, including declaring a source trusted** (for
  example the output of a program's own internal tool). This is what makes the policy decide what
  counts as untrusted. The cost: a policy that classifies outside material as trusted removes the
  protection for it, and bonyan cannot tell a correct decision of that kind from a wrong one. What
  bonyan does is record every decision, so the choice is visible in traces and recordings.

Two other positions were considered and rejected:

- **Narrower: a floor.** A policy could add sources to "untrusted" but never declare material from
  outside the program trusted. This is safer against a mistaken policy, but a policy could then
  only tighten, so what counts as untrusted would no longer be fully the policy's decision.
- **Wider: marking pluggable, including off.** A policy could then place untrusted content in
  instruction positions. The placement guarantee of this section and §7 would then depend on the
  configuration, and the untrusted type would protect nothing once a request is built.

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
- **Recall re-applies trust by source.** Every record carries its source and the trust decision it
  was stored under. On recall the configured trust policy (§3) is applied again to the recorded
  source, and the stricter of the two decisions wins, so a fact is never recalled as more trusted
  than the material it was extracted from. A fact extracted from untrusted material enters context
  inside the marked section, not as trusted memory. This is a rule of the memory interface, not left
  to each backend, so a fact planted by a fetched item cannot come back later as trusted context.
  The recorded source names the kind of material the fact was extracted from, remote tool output
  included, so a fact from a tool server is classified again as remote tool output and never as the
  program's own tool output. Model output is a kind of its own in the same way: a fact extracted from
  a model's reply keeps model output as its source, so it is classified again as model output and
  never as the user's message or as material the program supplied.
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
  in-process tools, except in what the server controls:
  - their results are remote tool output (§3), not tool output. An error result is the server's
    output like any other and enters the same way;
  - text the server writes into a tool's definition reaches the model in instruction position,
    outside any marked section. By default bonyan keeps only what a call needs to be valid, at every
    level of the definition, and drops the rest: the tool's description, and in each schema every
    keyword validation does not use, among them description, title, examples, default, `$comment`
    and any keyword the server invents. What stays is the tool's name (under a prefix the program
    chooses), property names, enum and const values, patterns, formats, the names of the
    definitions a reference points to, and the identifiers and anchors a reference resolves
    through; that is the server-written text that still reaches the model. A program can keep
    everything for a given server; doing so is the program accepting untrusted text in instruction
    position.
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

- Content the trust policy classifies as untrusted (§3) is marked, delimited and never concatenated
  into instruction positions.
- A **human-in-the-loop hook**: an agent, or the trust policy's handling (§3), can mark an action as
  needing approval; the loop suspends it and emits it to a configured approver through the approval
  hook point (§12), and resumes or cancels on the decision. Suspended actions are
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
- **Re-runs for debugging and comparison.** A recorded input can be run again against live models,
  either N times unchanged (to see how much the output varies) or across a grid of parameters (model,
  prompt version, temperature, context settings). The results go through the same evaluators and
  land in one report, so a flaky answer or a regression can be investigated after the fact rather
  than reproduced by hand. Re-runs spend real budget and follow the same recording rules.
  **A re-run never repeats side effects by default:** tool calls are answered from the recording,
  matched by call, and an unmatched call fails that run rather than executing; live execution is an
  explicit per-tool opt-in, and an action that needs approval is never auto-approved in a re-run.
  **A model grid only uses models already configured for the agent** unless the operator names others
  explicitly, because each grid model receives the recorded, possibly private, input.
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
  aimed at the judge), so it runs inside the same invariants, with its own budget. The answer it
  judges reaches it as model output (§3), untrusted under the default policy and inside a marked
  section, never as the user's message. **Sampled live
  evaluation that sends user context to an evaluator model is opt-in and off by default.**
- **Live evaluation is asynchronous.** An operator defines evaluators once; when live evaluation is
  enabled for an agent, each sampled run is handed off after it finishes, never inside it, so
  evaluation adds no latency to the agent. Scores are attached to the run's identifier (and its trace)
  and exported as metrics, so a number per evaluator arrives later and can be read per run, per agent
  and over time. The hand-off goes through a pluggable queue, so where evaluation runs is a
  configuration choice. **A queued item is a recording** (same format and rules as above:
  memory excluded unless full recordings are enabled, indexed by subject, retention applies, deletion
  by subject removes queued items), and the queue is a slot under the invariants, so an evaluator
  reads it exactly like an offline recording and the "unverifiable" rule carries over.
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
- **Names follow the OpenTelemetry semantic conventions, not our own:** the GenAI conventions
  (`gen_ai.*`: operation, request model, token usage, duration) for model, agent and tool spans and
  metrics, the general conventions for everything else. The same conventions are what managed agent
  platforms and observability backends emit and read, so bonyan's telemetry is portable and nothing is
  renamed later. Anything the conventions do not cover gets a `bonyan.*` name, documented in one place.
  The conventions' **content attributes** (messages, system instructions, tool arguments and results)
  are exactly §9's content capture: off by default and under the invariants. Following the conventions
  means using their names, not emitting everything they define.
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
  - **a failed call is charged its bound:** a call that fails after its request may have reached the
    provider (a timeout mid-generation, a dropped connection, an error response) can be billed while
    reporting no usage. It is charged its pre-call bound (input + max output) × price. Only a failure
    the adapter shows happened before the request reached the provider (the connection was refused,
    the name did not resolve) is not charged; when the adapter cannot tell, the call is charged.
    *Why:* charging nothing would leave the budget unenforced for exactly the calls that fail, with
    only the step limit bounding them, which is the case "missing usage is an error" rules out. The
    cost is overcharging a failure that was not billed, which can only end a run early.
- **Missing usage is an error in a budgeted run**, never counted as zero: some local servers report
  none, and treating that as zero would leave the budget silently unenforced.
- Cost charged to a run is computed from reported provider usage and a price table in
  configuration. The pre-call bound above is the only estimate. It refuses calls, and it stands in
  for a charge only for a failed call that reported no usage, as above.

### 12. Hooks

bonyan has no user interface; front-ends, policies and integrations attach to a run through hooks.
A hook is registered under a name like any other implementation and selected by configuration, or
attached in code.

- **Hook points**, for every run:
  - run start, and run end with its outcome (a final answer, not cleared, or a typed error);
  - user message in (after the trust policy has classified it) and reply out (the final answer,
    before it returns to the caller);
  - before and after each model call;
  - before and after each tool call;
  - approval (§7): an action that needs approval is handed to the approver through this point;
  - memory write and memory recall (§4).
- **What a hook may do** is fixed per point. Every hook may observe; beyond that:
  - run start and run end: observe only;
  - user message in: change the message, or deny, which ends the run with a typed error;
  - before a model call: change the request, or deny, which ends the run with a typed error;
  - after a model call: observe only; what reaches the caller is redacted at reply out;
  - before a tool call: change the arguments, or deny, which reports the call to the model as
    denied;
  - after a tool call: change the result (sanitise or trim it) before it enters context;
  - reply out: change the reply (redact it), or deny, which ends the run with a typed error;
  - memory write: change the text about to be stored (redact or trim it), or deny, which leaves it
    unstored; the run goes on. The record's source, and the decision it is stored under, are the
    memory store's own (§4), whatever a hook changed;
  - memory recall: change what was recalled (redact an item, or leave one out) before it enters
    context, or deny, which leaves that recall empty; the run goes on. A hook there never adds an
    item: what enters context from a recall is only what the store recalled;
  - approval: approve or deny the action handed to it, never change it, since what is approved is
    what runs. The hook receives the action as it will run, after the before-tool-call hooks and
    secret scrubbing, and why it needs approval: the agent marked it, or the trust policy's handling
    required it. Each hook there approves, denies, abstains or answers that the decision is pending.
    The action runs only when a hook approves it and none denies it. A denial is reported to the
    model as a denied call, as before a tool call. When every hook has answered and none approved,
    denied or said pending (they abstained, or only observing hooks are attached), the action is
    cancelled at once; a pending answer keeps it waiting until a decision or the approval timeout,
    which cancels it. A cancelled action is not cleared for a gate-type agent (§7). Every outcome is
    recorded: approved, denied, failed or timed out with the hook that decided it, or cancelled
    because no hook decided, with no hook named.

  A run ended by a denial is, for a gate-type agent, one more not-cleared case (§7).
- **Several hooks at one point** run in their configured order. Each sees the payload as the hook
  before it left it, and the first denial ends the point: the hooks after it there do not run.
- **What a hook receives:**
  - content as typed values: what the policy classified untrusted arrives as untrusted, with its
    provenance, and trusted text as trusted (§3);
  - content after secret scrubbing (§10): tool results, model responses and requests have had every
    resolved secret removed before a hook sees them, and bonyan never hands a hook a secret. What a
    hook changes is scrubbed again after it, so a hook cannot put a resolved secret back;
  - memory contents at the memory hook points, and inside a request wherever the request carries
    them. A hook is code running in the program's process: like the program's own logging (§4), what
    a hook does with content it is handed is outside bonyan's reach. bonyan's own exporters and
    recorders still exclude memory (§4, §9).
- **A change never raises trust.** Changed content keeps the provenance of what it replaced, and
  nothing a hook adds becomes trusted.
- **No hook API returns trusted content from untrusted content.** A hook that changes a payload
  returns the same types it was given: where it received an untrusted value it can return only an
  untrusted value, with provenance. Content a hook adds is recorded with the hook's name. The limit
  is the one §3's types have: Go cannot stop program code that holds a plain string from building
  trusted text with it, so the guarantee is about bonyan's paths and API.
- **Hooks run inside the invariants.** Every request a hook changes still passes the trust
  enforcement point (§3), which runs after all hooks. A hook runs under the run's deadline, and its
  time counts toward it. No hook can switch off the trust enforcement point, scrubbing, memory
  exclusion from exporters and recorders, the not-cleared result or the loop's limits.
- **What is approved is what runs.** Before-tool-call hooks run first, then approval, then the tool;
  nothing changes a tool call after it is approved.
- **A failing hook fails closed.** A hook that errors or panics at a point where it may change or
  deny counts as a denial. After a tool call, where a hook may change but not deny, that denial
  withholds the result: the model is told the result was withheld, as it is told a call was denied.
  At memory write it leaves the text unstored, and at memory recall it leaves the recall empty; the
  failure is recorded and the run goes on.
  At approval it denies the action.
  An observe-only hook's failure is recorded and does not change the run.
- **Re-runs (§8):** hooks do not run in a re-run unless opted in per hook, as with live tool
  execution, so a re-run does not reach a user or an outside service through a front-end.

## Consequences

- Programs built on bonyan stop owning these concerns and gain them together; the cost is a shared
  dependency whose interfaces must stay stable, so interface changes follow semantic versioning (before 1.0 a breaking change bumps the minor
  version, per the release config) and get their own ADR, or before the first release an amendment
  (ADR 0000).
- The trust policy and the hook points are part of the stable interface. Adding a hook point later is
  additive; removing one or changing what it receives is a breaking change.
- The first releases will ship interfaces and the basic implementations before any program migrates.
  Each migration is its own piece of work in that program's repository.
- The library stays generic: no instance's topics, names, providers or thresholds appear in code,
  tests or docs. Examples and fixtures are synthetic.

## Out of scope for this ADR

- Package-level API details (method signatures), which follow in the implementation PRs.
- Which external memory backend or secret store adapters are written first.
- Hosting: bonyan is a library; where an agent runs is the program's decision.
- Front-ends and user interfaces: bonyan provides the hooks they attach to (§12), not the front-ends.

## Amendments

- Context: bonyan's aim is stated as a complete agent harness built with minimal code, with no user
  interface of its own; front-ends and policies attach through hooks.
- Decision, the invariants: restated with the reason each is fixed, and extended to the trust
  enforcement point, recorded trust decisions and "what is approved is what runs".
- §3: the untrusted-content concept became a pluggable trust policy (classification, marking,
  handling) with a fixed enforcement point inside bonyan and a default policy equal to the behaviour
  §3 specified before; where the line between policy and mechanism sits, and what moving it costs.
- §4: recall re-applies the configured trust policy; the stricter decision wins.
- §7: untrusted means as classified by the policy; the approver is reached through the approval hook
  point, and the policy's handling can require approval.
- §11: a call that fails after its request may have reached the provider is charged its pre-call
  bound, unless the adapter shows the request never reached the provider; the bound, which before
  only refused calls, stands in for a charge in that case.
- §12: hooks, new.
- §12: what a hook may do is stated for each point; a change never raises trust; several hooks at
  one point see each other's changes and the first denial ends the point; what a hook changes is
  scrubbed again after it; a failing hook after a tool call withholds the result.
- §3, §4, §5: remote tool output is a source of its own, untrusted under the default policy and kept
  as a recalled fact's source; of the text a tool server writes into a tool's definition, only
  what a call needs to be valid is kept by default (the tool's name, property names, enum and const
  values, patterns, formats, referenced definition names, and the identifiers and anchors a
  reference resolves through), and keeping the rest for a server is the program accepting untrusted
  text in instruction position.
- §12: at memory write a hook may change or deny the text about to be stored, and at memory recall
  what was recalled but never add to it; a denial or a failing hook there leaves the record unstored
  or the recall empty, recorded, and the run goes on; the store's source and decision for a record
  do not change with what a hook changed.
- §12: at approval a hook may approve, deny, abstain or say the decision is pending, never change
  the action; it runs only when one approves and none denies, a denial is reported to the model as a
  denied call, it is cancelled at once when no hook approved, denied or said pending and at the
  timeout when one said pending, a failing hook denies it, and every outcome is recorded, with the
  deciding hook when there is one.
- §3, §4, §8: model output read back into a request (an answer an evaluator judges, a reply kept in
  session history) is a source of its own, untrusted under the default policy and kept as a recalled
  fact's source.
