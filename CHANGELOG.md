# Changelog

All notable changes to Yagent. Versioning: `git describe` via `make build`.

## v0.1.60 — 2026-08-14

### Added
- **`runtime_smoke` gate** — the codegen companion to the compile gate.
  After a write passes `workspace_diagnostics`, a final answer is also refused
  while the program **crashes at runtime**: the tool builds and briefly runs
  the generated program (Go `go build`+run, C/C++ g++/gcc with an `-lncurses`
  fallback, Cargo, Python) feeding scripted stdin, and reports PASS (survived)
  or FAIL (signal-killed, or output carrying panic/segfault/assertion markers,
  or a silent non-zero exit). Wired into both the `Run` loop and the `RunGoal`
  DONE verdict (codegen-only, `smokePassed` goes stale on any write). This
  converts the codegen guarantee from "compiles" into "compiles **and runs**" —
  the fix for the live-tested failures where Tetris compiled clean but crashed
  with a vector OOB and Snake died on its first food.
- **Golden eval 45** — codegen-smoke-gate: a compile-clean but panicking program
  is refused until fixed. Agent + tools unit tests cover crash detection,
  PASS/FAIL/no-runner/build-failure paths.

## v0.1.59 — 2026-08-14

### Added
- **`yagent chat --codegen`** — switches interactive chat to codegen mode; the
  autonomous build modes (`--goal`, `--resume-goal`, playbooks) auto-inherit it.
  Completion scripts (bash/zsh) updated.

## v0.1.58 — 2026-08-14

### Added
- **Codegen mode** (`codegen: true`, `/set codegen true`, `/settings`) — a
  greenfield-code strategy for small local models, so
  "build a program from scratch" turns succeed where incremental editing
  loops:
  - System prompt steers toward **one complete whole-file `fs_write` per file**
    instead of incremental `fs_edit` on text the model can't reproduce
    byte-for-byte.
  - **Compile-gated final answers** — after a write, a final answer is refused
    while `workspace_diagnostics` still fails (same deterministic gate as
    goal mode), so a turn can only end when the program compiles.
  - **Plan-narration-as-stall** — a final answer that lists "next steps...",
    "to finish, you can add...", etc. (instead of doing the work) is fed back
    until the work is done.
- **Golden eval 44** — codegen-plan-narration locks in the stall feed-back.

## v0.1.57 — 2026-08-14

### Added
- **Multi-line nearestLineHint** — fs_edit's nearest-match hint now handles
  multi-line old_string targets via window matching, so a small multi-line
  slip recovers in one turn.
- **Conversational gating** — semantic code/memory lookup is skipped for pure
  continuations ("ok", "continue", "thanks", short no-signal phrases), saving
  an embedding call and context tokens on quick turns.
- **ROOT GOAL anchor** — goal-mode TASK STATE pins the original objective at
  the top of every request, preventing objective drift across long runs.
- **Oscillating edit-loop detector** — an A-B-A-B 2-file flip-flop is detected
  and the model is nudged to understand the dependency instead of looping.
- **Server perf diagnostics** — yagent doctor probes KV-cache quantization and
  flags large-context spill risk on ≤12 GB GPUs with optimal launch flags.

## v0.1.53 — 2026-08-14

### Added
- **fs_patch tracked in the progress ledger / goal memory.** Multi-file patches
  now appear in TASK STATE and L3 goal facts (parsed from the tool result),
  not just single-path writes.
- **Playbook `tests:` success predicate.** Phases can require passing unit
  tests (optionally filtered by symbol) before completing — evaluated via
  test_runner, supporting TDD/refactor workflows.
- **Playbook diagnostics check aligned with `DiagnosticsFailed`.** Exported
  `agent.DiagnosticsFailed`; the phase check uses the same failure
  determination as the goal gate (informational banners no longer
  false-reject).
- **code_environment** now audits make, cmake and git.
- **errorFixHints** gained Rust cannot-find-in-scope, Python ImportError, and
  C/C++ undefined-reference/missing-header micro-recipes (with a "don't create
  stub headers" guard).
- **Recall/task-ledger dedup.** Recalled memories restating a touched path or
  the current failure are filtered from the same system message.

## v0.1.50 — 2026-08-14

### Added
- **Empty memory store short-circuit** — VectorStore.Search returns before the
  embedding request/SlotLock when the store has 0 entries (clean-slate
  projects skip a wasted GPU round-trip per turn).
- **subagent schema array types** — `tasks`/`tools` are now declared as string
  arrays (GBNF-safe), matching the `[]string` decode.
- **Rust test_runner** — `cargo test` for Cargo.toml projects, with symbol
  scope (`cargo test -- <symbol>`).
- **fs_edit ambiguous-match line guidance** — the error lists the exact match
  line numbers so the model disambiguates in one turn.
- **doctor project toolchain check** — verifies the cwd project's tool (go,
  cargo, node, python) is on PATH so diagnostics never hit a missing binary.
- **Per-file edit-stall counter** — failed write calls keyed by target file,
  so a model looping on the same file (minor variations) is nudged to re-read
  the exact text.

## v0.1.49 — 2026-08-14

### Fixed
- **Goal mode no longer freezes on a write tool.** An autonomous goal/playbook
  run is unattended, but the REPL approver prompted for a y/n on stdin when the
  model called a write/destructive tool mid-round — so the terminal appeared
  frozen and the run hung until `pkill -9`. Goal and playbook runs now always
  auto-approve writes (the goal checkpoint + `/undo` are the rollback safety
  net), and `AskUser` is left unset so `clarify`/`plan` aren't offered in an
  autonomous run. Verified live: a goal doing `fs_write` completes cleanly
  with no prompt and no hang.

## v0.1.48 — 2026-08-14

### Fixed
- **Failed-edit loop detector.** A model repeatedly attempting the same broken
  `fs_edit`/`fs_write`/`fs_patch` (wrong `old_string`, typically a whitespace
  or text mismatch) no longer grinds to max-iterations. Interleaved `fs_read`s
  defeated the consecutive-call dedup, and `fs_edit` wasn't in the tool-loop
  breaker set. Now: after 4 identical failed write signatures in a turn, the
  agent nudges the model to re-read the exact region and retry once with the
  corrected text (or use fs_write for a full replace). Covered by
  `TestFailedWriteLoopNudge` — found in real use (a Tetris-in-C++ session).

## v0.1.47 — 2026-08-13

