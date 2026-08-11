# AGENTS.md — Yagent contributor & agent guide

Read this file before touching anything. It defines how the project is built, where things live, and the rules that are not negotiable.

## What this is

Yagent is a **local-first AI agent** (code / audit / review / web search / research) in **Go**. It talks to a local OpenAI-compatible inference server (Ollama or llama.cpp `llama-server`) and implements its own agent loop, memory, orchestration and tools. Target hardware: RX 6700 XT 12 GB with 7B–14B Q4 models.

## Current status

**Milestone M6 complete** (see [`docs/PLAN.md`](docs/PLAN.md)); all milestones shipped. State of the tree:

- M1: streaming chat CLI. M2: tool loop (9 fs/shell/git tools, approvals, validation retry). Both accepted on real hardware (Qwythos-9B on :8089).
- M3 shipped: `internal/memory` — L2 SQLite session store (`yagent sessions`, `chat --continue <id>`, auto-titles, `HistoryAfter` for resumes), L1 token budget (summarizes oldest half into a running summary before every request; `context_window`/`YAGENT_CONTEXT_WINDOW` knob), L3 semantic memory. Since M3.5 L3 is **SQLite hybrid** (vectors + FTS5 keyword + importance + recency, no chromem): `memory_save`/`memory_search` tools, per-turn top-5 recall injection (budgeted, session-deduped), session-end summary job. `embedding_server_url` (defaults to `server_url`) lets you point embeddings at a dedicated model. Data dir: `$XDG_DATA_HOME/yagent` (deleting it = forget everything).
- M3.5 shipped: `internal/skills` — procedural memory. Filesystem SKILL.md store (global `<data>/skills/` + project `<workspace>/.yagent/skills/`, project shadows global), agentskills.io frontmatter via `yaml.v3` with store-managed lifecycle metadata, `skills_list`/`skill_view`/`skill_manage` tools, progressive disclosure (L0 index in the system context, capped 40/~3k tokens, evicted by `last_used`), end-of-turn autonomous-creation prompt (trigger: 5+ tool calls; dedup-on-create), **automatic skill creation by default** (`skills.write_approval` defaults **false** — writes apply immediately; set it true to stage under `<data>/pending/skills/` with `/skills pending|diff|approve|reject`), dangerous-pattern scanner (block `rm -rf /`/exfil; flag prompt-injection/eval), `/skill-name` to load a skill into context. `memory_save` is **self-gated** (saves without prompting).
- **Hardware constraint discovered in M3.5**: the Qwythos llama.cpp template accepts **only one system message** per request — `assembleContext` therefore merges system prompt + L0 index + running summary + recall + code retrieval + injected skills into a single leading system message. Keep it that way. See [`docs/models.md`](docs/models.md).
- M4 shipped: `internal/index` — gitignore-aware walker, tree-sitter structural chunking (go/py/js/ts/tsx; ~1200-char/80-line caps; needs **cgo**), content-hash incremental re-embed in the same SQLite file, `index_repo`/`index_search` tools, per-turn top-6 code injection (2000-token budget, `path:start-end`). Hybrid search (vector + FTS5 + recency) keeps it usable with the Qwythos embedder.
- All M3/M3.5/M4 tasks ticked; acceptance verified: 60-turn bounded session + resume, remember→recall across sessions (e2e, fake servers), clean-slate store, skills flow on real hardware, and M4 on real hardware (Qwythos-9B on :8089): repo indexed in-place (51 files/632 chunks), "where is tool validation implemented?" answered via `index_search`, one-line edit re-embedded only that file. `go build`/`vet`/`test`/`gofmt` clean.
- M5 shipped: `internal/web` — pluggable web tools. `web_search` defaults to **DuckDuckGo HTML** (`html.duckduckgo.com/html/?q=`, no API key, no self-hosted server; unofficial scraping — structure can change, rate-limits heavy use) with **Mojeek** (`web_search.provider: mojeek`; independent index, may serve a JS challenge from datacenter IPs) and **SearXNG** (JSON, `web_search.searxng_url`) as alternatives. `web_fetch` GETs a URL, strips scripts/nav/footer via `golang.org/x/net/html`, caps at 16 KiB, 15s timeout + redirect limit. System prompt requires citing URLs for web-sourced answers. Acceptance verified live on Qwythos-9B :8089: "does llama.cpp support ROCm on gfx1031" → `web_search` → 2× `web_fetch` → summarized with source URLs.
- M6 shipped: `internal/ui/tui.go` — bubbletea TUI (auto-selected on a real terminal; `--plain` for the REPL) with streaming pane, tool/progress lines, y/n approval prompts, status line; REPL + TUI share one runtime (`newChatEnv`/`newAgent`). `internal/logx` — slog to `<data>/yagent.log` + `--debug` mirror. `yagent doctor` (`internal/doctor`) diagnoses config/server/model/embeddings/chat with non-zero exit on failure. `internal/eval` — golden YAML evals (M2–M5 flows) run by `go test`, no network.
- M6.5 polish (post-M6, all tested): TUI transcript is now a scrollable viewport (mouse wheel, PgUp/PgDn) with `/skills` + `/skill-name` handled in-TUI; `web_search` gained a **Mojeek** backend (`web_search.provider: mojeek`; note: may serve a JS challenge from datacenter IPs — Brave's free API closed); `yagent skills list|import <file> [--scope ...]` CLI (imports are `source: user`, scanner-exempt, source preserved across edits); `chat --yolo` auto-approves every write/destructive tool and applies skill writes immediately.
- M6.6 hardening: TUI `/` menu with Tab completion (fixed commands + skill names); Ctrl-C during a running turn asks to confirm; both UIs print the session id on quit (`chat --continue <id>`); eval harness grew a budget-regression eval (`all_requests_have_user` assertion); `go test -race` clean.
- M6.7 skills verification harness: `/skills verify <id>` runs a staged write's `## Verification` section through a fresh read-only agent and parses a PASS/FAIL verdict; failures accumulate on the staged write (`/skills pending` marks them) and on the skill (L0 shows `(stale)` at 2 failures; `/skills approve` warns). `agent.VerifySkill` + `ParseVerdict`; `skills` store failure counters.
- M6.8 context gauge + thread safety: the TUI status line shows live `ctx <used>/<window>` (heuristic tokens, red when over budget); `yagent doctor` reports the configured window; `Agent` history access is now mutex-guarded (`ContextUsage`, `History`, `Reset`, `InjectSystem` are safe to call while a turn runs — verified under `-race`).
- M6.9 resilience (see `improvement.md`): `web_search` falls back DDG → Mojeek → SearXNG on error/empty (SearXNG-primary never falls back); `internal/scrub` redacts likely secrets + home paths before anything hits SQLite (messages, summaries, memories); `decodeArgs` runs a JSON-repair pass (trailing commas, raw newlines in strings) before failing tool args.
- M6.10 P1 polish: tree-sitter grammars for **rust/c/cpp/java** (chunker); `yagent chat --fork <id>` branches a session (independent copy, original untouched); background startup re-index when the index already exists (`Index()` is mutex-serialized); `git describe` versioning via `make build` + a `Makefile` (`build|test|vet|race|version`).
- M6.11 P1 done: **dynamic tool-schema filtering** — core tools always offered, web/index/`skill_manage` schemas added on a domain signal or when used this turn (saves ~1k tokens/turn; registry still holds every tool so anything the model calls still works); **TUI diff overlay** for `fs_edit`/`fs_write` approvals; `yagent completion bash|zsh`.
- M6.12 UX: `/yolo on|off` (or bare `/yolo` to toggle) switches yolo mode at runtime in both UIs — auto-approves writes and applies skill writes immediately (a toggleable approver); the TUI status line shows kaomojis for state (ready/working/awaiting approval) and a `· yolo` marker.
- M6.13 autonomy + advisor: **loop mode** — `yagent chat --goal "..."` or `/goal <text>` runs the agent toward a goal in DONE/CONTINUE rounds (capped, `--rounds`); the budget keeps long runs bounded. **`consult` tool** — `consult.server_url`/`consult.model` points at a second local "advisor" model, `consult.api_key` enables cloud OpenAI-compatible endpoints (Gemini/OpenRouter; this is the one deliberately opt-in cloud path), and `consult.cmd` (e.g. `[claude, -p]`) shells out to an installed terminal AI app with the prompt appended. Also `YAGENT_CONSULT_*` env vars. `llm.Client` gained `BearerToken` auth.
- M6.14 settings: `/settings` lists every editable setting and `/set <key> <value>` persists it to the config file (validated via a generalized `config.Set`; `skills.write_approval` applies live, others next session). The TUI `/settings` opens an interactive settings page (↑/↓ navigate, enter edits and saves immediately, esc closes; enum fields use a left/right chooser).
- M6.15 session mgmt + undo + symbols: `yagent sessions search <q>` (FTS5 over messages) and `yagent sessions export <id> [--output file.md]` (markdown, shared renderer with `/export`); `/undo` reverts the last turn's `fs_write`/`fs_edit` via an in-memory per-turn buffer (`internal/undo`); symbol-aware code search — `index_search` accepts `symbol:`/`type:` for exact declaration lookups (top-level decls indexed into `index_symbols`).
- M6.16: chunker grammars for **bash/html/css** (SQL needs a community grammar, not fetchable — line-window fallback covers it); eval harness gained **goal-mode** tasks (`goal:` runs `RunGoal`); benchmarks for the chunker, symbols, and hybrid search (~0.2 ms over 1000 chunks).
- M6.17 sandboxed shell: `shell.sandbox: bwrap` (or `YAGENT_SHELL_SANDBOX=bwrap`) wraps `shell_exec` in bubblewrap — workspace writable, system read-only, private `/tmp`, `--unshare-net`, `--die-with-parent`; common dev caches (`~/.cache`, `~/go`) stay writable. It **fails loudly** (never silently runs unsandboxed) when bubblewrap is missing or on non-Linux. Defense-in-depth for `--yolo`/untrusted-repo work — the approval gate covers *unconsidered* actions, the sandbox contains *consented-but-unsafe* ones.
- **Real-hardware note**: the dev llama.cpp server now runs `--embeddings --pooling mean`, so L3 semantic recall works on :8089 with Qwythos as the embedder (chat + embeddings share one server; the requested embedding model name is ignored). Ollama remains an alternative (`nomic-embed-text`). See [`docs/models.md`](docs/models.md).
- All milestones shipped. M7 (subagent orchestration) is optional — only if eval evidence shows the single loop is the bottleneck.

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
  - `modernc.org/sqlite` — pure-Go SQLite (M3+; also hosts L3 memory + FTS5 keyword index since M3.5)
  - `github.com/tree-sitter/go-tree-sitter` (+ `tree-sitter-go/python/javascript/typescript`) — repo chunking (M4, needs **cgo**; a C toolchain is now required to build)
  - `golang.org/x/net/html` — HTML parsing for the DDG scraper and `web_fetch` extraction (M5)
  - `github.com/tmc/langchaingo` — **NOT approved**; we implement our own loop/memory
  - `github.com/charmbracelet/bubbletea` + `lipgloss` (+ `bubbles/textinput`) — TUI (M6)
  - `github.com/philippgille/chromem-go` — **removed in M3.5**; L3 is SQLite hybrid (vector + FTS5), no chromem, no ANN
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
