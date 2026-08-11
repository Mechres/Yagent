# Changelog

All notable changes to Yagent. Versioning: `git describe` via `make build`.

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
