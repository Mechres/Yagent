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

- 🟡 **P1 project-instructions reader** — auto-discover `.yagent/instructions.md`
  / `AGENTS.md` / `CLAUDE.md` / `.cursorrules` and append to the system prompt
  (capped). `buildSystemPrompt` is currently a fixed template.
- 🟡 **P2 preset subagent roles** — architect/auditor/test-engineer/docs-writer
  presets (system prompt + tool subset + temperature) built on the existing
  `tools[]` scoping.
- 🟡 **P3 structured session exports** — `yagent sessions export <id>
  --format html|md` (markdown exists; HTML with inline styling, no new deps).
- 🟡 **P4 tool-output pruning in the budget** — collapse old tool results to
  `[Output concealed; N lines hidden]` instead of summarizing user/reasoning
  turns away. Refines `budget`, keeps user instructions alive.
- 🟡 **P5 `workspace_diagnostics` tool** — detect project type and run
  `go vet`/`tsc --noEmit`/`cargo check`/`ruff` as a typed read-only tool
  (explicit call, not an auto-hook after every write).
- 🟡 **P6 skills manager modal** — TUI overlay for `/skills pending`
  (diff, verify, approve/reject) following the `/settings`+`/sessions` modal
  patterns.
- 🟡 **P7 `fs_refactor` rename** — symbol rename across call sites using
  `index_calls`/`code_references`, applied via staged `fs_edit`s through the
  undo buffer + approval. Highest effort; do last, carefully.
- 🟡 **P8 declarative playbooks** — `.yagent/playbooks/*.yaml` = phases of
  `{goal, rounds, tools[], success criteria}` run through `RunGoal` + tool
  subsets. Effectively user-land M7 orchestration.
- ⚪ Git worktree isolation (`--worktree`) — overlaps `internal/checkpoint`
  rollback; conflicts with the no-git-mutations constraint.
- ⚪ Multimodal local vision — needs a multimodal message-part architecture
  change + a vision model on 12 GB VRAM (tight); not a local-first fit now.
- ⚪ `/plan` interactive mode — big TUI lift; goal mode + checkpoints already
  give a linear plan; playbooks cover the structured case.
- ⚪ TUI dual viewport (Ctrl+W live-diff split) — the TUI is already packed;
  high rework for medium value.

**Phase plan**: A = P1+P2+P3 (quick wins) → B = P4+P5+P6 (medium) →
C = P7+P8 (bigger). Status: **Phase A + B shipped** (2026-08-12); Phase C next.

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
