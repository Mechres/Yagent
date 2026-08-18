# Architecture

## System context

```
┌────────────────────────────────────────────────┐
│ yagent — single Go binary                      │
│                                                │
│  ui (REPL/TUI)                                 │
│    └─ agent (loop, context assembly, budget)   │
│        ├─ llm    (OpenAI-compatible client)    │
│        ├─ tools  (fs, shell, git, web, MCP…)   │
│        ├─ memory (sessions, vectors, summary)  │
│        ├─ index  (repo chunking + embeddings)  │
│        └─ mcp/jobs/gitops (optional services)  │
│                    │                           │
│              internal/config                   │
└───────────────────────┬────────────────────────┘
                        │ OpenAI-compatible HTTP
┌───────────────────────▼────────────────────────┐
│ Local Ollama / llama.cpp (default)             │
│ or an explicitly configured cloud endpoint     │
│   /v1/chat/completions  (chat + tool calling)  │
│   /v1/embeddings        (nomic-embed-text)     │
│   local GPU: ROCm (HSA override) or Vulkan     │
└────────────────────────────────────────────────┘
```

The Go binary owns everything except raw inference. The server is swappable because the interface is the OpenAI-compatible HTTP API — nothing Ollama-specific may leak into the codebase.

## Module layout

| Package | Responsibility | Depends on |
|---|---|---|
| `cmd/yagent` | flags, config wiring, subcommand dispatch | all internals |
| `internal/config` | yaml config + defaults + env overrides | stdlib only |
| `internal/llm` | chat completions (streaming SSE), tool-call parsing, embeddings, retry/backoff | config |
| `internal/tools` | `Tool` interface, registry, filesystem/shell/git/web/index tools, approvals and hooks | config, optional subsystems |
| `internal/memory` | session store (SQLite), summarizer, vector memory | llm, config |
| `internal/index` | repo walker, tree-sitter chunker, embedding store, semantic search | llm, config |
| `internal/mcp` | stdio/HTTP Model Context Protocol client | llm types, config |
| `internal/jobs` / `internal/gitops` | session-scoped background jobs / durable Git turn safety | stdlib |
| `internal/agent` | the loop, context assembly, token budgeting, tool dispatch | llm, tools, memory, index |
| `internal/ui` | shared Bubble Tea TUI and plain REPL runtime | agent |

Import direction is one-way: `ui → agent → {llm, tools, memory, index} → config`. No cycles. `llm` knows nothing about tools or agents — it speaks typed request/response structs.

## Data flow (one user turn)

1. **ui** receives input, hands to `agent.Run(ctx, input)`.
2. **agent** assembles one leading system message, budgeted memory/index retrieval, session history, and the current user message. It accounts for the tool-schema cost too.
3. **llm** streams a chat completion with tool schemas. Tokens stream to **ui** live.
4. If the response contains `tool_calls`: **agent** validates args, asks for approval when required, executes read-only calls in parallel, appends results, and loops to 3. Write verification and goal/test gates can add a deterministic follow-up before a final answer is accepted.
5. Loop ends on a verified final response, max iterations, context cancellation, or an unrecoverable startup error.

## Persistence

Global state lives under `$XDG_DATA_HOME/yagent` (falling back to
`~/.local/share/yagent`) unless `data_dir` overrides it:

- `sessions.db` — SQLite: sessions, messages, global memory, and repo indexes
- `skills/` and `pending/skills/` — global skills and staged skill writes
- `config.yaml` — user configuration under the OS config directory
- `.yagent/config.yaml` — optional repository overlay; `.yagent/memory/memory.db`
  holds project memory; checkpoints, scratchpad, research reports, and playbooks
  also live beneath `.yagent/`

## Decision log

| # | Decision | Alternatives rejected | Why |
|---|---|---|---|
| D1 | External inference server (HTTP) | Embedded llama.cpp via cgo | GPU/backend wrangling (ROCm gfx1031 quirks, Vulkan) stays in the server; Go binary iterates fast; language speed is irrelevant to agent latency |
| D2 | Go | Python, TS, Rust, C++ | Single static binary, good-enough ecosystem, fast iteration; see project discussion — agent quality is context engineering, not glue-code speed |
| D3 | OpenAI-compatible API | Ollama native API, per-server adapters | One client works against Ollama, llama.cpp, and user-configured cloud endpoints; tool calling + embeddings share one contract |
| D4 | Own loop/memory/orchestration | langchaingo, other frameworks | Frameworks are built for frontier models and cloud APIs; small local models need tight control of context and tool protocols |
| D5 | One primary loop with bounded subagents | Planner/executor swarm by default | The main loop remains simple; isolated read-only subagents are available for context-heavy work when explicitly delegated |
| D6 | SQLite (modernc.org/sqlite, pure Go) | boltDB, JSON files, external DB | One file, queryable, no cgo, covers sessions+memory+index |
| D7 | SQLite hybrid retrieval | chromem-go, sqlite-vec, faiss | Vectors, FTS5 keywords, importance, and recency share the existing SQLite store with no extra service or ANN dependency |
| D8 | Qwen-family primary model | Gemma 3 | Tool-calling reliability at 7B–14B is the project's biggest risk; Qwen is measurably better at it |
| D9 | Approval-gated destructive tools | full auto, allowlist-only | Local agent ≠ safe agent; shell and workspace writes ask unless the user explicitly enables session-level `/yolo` consent |
| D10 | Borrow contracts, not framework architecture | DeepSeek Harness/Cordis wholesale | Yagent should adopt evidence-backed replay, fault-injection, structured tool-result, and presentation contracts incrementally while retaining a small synchronous Go loop |
| D11 | Borrow deterministic context ergonomics, not deployment surface | Hermes gateway/plugin/cloud runtime wholesale | Progressive instructions, explicit context references, bounded memory, and structured compaction help local models; multi-platform delivery and remote backends do not |

## Non-goals

- Embedded inference or provider-specific SDKs; inference remains an external,
  OpenAI-compatible service.
- Unbounded autonomous swarms; subagents are deliberately isolated and scoped.
- A hosted Yagent service or telemetry pipeline; local operation is the default.

## External Harness Review

The DeepSeek Harness review identified five potentially useful contracts for
future work: a reusable LLM fault/replay testkit, compact durable request
manifests, an append-only event extension for exact replay, structured tool
outcomes with UI-neutral presentation metadata, and monotonic tool guards.
These are tracked in `improvement.md`. The Harness's Cordis plugin architecture
is intentionally not adopted: it would add indirection and lifecycle surface
without evidence that Yagent's local model loop needs it.

The Hermes Agent review identified a separate queue: progressive nested
instructions, deterministic `@` context references, model-facing historical
session search, structured boundary-safe compaction, bounded always-on memory,
skill bundles, and recoverable skill lifecycle maintenance. These are tracked
in `improvement.md`; the Hermes gateway, remote backends, and Python plugin
surface are explicitly outside Yagent's scope.
