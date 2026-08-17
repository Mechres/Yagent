<p align="center">
  <img width="300" height="300" alt="Yagent logo" src="https://github.com/user-attachments/assets/1516e379-c7e8-4ef1-b8be-bcd611ca6d01">
</p>

<h1 align="center">Yagent</h1>

<p align="center">
  A local-first AI agent for coding, audits, reviews, web search, and research.
</p>

<p align="center">
  <a href="https://go.dev/dl/"><img src="https://img.shields.io/badge/Go-1.25-blue.svg" alt="Go 1.25"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="MIT License"></a>
  <a href="https://github.com/Mechres/Yagent/actions/workflows/ci.yml"><img src="https://github.com/Mechres/Yagent/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
</p>

Yagent is written in Go and runs against OpenAI-compatible inference servers: Ollama, llama.cpp, or a cloud endpoint you explicitly configure. It owns the agent loop, memory, orchestration, and tools—no LLM framework required.

## Why Yagent

- **Make changes safely.** Stream tool use, review risk-gated writes and diffs, approve `fs_patch` hunks individually, and cancel a running turn with Esc without losing the session. A loop guard stops repeated generation; `/yolo` is available when you deliberately want automatic approvals.
- **Verify work before calling it done.** Built-in diagnostics, targeted tests, runtime smoke checks, syntax/YAML/JSON validation, and semantic-diff protection catch common mistakes. Goal and test gates can refuse completion until declared checks pass; codegen mode is tuned for greenfield builds.
- **Keep every turn recoverable.** In Git repositories, turn commits preserve pre-existing work and power crash-safe `/undo`; `/diff` shows the cumulative session change before you keep it. SQLite sessions support resume, search, and Markdown or HTML export.
- **Work across longer tasks.** Context is token-budgeted with old tool output pruned before summaries; hybrid memory combines vector search, FTS5, importance, and recency. Skills provide reusable `SKILL.md` procedures with progressive disclosure and optional verification.
- **Understand a codebase before editing it.** A gitignore-aware, tree-sitter index supports structural search, surgical symbol reads, call references, impact analysis, topology, unused-symbol candidates, and environment audits.
- **Delegate and automate deliberately.** Use goal mode, resumable checkpoints, declarative playbooks, parallel read-only subagents, and a shared scratchpad. `clarify` and `plan` provide structured handoffs; `/plan` keeps exploration read-only.
- **Stay local by default, extend when needed.** Web results are treated as untrusted data; DuckDuckGo, Mojeek, and SearXNG are supported. MCP tools, deterministic hooks, an optional advisor, and a separate summarizer model extend the workflow without changing the core loop.
- **Choose the model and interface that suit the job.** The Bubble Tea TUI and plain REPL share one runtime. The TUI includes provider/model selection, settings, sessions, skills, tool activity, workspace overview, transcript search, themes, accessibility modes, and notifications. Local models are discovered live; cloud choices come from models.dev.

For the full command and safety model, see [the tool documentation](docs/design/tools.md). For local-model results and recommended settings, see [the benchmark guide](docs/models-benchmark.md).

## Install and run

```bash
go install github.com/Mechres/Yagent@latest
```

Requires Go 1.22+ (built and tested with Go 1.25). Tree-sitter indexing requires cgo, so install a C toolchain too.

Start a local inference server and pull a chat model plus an embedding model:

```bash
ollama serve                 # or: llama.cpp llama-server --embeddings
ollama pull qwen3vl:8b       # or load a GGUF with llama.cpp
ollama pull nomic-embed-text
```

Yagent defaults to Ollama at `http://localhost:11434`. Configure another OpenAI-compatible endpoint through `YAGENT_SERVER_URL`, `YAGENT_MODEL`, or `config.yaml`; a repository-local `.yagent/config.yaml` overrides the global configuration. See [`config.example.yaml`](config.example.yaml) for every setting.

## Quickstart

```bash
yagent init                                      # write a starter configuration
yagent doctor                                    # verify server, model, embeddings, and toolchain
yagent chat                                      # open the streaming TUI (--plain for the REPL)
yagent chat --goal "refactor the parser package"  # run an autonomous goal loop
yagent chat --goal "build a tetris" --check "tetris.cpp exists"
yagent chat --playbook release-checklist         # run a declarative workflow
yagent bench --repeat 3                          # measure and record a model baseline
yagent calibrate                                 # find a sampling recipe for the current model
```

Useful follow-ons:

```bash
yagent sessions export <id> --format html                 # share a session
yagent export-dataset --format sharegpt --output data.jsonl  # export verified trajectories
yagent export-dataset --format dpo --output preferences.jsonl
```

In the TUI, `/model` selects a provider and model (local models are auto-detected; cloud choices refresh from models.dev).

### Useful TUI controls

| Command / key | Effect |
|---|---|
| `/tools` | Browse tool calls; `f` filters, `g` jumps to transcript activity, Enter expands details, and PgUp/PgDn/Home/End navigate. |
| `/workspace` | Show workspace, branch, context use, tool count, undo availability, and queued-work state. A compact drawer appears during active turns on wide terminals. |
| `/sessions <query>` | Filter sessions by ID or generated title. In the browser, `p` previews, `s` changes ordering, `n` renames, and `*` pins. |
| Enter while working | Queue one follow-up message; a later queued message replaces the earlier one. |
| `a` / `x` during patch review | Accept all remaining hunks / reject all remaining hunks. |
| `/set ui.accessibility high-contrast` | Persist a high-contrast TUI palette; set `standard` to restore it. |
| `/set ui.accessibility ascii` | Use ASCII labels instead of emoji for limited terminal fonts. `NO_COLOR=1` suppresses color styling. |
| `/set ui.reduced_motion true` | Static spinner (no animation) for vestibular sensitivity. |

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