### Fixed
- **C/C++ diagnostics respect the project's build system.** Bare
  `gcc -fsyntax-only` on all sources produced false "missing header" errors
  for real CMake/Make projects that rely on include dirs, which misled the
  model into inventing header files. `workspace_diagnostics` now prefers the
  actual build — `cmake --build <existing build dir>` (build/,
  cmake-build-*), `make -C <dir>` or a plain `make` — and only falls back to
  `gcc/g++ -fsyntax-only` for projects with no build system. Live-verified on a
  real CMake game project: reports the actual compile errors instead of fake
  missing headers. Covered by `TestDetectDiagnosticsPrefersBuildSystem`.

## v0.1.46 — 2026-08-13

### Added
- **C/C++ support in `workspace_diagnostics`.** A project with `.c`/`.cpp`/
  `.cc`/`.cxx` sources (or a Makefile/CMakeLists) and no higher-level manifest
  is now detected and checked with a syntax-only compile (`gcc`/`g++`
  `-fsyntax-only` over all sources — no linking, no build artifacts). C++ picks
  `g++`; the "no diagnostics configured" message now mentions C/C++.
  Covered by `TestWorkspaceDiagnosticsDetectC` + `TestDetectDiagnosticsCppPrefersGpp`.

## v0.1.45 — 2026-08-13

### Fixed
- **Skill creation via fs_write/fs_edit no longer prompts for approval.** The
  model sometimes writes a SKILL.md directly into the skills store instead of
  via `skill_manage`; those writes hit the generic y/n approver. Writes whose
  target path is inside a skills root (`.yagent/skills` or the global skills
  dir) are now recognized via a path-aware `SelfGatedFor` and governed by the
  skills gate (apply vs stage) — matching `skill_manage`. Non-skill writes
  still prompt. Covered by `TestSkillFsWriteSelfGated`.

## v0.1.44 — 2026-08-13

