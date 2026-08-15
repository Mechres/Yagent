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

Loop/stall hardening + reasoning budget (2026-08-12, from a real "it loops with
no output" report on a long reviewer prompt):

- ✅ **`sampling.reasoning_max_tokens`** — opt-in cap on the model's thinking
  span per request (llama.cpp/Ollama; confirmed accepted by Qwythos on :8089).
  The single biggest speed lever for a 12 GB card: each round-trip drops from
  ~25s to ~5s, so long turns stop churning and actually deliver.
- ✅ **Stall nudge** — a final answer that stops with a prose permission-ask
  ("should I...", "need to ask you...") is nudged to call clarify or just
  deliver the answer (fires regardless of prior tool use).
- ✅ **Tool-loop breaker** — the same exploration tool (glob/grep/shell_exec/
  index) called 6+ times in a turn nudges the model to converge (text loops
  were already caught by the loop guard; this catches tool-call loops).
- ✅ **Convergence nudge** — 12+ read-only calls with no write and no answer
  nudges the model to deliver based on what it has.
- Golden evals 23–25 lock in the stall nudge, tool-loop breaker, and
  convergence nudge.

Honest limit found: a single 15-item meta-review prompt is near the ceiling of
a 9B Q4 in one autonomous turn — with the reasoning cap it reaches the
deliverable but still takes minutes. Use `sampling.reasoning_max_tokens`,
smaller task slices, or `--goal` rounds for such prompts.

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

## Review batch 2026-08-13 (7 proposals — 3 shipped, 4 rejected)

A seven-proposal "local-LLM-first" review (2026-08-13). Screened against the
codebase; three implemented (all tested), four rejected. **Rejected items are
closed with reasons — do not re-propose them without new evidence.**

### Shipped

- ✅ **diff_semantic symbol-delta guardrail** (proposal #2) — `fs_edit`/`fs_patch`
  now compare the file's exported top-level declarations before/after a write
  (`index.ExportedSymbols`: Go = uppercase, Python = non-dunder, others = all
  names) and block an edit that would silently delete a public symbol (renames
  too — the error tells the model to use `fs_refactor`). Layered on the existing
  `preflightSyntax` check; `fs_write` (full rewrites) is exempt. Covered by
  `TestPreflightBlocksExportedDeletion`.
- ✅ **`yagent export-dataset`** (proposal #5) — `internal/dataset` converts
  verified session trajectories to OpenAI-chat or ShareGPT JSONL, dropping
  failed turns (empty assistant replies, `[redacted]`/`[home]` markers, scrubbed
  tool args). CLI: `--output`, `--format openai|sharegpt`, `--session`,
  `--min-messages`. No new deps (own JSON encoder).
- ✅ **VRAM pressure detector** (proposal #6) — `vram_threshold_tps` (default 5,
  `YAGENT_VRAM_THRESHOLD_TPS`, `/settings` + `/set`): the agent measures each
  stream's t/s (content + reasoning), flags pressure when it drops below the
  threshold (the KV-cache spill signature), the next `budget()` force-prunes
  old tool output even when under the window, and the TUI shows a `⚠ VRAM` pill
  until it clears. Covered by `TestVramPressureDetectAndPrune`.

### Rejected (do not re-propose)

- ⚪ **`code_skeleton` AST focal-point folding** (proposal #1) — *already
  covered by three existing tools:* `code_slice(path, symbol)` returns exactly
  the focal declaration's span (~80% cheaper than fs_read), `code_outline`
  lists signatures without bodies, and the code-injection path already
  collapses chunk bodies to `// …` under budget. A "focal symbol + folded
  neighbors" variant adds a fourth tool with marginal savings.
- ⚪ **In-memory workspace staging overlay (`/stage`)** (proposal #4) — a
  virtual filesystem layer across all write tools is a large architectural
  change for marginal safety gain: `/undo` (per-turn write buffer), `/checkpoint`
  (goal-mode snapshots) and `shell.sandbox: bwrap` already cover the
  rollback/safety story, including under `--yolo`.
- ⚪ **`task_decompose` multi-file decomposer** (proposal #7) — subagents are
  **read-only by design** (M7 v1) and already have parallel `tasks[]`; a
  writable multi-file decomposer requires the C3 structured subagent workspace,
  which is deliberately **gated** (the live fidelity eval shows string summaries
  lose zero facts). Contradicts the design until C3 un-gates.
- ⚪ **Pythonic tool-call format support** (LFM2.5-style `[tool(args)]`) —
  Yagent speaks OpenAI `tool_calls` end-to-end; llama.cpp already bridges
  Pythonic models to that format. LFM2.5's card says to force JSON via the
  system prompt (a per-model prompt hint, not a parser) and admits it is "not
  the best fit for heavy programming". A format-translation layer for one
  mid-tier model is not worth the complexity. If a future top-tier model is
  Pythonic-native, revisit.

## Review batch 2026-08-13 (second agy review — 6 proposals: 4 shipped, 2 rejected)

A follow-up agy review (2026-08-13) screened against improvement.md — none of
its proposals repeat the closed/rejected items above. Four implemented (all
tested), two rejected with reasons. **Rejected items are closed — do not
re-propose them without new evidence.**

### Shipped

- ✅ **Diagnostic error sanitizer + signature grouping** (#1) — `capResult`
  now runs a `groupErrorCascade` pass after `compactLines`: when output is an
  error cascade (≥5 error lines, ≥30% of the output), errors are grouped by
  signature (path:line:col stripped) and the top 3 distinct root causes are
  kept with their first precise pointer plus a fold count
  (`… and N more error lines in M other signatures omitted`). Normal tool
  output passes through untouched. Covered by `TestGroupErrorCascade` +
  `TestCapResultGroupsCascade`.
- ✅ **fs_edit whitespace soft-normalization** (#2) — when `old_string` isn't
  found exactly, `fs_edit` tries a leading-whitespace-normalized match (tabs
  vs spaces per line). If it lands at exactly one span, the edit is applied to
  the on-disk text, new_string is re-indented with the file's own indentation,
  and the result is marked `[auto-aligned whitespace indentation]`. Ambiguous
  matches never auto-apply. Covered by `TestEditWhitespaceNormalizedMatch`.
- ✅ **`code_topology` tool** (#4) — `index.BuildTopology` scans the workspace
  once (gitignore-aware) and renders a compact package-level import DAG:
  module path, which package dirs import which local packages, and entry
  points. Reads import statements directly (tree-sitter queries per language,
  Go module-prefix + relative + bare-dir resolution), no index/embedding
  needed. Offered with the code-tool group. Covered by `TestBuildTopology` +
  `TestCodeTopologyTool`.
- ✅ **`export-dataset --format dpo`** (#6) — the exporter gained a DPO/ORPO
  preference mode: within each user turn, a failed tool call (rejected) is
  paired with the eventual successful call/answer (chosen), emitting
  `{"prompt","chosen","rejected"}` lines. The model's self-correction IS the
  preference signal; turns without a failure yield no pair. Covered by
  `TestExportDPO` + `TestExportDPORequiresFailureThenSuccess`. (The proposed
  `--distill` collapse is not separate: openai/sharegpt export already drops
  failed turns, so the clean-trajectory case is covered.)

### Rejected (do not re-propose)

- ⚪ **Zero-LLM "static" subagent mode** (#3) — a mode that executes one
  deterministic structural query (call-graph / symbol search / regex) without
  the LLM is just calling the tool directly: the parent agent already has
  `code_references`, `index_search`, `code_outline`, and now `code_topology` in
  its read-only set. A static subagent adds no capability the parent doesn't
  already hold.
- ⚪ **TUI steering hotkeys (Tab / Ctrl+D / Ctrl+E)** (#5) — all three keys are
  taken: Tab = command completion, Ctrl+D = viewport scroll-down, Ctrl+E =
  textarea cursor-to-line-end. Remapping them would break existing UX for
  marginal gain; `/yolo`, `clarify`/`plan`, `/retry` and the approval prompts
  already cover developer steering.

## Review batch 2026-08-13 (third agy review — 6 proposals: 3 shipped, 3 rejected)

A third agy review (2026-08-13) screened against improvement.md — none of its
proposals repeat closed/rejected items. Three implemented (all tested), three
rejected/covered with reasons.

### Shipped

- ✅ **`code_impact` change-radius tool** (#1) — given a file (or symbol), it
  computes the downstream change radius *before* an edit: every direct caller
  file with its call-site lines (from `index_calls`), every package that
  imports the touched package (from the import DAG), and the test files
  covering the touched + caller packages. Deterministic — zero LLM calls,
  ~150 tokens. Requires `index_repo` first. Covered by `TestCodeImpact`.
- ✅ **`test_runner` targeted tests** (#2) — runs the unit tests affected by a
  change, scoped to `package | file | symbol` (Go `go test -run`, pytest, or
  vitest/jest), and prunes output to failures + a summary (passing tests and
  per-test RUN/PASS lines collapse). Complements `workspace_diagnostics`
  (compile-only) with the semantic "did the logic break" loop. Fixed commands
  → no approval gate; 120s timeout; process-group kill. Covered by
  `TestTestRunner*`.
- ✅ **`/compact` session ledger** (#4) — `agent.Compact` distills the *entire*
  conversation (all history before the current turn) into a structured
  `[SESSION LEDGER]` (validated facts, decisions, failed approaches, active
  task) in one pass, replacing the historical turns and persisting the ledger
  as the running summary. The manual counterpart to `budget()`'s automatic
  pressure-driven summarization. Available in both UIs (`/compact`); REPL help
  and TUI `/` menu updated. Covered by `TestCompactDistillsWholeHistory`.

### Rejected / covered (do not re-propose)

- ⚪ **KV-cache prefix alignment** (#3) — **already effectively optimal.**
  `assembleContext` already leads with the static system prompt and skills L0,
  then appends the dynamic sections (summary, recall, code, injected, ledger)
  in stable order. The cacheable prefix is already at the strict top;
  reordering the dynamic tail cannot extend the llama.cpp prompt-cache hit.
- ⚪ **Native `git_status`/`git_diff`** (#5) — **already exists** as core
  read-only tools (`internal/tools/git.go`) with `staged`/`path` support; they
  already avoid shell-exec approval prompts.
- ⚪ **`yagent-lint` dynamic failure nudges** (#6) — **mostly covered** by the
  per-failure recovery nudges: prose tool-call nudge, truncated-JSON recovery,
  tool-loop breaker, convergence nudge, agent loop guard, and structured error
  envelopes. A persistent aggregate hint in the system prompt adds marginal
  value over the existing immediate per-failure feedback.

## Review batch 2026-08-13 (fourth agy review — 8 proposals: 4 shipped, 4 skipped)

A fourth agy review (2026-08-13, A–H) verified against improvement.md — none
repeat closed items. Four implemented (all tested), four skipped with reasons.
A reviewer's independent pass corrected three implementation details before they
shipped (syntax-only on refactor, cache invalidation, candidates-not-truth).

### Shipped

- ✅ **fs_refactor write guardrails** (A) — the rename tool was the only writer
  with no preflight. Now every rewritten file is validated with the tree-sitter
  syntax check **before any write** (all-or-nothing — a project-wide rename
  touches many files at once). The exported-symbol delta guardrail is
  deliberately NOT applied: a rename `Foo→Bar` removes `Foo` by design, so it
  would block every public rename. Covered by
  `TestRefactorPreflightBlocksBrokenRewrite`.
- ✅ **Structured-file preflight (YAML + JSON)** (B) — `preflightStructured`
  validates `.yaml`/`.yml` (yaml.v3) and `.json` (encoding/json) before write,
  blocking a malformed config/playbook/skill-frontmatter with a parse error
  instead of breaking the next reload cryptically. Wired into fs_write, fs_edit
  (both paths), fs_patch, and fs_refactor. TOML deliberately dropped — no dep
  and Yagent config is YAML. Covered by `TestPreflightStructuredFiles`.
- ✅ **Cross-turn read-tool result cache** (C) — pure read tools
  (grep/glob/index_search/code_references/code_outline/code_slice/
  code_topology/code_impact/code_unused) memoize results keyed by canonical
  (tool, args) — key order/whitespace normalized. **Invalidated by any
  write/destructive tool (and index_repo)**, so a cached answer can never
  outlive the change that made it stale; network/git/diagnostics excluded.
  Bounded at 64 entries. Returns a `[cached result]` marker. Distinct from the
  loop-breaker (which nudges; this answers). Covered by `TestReadResultCache` +
  `TestDispatchCachesAndInvalidatesReads`.
- ✅ **`code_unused` dead-symbol candidates** (H) — inverts `index_calls`:
  exported top-level symbols with zero call sites anywhere (tests are indexed,
  so test-only symbols are NOT reported). Explicitly labeled **candidates, not
  truth** — interface implementations and dynamic dispatch produce no call
  edges, so the model must verify before deleting. Covered by `TestCodeUnused`.

### Skipped (do not re-propose)

- ⚪ **Local fine-tune pipeline / axolotl + QLoRA config** (D) — `export-dataset`
  already emits OpenAI/ShareGPT/DPO JSONL; the missing piece is a training
  script/config, which is plumbing rather than agent logic and trains models
  the project doesn't distribute. A documentation note / `--format axolotl` is a
  possible future add, but it is not a Yagent runtime feature.
- ⚪ **Embedding request batching** (E) — **already done** (the proposal's claim
  was false): `embedChunks` batches 8 chunks/request (index.go) and
  `client.Embed` sends them in one HTTP call (client.go) under a single
  SlotLock acquisition.
- ⚪ **Reasoning cache for retried/identical prompts** (F) — too risky: replaying
  a stale chain-of-thought alongside fresh content invites incoherent answers,
  and the loop-guard already re-streams identical context. The 25s→5s win is
  already captured by `sampling.reasoning_max_tokens`.
- ⚪ **Adaptive reasoning budget** (G) — a write-vs-read heuristic for the
  reasoning cap is exactly the kind of tuning that needs real model evidence;
  `reasoning_max_tokens` is a deliberate manual knob. No quality proof offered.

## Strategic roadmap (2026-08-13, self-generated — evidence-gated direction)

The project is feature-complete: every M1–M7 milestone and all post-M6 review
batches are shipped. The remaining backlog (C3 structured subagent returns, M7
deeper orchestration) is deliberately gated on evidence. The highest-value next
work is therefore (a) producing that evidence, and (b) fixing the one
limitation that repeats across every model benched. Ordered by leverage:

### T1-1 — Long-horizon goal stress-test (decides the C3/M7 roadmap)

The backlog question is: *is the single loop the bottleneck on real multi-file
work?* The eval harness is fake-server and the live bench measures single-turn
tool basics — neither answers it. Build a **scripted autonomous run**: one
multi-file refactor goal over a real repo, checkpointed, `--rounds 8`, with the
live-fidelity pattern applied (N facts buried in decoy prose, recall measured
after the run). The outcome either closes C3 permanently ("single loop is not
the bottleneck at this scale") or finally un-gates it with a concrete failure
case. This single experiment decides the remaining roadmap.

**Shipped 2026-08-13**: `TestLiveGoalStress` (`internal/eval/goal_stress_test.go`,
opt-in `YAGENT_LIVE_EVAL=1`). Fixture: a Go module where `Config` must move
from `pkg` into a new `pkg/config` package, with `main.go` + tests re-wired,
plus 4 decoy note files carrying facts the refactor must not clobber. The agent
runs `RunGoal(goal, 8, …)` with VerifyWrites on (production config) and the
harness measures GOAL DONE / rounds / wall time / new-package-exists / imports
rewired / decoy facts intact / "wrote any file". The last metric matters: a
DONE with zero writes is a **hallucinated completion**.

**First evidence (Qwen3VL-8B Q4 on :8089, `--repeat`-style fresh runs):**

| run | rounds | wall | new pkg | imports rewired | wrote any file | correct |
|---|---|---|---|---|---|---|
| 1 (VerifyWrites off — harness bug, not valid) | 1 | 29s | ✗ | ✗ | **✗ (claimed done, wrote nothing)** | ✗ |
| 2 (VerifyWrites on) | 1 | 6m24s | ✓ | ✗ | ✓ | ✗ |
| 3 (VerifyWrites on, fresh) | 1 | 29s | ✓ | ✓ | ✓ | ✓ |
| 4 | 0 | 40s | ✓ | ✗ | ✓ | ✗ |
| 5 | 1 | 11m35s | ✓ | ✗ | ✓ | ✗ |
| 6 | 0 | 3m39s | ✗ | ✗ | ✓ | ✗ |

**Verdict (N=5 valid, firmed up):** the single loop **fails 4/5** real multi-file
refactors at this scale, and the failure is *not* "can't start" — `wrote any
file` is true in every valid run. The recurring failure mode is **completion**:
the model creates the new package, then either **loops on the import wiring
until max-iterations** (runs 4, 6 — `rounds: 0`, no final answer) or **declares
DONE while narrating the remaining work** (runs 2, 5). The single success
(run 3) completed in 29s. Both failure modes slip past the existing nudges
because the final answer either *mentions* the pending work in prose (DONE path)
or never settles (loop path). **C3 stays gated** — a subagent would not fix
DONE-too-early or an import-wiring loop — but the stress-test gives a **concrete
actionable gap**: goal mode needs a deterministic completion check that refuses
DONE while the build/test still fails and breaks the import-loop with a targeted
nudge (e.g. "the old import still exists in main.go — run fs_edit on that file
now, not another plan"). Next: run the loop with that fix and re-measure.

**Fix shipped 2026-08-13 — `GoalGate`** (`agent.Config.GoalGate`, UI-enabled,
mirrors VerifyWrites): in `RunGoal`, after the model's DONE verdict, the agent
deterministically re-runs `workspace_diagnostics`; if the workspace still fails
its static check (`diagnosticsFailed` — compile/lint error markers), the DONE is
**refused**, the errors are fed back, and another round is forced. The model
cannot talk its way out of a failing build. Unit-tested
(`TestRunGoalGateRefusesDoneOnFailingBuild`, `TestRunGoalGateCleanBuildPasses`,
`TestDiagnosticsFailed`).

**Post-fix re-measure (Qwen3VL-8B on :8089, GoalGate on):**

| run | rounds | wall | new pkg | imports rewired | correct |
|---|---|---|---|---|---|
| 1 | 1 | 29s | ✓ | ✓ | ✓ |
| 2 | 0 | 35s | ✓ | ✓ | ✓ (did all work; hit max-iterations without emitting a final answer) |
| 3 | 1 | 32s | ✓ | ✓ | ✓ |

**Before vs after:** full-refactor correctness **1/5 → 3/3**; imports rewired
**1/5 → 3/3**; DONE-too-early **2/5 → 0**. The gate eliminated the
declared-DONE-before-finishing failure entirely. The one remaining miss is a
milder mode — the model finishes all the work but doesn't close with a final
answer (max-iterations). **C3 remains gated** (the single loop, with the gate,
is now reliable at this scale).

### T2-1 — Explicit fact extraction into memory during goal runs

Every benched model is flaky on **multi-turn recall** — the #1 repeated
weakness. `memory_save`/`memory_search` exist but weren't auto-wired into goal
runs. **Shipped 2026-08-13**: `agent.Config.GoalMemorize` (UI-enabled) — after
each `RunGoal` round, `memorizeGoalRound` deterministically persists the round's
touched paths, last tool failure, and goal text into the L3 memory store
(project preferred, deduped per agent instance so a multi-round goal doesn't
re-save the same fact). No LLM call — pure extraction from what the round
actually did. **Bug found during verification**: facts were only saved on
cleanly-finished rounds; moved the memorize call ahead of the round-error check
so a *failed* round's failure facts persist too (the most valuable kind). Unit-
tested (`TestMemorizeGoalRoundSavesFacts`, `TestMemorizeGoalRoundOnFailedRun`).

**Live evidence (Qwen3VL-8B on :8089)**: in a stalled round (import-wiring
loop, no DONE), the goal facts still persisted — 4 memories saved covering the
touched files (`main.go`, `pkg/config/config.go`), the goal text, and the exact
last failure ("syntax error at line 10, col 8 — pkg.config."). Searchable for a
resumed session (`--resume-goal`) or a later session. The feature does its job
even when the loop itself doesn't finish.

### T1-3 — Residual goal-loop failure: work done but no final answer

After GoalGate, the stress-test still showed a residual: runs that do ALL the
work (write files, update imports) then keep requesting tools until
max-iterations without emitting the closing answer (stress runs 2/4/6, and
post-gate run 2). The read-only convergence nudge never fired because it
requires `reads >= 12 && !wrote` — a model that *wrote* but won't stop looping
was invisible to it.

**Shipped 2026-08-13**: a **near-cap convergence nudge** in `agent.Run` — when
the loop is within 2 iterations of `MaxIterations` AND the turn has written a
file, the model is nudged: "You've made the changes and are near the iteration
limit. Stop making further tool calls — give your final answer now, summarizing
exactly what you changed." Fires once per turn. Covered by
`TestNearCapConvergenceNudge`.

**Re-measure (Qwen3VL-8B on :8089, post-nudge, N=3):** 2/3 fully correct
(runs 1 & 3, 38s and 11m41s); run 2 did new-pkg + main.go but looped on the
test file to max-iterations. Run 1's 11m41s case is the win — a run that
previously died at max-iterations was rescued by the nudge and closed with
DONE. Net: no regression, and the nudge rescues stalled runs that the model
would otherwise abandon. The residual (import/edit-wiring loop on a specific
file) is a model-level behavior the single nudge can't fully break — C3 stays
gated; if it recurs across runs, a follow-up is a loop-counter per specific
file rather than per tool.

### T1-2 — Live-bench regression gate

`yagent bench --repeat 3` is already run informally after every model swap.
Make it a recorded ritual: scores appended to a baseline file (e.g.
`docs/bench-baselines.md` or a JSON sidecar), and `yagent doctor` warns when the
configured model regresses vs. its recorded baseline. Prevents silently
shipping a worse model after a sampling/config change.

**Shipped 2026-08-13**: `bench.Baseline` — `yagent bench` records its pass score
to `<data_dir>/bench-baseline.json` (per model: best + last run, timestamped),
warns on stderr when the current run is below the model's own best (repeat≥2
only, so a single flaky run can't overwrite a solid best), and prints the
baseline at the end. `yagent doctor` reports the recorded baseline and raises a
WARN when the last run is below best. Covered by `TestBaselineRecordAndRegression`.
Live-verified: Qwen3VL-8B recorded 6/6; simulating a worse last-run produced the
doctor WARN with the exact re-check command.

### T2-1 — Explicit fact extraction into memory during goal runs

Every benched model (Qwythos, Ornith, LFM2.5, Qwopus) is flaky on **multi-turn
recall** — the #1 repeated weakness, and it's model-independent.
`memory_save`/`memory_search` exist but are not *auto-wired*: a long goal run
relies on the narrative summarizer. Add a deterministic hook: after each goal
round, extract `path:key` facts (touched files, decisions, failures) into
`memory_save` entries, injected cheaply next round. This targets the one
weakness no tool/guardrail currently addresses.

### T3-1 — Web-result cache (bounded, TTL'd)

DDG scraping is slow and occasionally times out (a timeout appears in the DPO
export data). Cache successful `web_search`/`web_fetch` results per session to
avoid re-fetching identical queries and soften rate limits. Small, contained,
deterministic.

**Shipped 2026-08-13**: `web.Client` gained a session-scoped cache — search
results keyed by `(query, k)`, fetched pages keyed by URL, TTL 10 minutes,
bounded at 64 entries, `ClearCache()` for reset. Cache hits surface as
`[cached results]` / `[cached page]` markers in the tool output. Covered by
`TestWebSearchCache` + `TestWebFetchCache` (counting providers / hit counters,
no network).

### T3-2 — Offload verification to the second machine

Consult + summarizer already run on the laptop. Extend the same pattern to
output-heavy *verification* (`test_runner`, `workspace_diagnostics` results are
generated on the GPU host today): one config knob to run them on the offload
server, freeing the GPU slot. Same `summarizer:`-style wiring.

**Rejected 2026-08-13 — false premise.** `workspace_diagnostics` and
`test_runner` never touch the GPU: both run local CPU subprocesses via
`os/exec` (`go vet`/`go test`/`tsc`/`cargo check`) with zero references to the
LLM client or SlotLock (verified: no `llm.Client`/`acquireSlot` in either file).
There is no GPU slot to free — those commands are off-GPU by nature. The real
GPU contention is already solved by SlotLock (serializes inference) plus the
v0.1.30 summarizer/consult offloads (which move actual GPU work to the laptop).
Building T3-2 would require syncing the workspace + a full toolchain to the
laptop for the marginal benefit of freeing desktop *CPU*, which is not the
bottleneck on a GPU-bound workflow. Do not re-propose without a concrete GPU
usage in these tools.

### Explicitly NOT next

No new read/write tools, guardrails, exporter formats, the fine-tune pipeline
(D), or the verification-offload (T3-2, rejected on a false premise — the
verification tools are already CPU/off-GPU). The review batches keep proposing
these and they are done, covered, or plumbing. The feature surface is
saturated; the ROI is in the evidence items above.

## Adversarial QA batch (Hermes, 2026-08-13 — 15 findings, 6 fixed / 2 false / 7 not-bugs)

An external QA agent ran an adversarial breakage audit (malformed tool inputs,
edge workloads, hostile repos, concurrency, persistence) via temporary test
files driving real tool code. 15 findings. Disposition:

### Fixed (all tested)

- **checkpoint name traversal → workspace wipe** (#13) — `Restore(ws, "../..")`
  resolved to the workspace itself, `os.Stat` succeeded, and Restore then
  deleted the whole tree. `Restore` lacked `Save`'s `/\` guard.
- **checkpoint.Delete traversal → delete outside workspace** (#14) — same root
  cause. Both now go through `validateName` (rejects separators, `.`/`..`,
  non-clean paths); `Restore` also requires the snapshot be a real dir before
  wiping. Covered by `TestRestoreDeleteRejectTraversal`.
- **fs_patch out-of-range hunk → process panic** (#1) — `applyHunks` sliced
  `fileLines[:oldStart-1]` with no bounds check; a hunk past EOF with only
  additions panicked ("slice bounds out of range"), and there was no `recover()`
  anywhere, so one malformed diff killed the process. Now returns a structured
  error naming the hunk start. Defense-in-depth: the agent's dispatch wraps
  every tool Execute in a `recover()` so a tool panic degrades to an error
  result instead of killing yagent. Covered by `TestFSPatchRejectsOutOfRangeHunk`.
- **/undo phantom entries from rejected writes** (#5) — fs_write/fs_edit/fs_patch
  recorded the undo entry BEFORE preflight, so a rejected write left a phantom
  "revert" (and could re-write a stale version on the next undo). Record now
  happens after preflight passes, just before the actual write. Covered by
  `TestRejectedWriteLeavesNoUndoEntry`.
- **shell_bg ran outside the workspace** (#7) — `jobs.Start` never set
  `cmd.Dir`, so a background job resolved relative paths against yagent's cwd
  (only the bwrap path was correct). `StartIn(command, dir)` added; shell_bg
  passes the workspace. Covered by `TestStartInSetsWorkingDir`.
- **web_fetch SSRF: no scheme validation** (#2) — `http.NewRequestWithContext`
  accepted any scheme; `file://...` was handed to the HTTP client. Now only
  http/https with a host are fetched; other schemes are rejected before any
  request. Covered by `TestFetchRejectsNonHTTPScheme`.

### False claims (verified — no change)

- **vram_threshold_tps pathological values force-prune a healthy session** (#8)
  — the claim is backwards: the check is `tps < threshold`, so a *tiny*
  threshold can't fire (2 t/s > 1e-9), and negatives are already rejected by
  config validation AND treated as disabled in `detectVramPressure`. Verified
  with boundary tests.
- **web_search(negative k) silently accepted** (#3) — `k <= 0` is clamped to
  the default 5 before searching; that is correct behavior, not a bug.

### Not bugs (confirmed working)

- #6 fs_refactor rewrites strings/comments — documented word-boundary behavior,
  and the result is surfaced in the tool output.
- #4 web_fetch returns raw bytes for a binary body — the fetch path is
  text-oriented but not harmful; not worth changing.
- #9 /compact on empty session, RunGoal max-rounds, MaxIterations 0/-1 — all
  degrade safely.
- #10 adversarial repos (deep nesting, huge files, symlink escapes, case
  collisions, invalid UTF-8) — all handled correctly (fs_read rejects symlink
  escapes and binary files).
- #11 concurrency (16 parallel writes/reads) — race-clean, no corruption.
- #12 shell_kill mid-run / double-kill / subagent recursion — prompt and safe.
- #15 persistence (session/memory/skills stores) — hostile inputs rejected,
  corrupted DB rejected at Open, no injection/wipe.

### QA harness note

The report's `go test ./...` "fails only on the QA harness tests" is expected —
those throwaway `*_test.go` files asserted the broken behavior and were not
part of the shipped tree.

## Golden-eval expansion for the v0.1.32–v0.1.35 deterministic fixes (2026-08-13)

The golden eval suite (testdata/evals) stopped at 25 (convergence nudge), but
v0.1.32–35 shipped several deterministic behaviors that were unit-tested yet
not locked into the fake-server regression net. Added:

- **26-goal-gate** — the GoalGate refuses a DONE verdict while go vet still
  fails (undefined fmt), feeds the error back, and forces a second round that
  fixes the import. Harness gained `goal_gate`.
- **27-fs-patch-bad-hunk** — a malformed fs_patch hunk (start past EOF, only
  additions) returns an error naming the hunk start instead of panicking; the
  file is untouched.
- **28-web-fetch-scheme-guard** — web_fetch rejects `file://` before any
  request ("unsupported scheme").
- **29-goal-memorize** — goal rounds persist touched-path facts to L3 memory.
  Harness gained `goal_memorize` and the `memory_contains` assertion (searches
  the eval's vector store after the run).

The eval harness Task now exposes `goal_gate`, `goal_memorize` and
`memory_contains`, so future goal-loop/memory behavior can be locked in the
same way. All evals run offline against scripted servers (no network).

## Golden-eval expansion round 2 — v0.1.28/v0.1.31 deterministic tools (2026-08-13)

Second sweep of untested deterministic behavior. Added:

- **30-fs-edit-whitespace-align** — fs_edit auto-aligns a tabs-vs-spaces
  old_string that lands at exactly one span, with the `[auto-aligned whitespace
  indentation]` marker and file-style indentation preserved.
- **31-diff-semantic** — fs_edit blocks deleting an exported symbol
  (diff_semantic), the file keeps PublicAPI.
- **32-structured-preflight** — a malformed YAML write is blocked before it
  reaches disk ("YAML" error, file absent).
- **33-read-tool-cache** — a repeated identical grep returns the
  `[cached result]` marker (pure-read memoization).
- **34-fs-refactor-guardrails** — a workspace-wide rename rewrites every file
  (all-or-nothing preflight) and updates all occurrences.
- **35-code-unused** — code_unused lists the dead exported symbol but not the
  live one. Harness gained `tool_results_not_contain` (negative assertion).

32 evals total, all offline against scripted servers.

## Golden-eval expansion round 3 — structural tools, jobs, /compact (2026-08-13)

Third sweep. Added:

- **36-code-topology** — code_topology renders the package import DAG from
  import statements (module path, per-package edges) with no index needed.
- **37-code-impact** — code_impact computes the change radius of a symbol
  (caller file + covering test file) after index_repo.
- **38-shell-bg-lifecycle** — background jobs start (`started job`), an unknown
  id returns `unknown job`. Harness gained `jobs` (wires a jobs.Registry).
- **39-compact-ledger** — /compact distills the conversation via the
  summarizer; the harness runs `a.Compact` after the turn and asserts the
  history length shrank to the current turn only. Harness gained `compact` and
  `compact_history_len`.

36 evals total, all offline against scripted servers.

## agy review batch 2026-08-13 (8 proposals — 4 shipped, 4 rejected)

An agy audit screened against improvement.md. Four implemented (all tested),
four rejected with reasons (verified against the code, not the prose).

### Shipped

- ✅ **error_fix_hints** (#4) — `errorFixHints` appends deterministic,
  language-specific micro-recipes to failing diagnostics/test_runner output
  (Go undefined → index_search+fs_edit; TS cannot-find-name; Rust E0432;
  Python ModuleNotFound; generic build-failed fallback). Wired into the
  goalGate DONE-refusal and the verifyBarrier feedback so a looping model gets
  "do THIS tool call" instead of re-guessing. Covered by `TestErrorFixHints`
  + eval 40.
- ✅ **`code_environment`** (#5) — read-only toolchain/env audit: installed
  compilers/interpreters (go/gcc/rustc/node/python3/pkg-config/bwrap/docker),
  env flags (CGO_ENABLED/CC/GOFLAGS/…), and native-binding detection (cgo
  `import "C"`, Rust `extern "C"`, C includes, node-gyp). Tells the model "this
  is an environment problem, don't edit source" before it wastes turns.
  Covered by `TestCodeEnvironment` + `TestScanNativeBindings` + eval 41.
- ✅ **Multi-turn undo** (#6) — `undo.Buffer` already stored turn-indexed
  entries; added `Turns()`/`Count()`/`UndoN(n)` and the `/undo list` + `/undo
  <N>` commands (REPL + TUI `/` menu). `/undo list` shows per-turn files;
  `/undo 3` reverts the 3 most recent turns (all-or-nothing). Covered by
  `TestUndoListAndUndoN` + `TestUndoNReversesMultiTurnOrder`.
- ✅ **Subagent-offload nudge** (#8) — when context exceeds 75% of the window
  during read-only exploration (reads ≥ 6, no write), the loop nudges the model
  to delegate the remaining exploration to a subagent and keep the main context
  lean. Distinct from the convergence nudge (which says "deliver"). Covered by
  `TestSubagentOffloadNudge`.

### Rejected (do not re-propose)

- ⚪ **Strict static-prefix KV-cache layout** (#1) — **false / duplicate.** The
  static system prompt + workspace rules are already first and byte-identical
  in `assembleContext`; the dynamic items (L0 index, summary, recall, task
  state) follow it, so llama.cpp caches the static prefix. Tool schemas are a
  separate API field, never in the prompt. Same finding as the v0.1.29
  rejection of KV-cache prefix alignment.
- ⚪ **Compact schema mode** (#2) — dynamic tool-schema filtering already exists
  (`activeToolSchemas`, M6.11); stripping parameter *descriptions* adds a
  tool_help tool + adaptive thresholds for marginal savings and risks confusing
  small models.
- ⚪ **fs_diff_preview AST dry-run** (#3) — fs_edit/fs_patch already return
  `simpleDiff` and the TUI shows colorized diff overlays on approval; an AST
  "mutation breakdown + shadowing" across 12 grammars is heavy for modest
  value.
- ⚪ **/skills distill** (#7) — post-goal playbook distillation + end-of-turn
  skill creation (5+ tool calls) already cover it.

## Model-guidance refresh (2026-08-13)

Re-ran `yagent bench --repeat 3` on the current default (Qwen3VL-8B-Instruct,
Q4_K_M, 32k Q8 KV on :8089): **17/18** — effectively holding the original
18/18, with the single miss a `fuzzy-path` flake (the documented-flakiest task,
swings run-to-run). The bench regression gate (v0.1.36) recorded 17/18 as the
new baseline and produced no regression warning. Verdict: **the default model is
validated, no change needed** — no newer Qwen3.5/4-family model is worth
chasing; docs updated to the honest 17/18. Ready for real-world soak.

## 2026-08-13 real-use bugfix: dead summarizer broke every turn

Two bugs found in real use (first messages in a fresh session):

1. **Configured-but-unreachable summarizer broke every turn.** The `summarizer:`
   section points at the laptop; when it's offline (no route to host), budget()
   failed hard with "summarize history: chat stream failed" — there was NO
   fallback to the main model. Fixed: budget() and /compact now fall back to
   the main model when the offloaded summarizer errors (the summarizer is an
   optimization, not a dependency). Covered by `TestSummarizerFallbackToMain`.
2. **VRAM pressure false-positive on warm-up.** A freshly-restarted server
   streams the first answer slowly (shader warm-up, ~1.4 t/s), which set the
   pressure flag (`tps < 5`), which forced budget() to summarize a healthy
   first turn — which then hit the dead laptop. Fixed: `detectVramPressure`
   requires ≥ 32 streamed tokens before flagging, so a one-line warm-up answer
   can't trigger pressure (a real KV-spill stall happens on sustained long
   generation). Covered by the extended `TestVramPressureDetectAndPrune`.

## agy review batch 2026-08-14 (8 proposals — 6 shipped, 2 rejected)

- ✅ **VectorStore.Search zero-count short-circuit** (#1) — an empty memory
  store now returns before the embedding request/SlotLock, so clean-slate
  projects skip a wasted GPU round-trip per turn. Covered by
  `TestVectorSearchEmptyStoreSkipsEmbed`.
- ✅ **subagent schema array types** (#2) — `tasks`/`tools` declared as
  `{"type":"array","items":{"type":"string"}}` (new `arrayProp` helper),
  matching the `[]string` decode; GBNF-constrained backends no longer reject
  arrays. Covered by `TestSubagentSchemaArrayTypes`.
- ✅ **Rust test_runner support** (#3) — `cargo test` for Cargo.toml projects
  (`cargo test -- <symbol>` for symbol scope). Covered by `TestTestRunnerRust`.
- ✅ **ambiguous_match line guidance** (#4) — the fs_edit error now lists the
  exact match line numbers (`matches 2 times at lines 14, 45`) so the model
  disambiguates in one turn. Covered by `TestFSEdit`.
- ✅ **doctor project toolchain check** (#5) — `yagent doctor` detects the
  cwd's project type (go.mod/Cargo.toml/package.json/pyproject) and verifies
  the matching tool is on PATH, so diagnostics/test_runner never hit a missing
  binary. Covered by `TestDoctorProjectToolchain`.
- ✅ **per-file edit stall counter** (#6) — failed write calls are now counted
  per target FILE (not identical signature), catching the "same file, minor
  variations" loop from the stress test. Nudge names the file and retry path.
- ⚪ **code_slice heuristic fallback** (#7) — rejected: a `[heuristic span]`
  line-window for ungrammar'd files risks the model trusting approximate
  slices; the audit itself flags the tradeoff. Marginal value.
- ⚪ **Headless browser / cloud swarms** (#8) — correctly out of scope
  (single-binary local-first; pure HTML extraction already covers web reading).

## Review & Hardening Pass 2026-08-14 (6 findings — all shipped & tested)

A holistic codebase review uncovered six concrete consistency, tool-visibility, and runtime gaps:

- ✅ **Dynamic Tool Schema Group Completeness** — `coreToolNames` was missing
  `fs_patch`, `test_runner`, and `subagent`, meaning chat turns with dynamic
  schema filtering never sent these tool definitions to the LLM. Added them to
  `coreToolNames`, and added `jobToolNames` (`shell_bg`, `shell_logs`, `shell_kill`,
  `scratch_write`, `scratch_read`) triggered on background/job signals. Covered by
  `TestActiveToolSchemasFilters`.
- ✅ **Multi-line ambiguous_match line guidance** — `matchLines` in `fs_edit`
  previously split content by line and called `strings.Index(ln, target)`, which
  always returned -1 when `target` was multi-line. Rewritten to compute match
  positions on full content with newline offsets. Covered by extended `TestFSEdit`.
- ✅ **`applySetting` Configuration Parity** — `applySetting` in `repl.go` was
  missing handlers for `api_key`, `vram_threshold_tps`, `summarizer.server_url`,
  and `summarizer.model`, causing changes from `/settings` or `/set` to only
  persist to disk without updating active runtime state. Handlers added.
- ✅ **REPL & TUI Command Parity** — Added `/sessions` command to REPL to list
  sessions directly; updated `/help` in both REPL and TUI to list all available
  commands (`/checkpoint`, `/playbook`, `/undo list`, `/undo <N>`, `/retry`,
  `/compact`, etc.); added all slash commands to TUI `slashCommands()` for tab
  completion.
- ✅ **Polyglot & C/C++ Doctor Toolchain Diagnostics** — `addProjectToolchain`
  in `internal/doctor/doctor.go` previously stopped on the first marker with a
  switch statement and lacked C/C++ checkers. Now verifies all present markers
  in polyglot repos and audits `make`/`cmake`.
- ✅ **Shell Completion & CLI Usage Synchronization** — `bashCompletion`,
  `zshCompletion`, and `usage()` in `cmd/yagent/main.go` updated to include
  `init`, `backup`, `export-dataset`, `--format dpo`, `--min-messages`, and
  `--repeat`.


## agy review batch 2026-08-14 round 2 (6 proposals — all shipped)

- ✅ **fs_patch targets in the progress ledger / goal memory** (#1) — touchedPaths
  now parses fs_patch's result ("patched N file(s): a.go, b.go") so multi-file
  patches appear in TASK STATE and L3 goal memory, not just single-path writes.
- ✅ **playbook `tests:` success predicate** (#2) — phases can require passing
  unit tests (optionally filtered by symbol name) before completing; evaluated
  via test_runner, like the diagnostics check. Supports TDD/refactor phases.
- ✅ **playbook diagnostics check aligned with DiagnosticsFailed** (#3) —
  exported `agent.DiagnosticsFailed`; the phase check now uses the same
  failure determination as the goal gate, so an informational banner on a
  clean run no longer false-rejects a phase.
- ✅ **code_environment toolchain completeness** (#4) — added make, cmake, git
  to the audited binaries, so C/C++ and repo tooling availability is known
  before the model runs commands.
- ✅ **errorFixHints expansion** (#5) — new micro-recipes for Rust
  cannot-find-in-scope (E0425/E0433/E0412), Python ImportError, and C/C++
  undefined-reference / missing-header (with an explicit "do NOT create a stub
  header" guard). Covered by the extended TestErrorFixHints.
- ✅ **recall/task-ledger dedup** (#6) — recalled memories that restate a path
  in touchedPaths or the current failure (GoalMemorize facts) are filtered
  from the same system message, saving ~50-100 tokens/turn on long runs.

## agy review batch 2026-08-14 round 3: TUI Overhaul & Interaction Parity (6 features — all shipped & tested)

A focused TUI improvement pass implemented six major interactivity, visual consistency, and workflow enhancements:

- ✅ **Prompt & Command History** (#1) — In-memory readline-style prompt history
  buffer (`history`, `historyIdx`, `draftInput`). Up/Down arrows on single-line
  or empty inputs cycle through past queries/commands with draft preservation.
  Covered by `TestPromptHistoryNavigation`.
- ✅ **Interactive Help Modal** (#2) — Centered 2-column modal (`helpView`)
  triggered by `/help`, `F1`, or `?` on empty input. Lists all keyboard shortcuts
  and categorized slash commands with themed styling. Covered by `TestHelpModal`.
- ✅ **Interactive Checkpoints Manager Modal** (#3) — `/checkpoint` or `/checkpoints`
  opens a centered snapshot manager (`checkpointsView`): lists snapshots with
  timestamps, `r` to restore, `d` (twice) to delete, `esc` to close. Covered by
  `TestCheckpointsModal`.
- ✅ **Smart LCS Context Hunk Differ** (#4) — Replaced naive index-based line
  diff in `renderApprovalDiff` with a full Longest Common Subsequence (LCS) differ
  (`internal/ui/diff.go`) with common prefix/suffix trimming and 3-line context
  hunk formatting (`···` breaks). Covered by `TestLCSDiffHunks`.
- ✅ **Active Workflow Header Indicator** (#5) — The header bar dynamically
  displays a highlighted badge (e.g. `🎯 goal: ...` or `🎯 playbook: ...`) when an
  autonomous goal or multi-stage playbook is active.
- ✅ **Quick Session Save Shortcut** (#6) — `Ctrl+S` exports the active session
  as Markdown (`session-<id>.md`) and reports a status confirmation without
  leaving the TUI. Covered by `TestQuickSaveSessionShortcut`.


## agy review batch 2026-08-14 round 4 (5 proposals — all shipped)

- ✅ **Multi-line nearestLineHint** (#1) — window matching: a multi-line
  old_string slip now reports the nearest N-line span (e.g. "lines 3-5")
  instead of failing entirely, so a small multi-line edit error recovers in
  one turn. Covered by the extended `TestNearestLineHint`.
- ✅ **Conversational gating for code/memory lookup** (#2) — `codeIntended`
  skips semantic `codeIndex`/`recall` for pure continuations ("ok", "yes",
  "continue", "thanks", short no-signal phrases), saving an embedding
  call/SlotLock and ~2k tokens of noise on quick chat turns. Covered by
  `TestCodeIntendedGating`.
- ✅ **ROOT GOAL anchor in TASK STATE** (#3) — in goal mode, the ledger pins
  the original objective at the top of every request (`- ROOT GOAL: ...`), so
  constraints don't dilute as history is pruned/summarized across 8+ rounds.
  `goalMode` flag ensures it only appears in autonomous runs, not interactive
  chat.
- ✅ **Oscillating edit-loop detector** (#4) — a rolling 4-slot ring detects
  the A-B-A-B 2-file flip-flop that slips past the per-file counter and nudges
  the model to use code_references/code_impact instead of editing back and
  forth. Covered by `TestOscillationDetection`.
- ✅ **Server perf diagnostics** (#5) — doctor probes `/props` for KV cache
  quantization and flags large-context-with-f16-KV spill risk on ≤12 GB GPUs
  with the optimal launch flags. Best-effort: builds that don't expose cache
  type get a clean INFO, not a false warning. Covered by
  `TestAddServerPerfLargeContextWarns`.

## Audit-fix batch 2026-08-15 (GPT sol review — P0 findings verified against the code)

Two external reviews (GPT sol / AGY) landed 2026-08-15 (`ideas/` untracked).
Every P0 claim was verified against the code before shipping (all confirmed
accurate):

- ✅ **Exit-status trust** (GPT sol #1) — `workspace_diagnostics`/`test_runner`
  discarded `cmd.Wait()` errors, so a non-zero exit was returned as success and
  the goal/test gates inferred failure from prose. Both tools now surface the
  exit code and prefix `[PASS]`/`[FAIL]`; `DiagnosticsFailed`/`TestsFailed`
  trust the marker first. The realistic failure (a checker failing with empty
  or unusual output) is now caught deterministically.
- ✅ **Successful writes only arm write state** (GPT sol #2) — `dispatch`
  marked `unverifiedWrite`/`touchedPaths`/`turnWrote` for any non-read-only
  call, even one that returned `error:`. A failed `fs_edit` no longer arms the
  verify barrier, pollutes the ledger, or invalidates the read cache.
- ✅ **Plan-mode enforced in dispatch** (GPT sol #3) — hiding write schemas was
  advisory (a hallucinated `fs_write` still ran, subject only to the
  approver). While plan mode is on, `dispatch` rejects any non-read-only call
  with a pointer back to `plan`.
- ✅ **Truncation-placeholder guard** (AGY P0) — `fs_write` blocks content with
  "// ... existing code ..."-style ellipses before it hits disk (safe for Go
  variadics, Python Ellipsis stubs, and `// Example: a, b, c...` comments).
- ✅ **Test-gate scope** (GPT sol #4) — `testGateCheck` tests every uniquely
  touched file, not just `touched[0]` (whole-project fallback when nothing was
  touched).
- 🟡 **Deferred as queued (P1, from AGY)**: proactive tool-output sliding
  window (prune read results older than ~2 turns), fenced/markdown tool-call
  extraction (rescue a ````json```-wrapped call), and path/quote sanitization —
  all value-carrying but lower-risk than the P0 correctness fixes; tracked for
  the next pass.
- ⚪ **Not a fit (agreed)**: C3 structured returns (gated on eval evidence),
  `/steer`, fine-tune script, capability probing, permission policies.

Golden evals 52–54 + unit tests; `go test -race` clean.
