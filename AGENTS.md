# AGENTS.md — Yagent contributor & agent guide

Read this file before touching anything. It defines how the project is built, where things live, and the rules that are not negotiable.

## What this is

Yagent is a **local-first AI agent** (code / audit / review / web search / research) in **Go**. It talks to a local OpenAI-compatible inference server (Ollama or llama.cpp `llama-server`) and implements its own agent loop, memory, orchestration and tools. Target hardware: RX 6700 XT 12 GB with 7B–14B Q4 models.

## Current status

**Milestone M2 complete** (see [`docs/PLAN.md`](docs/PLAN.md)); work the milestones in order, each acceptance criteria must pass against the real local model. State of the tree:

- M1 shipped: streaming chat CLI (config, `ChatStream` with SSE + tools, REPL `/exit`/`/clear`).
- M2 shipped: `internal/tools` (9 tools: fs_read/write/edit, glob, grep, shell_exec with env scrub + timeouts, git_status/diff/log; risk levels, workspace scoping rejecting absolute/escaping paths, strict arg validation), `internal/agent` (loop per `agent-loop.md`: system prompt v1, streaming with OnToken, approval-gated dispatch, parallel read-only batches, validation-retry with 3-failure block, max-iteration guard, tool-result feedback), `internal/ui` (agent-driven REPL, `Allow? [y/N]` approvals on shared stdin, tool activity lines).
- All M2 tasks ticked; acceptance flows verified against a scripted fake server + real tools (read→answer, edit→approval→denial-adapts, git tools without shell_exec, malformed-args recovery). `go build`/`vet`/`test` clean, gofmt clean.
- **Real-hardware acceptance done** (llama.cpp :8089, `Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf`): M1 streaming chat + all three M2 acceptance flows verified end-to-end (read→answer, fs_edit→approval→applied, git_status without shell_exec). The run surfaced three fixes, all in: `fnSchema` emits `[]`/`{}` not `null` (llama.cpp rejects `"required": null`), `glob "**/"` now matches root-level files (+ grep include), system prompt forbids narrating tool calls. Model quirks tracked in [`docs/models.md`](docs/models.md).
- Next: **M3 — memory: sessions, summarization, semantic recall**. Design: `docs/design/memory.md`. (M3.5 skills after that; both depend on the M2 loop.)

## Commands

```bash
go build ./...          # build everything
go build ./cmd/yagent   # build the binary
go test ./...           # run all tests
go vet ./...            # lint (must stay clean)
go run ./cmd/yagent chat   # smoke-test against local server
```

Module path is `yagent` (local-only; rename to a full VCS path if published). Go 1.22+.

## Module layout (create this in M1)

```
cmd/yagent/        main, flag/config wiring only — no logic
internal/
  config/          yaml config loading + defaults
  llm/             OpenAI-compatible client: chat, streaming (SSE), tools, embeddings
  agent/           the agent loop, context assembly, budget management
  tools/           tool registry + implementations (fs, shell, git, web, memory, index)
  memory/          conversation store, summarization, vector memory
  skills/          procedural memory: SKILL.md store, progressive disclosure (M3.5)
  index/           repo indexer: chunking + embeddings + semantic search
  ui/              CLI REPL first; bubbletea TUI in M6
```

Keep packages acyclic: `ui → agent → {llm, tools, memory, index} → config`. `llm` must never import `agent` or `tools`.

## Conventions

- **Stdlib first.** Own HTTP client (`net/http`), own SSE parser, `log/slog` for logging, `context.Context` plumbed everywhere.
- **Approved dependencies** (add nothing else without a comment in the PR/commit explaining why):
  - `gopkg.in/yaml.v3` — config
  - `modernc.org/sqlite` — pure-Go SQLite (M3+)
  - `github.com/tmc/langchaingo` — **NOT approved**; we implement our own loop/memory
  - `github.com/philippgille/chromem-go` — vector memory (M3+)
  - `github.com/tree-sitter/go-tree-sitter` — repo chunking (M4, needs cgo)
  - `github.com/charmbracelet/bubbletea` + `lipgloss` — TUI (M6 only)
- Errors: wrap with `fmt.Errorf("...: %w", err)`, no panics outside `main`, no `log.Fatal` in library code.
- Tests: table-driven where sensible; no network access in unit tests (use `httptest.Server` to fake the LLM API).
- No `interface{}` abuse; keep tool schemas and message types as typed structs.

## Hard constraints (do not violate)

1. **Local-first**: LLM and embedding requests go only to the configured server URL. Config default: `http://localhost:11434`; the client appends `/v1/chat/completions` and `/v1/embeddings` (see `internal/llm`). Never add cloud provider SDKs.
2. **Safety**: destructive tools (shell exec, git mutations, writes outside the workspace) require explicit user approval per `docs/design/tools.md`. Never auto-approve.
3. **No git mutations** (commit/push/reset/rebase) unless the user explicitly asks — this applies to the agent's tools AND to you while developing.
4. **Small-model discipline**: few tools, compact schemas, strict argument validation, feed validation errors back to the model for retry. See `docs/design/agent-loop.md`.
5. **Context budget**: default window 16384 tokens; never let history grow unbounded — summarize per `docs/design/memory.md`.

## Environment notes

- Dev server in use: llama.cpp `llama-server` (Vulkan) on **port 8089**, model `Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf` — set `YAGENT_SERVER_URL=http://localhost:8089` and `YAGENT_MODEL=Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf`. See [`docs/models.md`](docs/models.md) for its quirks.
- Ollama on RX 6700 XT (gfx1031) needs `HSA_OVERRIDE_GFX_VERSION=10.3.0`; default config targets Ollama at `http://localhost:11434`.
- Alternative server: llama.cpp `llama-server` built with Vulkan backend (`-DGGML_VULKAN=1`).
- Embeddings via the same server: model `nomic-embed-text`, endpoint `/v1/embeddings`.
- If SearXNG is used for web search, enable `format: json` in its settings.yml.

## Definition of done (every milestone)

Builds clean, `go vet` clean, tests pass, milestone acceptance criteria in `docs/PLAN.md` verified against the real local model, and `AGENTS.md`/docs updated if anything described here changed.
