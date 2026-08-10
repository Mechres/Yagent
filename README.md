# Yagent

A local-first AI agent for **code, audit, review, web search and research** — written in Go, running against locally-hosted open-weight models. No cloud LLM calls, ever.

## Principles

- **Local-first** — all inference and embeddings run on your own hardware via an OpenAI-compatible server (Ollama or llama.cpp). Nothing leaves the machine except explicit web-search/fetch tool calls.
- **Single binary** — one Go executable containing the agent loop, memory, orchestration, tools and UI. The only external process is the inference server.
- **Owned, not rented** — memory, orchestration and tool plumbing are implemented in-house (stdlib-first, minimal dependencies). No LLM frameworks.
- **Small-model-native** — designed around the strengths and limits of 7B–14B local models: few well-scoped tools, tight context budgets, explicit validation and retry.

## Target hardware

Developed and tuned for a single **AMD RX 6700 XT (12 GB, RDNA2 / gfx1031)**:

- 12B–14B class models at Q4 quantization, ~16–32k context
- Ollama via ROCm (requires `HSA_OVERRIDE_GFX_VERSION=10.3.0` on gfx1031) or llama.cpp server with the Vulkan backend

## Recommended models

| Role | Model | Notes |
|---|---|---|
| Chat / agent | `qwen2.5-coder:14b` (Q4) | Best tool-calling reliability at this size; tight on VRAM |
| Chat / agent (alt.) | `qwen3:8b` | Comfortable VRAM headroom, larger context |
| Chat / agent (alt.) | `gemma3:12b-it-qat` | Good quality, weaker tool calling than Qwen |
| Embeddings | `nomic-embed-text` | Used for memory + repo index |

## Status

**M1–M3.5 complete** — streaming chat CLI, tool loop (9 workspace-scoped tools with risk-gated approvals), memory (SQLite sessions with `yagent sessions` / `chat --continue <id>`, running-summary context budget, **SQLite hybrid semantic recall** — vector + FTS5 keyword + importance + recency — with `memory_save`/`memory_search`, session-end summaries), and skills (procedural memory): filesystem `SKILL.md` store, progressive disclosure, autonomous creation with a write-approval gate and dangerous-pattern scanner, `/skills` review commands and `/skill-name` invocation. Acceptance verified: 60-turn bounded session + cross-session recall e2e; chat, memory and skills run on Qwythos-9B via llama.cpp :8089 (`--embeddings --pooling mean`; the model-proposed skill flow — corrected → staged → approved → loaded next session — was exercised live). Next: **M4 — codebase index** per [`docs/PLAN.md`](docs/PLAN.md).

- Start here: [`AGENTS.md`](AGENTS.md) (contributor/agent guide)
- Execution plan: [`docs/PLAN.md`](docs/PLAN.md)
- Designs: [`docs/design/`](docs/design/)

## Quickstart (target state, post-M1)

```bash
# 1. Inference server (pick one)
ollama serve                       # ROCm build; on RX 6700 XT:
# HSA_OVERRIDE_GFX_VERSION=10.3.0 ollama serve

ollama pull qwen2.5-coder:14b
ollama pull nomic-embed-text

# 2. Build & run
go build ./cmd/yagent
./yagent chat                      # streaming REPL against localhost:11434
```

## Documentation

| Doc | Contents |
|---|---|
| [`AGENTS.md`](AGENTS.md) | Build/test commands, conventions, constraints — read this first |
| [`docs/PLAN.md`](docs/PLAN.md) | Milestones M1–M6 with tasks and acceptance criteria |
| [`docs/design/architecture.md`](docs/design/architecture.md) | System design, module layout, decision log |
| [`docs/design/agent-loop.md`](docs/design/agent-loop.md) | Agent loop, tool calling, context budgeting |
| [`docs/design/memory.md`](docs/design/memory.md) | Memory layers, storage schema, retrieval |
| [`docs/design/skills.md`](docs/design/skills.md) | Hermes-style skills: procedural memory, `SKILL.md` format, approval gate |
| [`docs/design/tools.md`](docs/design/tools.md) | Tool specifications and safety model |
| [`docs/models.md`](docs/models.md) | Model quirks from acceptance runs (tool-call reliability, embeddings) |
