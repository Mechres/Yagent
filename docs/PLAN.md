# PLAN — Yagent milestones

Execute in order. Do not skip ahead; each milestone's acceptance criteria must pass against the **real local model** before starting the next. Keep `AGENTS.md` and the design docs in sync with whatever you actually build.

## Environment setup (do once, verify first)

```bash
# inference server — option A: Ollama (ROCm)
HSA_OVERRIDE_GFX_VERSION=10.3.0 ollama serve
ollama pull qwen2.5-coder:14b
ollama pull nomic-embed-text

# option B: llama.cpp llama-server with Vulkan
# llama-server -m qwen2.5-coder-14b-instruct-q4_k_m.gguf --port 11434 -ngl 99

# sanity check the OpenAI-compatible API
curl http://localhost:11434/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "qwen2.5-coder:14b",
  "messages": [{"role":"user","content":"say ok"}],
  "stream": false
}'
```

Prereqs: Go 1.22+, a running server from above, `git` on PATH.

---

## M1 — Skeleton + streaming chat CLI

**Goal**: a binary that streams chat from the local model. No tools, no memory.

Tasks:
- [x] `go mod init yagent`; create the module layout from `AGENTS.md`
- [x] `internal/config`: yaml config (`~/.config/yagent/config.yaml`), defaults, env overrides (`YAGENT_SERVER_URL`, `YAGENT_MODEL`)
- [x] `internal/llm`: `ChatStream` against `/v1/chat/completions`; own SSE parser (`data:` lines, `[DONE]`); typed `Message`/`Response`; backoff-retry (3×) on transport errors; `Embed` stub (implemented, unused until M3)
- [x] `internal/ui`: stdin/stdout REPL; tokens print as they arrive; `/exit` quits, `/clear` resets history
- [x] `cmd/yagent`: `chat` subcommand, `--version`, `--config` flag
- [x] tests: SSE parser and client against `httptest.Server` (no network)

Acceptance:
```bash
go build ./... && go vet ./... && go test ./...     # all clean
go run ./cmd/yagent chat                            # streams replies from the local model
```

## M2 — Tool loop + fs/shell/git tools

**Goal**: the agent loop from `design/agent-loop.md` with the M2 tool set from `design/tools.md`.

Tasks:
- [x] `internal/tools`: `Tool` interface, registry, `fs_read/fs_write/fs_edit/glob/grep/shell_exec/git_status/git_diff/git_log`
- [x] workspace scoping + risk levels; `Approver` prompt in the REPL (y/n, shows command or diff)
- [x] `internal/agent`: loop, context assembly (system prompt + history), tool-call validation/retry, truncation, max-iteration guard
- [x] system prompt v1: identity, tool usage rules, workspace path, "be concise" bias
- [x] tests: fake LLM server returning scripted tool_calls (multi-turn); each tool against `t.TempDir()`; approval denial path

Acceptance: *(verified on real hardware — llama.cpp :8089, `Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf`; quirks in `docs/models.md`)*
- [x] "Read main.go and explain what it does" → uses `fs_read`, answers correctly
- [x] "Fix the typo in README.md" → `fs_edit`, diff shown, asks approval; denied → agent adapts
- [x] "What branch are we on and is the tree clean?" → uses git tools, no shell_exec needed
- [x] malformed tool args from the model recover via validation-error feedback (test with fake server)

## M3 — Memory: sessions, summarization, semantic recall

**Goal**: L1–L3 from `design/memory.md`. Long sessions don't overflow; facts survive restarts.

Tasks:
- [x] `internal/memory`: SQLite (`modernc.org/sqlite`) schema + `Store` interface; `chat --continue <id>`, `yagent sessions`
- [x] token-budget manager: summarize oldest 50% of history when over window; running summary injected per `agent-loop.md`
- [x] L3 semantic memory. **M3.5 rewrite**: SQLite hybrid — `memories` table (float32 vector BLOB) + FTS5 keyword index, hybrid score `0.4·cos + 0.3·bm25 + 0.2·importance + 0.1·recency`, chromem-go removed; `memory_save` (optional `importance`) / `memory_search` tools; per-turn recall injection (top-5, budgeted, session-deduped)
- [x] session-end summary job → embedded into L3
- [x] tests: budget math (forced overflow with fake summarizer), Store round-trips, recall ranking + hybrid (keyword rescue, importance, recency)