### Fixed
- **Instruction-echo on every turn.** Qwen3VL was ending each answer with an
  acknowledgment restating its instructions ("Understood. I will complete tasks
  directly without unnecessary pauses or confirmations, unless using the
  clarify tool..."). Two-layer fix: an explicit system-prompt rule forbidding
  instruction acknowledgment, plus a deterministic `stripInstructionEcho`
  post-filter that removes trailing acknowledgment-filler (Understood / I
  will / Proceeding with the task / Let me know if you'd like / …) from the
  final answer without ever splitting URLs, numbers or abbreviations.
  Verified clean across repeated greeting and task turns.

## v0.1.43 — 2026-08-13

### Fixed (found in live use)
- **Summarizer fallback.** A configured-but-unreachable `summarizer:` server
  (e.g. a laptop offline) no longer breaks every turn — `budget()` and
  `/compact` fall back to the main model when the offloaded summarizer errors.
- **VRAM pressure warm-up false positive.** The detector now requires ≥ 32
  streamed tokens before flagging, so a freshly-restarted server's slow first
  stream (shader warm-up) no longer triggers a needless force-prune/summarize
  of a healthy first turn.

## v0.1.41 — 2026-08-13

### Added
- **error_fix_hints.** Deterministic, language-specific micro-recipes appended
  to failing diagnostics output (Go undefined → index_search+fs_edit, TS/Rust/
  Python variants, generic fallback) so a looping model gets "do THIS tool call"
  instead of re-guessing. Wired into the GoalGate DONE-refusal and the
  verify-barrier feedback.
- **`code_environment` tool.** Read-only toolchain/env audit: installed
  compilers/interpreters, CGO_ENABLED/CC/GOFLAGS flags, and native-binding
  detection (cgo, extern "C", C includes, node-gyp). Tells the model "this is
  an environment problem" before it edits source.
- **Multi-turn undo.** `undo.Buffer` gained `Turns()`/`UndoN(n)`; the `/undo
  list` command shows per-turn files and `/undo <N>` reverts the N most recent
  turns all-or-nothing (REPL + TUI).
- **Subagent-offload nudge.** At >75% context during read-only exploration, the
  loop nudges the model to delegate the remaining reads to a subagent, keeping
  the main context lean.
- Golden evals 40–41 lock in the error-hint and code_environment behavior.

## v0.1.40 — 2026-08-13

### Added
- **Golden evals 36–39.** The fake-server regression suite now covers
  code_topology (package import DAG), code_impact (change radius), the shell_bg
  job lifecycle, and /compact (session ledger via the summarizer with history
  shrunk to the current turn). The harness gained `jobs`, `compact` and
  `compact_history_len` toggles. 36 evals total.

## v0.1.39 — 2026-08-13

### Added
- **Golden evals 30–35.** The fake-server regression suite now locks in the
  v0.1.28/v0.1.31 deterministic tools: fs_edit whitespace auto-align, the
  diff_semantic exported-symbol block, structured-file preflight (malformed
  YAML blocked), the read-tool result cache (`[cached result]`), fs_refactor
  all-or-nothing guardrails, and code_unused (dead vs live symbol). The harness
  gained a `tool_results_not_contain` negative assertion. 32 evals total.

## v0.1.38 — 2026-08-13

### Added
- **Golden evals 26–29.** The fake-server regression suite now locks in the
  v0.1.32–35 deterministic fixes: GoalGate (refuses DONE on a failing build and
  forces a fix round), fs_patch bad-hunk (structured error, file untouched),
  web_fetch scheme guard (rejects `file://`), and GoalMemorize (round facts
  persist to L3 memory). The harness Task gained `goal_gate`, `goal_memorize`
  and the `memory_contains` assertion.

## v0.1.37 — 2026-08-13

### Added
- **Near-cap convergence nudge.** When a goal turn is within 2 iterations of
  `MaxIterations` and the model has written files, the agent nudges it to stop
  making tool calls and close with a final answer. Targets the residual
  stress-test failure — runs that do all the work but never emit the closing
  answer (the read-only convergence nudge only fired on write-free turns).
  Live-measured on Qwen3VL-8B: rescued a previously-doomed 11m41s stall into a
  clean DONE; no regression (2/3 fully correct, matching the gate-only rate).

## v0.1.36 — 2026-08-13

### Added
- **Bench regression gate.** `yagent bench` now records its pass score to
  `<data_dir>/bench-baseline.json` (per-model best + last run, timestamped) and
  warns when a run is below the model's own best (repeat≥2 only, so a flaky
  single run can't overwrite a solid best). `yagent doctor` reports the recorded
  baseline and raises a WARN when the last run is below best — a model or
  sampling change that silently degrades the agent loop is now caught.
- Closes **T1-2**, the last open item on the strategic roadmap.

## v0.1.35 — 2026-08-13

### Fixed (adversarial-QA audit)
- **Checkpoint name traversal** — `Restore`/`Delete` now validate names like
  `Save` does, closing a workspace-wipe (Restore `../..`) and an
  outside-workspace delete (Delete `../../../victim`). `Restore` also requires
  the snapshot be a real directory before removing anything.
- **fs_patch out-of-range hunk panic** — a hunk starting past EOF with only
  additions panicked the process; it now returns a structured error. Every tool
  execution is additionally wrapped in `recover()` so a tool panic degrades to
  an error result instead of killing yagent.
- **/undo phantom entries** — a write rejected by preflight no longer records an
  undo entry (previously /undo would "revert" a write that never touched disk).
- **shell_bg working directory** — background jobs now run in the workspace
  (`jobs.StartIn`), not yagent's process cwd.
- **web_fetch scheme validation** — non-http(s) URLs (file://, gopher://, …)
  are rejected before any request (SSRF hardening).

## v0.1.34 — 2026-08-13

### Added
- **Session-scoped web-result cache.** `web.Client` caches `web_search` results
  (keyed by query + k) and `web_fetch` pages (keyed by URL) with a 10-minute
  TTL, bounded at 64 entries, with `ClearCache()`. Cache hits surface as
  `[cached results]` / `[cached page]` markers — so identical queries stop
  re-hitting the slow, rate-limited network.
- **T3-2 (offload verification to the laptop) rejected on a false premise**: the
  verification tools are pure CPU `os/exec` subprocesses with no GPU usage, so
  there is nothing to offload. Recorded in improvement.md with the evidence.

## v0.1.33 — 2026-08-13

### Added
- **`GoalMemorize` — deterministic goal-fact memory.** In goal mode, each round's
  touched paths, last tool failure, and goal text are persisted to the L3
  memory store (`memorizeGoalRound`, project preferred, deduped, no LLM call).
  A **failed** round's failure facts persist too (fixed after verification
  surfaced that facts were only saved on clean rounds) — so a stalled or
  interrupted goal leaves searchable memory behind for `--resume-goal` or a
  future session. UI-enabled.
- Live-verified: 4 facts saved during a stalled goal round (touched files +
  the exact syntax error), searchable afterward.

## v0.1.32 — 2026-08-13

### Added
- **`GoalGate` — deterministic completion gate for goal mode.** `RunGoal`
  refuses a DONE verdict while `workspace_diagnostics` still reports a failing
  build/lint (`diagnosticsFailed`), feeding the errors back and forcing another
  round. A model can no longer "declare DONE while narrating the remaining
  work". UI-enabled (mirrors VerifyWrites).
- **Long-horizon goal stress-test.** `TestLiveGoalStress` (opt-in
  `YAGENT_LIVE_EVAL=1`) runs a scripted multi-file refactor goal and measures
  DONE verdict, rounds, wall time, package/import/test correctness, and decoy
  fact preservation. The evidence harness for the C3/M7 gating question.
- Measured on Qwen3VL-8B: full-refactor correctness went **1/5 → 3/3** after
  the gate, and DONE-too-early went 2/5 → 0. C3 (structured subagent returns)
  stays gated.

## v0.1.31 — 2026-08-13

### Added
- **fs_refactor write guardrails.** Every rewritten file is now validated with
  the tree-sitter syntax check before any write (all-or-nothing). The
  exported-symbol delta guardrail is deliberately NOT applied — a rename
  `Foo→Bar` removes `Foo` by design, so it would block every public rename.
- **Structured-file preflight.** `.yaml`/`.yml` and `.json` writes are parsed
  (yaml.v3 / encoding/json) before hitting disk, so a malformed
  config/playbook/skill-frontmatter breaks the next reload instead of failing
  cryptically. Wired into fs_write, fs_edit (both paths), fs_patch, fs_refactor.
- **Cross-turn read-tool result cache.** Pure read tools (grep, glob,
  index_search, code_references, code_outline, code_slice, code_topology,
  code_impact, code_unused) memoize results keyed by canonical (tool, args),
  returning a `[cached result]` marker. Invalidated by any write/destructive
  tool or index_repo, so a cached answer never outlives the change that made it
  stale. Bounded at 64 entries.
- **`code_unused` dead-symbol candidates.** Exported top-level symbols with zero
  call sites anywhere (tests are indexed, so test-only symbols are excluded).
  Labeled candidates-not-truth — interface implementations and dynamic dispatch
  produce no call edges.

## v0.1.30 — 2026-08-13

### Added
- **`summarizer:` config section.** Overrides which model condenses old history
  (the budget summarizer and `/compact`) — e.g. `summarizer.server_url` +
  `summarizer.model` pointing at a second machine. Unset = the main model
  summarizes, so the GPU loop is untouched unless you opt in. Wired via
  `chatEnv.summ` → `agent.Config.Summarizer`; editable with `/set
  summarizer.model` / `/set summarizer.server_url`. Live-verified: `/compact`
  distilled a session through a separate laptop server (qwen3:4b) while the
  main server was unreachable.
- **Dev server raised to 32k context with Q8_0 KV** — `-c 32768
  --cache-type-k q8_0 --cache-type-v q8_0` halves KV VRAM vs f16 (~90 t/s on
  12 GB); `context_window: 32768` to match.

## v0.1.29 — 2026-08-13

### Added
- **`code_impact` change-radius tool.** Given a file or symbol, computes the
  downstream blast radius *before* an edit: every direct caller file with its
  call-site lines (call-graph), every package that imports the touched package
  (import DAG), and the covering test files. Deterministic — zero LLM calls.
- **`test_runner` targeted tests.** Runs the unit tests affected by a change,
  scoped to `package | file | symbol` (Go `go test -run`, pytest, vitest/jest),
  with output pruned to failures plus a summary. Complements
  `workspace_diagnostics` (compile-only) with a semantic verification loop.
- **`/compact` session ledger.** `agent.Compact` distills the entire
  conversation into a structured `[SESSION LEDGER]` (facts, decisions, failed
  approaches, next step) in one pass, replacing the historical turns. The
  manual counterpart to the automatic budget summarizer; available in both UIs.

## v0.1.28 — 2026-08-13

### Added
- **Diagnostic error sanitizer with signature grouping.** `capResult` now
  groups compiler/linter error cascades by signature and keeps the top 3
  distinct root causes with their first precise `path:line:col` pointer plus a
  fold count, instead of flooding the context with 200 derived errors. Applied
  only when output is error-dominated; normal tool results pass through.
- **`fs_edit` whitespace soft-normalization.** When `old_string` isn't found
  exactly, a leading-whitespace-normalized match (tabs vs spaces) that lands at
  exactly one span auto-applies with an `[auto-aligned whitespace indentation]`
  marker, re-indenting `new_string` with the file's own indentation. Ambiguous
  matches never auto-apply.
- **`code_topology` tool.** `index.BuildTopology` scans the workspace once
  (gitignore-aware) and renders a compact package-level import DAG — module
  path, per-package local imports, entry points — from import statements
  directly. No index or embedding needed.
- **`yagent export-dataset --format dpo`.** Preference-mode export: each failed
  tool call (rejected) is paired with the eventual success (chosen) per turn
  as `{"prompt","chosen","rejected"}` lines for DPO/ORPO fine-tuning.

## v0.1.27 — 2026-08-13

### Added
- **`diff_semantic` symbol-delta guardrail.** `fs_edit`/`fs_patch` now compare
  the file's exported top-level declarations before/after a write
  (`index.ExportedSymbols`) and block an edit that would silently delete a
  public symbol (renames too — the error points at `fs_refactor`). Layered on
  the existing tree-sitter `preflightSyntax` check; `fs_write` (full rewrites)
  is exempt.
- **`yagent export-dataset`.** `internal/dataset` converts verified session
  trajectories into OpenAI-chat or ShareGPT JSONL for fine-tuning local models,
  dropping failed turns (empty assistant replies, `[redacted]`/`[home]`
  markers, scrubbed tool args). Flags: `--output`, `--format openai|sharegpt`,
  `--session`, `--min-messages`.
- **VRAM pressure detector.** `vram_threshold_tps` (default 5,
  `YAGENT_VRAM_THRESHOLD_TPS`, `/settings` + `/set`) — the agent measures each
  stream's t/s, flags KV-cache spill when it drops below the threshold, the
  next `budget()` force-prunes old tool output, and the TUI shows a `⚠ VRAM`
  pill until it clears.
- **Qwen3VL-8B-Instruct is now the default model** (18/18 on the benchmark;
  see `docs/models-benchmark.md`). Ornith-1.0-9B (16/18) and
  Qwopus3.5-9B-coder-Exp (12/18 @ temp 1.0) recorded as alternatives.

## v0.1.26 — 2026-08-12

### Fixed
- **Tool-call dedup no longer skips read-only tools.** An identical re-read was
  being skipped with `"skipped: duplicate"`, which a small model reads as "it
  didn't run — retry" and loops on forever (observed on the edit-verify task
  with LFM2.5). Dedup now applies only to write/destructive tools; repeated
  reads are legitimate (verify-don't-trust) and the `fs_read` cache already
  returns an informative `[cached]` marker for unchanged files.

## v0.1.25 — 2026-08-12

### Changed
- **Model guidance refresh 3**: benchmarked `LFM2.5-8B-A1B-UD` and re-ran
  `LFM2.5-2.6B` with their recommended recipes. Finding: per-model sampling
  matters but isn't uniform — the 8B recipe lifted it 10 → 13/18, the 2.6B
  recipe was within noise. Both LFM models loop into "max iterations" on
  edit-verify/multi-turn. `config.example.yaml` gained the split `models:`
  profiles; docs updated.

## v0.1.24 — 2026-08-12

### Changed
- **Model guidance refresh 2**: benchmarked `Qwen3-8B` (non-VL) and
  `LFM2.5-2.6B`. Finding: the plain Qwen3-8B-Instruct **thinks by default on
  llama.cpp** (~4× slower than the VL variant), so Qwen3VL-8B stays the
  recommended default; LFM2.5-2.6B is a surprisingly capable 1.7 GB model
  (13/18) but loops on the diagnostics task. Docs updated.

## v0.1.23 — 2026-08-12

### Changed
- **Model guidance refresh**: `docs/models-benchmark.md` now covers
  **Qwen3VL-8B-Instruct (18/18 — the recommended default on 12 GB)** and
  **gpt-oss-20b (14/18)**, measured with `--repeat 3`; the repeat-3 score makes
  the honest picture visible (Qwythos 11/18, not the single-run 4/6). The
  compatibility matrix in `docs/models.md` was corrected accordingly.

## v0.1.22 — 2026-08-12

### Changed
- **`yagent bench` enhanced**: reports per-task **generation speed (t/s)** and
  **reasoning tokens** alongside pass/time (shows how much a model "thinks" per
  task), and gained `--repeat N` to run each task N times for a stabler score —
  the flaky `multi-turn` / `code-locate` tasks swing run-to-run. `--json` now
  carries the per-task aggregates. `bench.RunTask` counts tokens and times
  itself internally.

## v0.1.21 — 2026-08-12

### Added
- **`yagent bench`**: runs the six canonical agent-loop tasks against the
  configured model with per-task pass/fail + timing; `--json` emits a
  machine-readable report for collecting results across models.
- **`docs/models-benchmark.md`**: which model to run and what to expect,
  benchmarked on the RX 6700 XT — fable-qwen2.5-3b (6/6, fastest), gemma-4-12B
  (6/6, strongest), Qwythos-9B / Qwen3.6-14B (4/6, flaky recall), and
  qwen2.5-coder-7b-instruct (1/6 — doesn't emit tool calls on llama.cpp; use
  Ollama or an agent-tuned variant). Includes recommended sampling per model.

## v0.1.20 — 2026-08-12

### Added
- **`sampling.reasoning_max_tokens`**: opt-in cap on the model's thinking span
  per request (llama.cpp/Ollama). The single biggest speed lever on a 12 GB
  card — each round-trip drops from ~25s to ~5s on Qwythos, so long turns stop
  churning and actually deliver.
- **Stall nudge**: a final answer that stops with a prose permission-ask
  ("should I...", "need to ask you...") is nudged to call clarify or just
  deliver — fires regardless of prior tool use.
- **Tool-loop breaker**: the same exploration tool called 6+ times in a turn
  nudges the model to converge (catches tool-call loops, which the text loop
  guard can't).
- **Convergence nudge**: 12+ read-only calls with no write and no answer nudge
  the model to deliver based on what it has.
- **Golden evals 23–25** lock in the three nudges.

## v0.1.19 — 2026-08-12

### Fixed
- **TUI message input**: replaced the single-line `textinput` with a multi-line
  `textarea` — pasted multi-line text now wraps instead of overflowing
  horizontally, enter submits and alt+enter inserts a literal newline, and the
  input grows with its content (capped at a third of the screen). The input is
  now a rounded, bordered bar matching the theme, so the layout reads as
  finished.

## v0.1.18 — 2026-08-12

### Added
- **Post-goal playbook distillation**: after a successful autonomous goal run
  with ≥ 3 tool calls, the agent offers to distill the workflow into a reusable
  `.yagent/playbooks/<name>.yaml` (the model writes it with `fs_write`, or
  declines with "no playbook"). Complements the existing end-of-turn
  skill-creation opportunity.

## v0.1.17 — 2026-08-12

### Added
- **Benchmark expansion**: the live small-model benchmark grows to six canonical
  tasks (adds fuzzy-path, code-locate, grep-find), making the sampling sweep a
  stabler tuning signal.

## v0.1.16 — 2026-08-12

### Fixed
- **Truncated tool-call marshal crash**: a cut-off tool-call argument is
  sanitized into a `{"__truncated":true}` marker at the SSE layer so the
  assistant message re-serializes on the next request (previously the invalid
  RawMessage broke the client with "unexpected end of JSON input").

### Added
- **Golden evals 18–22**: the new deterministic behaviors are locked into the
  harness — prose tool-call nudge, verify barrier, truncated tool-call
  recovery, structured error envelopes, and the task-state ledger. Harness
  gained `requests_contain` and `verify_writes`.

## v0.1.15 — 2026-08-12

### Fixed
- **Agent-side loop guard**: `agent.Run` now watches the streamed content for a
  repeating unit and cancels + feeds back a stop-repeating nudge — previously
  the loop guard was TUI-only, so a looping *subagent* burned the whole request
  timeout.
- **Safe fs_read dedup marker**: the `[cached]` marker no longer suggests
  reusing an earlier result from history (which invited hallucinated file
  contents once pruning removed it); it states the file is unchanged and offers
  a line-range re-read.

## v0.1.14 — 2026-08-12

### Added
- **Goal-progress ledger**: the agent tracks touched files and the last tool
  failure and injects a compact `TASK STATE` block into the system message each
  request — the model stays oriented across long multi-turn runs without
  re-reading history.
- **fs_read dedup cache**: a repeated full read of an unchanged file returns a
  `[cached]` marker instead of re-injecting the whole content (per-session hash;
  line-range reads and changed files bypass it).
- **t/s meter**: the TUI status line shows a live tokens/second reading while a
  turn streams.
- **`/retry`**: re-runs the last input with a stable sampling profile
  (temp 0.3, repetition_penalty 1.05) in both UIs.

## v0.1.13 — 2026-08-12

### Added
- **Verify-don't-trust barrier**: when a turn writes files without running
  `workspace_diagnostics`, the agent deterministically runs it before accepting
  a final answer and feeds the result back — "done" after an unverified write
  is impossible. The model's own diagnostics call clears the flag.
- **Playbook success predicates**: playbook phases can declare machine-
  verifiable `checks:` (`file_contains`, `file_not_contains`, `file_exists`,
  `diagnostics`); a phase completes only when they pass (with one automatic
  re-run to fix failures).
- **Structured error envelopes**: key tool errors carry
  `[class=… retryable=… suggest=…]` markers (`missing_path`→glob,
  `old_string_not_found`→fs_read, `ambiguous_match`, `timeout`) the model can
  act on programmatically.

## v0.1.12 — 2026-08-12

### Added
- **SlotLock (inference serialization)**: a process-wide per-server semaphore
  serializes chat/embed/tokenizer requests so parallel subagents, consult and
  embeddings never hit a single-slot local server concurrently (HTTP 500 / VRAM
  thrash). Capacity defaults to 1 and is raised to the server's real slot count
  from `/props`.
- **Deterministic error remediation**: `fs_edit` "old_string not found" now
  hints at the nearest matching line (substring/Levenshtein), recovering a
  typo in one turn.
- **Truncated tool-call recovery**: cut-off tool-call arguments return
  "arguments were truncated (incomplete JSON) — re-emit the full tool call"
  instead of a generic syntax error the model can't act on.
- **Prose tool-call nudge**: when the model narrates a tool call ("I will
  fs_read main.go") without emitting tool_calls and no tool has run this turn,
  the agent feeds back a nudge to emit it — never auto-executing. Past-tense
  mentions and code fences are ignored.

## v0.1.11 — 2026-08-12

### Added
- **`code_slice` tool**: reads one declaration's exact source span (body + doc
  comment) via tree-sitter instead of a whole file — ~80% fewer tokens on large
  modules (`index.SliceSymbol`).
- **Pre-flight syntax validation**: `fs_edit`, `fs_write` and `fs_patch` parse
  the modified source in-memory and block the write when tree-sitter finds a
  syntax error, reporting the exact line/col — a broken edit never reaches disk
  or costs a diagnostics roundtrip. Non-source files are untouched.

## v0.1.10 — 2026-08-12

### Added
- **`clarify` tool**: the model calls `clarify(question, choices[])` when a
  task is ambiguous or a choice matters; the UI renders real options (REPL
  numbered prompt, TUI modal) and the user's pick returns as tool data —
  ambiguity is a structured handoff, not a prose guess.
- **`plan` tool**: a lightweight plan-approval gate — the model proposes steps,
  the user approves or gives revision feedback, returned to the agent as
  `plan approved` / `plan rejected: <feedback>`.
- **Verify-don't-trust rule**: the system prompt now requires re-reading a
  touched file region (fs_read) after any code write and confirming it matches
  intent before running `workspace_diagnostics`.

## v0.1.9 — 2026-08-12

### Added
- **Per-model sampling profiles**: a `models:` config section overrides
  sampling per model-name substring (unset fields inherit the base recipe) —
  docs/models.md's tuning matrix is now code.
- **Context-window auto-detect**: the client reads llama.cpp `/props` n_ctx;
  `yagent chat` caps the agent budget at the server's real window (prevents
  over-length 400s), the reserve is now a percentage of the window, and
  `yagent doctor` reports `server n_ctx` vs the budget.
- **`yagent calibrate`**: runs the canonical small-model tasks across the
  sampling recipes against the live model and reports per-task pass/fail; with
  `--write` it persists the best recipe into the config. Shared `internal/bench`
  package drives both the command and the live eval tests.
- **Fuzzy path pre-resolution**: `fs_read`/`fs_edit` auto-correct a dropped
  file extension when exactly one file matches (e.g. `README` → `README.md`)
  with a "resolved to" note, saving a wasted turn.

## v0.1.8 — 2026-08-12

### Added
- **Local-model tuning**: the system prompt now instructs the model to run
  `workspace_diagnostics` after code edits and to ask clarifying questions on
  ambiguous tasks, plus two worked tool-use examples.
- **Loop-guard auto-retry**: a repetition-loop cancellation now retries the
  same input once with `sampling.repetition_penalty 1.05` applied (persisted);
  explicit Esc cancels never retry.
- **`sampling.min_p` knob**: opt-in nucleus lower-bound filter for
  llama.cpp/Ollama, editable via `/settings` and `/set`.
- **Live small-model benchmark** (`YAGENT_LIVE_EVAL=1`): canonical tasks
  (tool JSON + read, two-turn recall, edit-then-verify, fuzzy path, code
  locate, grep find). `YAGENT_LIVE_SWEEP=1` compares sampling recipes.

## v0.1.7 — 2026-08-12

### Added
- **`fs_refactor`**: word-boundary symbol rename across source files (build /
  vendored dirs and binaries skipped), undo-aware and approval-gated.
- **Declarative playbooks**: `.yagent/playbooks/<name>.yaml` define multi-stage
  workflows — phases of `{goal, rounds, tools[], success}`. Run with
  `yagent chat --playbook <name>` or `/playbook <name>` (bare `/playbook`
  lists them; `yagent playbook list` too). Each phase is an autonomous goal run
  scoped to its tool subset, snapshotted per phase.

## v0.1.6 — 2026-08-12

### Added
- **Project-instructions reader (P1)**: the agent auto-discovers developer
  instructions in the workspace — `.yagent/instructions.md` > `AGENTS.md` >
  `CLAUDE.md` > `.cursorrules` (first found) — and folds them into the system
  prompt (capped at 16 KiB), so repo-specific rules are respected without
  manual prompting.
- **Preset subagent roles (P2)**: the `subagent` tool accepts `role:
  architect|auditor|test-engineer|docs-writer` — each applies a focused system
  prompt, a default read-only tool subset, and a temperature (child client
  cloned via `llm.Client.Clone`). Unknown roles are rejected with a clear
  error.
- **Structured session exports (P3)**: `yagent sessions export <id> --format
  html|md` (default `md`); `Store.RenderHTML` emits an escaped, styled,
  dependency-free HTML archive.
- **Tool-output pruning in the budget (P4)**: when the context window is
  exceeded, the budget now first collapses old tool results (before the current
  user turn) into a one-line `[tool output concealed; N lines hidden]` marker,
  keeping the user's instructions and the model's reasoning turns alive —
  summarization only runs if still over budget.
- **`workspace_diagnostics` tool (P5)**: detects the project type and runs its
  static checker (`go vet ./...`, `cargo check`, `npx tsc --noEmit`, eslint,
  `ruff check .` or a python syntax check) with a 120s timeout. Read-only (the
  commands are fixed by the tool, not the model), so edits can be verified
  without the approval gate.
- **Skills manager modal (P6)**: bare `/skills` in the TUI opens a modal over
  pending staged skill writes — ↑/↓ pick, `d` diff, `v` verify, `a` approve,
  `r` reject, esc close.
- **`fs_refactor` rename (P7)**: word-boundary symbol rename across every
  source file (build/vendored dirs and binary files skipped), rewriting all
  occurrences and recording the originals so `/undo` reverts the whole rename.
  Approval-gated; validates identifier names.
- **Declarative playbooks (P8)**: `.yagent/playbooks/<name>.yaml` define
  multi-stage workflows — phases of `{goal, rounds, tools[], success}`. Run
  with `yagent chat --playbook <name>` or `/playbook <name>` (bare `/playbook`
  lists them; `yagent playbook list` too). Each phase is an autonomous goal run
  scoped to its tool subset, snapshotted per phase.

## v0.1.5 — 2026-08-12

### Added
- **Eval harness: failure-recovery and partial-approval scripting.** Tasks can
  set `deny_first` (the scripted user denies the first N write/destructive
  approvals), `patch_filter: first_hunk|last_hunk` (fs_patch is approved with
  rewritten args that keep only that hunk, exercising the real `Approval.Args`
  path), and `file_contains` / `file_not_contains` post-run assertions.
- **Three new golden evals (15–17)**: execution-error recovery (a missing-file
  read is fed back and the model self-corrects via glob), approval-denial
  recovery (denied write leaves no file), and partial fs_patch approval (only
  the accepted hunk is applied).
- **Benchmarks**: patch split/rebuild over a 200-hunk multi-file diff
  (~65–120µs) and subagent fan-out/fan-in for 4/8/32 parallel tasks
  (~3–21µs), the per-delegation overhead floor.
- **Esc cancels the running turn** (TUI): the model stops generating, the
  partial reasoning/answer is dropped, and the session stays alive — send a
  new message immediately. Ctrl-C still quits the whole session.
- **Loop guard** (`ui.loop_guard`, default on): a turn that visibly repeats
  itself (thinking/content loop) is auto-cancelled with a hint to try
  `sampling.repetition_penalty`; toggleable from `/settings`. The plain REPL
  prints the same hint (no cancel there).

## v0.1.4 — 2026-08-12

### Added
- **Accurate token counting (C1)**: `llm.Client.CountTokens` calls the model
  server's own tokenizer (llama.cpp `/tokenize`, Ollama `/api/tokenize`,
  probed once, len/4 fallback), and the agent now counts the system prompt,
  running summary, injected skills and every history message with it. The TUI
  context gauge and the summarization trigger reflect the real token counts;
  no network under the context lock.
- **`--trace <file>` (B2)**: `yagent chat --trace` writes every assembled
  context with per-section token estimates (system / skills L0 / code index /
  summary / recall / injected / history) that always sum to the live
  `ContextUsage` gauge — a real prompt dump for budget debugging.
- **`--resume-goal <session>` (C2)**: goal mode now snapshots the workspace
  after every completed round (not just before the run), and `--resume-goal`
  restores the goal checkpoint and continues the session — an interrupted
  multi-round goal run picks up where it left off, not from scratch.
- **TUI transcript search (B3)**: `Ctrl+F` opens an in-viewport find bar;
  typing searches the transcript (case-insensitive), enter jumps to the next
  match, esc closes. Works in the REPL-free TUI only.
- **`consult.cmd` editable via `/set` (B4)**: `/set consult.cmd claude -p`
  persists `consult.cmd: [claude, -p]` as a YAML sequence (round-trips through
  reload); `/settings` now lists it.
- New regression tests closing eval-coverage gaps (B1): the staged-skill
  verify flow (FAIL accumulates / PASS clears pending+skill failure counters),
  an end-to-end `/undo` revert over a scripted agent write, consult soft-fail
  on a failing advisor server, accurate-counter and trace==gauge agent tests,
  and `CountTokens` server-tokenizer tests.

## v0.1.3 — 2026-08-11

### Added
- **fs_patch per-hunk approval**: multi-hunk patches open an interactive hunk
  walker in the TUI (y accept / n skip / q finish); only accepted hunks are
  applied — the `Approver` interface returns an optional rewritten-args
  override for this.
- **Tool-call dedup**: a repeated identical (tool+args) call is skipped
  instead of re-executed (small-model habit); repeated *failing* calls still
  count toward the validation block.
- **Subagent token accounting**: subagent results now report a heuristic token
  tally per child.
- **Reasoning controls**: `ui.show_reasoning` toggles the thinking block
  (default on), and the per-turn reasoning buffer is capped (tail kept with an
  omission marker) so a verbose model can't flood the terminal.
- **Consult soft-fail**: an unavailable advisor returns "consult unavailable —
  continue without the advisor" so the turn degrades instead of derailing.
- New golden evals: fuzzy-args aliasing and code-references.

### Changed
- **Stable streaming layout**: the live answer/thinking tail now streams INSIDE
  the transcript viewport instead of in a detached strip below it, so the
  header/input/status rows never shift while a turn runs — text can no longer
  appear to build upward from the bottom. The viewport wraps to the window
  width.
- **Expandable thinking**: committed thinking blocks collapse to a
  `🧠 thought (N tok)` header by default; **clicking the header** (or pressing
  `t` with an empty input) expands/collapses the full dimmed reasoning in
  place, and the preference persists across turns. Mouse capture is **off by
  default** so drag-selecting transcript text stays with the terminal —
  `Ctrl+M` or `/mouse` toggles it on (click thinking, wheel-scroll, status
  shows `· mouse`) and auto-releases on quit.

## v0.1.2 — 2026-08-11

### Security
- **Symlink containment**: `resolvePath` now resolves symlinks (`EvalSymlinks`
  on the deepest existing ancestor) and re-checks workspace containment, so a
  symlink inside a cloned repo can no longer make `fs_read`/`fs_write`/
  `fs_edit`/`fs_patch` read or write outside the workspace. The TUI approval
  preview (`approvePath`) applies the same rule.
- **Env scrubbing**: `shell_exec`'s env denylist is broadened (suffixes like
  `_PASSWORD`/`_AUTH`/`_CREDENTIALS`, conventional names like `GH_PAT`,
  `DATABASE_URL`) and gains value heuristics (SSH/PEM keys, bearer tokens,
  credential-bearing URLs) via `internal/scrub.SecretEnv`.
