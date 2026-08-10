# AGENTS.md — Yagent contributor & agent guide

Read this file before touching anything. It defines how the project is built, where things live, and the rules that are not negotiable.

## What this is

Yagent is a **local-first AI agent** (code / audit / review / web search / research) in **Go**. It talks to a local OpenAI-compatible inference server (Ollama or llama.cpp `llama-server`) and implements its own agent loop, memory, orchestration and tools. Target hardware: RX 6700 XT 12 GB with 7B–14B Q4 models.

## Current status

**Milestone M3.5 complete** (see [`docs/PLAN.md`](docs/PLAN.md)); work the milestones in order, each acceptance criteria must pass against the real local model. State of the tree:

- M1: streaming chat CLI. M2: tool loop (9 fs/shell/git tools, approvals, validation retry). Both accepted on real hardware (Qwythos-9B on :8089).
- M3 shipped: `internal/memory` — L2 SQLite session store (`yagent sessions`, `chat --continue <id>`, auto-titles, `HistoryAfter` for resumes), L1 token budget (summarizes oldest half into a running summary before every request; `context_window`/`YAGENT_CONTEXT_WINDOW` knob), L3 chromem vector memory with real `/v1/embeddings` wiring (`nomic-embed-text`), `memory_save`/`memory_search` tools, per-turn top-5 recall injection (budgeted, session-deduped), session-end summary job. Data dir: `$XDG_DATA_HOME/yagent` (deleting it = forget everything).
- M3.5 shipped: `internal/skills` — procedural memory. Filesystem SKILL.md store (global `<data>/skills/` + project `<workspace>/.yagent/skills/`, project shadows global), agentskills.io frontmatter via `yaml.v3` with store-managed lifecycle metadata, `skills_list`/`skill_view`/`skill_manage` tools, progressive disclosure (L0 index in the system context, capped 40/~3k tokens, evicted by `last_used`), end-of-turn autonomous-creation prompt (trigger: 5+ tool calls; ≤2 staged writes/session), write approval gate `skills.write_approval` (default ON → stage under `<data>/pending/skills/`; `/skills pending|diff|approve|reject|approval`), dangerous-pattern scanner (block `rm -rf /`/exfil; flag prompt-injection/eval), dedup-on-create (suggests patch), `/skill-name` to load a skill into context.
- **Hardware constraint discovered in M3.5**: the Qwythos llama.cpp template accepts **only one system message** per request — `assembleContext` therefore merges system prompt + L0 index + running summary + recall + injected skills into a single leading system message. Keep it that way. See [`docs/models.md`](docs/models.md).
- All M3/M3.5 tasks ticked; acceptance verified: 60-turn bounded session + resume, remember→recall across sessions (e2e, fake servers), clean-slate store, and on real hardware (Qwythos-9B on :8089): model-proposed skill → corrected after a validation error → staged → approved via `/skills` → listed next session → loaded via `/skill-name`. `go build`/`vet`/`test`/`gofmt` clean.
- **Real-hardware note**: the dev llama.cpp server now runs `--embeddings --pooling mean`, so L3 semantic recall works on :8089 with Qwythos as the embedder (chat + embeddings share one server; the requested embedding model name is ignored). Ollama remains an alternative (`nomic-embed-text`). See [`docs/models.md`](docs/models.md).
- Next: **M4 — codebase index** (tree-sitter chunking, `index_repo`/`index_search`, incremental re-embed).

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

- Dev server in use: llama.cpp `llama-server` (Vulkan) on **port 8089**, model `Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf`, started with `--embeddings --pooling mean --jinja` — set `YAGENT_SERVER_URL=http://localhost:8089` and `YAGENT_MODEL=Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf`. See [`docs/models.md`](docs/models.md) for its quirks.
- **Embeddings**: served by the same llama.cpp server (the loaded model embeds; the requested model name is ignored), so `YAGENT_EMBEDDING_MODEL` (default `nomic-embed-text`) is cosmetic on this setup. Ollama serves `nomic-embed-text` out of the box as an alternative.
- Ollama on RX 6700 XT (gfx1031) needs `HSA_OVERRIDE_GFX_VERSION=10.3.0`; default config targets Ollama at `http://localhost:11434`.
- Context window: `YAGENT_CONTEXT_WINDOW` / `context_window` (default 16384). Data lives under `YAGENT_DATA_DIR` / `data_dir` (default `$XDG_DATA_HOME/yagent`); deleting it is a complete "forget everything".
- If SearXNG is used for web search, enable `format: json` in its settings.yml.

## Definition of done (every milestone)

Builds clean, `go vet` clean, tests pass, milestone acceptance criteria in `docs/PLAN.md` verified against the real local model, and `AGENTS.md`/docs updated if anything described here changed.
