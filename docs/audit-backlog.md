# Audit backlog — items NOT yet shipped

Persistent cross-pass tracker for the external-review backlog. Anything marked
`TODO` here is genuinely unimplemented: a future pass should read this file
BEFORE planning work so it isn't forgotten. Mark items `DONE` (with version +
test evidence) as they ship. Source reviews live in `ideas/audit_15_08_2026.md`.

Rule: every TODO must also exist in `improvement.md` (or reference this file);
this file is the canonical status, `improvement.md` is the narrative roadmap.

Status: **all audit items shipped through v0.1.79** (2026-08-15). The only
remaining entries are deferred-with-rationale below (do not re-open without new
evidence). v0.1.80 shipped a research-mode overhaul and v0.1.81 shipped
scholarly search (`paper_search` + LangSearch); v0.1.82 closed the two
previously-deferred live-soak bench cases as deterministic tasks (see
`improvement.md`).

---

## Pass 1 — v0.1.77 (shipped 2026-08-15) — P0 correctness

- DONE — **Exit-status trust** (GPT sol #1) — diagnostics/test_runner surface
  exit codes as `[PASS]`/`[FAIL]`; `DiagnosticsFailed`/`TestsFailed` trust the
  marker first. Evals 52-54 cover the placeholder/plan-mode/PASS-marker paths.
- DONE — **Only successful writes arm write state** (GPT sol #2) — a failed
  fs_edit no longer sets `unverifiedWrite`/`touchedPaths`/`turnWrote`.
- DONE — **Plan-mode enforced in dispatch** (GPT sol #3) — non-read-only calls
  rejected while plan mode is on.
- DONE — **Truncation-placeholder guard** (AGY #1) — fs_write blocks
  "// ... existing code ..."-style content.
- DONE — **Test-gate scope** (GPT sol #4) — tests every uniquely-touched file.

## Pass 2 — v0.1.78 (this file created; shipped 2026-08-15) — next-priority + P1

- DONE — **Truncated-response detection** (GPT sol #5) — `ParseSSE` now returns
  `ErrStreamTruncated` on EOF-without-`[DONE]` (tolerated when a terminal
  `finish_reason` was seen), the client captures `finish_reason` ("length" =
  hit the token cap), and the agent recovers with a bounded nudge instead of
  aborting or accepting a truncated prose reply as final. Evals 57-58 + unit
  tests (sse/client/agent).
- DONE — **Tool schemas in context accounting** (GPT sol #6) — `setSchemaTokens`
  records the serialized `tools` field cost before each request and
  `estTokensLocked` adds it (the gauge/budget now reflect the real prompt, MCP
  included); resumed histories retokenize via the server tokenizer instead of
  len/4.
- DONE — **Fenced/markdown tool-call extractor** (AGY #3) — a tool call inside
  a ```json fence is extracted and executed on the same turn (approval still
  applies) instead of burning a prose-nudge round-trip. Eval 55 + unit tests.
- DONE — **Path & syntax sanitizer** (AGY #4) — `sanitizePathArg` trims
  wrapping quotes, normalizes `\` -> `/`, strips a leading workspace-basename
  prefix; `caseInsensitiveResolve` fixes Readme.md -> README.md (exactly-one
  match). Eval 56 + unit tests.

- TODO — **MCP schema exposure selective** (GPT sol #7) — every advertised MCP
  tool is appended on every request (agent.go). Add per-server allowlists /
  signal-based activation so a big MCP server can't undo dynamic filtering.
- TODO — **Proactive tool-output sliding window** (AGY #2) — collapse read-tool
  results older than ~2-3 turns into a one-line marker (errors kept), reducing
  attention drift on 7B models well before the hard budget trips.
- TODO — **Dependency-ranked fix sequencing** (AGY #5) — on multi-file compile
  errors, hint the model to fix upstream definitions before downstream callers
  (topological order via the import graph).
- TODO — **/steer + plan-step tracker** (AGY #6 / luna #1) — `/steer <text>`
  injects a high-priority pinned instruction into TASK STATE for the next
  round; extract approved `plan` steps into an `ACTIVE PLAN: step N/M` block.
- TODO — **Benchmark/measurement expansion** (GPT sol, measurement section) —
  add bench cases: edit-fail-recover, multi-package refactor, rejected-write
  recovery, plan-mode attempted write, truncated-response recovery, long
  resumed session near the real context limit, big-MCP-server context cost.

## Pass 3 — v0.1.79 (shipped 2026-08-15) — the remaining TODO backlog

- DONE — **MCP schema exposure selective** (GPT sol #7) — `MCPToolNamesForSignal`
  offers only the MCP tools whose server the input names or the model already
  used this turn, instead of re-flooding every request with a big server's full
  schema set; the registry still resolves any called tool at dispatch.
- DONE — **Proactive tool-output sliding window** (AGY #2) —
  `proactivePruneToolOutputs` collapses read-tool results older than the current
  + immediately preceding turn into a one-line marker on EVERY request (errors
  kept), so a 7B model's attention isn't diluted before the hard budget trips.
- DONE — **Dependency-ranked fix sequencing** (AGY #5) — `dependencyFixHint`
  parses the failing files from a diagnostics report and, via
  `index.Topology.OrderByDeps`, names the upstream-definition-first fix order
  in the goal gate and verify barrier.
- DONE — **/steer + plan-step tracker** (AGY #6 / luna #1) — `/steer <text>`
  (REPL + TUI) pins a `USER STEER` line at the top of TASK STATE until replaced
  or cleared; an approved `plan` call records its steps into an `ACTIVE PLAN`
  block so a small model can't skip intermediate steps.
- DONE — **Benchmark/measurement expansion** (GPT sol, measurement section) —
  `bench.Tasks` gained edit-recover (real edit→fail→verify), denied-write
  recovery, plan-mode enforcement, truncated-recover (client-wrapper-injected),
  multi-file-refactor, and (v0.1.82) **long-resumed-session** (seeded near-limit
  `InitialHistory`, budget must prune before the answer) and
  **big-mcp-context** (synthetic MCP-scale schema registered via
  `ConfigureRegistry`; the schema-accounting fix keeps the gauge honest);
  `Task` gained `Configure`/`WrapLLM`/`ConfigureRegistry` hooks and
  `agent.Config.PlanMode`. The two previously-deferred live-soak cases are now
  deterministic bench tasks (no live server needed — the accounting +
  retokenize fixes are exercised in-process).

## Rejected / deferred with rationale (do not re-open without new evidence)

- REJECTED — **QLoRA fine-tune script** (AGY #7) — plumbing, not a runtime
  feature; no evidence the loop is the bottleneck (C3-gated).
- REJECTED — **Scoped permission policies** (luna #6) — allow-remember + /yolo
  already cover the ergonomics; a path-scoped policy adds config surface for
  marginal safety gain.
- DEFERRED — **Retrieval confidence/abstention** (luna #2) — quality thresholds
  on memory/code recall so weak semantic matches don't distract; no evidence of
  harm yet.
- DEFERRED — **Persistent tool-failure memory** (luna #3) — failure signatures
  + recovery paths across resumed sessions; overlaps with GoalMemorize.
- DEFERRED — **Structured subagent artifacts** (luna #4 / C3) — gated on
  live-fidelity evidence; 18/18 free-text recall keeps it gated.
- DEFERRED — **Automatic capability probing** (luna #5) — probe tool-call
  reliability / reasoning / tokenizer per model and auto-select settings; needs
  a stable probe harness first.
- DEFERRED — **Doc synchronization** (luna #7) — design docs still describe
  heuristic counting / older defaults; do a doc pass after the next feature
  freeze.