- **Sandbox home masking**: `shell.sandbox: bwrap` still binds `$HOME`
  read-only, but now overlays empty tmpfs over sensitive entries (`~/.ssh`,
  `~/.aws`, `~/.gnupg`, `~/.kube`, `~/.docker`, keyrings, browser profiles…)
  and rebinds credential files (`~/.git-credentials`, `~/.npmrc`, `~/.netrc`)
  to `/dev/null`, so a sandboxed command cannot read them.

### Changed
- **Sampling parameters**: chat requests now forward `temperature`/`top_p`
  (defaulting to the Qwythos-9B recipe 0.6/0.95) plus optional `top_k` and
  `repetition_penalty` (`sampling.*` settings / `/set`; zero values are
  omitted so cloud endpoints that reject them stay working). Qwythos's card
  warns that greedy/low-temp sampling degenerates into repetition loops — this
  applies its recommended recipe instead of server defaults.
- `resolvePath` accepts **absolute paths inside the workspace** (models
  habitually emit them; containment is still enforced either way) — a rejected
  absolute grep/fs path no longer derails the agent loop.
- System prompt hardened against the Qwythos persona quirk: explicit
  "never self-identify as a model/creator" rule, plus "do not narrate your
  plan before a tool call".
- **TUI wrapping**: the live streaming tail is hard-wrapped to the window
  width; glamour markdown renders with a word-wrap and over-wide lines (long
  URLs/code) are hard-wrapped after; header/status pill bars drop pills that
  don't fit narrow windows; the input view is capped to the window width so
  nothing runs off the right edge anymore.
