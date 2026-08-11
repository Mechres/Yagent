# Yagent

[![Go](https://img.shields.io/badge/Go-1.25-blue.svg)](https://go.dev/dl/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
<img width="992" height="681" alt="resim" src="https://github.com/user-attachments/assets/d96ec867-b821-4011-ba39-54b3fda60f15" />

A local-first AI agent for **code, audit, review, web search and research** — written in Go, running against OpenAI-compatible inference servers (Ollama, llama.cpp, or any cloud endpoint you opt into). It implements its own agent loop, memory, orchestration and tools — no LLM frameworks.

## Features

- **Agent loop with tools** — streaming chat, tool calling with risk-gated approvals, validation + retry with fuzzy argument aliasing, `/yolo` mode, goal mode with **workspace checkpoints** (`/checkpoint`) for safe autonomous runs, and per-hunk `fs_patch` approval in the TUI.
- **Memory** — SQLite sessions (`yagent sessions`, `chat --continue`, auto-titles, `/undo`), running-summary context budgeting, and hybrid semantic recall (vector + FTS5 + importance + recency).
- **Skills** — procedural memory as `SKILL.md` files; the agent creates, discovers and applies its own skills.
- **Codebase index** — gitignore-aware walker, tree-sitter chunking (go/py/js/ts/rust/c/cpp/java/bash/html/css), incremental re-embed, symbol-aware search, **call-graph references** (`code_references`: who calls X), per-turn code retrieval.
- **Web tools** — `web_search` (DuckDuckGo by default, Mojeek or SearXNG as alternatives) and `web_fetch` with HTML→text extraction.
- **Orchestration** — goal mode, parallel subagents with per-subagent **tool subsets** and a shared **scratchpad** (`scratch_write`/`scratch_read`), an advisor (`consult`) model, tool-output compaction to protect the context window.
- **Two UIs** — a bubbletea TUI and a plain REPL, sharing one runtime. The TUI has a 24-bit theme system (Tokyo Night default; Catppuccin Mocha and Nord selectable in `/settings`), a pill-style header/status bar with a live context gauge, markdown rendering for assistant messages, and **collapsible "thinking" blocks** for reasoning models (click the `🧠 thought` header, or press `t`; `Ctrl+M` toggles mouse capture so drag-select keeps working by default).
- **Diagnostics** — `yagent doctor`, slog logging, a golden YAML eval harness.

## Install

```bash
go install github.com/Mechres/Yagent@latest
```

Requires Go 1.22+ (built and tested with 1.25). Note: tree-sitter chunking needs **cgo** (a C toolchain) to build.

Then set up an inference server and pull a model:

```bash
ollama serve                 # or: llama.cpp llama-server --embeddings
ollama pull qwen2.5-coder:14b
ollama pull nomic-embed-text
```

## Quickstart

```bash
yagent doctor                 # diagnose config / server / model / embeddings
yagent chat                   # streaming TUI (or REPL with --plain)
yagent chat --goal "refactor the parser package"
```

By default Yagent talks to `http://localhost:11434` (Ollama). Point it elsewhere with `YAGENT_SERVER_URL` / `YAGENT_MODEL` / `config.yaml` — see [`config.example.yaml`](config.example.yaml).

## Security & privacy

- **Local-first by default** — LLM and embedding requests go only to the configured server. By default that's a local Ollama/llama.cpp — nothing leaves the machine.
- **Opt-in cloud** — set `api_key` (or `YAGENT_API_KEY`) and point `server_url` at any OpenAI-compatible endpoint (OpenRouter, Groq, Together, Gemini) to run the whole loop in the cloud; `consult` has its own `api_key` for a separate advisor model. Both are deliberate opt-ins — the default config stays local.
- **Redaction** — before anything is written to SQLite (messages, summaries, memories) or exported, likely secrets (`api_key`/`token`/`password`/`bearer` values) and home paths are scrubbed to `[redacted]`/`[home]` markers. This is a heuristic guard, not a security boundary. Session exports warn when they contain these markers.
- **Approvals + sandbox** — write/destructive tools require explicit approval (unless `/yolo`, which is pre-granted consent); `shell.sandbox: bwrap` additionally wraps `shell_exec` in bubblewrap and fails loudly if bubblewrap isn't installed.
- **No telemetry** — nothing leaves your machine except explicit `web_search`/`web_fetch` calls and, if configured, the consult advisor.

## Documentation

| Doc | Contents |
|---|---|
| [`AGENTS.md`](AGENTS.md) | Build/test commands, conventions, constraints — read this first |
| [`docs/PLAN.md`](docs/PLAN.md) | Milestones M1–M7 with tasks and acceptance criteria |
| [`docs/design/architecture.md`](docs/design/architecture.md) | System design, module layout, decision log |
| [`docs/design/agent-loop.md`](docs/design/agent-loop.md) | Agent loop, tool calling, context budgeting |
| [`docs/design/memory.md`](docs/design/memory.md) | Memory layers, storage schema, retrieval |
| [`docs/design/skills.md`](docs/design/skills.md) | Hermes-style skills: procedural memory, `SKILL.md` format, approval gate |
| [`docs/design/tools.md`](docs/design/tools.md) | Tool specifications and safety model |
| [`docs/models.md`](docs/models.md) | Model quirks from acceptance runs (tool-call reliability, embeddings) |
| [`config.example.yaml`](config.example.yaml) | Annotated configuration reference |
| [`CHANGELOG.md`](CHANGELOG.md) | Release history |

## Development

```bash
make build     # or: go build ./cmd/yagent
make test      # go test ./...
make vet       # go vet ./...
make race      # go test -race ./...
```

## License

[MIT](LICENSE) — see [CONTRIBUTING](CONTRIBUTING.md) if you'd like to help.
