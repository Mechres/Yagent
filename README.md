# Yagent

[![Go](https://img.shields.io/badge/Go-1.25-blue.svg)](https://go.dev/dl/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
<img width="992" height="681" alt="resim" src="https://github.com/user-attachments/assets/d96ec867-b821-4011-ba39-54b3fda60f15" />

A local-first AI agent for **code, audit, review, web search and research** — written in Go, running against OpenAI-compatible inference servers (Ollama, llama.cpp, or any cloud endpoint you opt into). It implements its own agent loop, memory, orchestration and tools — no LLM frameworks.

## Features

- **Agent loop with tools** — streaming chat, risk-gated approvals with TUI diff previews, per-hunk `fs_patch` approval, validation + retry with fuzzy argument aliasing, `/yolo`, **Esc cancels the running turn** (the session stays alive), and a **loop guard** that auto-stops repeating-generation loops.
- **Memory** — SQLite sessions (`yagent sessions`, `chat --continue`, `/undo`, Markdown/HTML exports), **accurate token counting** (server tokenizer), a budget that first prunes old tool output then summarizes, and hybrid semantic recall (vector + FTS5 + importance + recency).
- **Skills** — procedural memory as `SKILL.md` files with progressive disclosure, autonomous creation + a verification harness (`/skills verify`), a TUI skills manager modal (bare `/skills`), and `yagent skills import`.
- **Codebase index** — gitignore-aware walker, tree-sitter chunking (go/py/js/ts/rust/c/cpp/java/bash/html/css), incremental re-embed, symbol-aware search, call-graph `code_references`, and **`code_slice`** for surgical single-declaration reads.
- **Code guardrails** — `workspace_diagnostics` (auto-runs `go vet`/`tsc --noEmit`/`cargo check`/`ruff`), **pre-flight syntax validation** (a broken edit never reaches disk), `fs_refactor` (undo-aware symbol rename), and fuzzy path pre-resolution (`README` → `README.md`).
- **Web tools** — `web_search` (DuckDuckGo default, Mojeek/SearXNG alternatives, provider fallback) and `web_fetch` with HTML→text extraction.
- **Orchestration** — goal mode with workspace checkpoints and `--resume-goal`, declarative **playbooks** (`.yagent/playbooks/*.yaml`), parallel subagents with preset **roles** (architect/auditor/test-engineer/docs-writer), tool subsets and a shared scratchpad, an advisor (`consult`) model, and **`clarify`/`plan`** tools for structured user handoffs.
- **Two UIs** — a bubbletea TUI and a plain REPL sharing one runtime: 24-bit themes (Tokyo Night default; Catppuccin/Nord in `/settings`), pill header/status bar with a live context gauge, markdown rendering, collapsible "thinking" blocks, **Ctrl+F transcript search**, and interactive settings/sessions/skills modals.
- **Tuning & diagnostics** — per-model sampling profiles, `sampling.min_p`/`repetition_penalty` knobs, context-window auto-detect (budget capped at the server's real `n_ctx`), `yagent doctor`, **`yagent calibrate`** (live benchmark across sampling recipes), `--trace` prompt dumps, a golden YAML eval harness, and a live small-model benchmark.

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
yagent chat --playbook release-checklist   # run a declarative workflow
yagent calibrate              # tune sampling for your local model
yagent sessions export <id> --format html  # share a session as HTML
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
| [`docs/models-benchmark.md`](docs/models-benchmark.md) | Which model to run, what to expect, and recommended settings (benchmarked on an RX 6700 XT) |
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