- `docs/models.md` gained a community model compatibility matrix
  (model, server, tool-call reliability, context behavior).
- `improvement.md`: reconciled the stale "CI not a fit" note (CI shipped in
  M6.18) and marked M7 v2 parallel subagents shipped.

## v0.1.1 — 2026-08-11

### Added
- **Fuzzy tool arguments**: `decodeArgs` now maps unknown keys onto the closest
  schema field (synonyms like `filename`→`path`, plus Levenshtein) so small
  models stop burning retry turns on `{"filename":"x"}` instead of
  `{"path":"x"}`; canonical keys win when both are present.
- **Workspace checkpoints**: goal mode auto-snapshots the workspace to
  `.yagent/checkpoints/goal/` before running; `/checkpoint list | save
  <name> | restore <name> | delete <name>` reverts a stray autonomous run
  (snapshots exclude `.git`/`.yagent`).
- **Call graph**: the indexer now records function-call edges (tree-sitter
  queries for go/python/js/ts/rust/c/cpp/java) and a new `code_references`
  tool answers "who calls X?" with `path:line` call sites.
- **Subagent scratchpad**: `scratch_write`/`scratch_read` let parallel
  subagents share structured notes under `.yagent/scratch/` (scratch_write is
  the one confined write tool available to read-only subagents).
