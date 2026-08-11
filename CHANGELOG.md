# Changelog

All notable changes to Yagent. Versioning: `git describe` via `make build`.

## Unreleased

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
