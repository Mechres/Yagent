<div align="center">
<img width="300" height="300" alt="resim" src="https://github.com/user-attachments/assets/1516e379-c7e8-4ef1-b8be-bcd611ca6d01" />


# Yagent
</div>

[![Go](https://img.shields.io/badge/Go-1.25-blue.svg)](https://go.dev/dl/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![CI](https://github.com/Mechres/Yagent/actions/workflows/ci.yml/badge.svg)](https://github.com/Mechres/Yagent/actions/workflows/ci.yml)

A local-first AI agent for **code, audit, review, web search and research** — written in Go, running against OpenAI-compatible inference servers (Ollama, llama.cpp, or any cloud endpoint you opt into). It implements its own agent loop, memory, orchestration and tools — no LLM frameworks.

## Features

- **Agent loop with tools** — streaming chat, risk-gated approvals with TUI change summaries and diff previews, per-hunk `fs_patch` approval (`a` accepts remaining hunks; `x` rejects them), validation + retry with fuzzy argument aliasing, `/yolo`, **Esc cancels the running turn** (the session stays alive), and a **loop guard** that auto-stops repeating-generation loops.
- **Deterministic "compiles, runs, behaves, and actually finished" gates** — `workspace_diagnostics` (auto `go vet`/`tsc`/`cargo check`/`ruff`/`make`/`cmake`), **`test_runner`** (targeted tests for Go/Rust/Python/JS-TS), **`runtime_smoke`** (builds and runs the program — even browser JS under a headless node DOM shim — asserting it doesn't crash and behaves, via scripted `steps`), **pre-flight syntax + YAML/JSON validation**, `diff_semantic` (symbol-delta guardrail), **`GoalGate`/`TestGate`** (refuse completion on broken builds/tests), **goal success predicates** (`--check "file contains text"` refuses DONE until the declared conditions hold), and **codegen mode** (whole-file writes + compile-gated answers for greenfield builds).
- **Git-backed session safety** (aider-style) — each turn auto-commits as `yagent: turn N` (dirty user files are snapshotted up front, never lost or mixed in), `/undo`/`/undo list`/`/undo <N>` revert via git (crash-safe), and **`/diff`** shows the cumulative session diff with `/diff discard` — a plandex-style "review before you keep" sandbox.
- **Memory** — SQLite sessions (`yagent sessions`, `chat --continue`, `/undo` multi-turn revert, Markdown/HTML exports), **accurate token counting** (server tokenizer), a budget that first prunes old tool output then summarizes, and hybrid semantic recall (vector + FTS5 + importance + recency).
- **Skills** — procedural memory as `SKILL.md` files with progressive disclosure, autonomous creation + a verification harness (`/skills verify`), a TUI skills manager modal (bare `/skills`), and `yagent skills import`.
- **Codebase index** — gitignore-aware walker, tree-sitter chunking (go/py/js/ts/rust/c/cpp/java/bash/html/css), incremental re-embed, symbol-aware search, call-graph `code_references`, **`code_impact`** (change radius before an edit), **`code_topology`** (package DAG), **`code_unused`** (dead-symbol candidates), **`code_slice`** for surgical single-declaration reads, **`code_environment`** (toolchain & native-binding audit), and **`code_references`**-style downstream-impact hints in compile errors.
- **Web tools** — `web_search` (DuckDuckGo default, Mojeek/SearXNG alternatives, provider fallback) and `web_fetch` with HTML→text extraction — results wrapped as untrusted data (prompt-injection defense).
- **Orchestration** — goal mode with workspace checkpoints and `--resume-goal`, declarative **playbooks** (`.yagent/playbooks/*.yaml`), parallel subagents with preset **roles** (architect/auditor/test-engineer/docs-writer), tool subsets and a shared scratchpad, an advisor (`consult`) model, a **`summarizer`** model (offload history condensation to a second machine), and **`clarify`/`plan`** tools for structured user handoffs — plus **read-only plan mode** (`/plan`: explore before editing).
- **Extensible** — **MCP support** (`mcp:` config attaches any Model Context Protocol server; each tool registers as `<server>_<tool>`), a **hook bus** (`hooks:` config runs deterministic pre/post-tool policy — a pre-hook can veto a call), and **approval allow-remember** (approved tool+args auto-approve for the session).
- **Provider/model selector** (`/model` in the TUI) — pick from a built-in catalog (Local llama.cpp/Ollama, OpenCode Zen/Go, DeepSeek, OpenRouter, Groq, Together, Mistral, NVIDIA NIM) with a **live model list** for local servers (`/v1/models`) and **cloud models synced from models.dev** — no stale catalogs. API keys are entered inline in the TUI (or `/key` in the REPL) and stored in the config file's `api_key` field (`/key clear` removes them; env vars like `DEEPSEEK_API_KEY` take precedence and are never written to disk). Model selection warns when a model is weak at tool calling.
- **Two UIs** — a bubbletea TUI and a plain REPL sharing one runtime: 24-bit themes (Tokyo Night default; Catppuccin/Nord in `/settings`), pill header/status bar with a live context gauge and turns-to-window forecast, markdown rendering, collapsible "thinking" blocks, **Ctrl+F transcript search**, **`/compact`** (distill the session into a ledger), interactive settings/sessions/skills modals, **`/tools`** activity inspector, **`/workspace`** overview, `/sessions <query>` filtering, **`/model`** provider picker, and **OS notifications** when an approval is needed or a goal run finishes. Saved API keys are masked in the TUI settings list.
- **Tuning & diagnostics** — per-model sampling profiles, `sampling.min_p`/`repetition_penalty`/`reasoning_max_tokens` knobs, context-window auto-detect (budget capped at the server's real `n_ctx`), **adaptive system-prompt compression** (lean prompt above 70% context), `yagent doctor`, **`yagent calibrate`** (live benchmark across sampling recipes), `yagent bench --repeat 3` with regression baseline tracking, `--trace` prompt dumps, a golden YAML eval harness, a live small-model benchmark, a **VRAM pressure detector** (auto-prunes context when streaming slows — KV spill), a **diagnostic error sanitizer** (error cascades collapse to top root causes), `fs_edit` **whitespace auto-alignment**, **missing-import preflight** on writes, **atomic multi-file `fs_patch`** (all-or-nothing), and **`yagent export-dataset`** (verified sessions → OpenAI/ShareGPT/DPO fine-tuning JSONL).

## Install

```bash
go install github.com/Mechres/Yagent@latest
```

Requires Go 1.22+ (built and tested with 1.25). Note: tree-sitter chunking needs **cgo** (a C toolchain) to build.

Then set up an inference server and pull a model:

```bash
ollama serve                 # or: llama.cpp llama-server --embeddings
ollama pull qwen3vl:8b       # or your GGUF on llama.cpp (e.g. Qwen3VL-8B-Instruct-Q4_K_M.gguf)
ollama pull nomic-embed-text
```

## Quickstart

```bash
yagent init                   # create starter config if none exists
yagent doctor                 # diagnose config / server / model / embeddings / toolchain
yagent chat                   # streaming TUI (or REPL with --plain)
yagent chat --goal "refactor the parser package"    # autonomous goal loop
yagent chat --goal "build a tetris" --check "tetris.cpp exists"  # + deterministic success checks
yagent chat --playbook release-checklist   # run a declarative workflow
yagent bench --repeat 3       # run canonical benchmarks and record baseline
yagent calibrate              # tune sampling for your local model
yagent export-dataset --output fine-tune.jsonl --format sharegpt   # turn verified sessions into a training set
yagent export-dataset --format dpo --output preferences.jsonl      # DPO/ORPO preference pairs (failed -> success)
yagent sessions export <id> --format html  # share a session as HTML
```

By default Yagent talks to `http://localhost:11434` (Ollama). Point it elsewhere with `YAGENT_SERVER_URL` / `YAGENT_MODEL` / `config.yaml` — see [`config.example.yaml`](config.example.yaml). In the TUI, **`/model`** picks a provider and model (local models auto-detected; cloud lists live from models.dev).

### TUI controls

| Command / key | Effect |
|---|---|
| `/tools` | Browse tool calls; `f` filters, `g` jumps to the transcript activity, Enter expands details, and PgUp/PgDn/Home/End navigate long lists. |
| `/workspace` | Show workspace, branch, context use, tool count, undo availability, and queued-work state. A compact drawer appears during active turns on wide terminals. |
| `/sessions <query>` | Filter sessions by ID or generated title. In the browser, `p` previews the selection and `s` toggles recent/title order. |
| Enter while working | Queue one follow-up message; a later queued message replaces the earlier one. |
| `a` / `x` during patch review | Accept all remaining hunks / reject all remaining hunks. |
| `/set ui.accessibility high-contrast` | Persist a high-contrast TUI palette; set `standard` to restore it. |
| `/set ui.accessibility ascii` | Use ASCII labels instead of emoji for limited terminal fonts. `NO_COLOR=1` suppresses color styling. |

## Security & privacy

- **Local-first by default** — LLM and embedding requests go only to the configured server. By default that's a local Ollama/llama.cpp — nothing leaves the machine.
- **Opt-in cloud** — set `api_key` (or `YAGENT_API_KEY`) and pick a cloud provider via **`/model`** (OpenCode Zen/Go, DeepSeek, OpenRouter, Groq, Together, Mistral, NVIDIA NIM) to run the whole loop in the cloud; `consult` has its own `api_key` for a separate advisor model. Keys entered via the TUI `/model` prompt or REPL `/key` are stored in the config file's `api_key` field (`/key clear` removes them); keys from environment variables are never written to disk and take precedence. Both are deliberate opt-ins — the default config stays local.
- **Redaction** — before anything is written to SQLite (messages, summaries, memories) or exported, likely secrets (`api_key`/`token`/`password`/`bearer` values) and home paths are scrubbed to `[redacted]`/`[home]` markers. This is a heuristic guard, not a security boundary. Session exports warn when they contain these markers.
- **Approvals + sandbox** — write/destructive tools require explicit approval (unless `/yolo`, which is pre-granted consent; approved tool+args auto-approve for the rest of the session); `shell.sandbox: bwrap` additionally wraps `shell_exec` in bubblewrap and fails loudly if bubblewrap isn't installed. **Read-only plan mode** (`/plan`) lets you keep the agent in explore-only mode until you approve a plan.
- **Untrusted web content** — `web_fetch`/`web_search` results are wrapped as `<untrusted data ...>` data (never commands), closing prompt-injection via a fetched page.
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
| [`docs/RESEARCH-other-tools.md`](docs/RESEARCH-other-tools.md) | What we borrow from opencode/aider/plandex (and the broader field) and what we deliberately skip |
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