Acceptance: *(all verified — 60-turn budget + remember/recall e2e against fake servers, clean slate unit-tested; real-hardware note: L3 recall verified on Qwythos-9B :8089 as embedder, remember→recall across sessions; embedding quality improved post-M3.5 by hybrid vector+FTS5 recall, see `docs/design/memory.md`)*
- [x] 60+ turn session (scripted) never exceeds the context window; earlier decisions still answerable via the running summary
- [x] "Remember that I prefer X" → quit → new session → ask about X → recalled
- [x] deleting the data dir = clean slate, no errors

## M3.5 — Skills (procedural memory)

**Goal**: Hermes-style autonomous skill creation — the agent saves reusable workflows as `SKILL.md` files it can load on demand. Design: [`docs/design/skills.md`](docs/design/skills.md). Depends on the M2 tool loop.

Tasks:
- [x] `internal/skills`: filesystem store — global `<data>/skills/` + project `<workspace>/.yagent/skills/` (both read roots), agentskills.io-compatible frontmatter subset via `yaml.v3`, lifecycle metadata (`source`/`created_at`/`last_used`, store-managed), validation (slug regex, ≤60-char description, required sections, size caps), path hardening for `references/`, dangerous-pattern scanner (block/flag verdicts), dedup helper
- [x] tools: `skills_list` / `skill_view` (read; bumps `last_used`), `skill_manage` (create/patch/edit/delete/write_file/remove_file, `scope: global|project`; write-gated; per-session cap)
- [x] end-of-turn creation-trigger prompt (5+ tool calls succeeded / user correction / error→working path / non-trivial workflow) with embedded authoring rules + dedup-before-create + ≤2 staged writes/session
- [x] approval gate `skills.write_approval` (default true): staging under `<data>/pending/skills/`, `/skills pending|diff|approve|reject`
- [x] REPL invocation: `/skill-name` loads SKILL.md; `/skills list`
- [x] L0 budget: skills_list in system prompt capped (~3k tokens / 40 skills, evict by `last_used`); activation respects L1 budget
- [x] tests: fake-LLM scripted skill_manage flows, gate on/off, patch ambiguity, path traversal, frontmatter validation retry, dedup rejection, session cap, scanner block/flag, project-store write

