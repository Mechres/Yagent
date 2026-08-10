# Architecture

## System context

```
┌────────────────────────────────────────────────┐
│ yagent — single Go binary                      │
│                                                │
│  ui (REPL/TUI)                                 │
│    └─ agent (loop, context assembly, budget)   │
│        ├─ llm    (OpenAI-compatible client)    │
│        ├─ tools  (fs, shell, git, web, ...)    │
│        ├─ memory (sessions, vectors, summary)  │
│        └─ index  (repo chunking + embeddings)  │
│                    │                           │
│              internal/config                   │
└───────────────────────┬────────────────────────┘
                        │ HTTP, localhost only
┌───────────────────────▼────────────────────────┐
│ Ollama or llama.cpp llama-server               │
│   /v1/chat/completions  (chat + tool calling)  │
│   /v1/embeddings        (nomic-embed-text)     │
│   GPU: ROCm (HSA override) or Vulkan           │
└────────────────────────────────────────────────┘
```

The Go binary owns everything except raw inference. The server is swappable because the interface is the OpenAI-compatible HTTP API — nothing Ollama-specific may leak into the codebase.

## Module layout

| Package | Responsibility | Depends on |
|---|---|---|
| `cmd/yagent` | flags, config wiring, subcommand dispatch | all internals |
| `internal/config` | yaml config + defaults + env overrides | stdlib only |
| `internal/llm` | chat completions (streaming SSE), tool-call parsing, embeddings, retry/backoff | config |
| `internal/tools` | `Tool` interface, registry, fs/shell/git/web implementations, approval hooks | config |
| `internal/memory` | session store (SQLite), summarizer, vector memory | llm, config |
| `internal/index` | repo walker, tree-sitter chunker, embedding store, semantic search | llm, config |
| `internal/agent` | the loop, context assembly, token budgeting, tool dispatch | llm, tools, memory, index |
| `internal/ui` | REPL first, bubbletea TUI in M6 | agent |

Import direction is one-way: `ui → agent → {llm, tools, memory, index} → config`. No cycles. `llm` knows nothing about tools or agents — it speaks typed request/response structs.

## Data flow (one user turn)

1. **ui** receives input, hands to `agent.Run(ctx, input)`.
2. **agent** assembles context: system prompt + tool schemas → long-term memory retrieval (memory + index) → session history (budgeted, summarized if needed) → user message.
3. **llm** streams a chat completion with tool schemas. Tokens stream to **ui** live.
4. If the response contains `tool_calls`: **agent** validates args against the schema, runs tools (read-only ones in parallel via `errgroup`-style goroutines; approvals prompted via ui), appends results, loops to 3.
5. Loop ends on: final text response, max iterations, context cancellation, or unrecoverable error.

## Persistence

Everything under `~/.local/share/yagent/` (configurable):

- `yagent.db` — SQLite: sessions, messages, memories (schema in `memory.md`)
- `config.yaml` — user config at `~/.config/yagent/config.yaml`
- repo indexes live in the same DB, keyed by absolute repo path + content hash

## Decision log

| # | Decision | Alternatives rejected | Why |
|---|---|---|---|
| D1 | External inference server (HTTP) | Embedded llama.cpp via cgo | GPU/backend wrangling (ROCm gfx1031 quirks, Vulkan) stays in the server; Go binary iterates fast; language speed is irrelevant to agent latency |
| D2 | Go | Python, TS, Rust, C++ | Single static binary, good-enough ecosystem, fast iteration; see project discussion — agent quality is context engineering, not glue-code speed |
| D3 | OpenAI-compatible API only | Ollama native API, per-server adapters | One client works against Ollama AND llama-server; tool calling + embeddings both covered |
| D4 | Own loop/memory/orchestration | langchaingo, other frameworks | Frameworks are built for frontier models and cloud APIs; small local models need tight control of context and tool protocols |
| D5 | Single agent loop first | Multi-agent / planner-executor from day one | Premature orchestration is the top failure mode; add subagents (M7) only if M1–M6 prove the need |
| D6 | SQLite (modernc.org/sqlite, pure Go) | boltDB, JSON files, external DB | One file, queryable, no cgo, covers sessions+memory+index |
| D7 | chromem-go for vectors | sqlite-vec, faiss | Pure Go, in-memory + persistable, OpenAI-compat embedding funcs built in; revisit if it bottlenecks |
| D8 | Qwen-family primary model | Gemma 3 | Tool-calling reliability at 7B–14B is the project's biggest risk; Qwen is measurably better at it |
| D9 | Approval-gated destructive tools | full auto, allowlist-only | Local agent ≠ safe agent; shell/git-mutation/cross-workspace writes always ask |

## Non-goals (for now)

- Cloud LLM providers (hard constraint, permanent)
- Embedded inference (revisit only after the agent design stabilizes)
- Multi-agent orchestration (M7, optional)
- Plugin system / MCP client (revisit after M6)