- **Context compactor**: `capResult` collapses runs of identical lines
  (`… [N×]`) and blank-line runs, so repetitive build/test logs stop flooding
  the context window.
- **fs_patch approval preview**: the TUI now shows a colorized unified-diff
  preview before approving a multi-file patch.
- **Reasoning display**: the model's thinking (`reasoning_content`, emitted by
  Qwythos/Qwen3.5 templates) now streams into the UI as a dimmed/italic
  "thinking" block above the answer — TUI and REPL. It stays display-only and
  never enters history or context (`agent.OnReasoning` → SSE parser).
- **M7 beyond v2 — subagent tool subsets**: `subagent` gained a `tools[]`
  array that scopes each child to a subset of the read-only tools (e.g.
  `["web_search","web_fetch"]` for research, `["grep","fs_read",
  "index_search"]` for code exploration). Invalid or destructive requests
  (e.g. `["shell_exec"]`) are rejected with the available set fed back to the
  model, and each child's system prompt lists exactly the tools it has.
- **M7 v2 parallel subagents**: `subagent` gained a `tasks[]` array — each
  subtask runs in an isolated read-only child agent concurrently and the
  summaries are combined in order.
- **TUI theme selector**: the `theme` setting (tokyo | catppuccin | nord,
  `YAGENT_THEME` env) picks the palette; `/settings` uses the left/right
  chooser and applies it live. Default `tokyo`.