Acceptance: *(real-hardware verified on Qwythos-9B on :8089 — a skill was created by the model, rejected once for `#` instead of `##` section headers, corrected, staged, approved via `/skills`, listed in the next session, and loaded via `/skill-name`. Note this server's template accepts only ONE system message; `assembleContext` merges system content. The dev server runs `--embeddings --pooling mean`, so semantic recall works with Qwythos as the embedder.)*
- [x] scripted 5+ tool-call task → agent proposes a skill → staged → approved → `skills_list` shows it next session
- [x] "Remember how I fixed the Ollama ROCm env issue" → skill created, recalled and followed later
- [x] user correction → existing skill patched via `skill_manage patch`, never applied without approval
- [x] duplicate skill proposal merged into the existing skill; ≤2 staged writes per session enforced
- [x] `rm -rf /` skill blocked at write; "ignore previous instructions" skill loads with a visible warning
- [x] project-scoped skill in `.yagent/skills/` available in any session on that repo
- [x] `write_approval: false` writes immediately; 100-skill store stays under the L0 cap (40 most recently used)

## M4 — Codebase index

**Goal**: L4 — semantic code search over the workspace.

Tasks:
- [x] `internal/index`: walker (`.gitignore`-aware incl. nested files + negation, size/binary/hidden/lock-file filters), tree-sitter chunker (go/py/js/ts/tsx via `github.com/tree-sitter/go-tree-sitter`, cgo; line-window fallback capped at ~1200 chars/80 lines), content-hash incremental re-embed (only changed files re-embedded, stale files pruned)
- [x] `index_repo` (RiskWrite, synchronous with progress lines to the UI) and `index_search` tools
- [x] per-turn index retrieval injection (top-6, 2000-token budget, `path:start-end` prefixes; `agent.Config.Index` + `IndexAutoInject`)
- [x] tests: chunker on real fixtures of Go/md, gitignore matcher, hash-skip on re-index (embed-request counting), incremental rebuild on edit/delete, search relevance

Acceptance: *(real-hardware verified on Qwythos-9B :8089 — the repo indexed in-place: 51 files / 632 chunks; "where is tool argument validation implemented?" → the model opened with `index_search` and landed on `internal/tools/tools.go`; a one-line edit re-embedded only that file (progress lines + summary counts prove it))*
- [x] index this very repo; "where is tool validation implemented?" → `index_search` returns the right chunk without any grep
- [x] edit one file → re-index → only that file re-embedded (log lines prove it)

> Note: tree-sitter needs **cgo** (approved dependency). A C toolchain is now a build requirement; `go build` uses it automatically.

## M5 — Web tools

**Goal**: research capability.

Tasks:
- [x] `internal/web`: pluggable `web_search` backends — **DuckDuckGo HTML** (`html.duckduckgo.com/html/?q=`, default, no key/server; parses `result__a`/`result__snippet`, decodes the `uddg` redirect) and **SearXNG** (`format=json`, via `web_search.provider` + `web_search.searxng_url` / env)
- [x] `web_fetch`: GET → HTML→text extraction (scripts/nav/footer stripped, links preserved) via `golang.org/x/net/html`; 16 KiB text cap; 15s timeout; redirect limit 5
- [x] system-prompt guidance: cite URLs when using web results
- [x] tests: fake DDG HTML page, fake SearXNG JSON, fake fetch page (chrome-strip + truncation + 404), config provider/env, tool wiring

Acceptance: *(real-hardware verified on Qwythos-9B :8089 against live DuckDuckGo — "Research whether llama.cpp supports ROCm on gfx1031" → model ran `web_search`, fetched two pages (rocm.docs.amd.com + github.com/lemonade-sdk/llamacpp-rocm), and summarized with source URLs)*
- [x] "Research whether llama.cpp supports ROCm on gfx1031 and summarize with sources" → searches, fetches ≥2 pages, answer contains URLs

> Backends are a small `Provider` interface: Mojeek (no-key HTML), Brave (API key), DDG Lite are ~30 lines each if needed later.

## M6 — TUI + polish

**Goal**: daily-driver quality.

Tasks:
- [x] bubbletea TUI (`internal/ui/tui.go`, `yagent chat` uses it when stdin is a real terminal; `--plain` forces the REPL): streaming answer pane, inline tool calls + `index_repo` progress lines, y/n approval prompts, status line (model, session, tokens, tool count, state)
- [x] structured logging: `internal/logx` — slog to `<data>/yagent.log` (Info) plus `--debug` mirror to stderr (Debug); key agent events logged
- [x] `yagent doctor` (`internal/doctor`): config URL/model/data-dir, server reachability, model present in `/v1/models`, embeddings endpoint (warns on 501 → `--embeddings --pooling mean`), chat ping, best-effort backend/GPU line; FAIL → non-zero exit
- [x] eval harness: `internal/eval` + `testdata/evals/*.yaml` golden tasks (M2–M5 flows: fs-read, validation-retry, memory-recall, skill-creation, code-index, web-research) run by `go test` against scripted fake servers — no network
- [x] REPL/TUI share one runtime (`newChatEnv`/`newAgent`); README quickstart (`go build ./cmd/yagent && ./yagent chat`) verified

Acceptance:
- [x] full M2–M5 acceptance suite passes through the TUI build — the TUI runs the identical agent loop (shared `chatEnv`); the 6 eval tasks exercise M2–M5 flows and all pass; TUI smoke-tested under a pty (renders, streams, status line)
- [x] `yagent doctor` correctly diagnoses: server down (FAIL + exit 1), model missing (WARN), bad config (FAIL)

> TUI v1 notes: transcript is a tailing list (no full scroll-back yet); `skill_manage` stays self-gated and `/skills` slash-commands remain REPL-only for now.

**M6.5 polish (shipped, all tested):**
- TUI transcript is a scrollable viewport (mouse wheel, PgUp/PgDn); `/skills` + `/skill-name` handled inside the TUI
- `web_search` Mojeek backend (`web_search.provider: mojeek`; independent index; may serve a JS challenge from datacenter IPs — Brave's free API has closed)
- `yagent skills list|import <file> [--scope global|project]` CLI (imports are `source: user`, dangerous-pattern-scanner-exempt, source preserved across edits)
- `yagent chat --yolo`: auto-approves every write/destructive tool and applies skill writes immediately

## M7 — Orchestration (optional; only if M1–M6 show a real need)

Subagent primitive: spawn a child `Agent` with a narrowed system prompt and tool subset (e.g., read-only "researcher"), run to completion, return its final message as a tool result to the parent. Requires evidence (eval failures) that the single loop is the bottleneck before starting.

---

## Working agreements

- One milestone = one working state of the binary; don't leave `main` broken between tasks
- If a design doc and reality disagree while implementing, fix the doc in the same commit
- If you must add a dependency beyond the approved list in `AGENTS.md`, write down why (commit message + update `AGENTS.md`)
- Track model-specific quirks (tool-call format failures, context limits) in `docs/models.md` as you discover them
