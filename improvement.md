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

### T1-2 — Live-bench regression gate

`yagent bench --repeat 3` is already run informally after every model swap.
Make it a recorded ritual: scores appended to a baseline file (e.g.
`docs/bench-baselines.md` or a JSON sidecar), and `yagent doctor` warns when the
configured model regresses vs. its recorded baseline. Prevents silently
shipping a worse model after a sampling/config change.

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

### T3-2 — Offload verification to the second machine

Consult + summarizer already run on the laptop. Extend the same pattern to
output-heavy *verification* (`test_runner`, `workspace_diagnostics` results are
generated on the GPU host today): one config knob to run them on the offload
server, freeing the GPU slot. Same `summarizer:`-style wiring.

### Explicitly NOT next

No new read/write tools, guardrails, exporter formats, or the fine-tune
pipeline (D) — the review batches keep proposing these and they are done,
covered, or plumbing. The feature surface is saturated; the ROI is in the
evidence items above.
