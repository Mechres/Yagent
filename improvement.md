# Yagent improvement roadmap

Consolidated, prioritized plan for post-M6 work. Status: **P0, P1, B1–B4,
C1/C2 and the eval/benchmark expansion are all shipped** (2026-08-12 batches);
the remaining items are C3 and the M7 gated/deferred items, both waiting on
evidence that the current design is the bottleneck.

Legend: ✅ shipped · 🟡 queued · ⚪ not a fit for a local-first tool.

## P0 — done this pass

- ✅ **Web search provider fallback chain** — `web_search` now tries
  DDG → Mojeek → SearXNG (when configured) on error/empty results. SearXNG as
  the *primary* never falls back to third parties (privacy). (`internal/web`)
- ✅ **Sensitive-data redaction** — API keys, bearer tokens, `key=value`
  secrets and home paths are scrubbed before anything is written to SQLite
  (messages, summaries, semantic memories). (`internal/scrub`)
- ✅ **Tool-call JSON repair heuristic** — `decodeArgs` runs a repair pass
  (trailing commas, raw newlines inside strings) before failing, so minor
  small-model JSON slips don't burn a retry turn. (`internal/tools`)

## P1 — next pass

- ✅ Tree-sitter language expansion: Rust / C / C++ / Java grammars (YAML and
  Markdown already get the line-window fallback).
- ✅ Session forking: `yagent chat --fork <id>` branches a session's history so
  experiments don't mutate the original.
- ✅ Startup re-index: at session start, if an index exists, `Index()` runs in
  the background and hash-checks files, re-embedding only the changed ones
  (the first build stays an explicit `index_repo`).
- ✅ Real versioning (`git describe` via `make build`) instead of `v0.0.0`;
  added a `Makefile` (`make build|test|vet|race|version`).
- ✅ Dynamic tool-schema filtering: the core tool set is always offered; web /
  index / `skill_manage` schemas are added only when the input signals that
  domain or the model already used them this turn. Filtering shrinks what the
  model *sees* (saves ~1k tokens/turn); the registry still holds every tool,
  so a tool the model calls anyway still works.
- ✅ TUI diff overlay: `fs_edit`/`fs_write` approval prompts render a colorized
  before/after diff instead of raw argument JSON.
- ✅ Shell completions: `yagent completion bash|zsh` emits a completion script
  for `chat|sessions|skills|doctor`.

## P2 — deferred / gated

- ✅ Loop mode (M6.13): `yagent chat --goal "..."` / `/goal <text>` runs the
  agent toward a goal in DONE/CONTINUE rounds (capped by `--rounds`, default 8).
  This is also the natural M7 evidence: run an autonomous goal and watch whether
  the single loop bottlenecks.
- ✅ `consult` tool (M6.13): a second local "advisor" model (`consult.server_url`
  / `consult.model`), a cloud OpenAI-compatible endpoint (`consult.api_key`), or
  an installed terminal AI app (`consult.cmd`, e.g. `[claude, -p]`).
- ✅ Session mgmt (M6.15): `yagent sessions search <q>` + `yagent sessions
  export <id>`.
- ✅ `/undo` (M6.15): in-memory per-turn file-write buffer; reverts the last
  turn's fs_write/fs_edit (incl. created files).
- ✅ Symbol-aware search (M6.15): top-level decls indexed into `index_symbols`;
  `index_search` supports `symbol:`/`type:` exact lookups.
