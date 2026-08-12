# Changelog

All notable changes to Yagent. Versioning: `git describe` via `make build`.

## Unreleased — 2026-08-12

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
- **Local-model tuning**: the system prompt now instructs the model to run
  `workspace_diagnostics` after code edits and to ask clarifying questions on
  ambiguous tasks, plus two worked tool-use examples.
- **Loop-guard auto-retry**: a repetition-loop cancellation now retries the
  same input once with `sampling.repetition_penalty 1.05` applied (persisted);
  explicit Esc cancels never retry.
- **`sampling.min_p` knob**: opt-in nucleus lower-bound filter for
  llama.cpp/Ollama, editable via `/settings` and `/set`.
- **Live small-model benchmark** (`YAGENT_LIVE_EVAL=1`): three canonical tasks
  (tool JSON + read, two-turn recall, edit-then-verify). `YAGENT_LIVE_SWEEP=1`
  compares sampling recipes. First Qwythos sweep: default (0.6/0.95) and
  cold (0.3) 3/3; repetition_penalty and min_p 2/3 — the shipped recipe stands.

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