- **TUI overhaul**: 24-bit Tokyo Night theme (one shared palette replaces the
  ad-hoc 256-color codes); pill-style header (app, workspace, model, session,
  git branch) and status bar (state, live context gauge with a progress bar,
  tool count, YOLO badge); animated spinner while working; emoji icons for
  tool calls, approvals and states; centered modal overlays for `/settings`
  and `/sessions`; a bordered `/` command palette popover above the input;
  markdown rendering for committed assistant messages via `glamour`.

### Changed
- Main loop is cloud-capable: `api_key`/`YAGENT_API_KEY` sends
  `Authorization: Bearer` on chat + embedding requests (local stays the
  default); `consult` already had its own.
- Chunker parses each file once (`chunkAndSymbols`) instead of once for
  chunks and again for symbols; symbol lines now point at the declaration,
  not its doc comment.

### Fixed
- `shell_exec`/`shell_bg` timeouts now kill the whole process group, so
  descendant commands (`sh -c "sleep 5 & wait"`) can no longer hold the
  output pipe open and stall the timeout until they finish on their own.
- `TestDefaultPath` clears an inherited `$XDG_CONFIG_HOME` so it resolves
  `$HOME` deterministically on CI runners.
- CI pins Go 1.25 (matches `go.mod`) instead of relying on toolchain
  auto-download from 1.22.

