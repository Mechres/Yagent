# Yagent improvement roadmap

Consolidated, prioritized plan for post-M6 work. Status: **in progress** — the
three P0 items are implemented; the rest are queued for the next pass.

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

- 🟡 Tree-sitter language expansion: Rust / C / C++ / Java grammars (YAML and
  Markdown already get the line-window fallback).
- 🟡 Session forking: `yagent chat --fork <id>` branches a session's history
  so experiments don't mutate the original.
- 🟡 Startup re-index: at session start, hash-check indexed files and re-run
  `index_repo` in the background if anything changed (no file watcher).
- 🟡 Dynamic tool-schema filtering: omit irrelevant tool definitions (e.g. web
  tools outside research turns) to reclaim ~1.5k system-prompt tokens/turn.
- 🟡 TUI diff overlay for `fs_edit`/`fs_write` approvals (colorized side-by-side
  instead of raw patch lines).
- 🟡 Shell completions (bash/zsh) for `chat|sessions|skills|doctor`.
- 🟡 Real versioning (`git describe` based) instead of `v0.0.0`; a small
  Makefile (`make test`, `make vet`).

## P2 — deferred / gated

- 🟡 Subagent primitive (`SpawnSubagent`) — M7. Gated: only start if eval
  evidence shows the single agent loop is the bottleneck.
- 🟡 More eval coverage (TUI/verification flows) + benchmarks for chunker and
  hybrid search.
- ⚪ Telemetry / metrics / Docker / systemd / man pages / docs site / CI —
  not a fit for a local-first single binary; would add surface and, for
  telemetry, conflict with the privacy stance.