- ✅ Chunker grammars for bash/html/css (M6.16); SQL stays on the line-window
  fallback (its tree-sitter grammar isn't fetchable).
- ✅ Goal-mode eval + chunker/symbol/search benchmarks (M6.16).
- ✅ Sandboxed shell (M6.17): `shell.sandbox: bwrap` wraps `shell_exec` in
  bubblewrap (workspace rw, system ro, private /tmp, no network). It exists to
  contain *consented-but-unsafe* work under `--yolo` on untrusted repos, not to
  replace the approval gate. Fails loudly when bubblewrap is missing (Linux
  only).
- ✅ Multi-user (M6.18): per-project config (`.yagent/config.yaml`), project
  memory (`scope: global|project`), `yagent init`, `yagent backup`,
  `yagent skills import <url>`, repo hygiene (LICENSE/CONTRIBUTING/CHANGELOG/
  example config/CI), expanded doctor, export redaction warnings.
- ✅ M7 v1 started (M6.19): `subagent` tool (isolated read-only child agent),
  `fs_patch` (multi-file unified diff, undo-aware), background jobs
  (`shell_bg`/`shell_logs`/`shell_kill`), `code_outline` (declaration
  signatures), compact code injection, TUI `/sessions` browser.
- ✅ M7 v2: parallel `subagent tasks[]` (each subtask in its own isolated
  read-only child agent, results combined in order).
- ✅ M7 beyond v2: `subagent` gained a `tools[]` subset — each child registry
  is scoped to the requested read-only tools (invalid/destructive requests are
  rejected and fed back), and the child's summary already feeds back into the
  parent as the tool result.
- ✅ M7 beyond v2 (more): shared subagent scratchpad (`scratch_write`/
  `scratch_read` under `.yagent/scratch/`), call-graph `code_references`,
  workspace checkpoints for goal mode (`/checkpoint`), fuzzy tool-argument
  aliasing, and tool-output compaction.
- 🟡 M7 beyond v2 (remaining): deeper orchestration — a shared structured
  memory beyond a string summary (a true subagent workspace), and an
  interactive per-hunk fs_patch approval modal. Only if real use shows the
  summary / atomic approval is the bottleneck.
- ✅ More eval coverage + benchmarks (2026-08-12): the harness gained
  `deny_first` (approval-denial recovery), `patch_filter` (partial fs_patch
  approval via `Approval.Args` rewrite), and `file_contains`/`file_not_contains`
  assertions; new golden evals 15–17 (execution-error recovery, approval-denial
  recovery, partial fs_patch); benchmarks for patch split/rebuild (200 hunks,
  ~65–120µs) and subagent fan-out/fan-in (4/8/32 tasks, ~3–21µs). TUI flows are
  covered by `tui_test.go` (hunk walker, find, settings, sessions); chunker /
  symbol / hybrid-search benchmarks were already present (M6.16).
- ⚪ Telemetry / metrics / Docker / systemd / man pages / docs site —
  not a fit for a local-first single binary; would add surface and, for
  telemetry, conflict with the privacy stance. (CI shipped in M6.18 —
  `.github/workflows/ci.yml` runs gofmt/vet/test/race on every push.)

## Proposed (external agent reviews — not yet scoped/started)

Copilot's "explore-codebase-and-plan-improvements" plan (2026-08-11). Ideas
only; status notes reflect what already exists so they can be evaluated on top
of reality:

- 🟡 **Structured subagent workspace** (M7 "deeper orchestration"). Upgrade
  subagent output from plain summary text to shared structured artifacts
  (task id, type, payload, provenance, confidence). Suggested trimmed scope:
  add `return_artifacts` to the subagent tool — the child lists which scratch
  notes are its results, and the parent's result comes back structured instead
  of free-text. Backward compatible with existing subagent/scratch behavior.
  *Already present:* scratchpad (`scratch_write`/`scratch_read`), parallel
  `tasks[]` ordering, per-child token tally, tool subsets.
- 🟡 **fs_patch approval UX v2** — true centered modal with file/hunk
  navigation, selection state, select-all/select-none, and a final apply
  summary (vs. today's in-stream per-hunk walker). Partial selection must
  always yield a valid reconstructed patch or an explicit denial.
  *Already present:* per-hunk y/n/q walker + `RebuildPatch` args rewrite.
- ✅ **Eval + benchmark expansion** (2026-08-12) — partial fs_patch approvals
  (`patch_filter` + `file_contains` assertions), failure-recovery paths
  (execution-error retry, approval denial), and benchmarks for subagent
  fan-out/fan-in and patch split/rebuild are all shipped. Structured subagent
  workflows remain deferred with C3 (evidence: the live fidelity eval shows no
  summary loss). *Already present:* evals for subagent/toolset, fuzzy args,
  code_references; chunker/symbol/hybrid-search benchmarks.

A third review (2026-08-12, "agy") proposed 12 items across four domains.
Screened against the codebase (see session notes); **skipped** items marked ⚪:

- ✅ **P1 project-instructions reader** — `.yagent/instructions.md` >
  `AGENTS.md` > `CLAUDE.md` > `.cursorrules` (first found, capped) are folded
  into the system prompt by `repoInstructions`.
- ✅ **P2 preset subagent roles** — `subagent.role: architect|auditor|
  test-engineer|docs-writer` (prompt suffix + default tool subset +
  temperature via `llm.Client.Clone`).
- ✅ **P3 structured session exports** — `yagent sessions export <id>
  --format html|md`.
- ✅ **P4 tool-output pruning in the budget** — old tool results collapse to
  `[tool output concealed; N lines hidden]` before summarization runs.
- ✅ **P5 `workspace_diagnostics` tool** — detects the project and runs
  `go vet`/`tsc --noEmit`/`cargo check`/`ruff` as a read-only tool.
- ✅ **P6 skills manager modal** — bare `/skills` opens the pending-writes
  modal (diff/verify/approve/reject).
- ✅ **P7 `fs_refactor` rename** — word-boundary symbol rename across source
  files, undo-aware and approval-gated.
- ✅ **P8 declarative playbooks** — `.yagent/playbooks/*.yaml` phases of
  `{goal, rounds, tools[], checks}` run through `RunGoal` + tool subsets, with
  deterministic success predicates.
- ⚪ Git worktree isolation (`--worktree`) — overlaps `internal/checkpoint`
  rollback; conflicts with the no-git-mutations constraint.
- ⚪ Multimodal local vision — needs a multimodal message-part architecture
  change + a vision model on 12 GB VRAM (tight); not a local-first fit now.
- ⚪ `/plan` interactive mode — big TUI lift; goal mode + checkpoints already
  give a linear plan; playbooks cover the structured case.
- ⚪ TUI dual viewport (Ctrl+W live-diff split) — the TUI is already packed;
  high rework for medium value.

**Phase plan**: A = P1+P2+P3 (quick wins) → B = P4+P5+P6 (medium) →
C = P7+P8 (bigger). Status: **Phases A + B + C all shipped** (2026-08-12).

Phase A status:
- ✅ **P1 project-instructions reader** — `repoInstructions` (agent.go) appends
  `.yagent/instructions.md` > `AGENTS.md` > `CLAUDE.md` > `.cursorrules`
  (first found, 16 KiB cap) to the system prompt. Verified live on :8089 via
  `--trace`.
- ✅ **P2 preset subagent roles** — `architect`/`auditor`/`test-engineer`/
  `docs-writer` (`tools.SubagentRole`): role system-prompt suffix + default
  read-only tool subset + temperature (child client cloned via `llm.Client.
  Clone`); `subagent.role` arg, unknown roles rejected.
- ✅ **P3 structured exports** — `yagent sessions export <id> --format
  html|md` (default md); `Store.RenderHTML` emits an escaped, styled,
  dependency-free HTML page.

Phase B status:
- ✅ **P4 tool-output pruning in the budget** — `budget` first collapses old
  tool results (before the current user turn) to a one-line
  `[tool output concealed; N lines hidden]` marker, keeping user/assistant
  turns alive; summarization only runs if still over budget. In-memory only
  (resumed sessions reload full messages and re-prune). Eval 07 now asserts
  the concealed marker.
- ✅ **P5 `workspace_diagnostics` tool** — detects the project
  (go.mod → `go vet ./...`, Cargo.toml → `cargo check`, package.json+tsconfig →
  `npx tsc --noEmit`, eslint when configured, py → `ruff check .` or
  compileall) and runs the checker with a 120s timeout. Read-only: commands are
  fixed by the tool, so no approval gate, but the agent gets a first-class
  self-healing loop after edits. In the core schema set.
- ✅ **P6 skills manager modal** — bare `/skills` opens a TUI modal over the
  pending staged writes (following the `/settings`+`/sessions` patterns):
  ↑/↓ pick, `d` diff, `v` verify, `a` approve, `r` reject, esc close.

Phase C status:
- ✅ **P7 `fs_refactor` rename** — word-boundary symbol rename across all
  source files (skips .git/.yagent/build dirs, binary files), rewrites every
  occurrence (comments/strings included) and records originals for /undo.
  Write-gated (approval); validation for empty/equal/non-identifier names.
- ✅ **P8 declarative playbooks** — `.yagent/playbooks/<name>.yaml` of phases
  `{goal, rounds, tools[], success}`; `yagent chat --playbook <name>` and
  `/playbook <name>` (or bare `/playbook` to list) run each phase as an
  autonomous goal run scoped to its tool subset (`agent.SetRegistry`),
  snapshotted per phase. `yagent playbook list` lists them. Live-verified on
  :8089.

With this, the agy roadmap (all non-⚪ items) is complete. Remaining backlog:
C3 (structured subagent returns, gated on evidence) and the M7 gated items.

## Local-model tuning (2026-08-12)

A second "6 ideas" review (2026-08-12) — deterministic guardrails over prompt
hope, token frugality — implemented here:

- ✅ **`code_slice`** (#1 surgical slicing) — `code_slice(path, symbol)`
  returns exactly one declaration's source span (body + doc comment) via the
  tree-sitter parser (`index.SliceSymbol`), a surgical read far cheaper than
  fs_read on large modules. ~80% fewer tokens than a whole-file read.
- ✅ **Pre-flight tree-sitter syntax validation** (#2) — `fs_edit`, `fs_write`
  and `fs_patch` parse the modified source in-memory (`index.SyntaxErrors`); if
  tree-sitter finds ERROR/MISSING nodes the tool returns the exact
  line/col/preview and the file is NOT written — a broken edit never hits disk
  or wastes the diagnostics roundtrip. Non-source files are never touched.
- #3 BM25/keyword search — the hybrid index already has an FTS5 (BM25) keyword
  pool, and `symbol:`/`type:` lookups use a dedicated no-embed path; a full
  zero-embed rerank for plain queries changes ranking semantics and was
  deferred.
- #4 skill/playbook distillation — the end-of-turn skill-creation harness
  (5+ tool calls) already covers the distillation case; playbook-drafting from
  a session is a small future add-on.
- #5 TUI side drawer, #6 shadow speculative execution — deferred (big TUI lift
  / single-GPU contention; same call as the earlier dual-viewport skip).

Four external reviews (gemini/luna/mistral/nemotron, 2026-08-12, `ideas/` —
untracked) deduped. Highest recurring picks shipped here:

- ✅ **SlotLock — serialize inference** (gemini #3, luna #1): a process-wide,
  per-server semaphore (`internal/llm/slot.go`) around chat/embed/tokenizer
  requests so parallel subagents, consult and embeddings never hit a
  single-slot llama.cpp/Ollama concurrently (HTTP 500 / VRAM thrash). Capacity
  defaults to 1 and is raised to the server's real slot count from `/props`.
- ✅ **Deterministic error remediation** (gemini #1): `fs_edit` "old_string not
  found" now reports the nearest matching line (substring/Levenshtein), so a
  typo recovers in one turn instead of three loops.
- ✅ **Truncated tool-call recovery** (luna #2): a cut-off tool-call args
  object (unclosed string/brace) returns "arguments were truncated (incomplete
  JSON) — re-emit the full tool call" instead of a generic syntax error.
- ✅ **Prose tool-call nudge** (gemini #2): when the model narrates a tool call
  ("I will fs_read main.go") with no tool_calls emitted and no tool has run,
  the agent nudges it to emit the call (never auto-executes); past-tense
  mentions and code fences are ignored. Unit-tested deterministically.
- Already covered by earlier work: mistral's arg-repair/sampling profiles/
  pruning, gemini's pre-flight syntax + diagnostics, luna's dedup/validation
  counters. Deferred: structured error envelopes, verify-barrier enforcement,
  goal-progress ledger, retrieval thresholds, crash-safe undo, TUI arg editor.

The "make DONE real" batch (2026-08-12) — deterministic enforcement instead of
prompt hope:

- ✅ **Verify-don't-trust barrier** (luna #3) — `agent.Config.VerifyWrites`
  (UI-enabled): any write marks the turn unverified; before accepting a final
  answer the agent deterministically runs `workspace_diagnostics` and feeds the
  result back, so "done" after an unverified write is impossible. The model's
  own `workspace_diagnostics` call clears the flag (a new write re-arms it).
  Unit-tested with a compile-broken-but-syntax-valid file (pre-flight can't
  catch missing imports; go vet does).
- ✅ **Playbook success predicates** (luna #11) — playbook phases can declare
  machine-verifiable `checks:` (`file_contains`, `file_not_contains`,
  `file_exists`, `diagnostics`). The model's DONE is a proposal: a phase only
  completes when its checks pass, with one automatic re-run to let the agent
  fix failures.
- ✅ **Structured error envelopes** (luna #8 / nemotron #4/#9) — key tool
  errors now carry `[class=… retryable=… suggest=…]` markers
  (`missing_path`→glob, `old_string_not_found`→fs_read, `ambiguous_match`,
  `timeout`) the model can act on programmatically.
- Deferred from the same reviews: goal-progress ledger, retrieval confidence
  thresholds, crash-safe undo journal, TUI arg editor, /retry, dedup read
  buffer, t/s meter.

Lighter-wins batch (2026-08-12) — the smaller deferred items:

- ✅ **Goal-progress ledger** (gemini #7 / luna #5) — the agent tracks touched
  files and the last tool failure and injects a compact `TASK STATE` block
  (changed / last failure) into the system message each request, so the model
  stays oriented across long multi-turn runs without re-reading history.
- ✅ **fs_read dedup cache** (gemini #6) — a repeated full read of an unchanged
  file returns a `[cached]` marker instead of re-injecting the whole content
  (hash-keyed per session; line-range reads and changed files bypass it).
- ✅ **t/s meter** (gemini #10) — the TUI status line shows a live
  tokens/second reading while a turn streams (spot VRAM thrashing vs a freeze).
- ✅ **`/retry`** (luna #10) — re-runs the last input with a stable sampling
  profile (temp 0.3, repetition_penalty 1.05) in both UIs, so a one-off loop or
  malformed call recovers without retyping.
- Still deferred: crash-safe undo journal, TUI arg editor on approval,
  retrieval confidence thresholds, structured tool-failure session memory.

QA / consolidation pass (2026-08-12): the full live re-validation (benchmark,
sweep, fidelity eval) surfaced two real regressions from the lighter-wins batch,
now fixed and re-verified at 100%:

- ✅ **Agent-side loop guard** — `agent.RepeatLoop` (shared with the TUI) now
  watches the streamed content *inside* `agent.Run`: on a repeating unit the
  request is cancelled and a stop-repeating nudge fed back. Previously the loop
  guard was TUI-only, so a looping *subagent* could burn the whole request
  timeout (this made the live fidelity eval hit "context deadline exceeded").
- ✅ **Safe fs_read dedup marker** — the `[cached]` marker no longer tells the
  model to "reuse the earlier result in history" (pruning may have removed it,
  and that suggestion invited hallucinated file contents — the fidelity eval
  caught the child inventing "value = 10/15/20"); it now states the file is
  unchanged and offers a line-range re-read.
- Live-evidence note: the sampling sweep is noisy at N=3 tasks per recipe
  (default/rep/minp/cold all swing between 1–3/3 across runs) — the shipped
  recipe is fine; `yagent calibrate` output should be read as a range, and a
  larger task set would be needed for a hard ranking.

Eval/acceptance expansion (2026-08-12):

- ✅ **Golden evals 18–22** — the new deterministic behaviors are now locked
  into the fake-server harness: prose tool-call nudge, verify barrier,
  truncated tool-call recovery, structured error envelopes, and the task-state
  ledger. Harness gained `requests_contain` and `verify_writes`.
- ✅ **Truncated tool-call marshal fix** — a cut-off tool-call argument is
  sanitized into a `{"__truncated":true}` marker at the SSE layer so the
  assistant message re-serializes on the next request (previously the invalid
  RawMessage crashed the client with "unexpected end of JSON input"); the
  decoder maps the marker to the "re-emit the full call" feedback.
- ✅ **Live acceptance re-run** (Qwythos :8089) — web search (calls web_search,
  cites the source URL), goal mode (writes → self-runs diagnostics → re-reads
  → round done, checkpoint snapshotted), and the benchmark/fidelity evals all
  pass. The sampling sweep remains noisy at N=3 (see QA note).

Skill/playbook auto-distillation (2026-08-12) — the last deferred idea from the
6-ideas review:

- ✅ **Post-goal playbook distillation** — after a successful autonomous goal
  run with ≥ 3 tool calls, the agent offers to distill the workflow into a
  reusable `.yagent/playbooks/<name>.yaml` (the model writes it with fs_write,
  or declines with "no playbook"). Complements the existing end-of-turn skill-
  creation opportunity. Live-verified on Qwythos (:8089).

Making the small local model work better is now a measurable loop, not folklore. A Hermes review (2026-08-12) — "push correctness into tools, treat the model as proposer not executor" — transferred next:

- ✅ **`clarify` tool** (Hermes #1/#5) — the model calls `clarify(question, choices[])` when a task is ambiguous or a decision matters; the UI renders the question as real options (REPL numbered prompt, TUI modal) and the user's pick returns to the agent as tool data (`user answered: X`). Ambiguity is now a hard stop with a structured handoff, not a prose guess. Live-verified on Qwythos (:8089): model asked via clarify, piped pick flowed back as data.
- ✅ **`plan` tool** (Hermes #4) — a lightweight plan-approval gate: the model proposes steps, the UI shows them and the user approves or gives revision feedback, returned as `plan approved — execute it now` / `plan rejected: <feedback>`. No TUI lift needed; reuses the same AskUser plumbing.
- ✅ **Verify, don't trust** (Hermes #2) — the system prompt now requires re-reading the touched region with fs_read after any code write (and confirming the diff matches intent) before `workspace_diagnostics`. Write tools already return confirmable shaped data (`wrote path (N bytes; overwrote M bytes)` + diff), so #3 is largely in place without a reshape.
- The AskUser callback (`tools.SetAskUser`) powers both tools; they're only registered/offered when the UI wires it (subagents/evals never see them).

Making the small local model work better is now a measurable loop, not folklore. A peer review (2026-08-12) proposed 8 items; implemented here:

- ✅ **P1 per-model sampling profiles** — a `models:` config section matches
  sampling overrides by model-name substring (pointer fields: unset = inherit
  the base recipe), turning docs/models.md's hand-maintained matrix into code.
- ✅ **P2 context-window auto-detect** — the client probes llama.cpp `/props`
  (`default_generation_settings.n_ctx`); `yagent chat` caps the agent budget at
  the server's real window (no more over-length 400s), the reserve is now a %
  of the window, and `yagent doctor` reports `server n_ctx` vs the budget.
- ✅ **P3 `yagent calibrate`** — runs the 3 canonical tasks across the 4 sampling
  recipes against the live model, prints per-task pass/fail, and (with
  `--write`) persists the best recipe into the config. `internal/bench` is the
  shared task/recipe package also used by the live eval tests.
- ✅ **P6 fuzzy path pre-resolution** — `fs_read`/`fs_edit` auto-correct a
  dropped extension when exactly one file matches (`README` → `README.md`),
  with a "resolved to" note; saves a wasted turn per slip.
- Deferred from the review: #4 reasoning-budget passthrough (server support via
  the OpenAI API is unclear), #5 single-slot concurrency guard, #7 truncated
  tool-call recovery, #8 GBNF grammar passthrough (risky/uncertain).

First sweep on Qwythos (:8089):

- ✅ **Prompt rules** — `buildSystemPrompt` now tells the model to run
  `workspace_diagnostics` after code edits, ask clarifying questions when a task
  is ambiguous, and follow two worked examples (locate-then-read, retry-edit
  with the exact text).
- ✅ **Loop-guard auto-retry** — when a repetition loop is auto-cancelled, the
  same input is retried once with `sampling.repetition_penalty 1.05` applied
  (persisted for the session). Esc-cancel never retries.
- ✅ **`sampling.min_p` knob** — opt-in nucleus lower-bound filter for
  llama.cpp/Ollama (0 = off; `/settings` + `/set`), matching `top_k`/rep_penalty
  semantics so cloud endpoints aren't broken.
- ✅ **Live small-model benchmark** (`internal/eval/live_test.go`,
  `YAGENT_LIVE_EVAL=1`) — three canonical tasks: correct tool JSON + read,
  two-turn recall, edit-then-verify (diagnostics surfaces a planted compile
  error). `YAGENT_LIVE_SWEEP=1` runs the same tasks across sampling recipes.
  **First sweep on Qwythos (:8089): default (0.6/0.95) and cold (0.3) both
  3/3; repetition_penalty 1.05 and min_p 0.05 each 2/3** — the shipped recipe
  is already good; penalties are remedial toggles, not defaults.

A second external agent's plan (2026-08-11). Its section A (fs_patch modal v2)
was superseded mid-inspection — that work is already shipped (per-hunk walker
+ `RebuildPatch`). Sections B/C/D recorded for evaluation:

### B — quick wins (no new deps, local-first)

- ✅ **B1 eval-coverage gaps** — `/skills verify` PASS/FAIL flow (staged
  skill, verdict parse, pending+skill failure counters), end-to-end `/undo`
  revert over a scripted agent write, and consult soft-fail are now covered
  by `internal/eval/gaps_test.go` + `internal/tools/consult_test.go`;
  `/checkpoint` save|restore and `shell.sandbox: bwrap` loud-deny were already
  covered (`checkpoint_test.go`, `TestShellExecSandboxNotInstalled`).
  Scripted httptest servers, no network.
- ✅ **B2 `--trace` prompt dump** — `yagent chat --trace <file>` writes each
  assembled context with per-section token estimates (system / skills L0 /
  code / summary / recall / injected / history) plus the full content. Local
  file only (fits privacy stance). Acceptance met: the trace's section
  estimates are exactly what `ContextUsage` accounts from (non-history
  sections + live history), verified by `TestTraceSegmentsSumToContextUsage`.
- ✅ **B3 TUI transcript search** — `Ctrl+F` opens an in-viewport find bar:
  type to search (case-insensitive), enter jumps to the next match, esc
  closes. *Already present:* scrollable viewport.
- ✅ **B4 config schema completeness** — `consult.cmd` is now editable:
  `/set consult.cmd claude -p` persists a YAML sequence and round-trips on
  reload (tested). `shell.sandbox` was already exposed; shell/job timeouts are
  per-call `timeout_sec` arguments, not global keys, so there is nothing to
  expose — `/settings` is the single surface for all config keys.

### C — capability features (medium effort)

- ✅ **C1 accurate token counting via `/tokenize`** — `llm.Client.CountTokens`
  calls the server tokenizer (llama.cpp `/tokenize` at the root, Ollama
  `/api/tokenize`; probed once, len/4 fallback) and is wired into the agent:
  the system prompt, running summary, injected skills and every history
  message are counted accurately (no network under the lock — the gauge sums a
  cached per-section summary plus live history tokens). Verified on :8089
  format + fake server: the gauge and summarization trigger now reflect real
  counts.
- ✅ **C2 goal-mode resume across restarts** — goal mode snapshots the
  workspace after every completed round, and `yagent chat --resume-goal
  <session>` restores the goal checkpoint and continues the session, picking
  the goal back up from its last user message. *Was already present:* pre-goal
  snapshot (`/checkpoint restore goal`).
- 🟡 **C3 subagent structured returns** — children return structured JSON
  (findings + paths + confidence) the parent can index/act on, plus a shared
  read-only subagent workspace. Gated: only if string-summary is the real
  bottleneck. (Same target as the Copilot artifact item above.)
  *Evidence (2026-08-12, `internal/eval/live_test.go` vs Qwythos on :8089):*
  a 18-fact inventory (6 files × 3 high-entropy values buried in decoy prose)
  shows **100% fidelity for every output style** — free-text summary, concise
  summary, structured findings list, and strict JSON (which Qwythos emits
  validly on demand). Reported 18/18, recalled 18/18 across all cells. The
  string-summary bottleneck **does not reproduce at this scale**, so C3 stays
  gated; the live eval is the repeatable evidence harness if a larger/adversarial
  scenario is ever wanted.

### D — out of scope (local-first / privacy)

Telemetry/metrics, Docker/systemd units, man pages, docs site, embedded
inference, MCP client, cloud provider SDKs (CI already ships).