## v0.1.0 — 2026-08-11

### Added
- M7 v1: `subagent` tool (isolated read-only child agent); `fs_patch`
  (multi-file unified diff); background jobs (`shell_bg`/`shell_logs`/
  `shell_kill`); `code_outline`; compact code injection; TUI `/sessions`
  browser; session delete.
- Per-project config: `<workspace>/.yagent/config.yaml` overlays the global
  config for a repo (model, server, sandbox, ...), committed for the team;
  `/set` writes to it when present.
- Project-scoped memory: `memory_save`/`memory_search` gain a `scope`
  (global|project); recall merges the shared repo store.
- Sandboxed shell: `shell.sandbox: bwrap` wraps `shell_exec` in bubblewrap
  (workspace rw, system read-only, no network, private `/tmp`); fails loudly
  when bubblewrap is missing.
- `yagent init`, `yagent backup`, `yagent sessions search|export`,
  `yagent completion bash|zsh`, `yagent skills list|import`.
- Loop mode (`--goal`/`/goal`), `consult` advisor (local / cloud API / CLI),
  `/undo`, symbol-aware `index_search`, goal-mode evals, benchmarks.
- Repo hygiene: LICENSE, CONTRIBUTING, CHANGELOG, example config, CI workflow.

### Changed
- Skills writes are automatic by default (`skills.write_approval` default
  `false`); re-enable review with `/skills approval on`.
- `memory_save` is self-gated (no approval prompt).
- Tool-call JSON repair; web search fallback chain; secret redaction before
  persistence.

### Fixed
- Context budget no longer swallows the current user turn (Qwythos template
  rejects tool-only requests).
- TUI scrolling/copy/tab-cycle; settings edit input focus.

## M6.15 — 2026-08
- Sessions search + markdown export; `/undo` buffer; symbol-aware search.

## M6.14 — 2026-08
- `/settings` + `/set` + interactive TUI settings page; choice fields.

## M6.13 — 2026-08
- Loop mode; `consult` advisor tool.

## M6.12 — 2026-08
- Runtime `/yolo` toggle; TUI kaomojis.

## M6.11 — 2026-08
- Dynamic tool-schema filtering; TUI approval diff overlay; shell completions.

## M6.10 — 2026-08
- Tree-sitter rust/c/cpp/java; session `--fork`; startup re-index; versioning.

## M6.9 — 2026-08
- Web search fallback chain; secret redaction; tool-arg JSON repair.

## M6.8 — 2026-08
- Live context gauge; thread-safe agent history.

## M6.7 — 2026-08
- Skills verification harness (`/skills verify`) + staleness counters.

## M6.6 — 2026-08
- TUI slash menu + tab completion; session-id on quit; budget-regression eval.

## M6.5 — 2026-08
- TUI scrollable viewport + in-TUI `/skills`; Mojeek backend; skills CLI; `--yolo`.

## M6 — 2026-08
- bubbletea TUI, slog logging, `yagent doctor`, golden eval harness.

## M5 — 2026-08
- Web tools: DuckDuckGo `web_search`, optional SearXNG, `web_fetch`.

## M4 — 2026-08
- Codebase index: tree-sitter chunking, `index_repo`/`index_search`, per-turn
  code retrieval.

## M3.5 — 2026-08
- Skills (procedural memory); SQLite hybrid memory (chromem removed).

## M3 — 2026-08
- Memory: SQLite sessions, running-summary budget, semantic recall.

## M2 — 2026-08
- Tool loop: fs/shell/git tools, approvals, validation retry.

## M1 — 2026-08
- Streaming chat CLI, SSE parser, config.
