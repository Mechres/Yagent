# Yagent Handoff

Prepared 2026-08-18 for the next agent.

## Start Here

1. Read `/home/mechres/Projeler/Yagent/AGENTS.md`.
2. Read `docs/audit-backlog.md` before proposing audit work. Do not reopen
   rejected or deferred items without new evidence.
3. Inspect `git status` and preserve the existing uncommitted changes. They are
   intentional and belong to the current feature batch.
4. Run `go test ./...`, `go vet ./...`, and `go test -race ./...` before making
   further changes if the environment permits.

## Current Worktree

The worktree is intentionally uncommitted. The current batch adds:

- `/grill-with-docs <topic>` in the REPL and TUI.
- Scoped grill prompt injection and a deterministic eight-question limit.
- Grill-mode write restrictions to `CONTEXT.md` and `docs/adr/*.md`.
- Structural validation for glossary and ADR artifacts.
- `/handoff`, which converts a grill discussion into an approved plan-mode
  planning turn.
- Recoverable, content-aware previews for large JSON, logs, diffs, MCP results,
  and fetched web pages. Full results remain in `.yagent/scratch/`.
- Compression metrics exposed through `tools.CompressionStats()` and `/stats`.
- Golden eval `internal/eval/testdata/evals/61-grill-with-docs.yaml`.
- Documentation updates in `docs/design/grill-with-docs.md`,
  `docs/design/agent-loop.md`, `docs/design/architecture.md`, and
  `improvement.md`.

The relevant modified and new files can be found with:

```bash
git status --short
git diff --stat
```

Do not discard unrelated or pre-existing worktree changes.

## Important Constraints

- Yagent is local-first and targets small 7B-14B models. Prefer deterministic
  guardrails and compact context over elaborate orchestration.
- Keep packages acyclic: `ui -> agent -> {llm, tools, memory, index} -> config`.
- The llama.cpp Qwythos template accepts only one system message. All system
  prompt content must remain merged into one leading system message.
- Destructive tools require approval. Never silently widen approval or use
  destructive git operations.
- Full tool outputs and source code must remain recoverable; lossy previews
  need an explicit recovery path.
- Run `gofmt`, tests, race tests, and vet before declaring changes complete.

## Strategic Direction

The next major product direction is a GUI plus a separate research-only mode.
Build reusable core contracts before adding GUI-specific code.

### Do Before GUI

1. Extend the research profile with any missing deterministic coverage or
   presentation metadata. Research mode now allows web/paper search, web
   fetch, read/search tools, memory, and markdown report writes under
   `.yagent/research/`; it denies shell, source writes, git mutation, and MCP.
2. Extend the neutral `tools.ToolOutcome` contract into persisted session
   events when the GUI work begins. Dispatch now emits status/risk/timing and
   presentation metadata for terminal, diff, read, search, web, approval, and
   failure cards without requiring TUI-string parsing.
3. Extend progressive nested project instructions only if live use reveals a
   missing path/tool case. Nested `AGENTS.md`/`CLAUDE.md`/`.cursorrules` files
   are discovered on first subtree touch with containment, scanner checks,
   size caps, caching, and single-system-message injection.
4. Add a model-facing `session_search` tool. Keep historical transcript search
   distinct from semantic durable memory.
5. Improve compaction with structured summaries, protected first/last user
   anchors, and tool-call/result boundary alignment.
6. Persist compact request manifests: route, sampling, system hash, schema hash,
   and token estimates. Keep full context dumps opt-in through `--trace`.

### Do Later

- Build the GUI on stable session events and neutral tool presentation types,
  not on Bubble Tea callbacks.
- Add deterministic `@file`, `@file:path:line-line`, `@diff`, `@staged`, and
  `@folder` context references after the core event/context APIs exist.
- Add skill bundles after the skill library has recurring combinations.
- Add skill pin/archive/restore/audit lifecycle only when skill volume justifies
  it.

## External Reviews

Detailed findings are recorded in `improvement.md`.

- **DeepSeek Harness:** borrow replay/fault-test contracts, request manifests,
  structured tool results, neutral presentation, and monotonic guards. Do not
  adopt Cordis or its full plugin architecture.
- **Hermes Agent:** borrow progressive nested instructions, deterministic context
  references, model-facing session search, structured compaction, bounded
  always-on memory, skill bundles, and recoverable skill lifecycle maintenance.
  Do not adopt its gateway, remote backends, cloud sandboxes, or large Python
  runtime.

## Verification Status

The current feature batch was verified with:

- `go test ./...`
- `go vet ./...`
- `go test -race ./...`
- `git diff --check`

No commit, push, reset, rebase, or destructive workspace operation was made.
