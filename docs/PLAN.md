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
- [ ] `internal/memory`: SQLite (`modernc.org/sqlite`) schema + `Store` interface; `chat --continue <id>`, `yagent sessions`
- [ ] token-budget manager: summarize oldest 50% of history when over window; running summary injected per `agent-loop.md`
- [ ] chromem-go collection + `Embed` wiring (`nomic-embed-text`); `memory_save` / `memory_search` tools; per-turn recall injection (top-5, budgeted)
- [ ] session-end summary job → embedded into L3
- [ ] tests: budget math (forced overflow with fake summarizer), Store round-trips, recall ranking

Acceptance:
- [ ] 60+ turn session (scripted) never exceeds the context window; earlier decisions still answerable via the running summary
- [ ] "Remember that I prefer X" → quit → new session → ask about X → recalled
- [ ] deleting the data dir = clean slate, no errors

## M3.5 — Skills (procedural memory)

**Goal**: Hermes-style autonomous skill creation — the agent saves reusable workflows as `SKILL.md` files it can load on demand. Design: [`docs/design/skills.md`](docs/design/skills.md). Depends on the M2 tool loop.

Tasks:
- [ ] `internal/skills`: filesystem store — global `<data>/skills/` + project `<workspace>/.yagent/skills/` (both read roots), agentskills.io-compatible frontmatter subset via `yaml.v3`, lifecycle metadata (`source`/`created_at`/`last_used`, store-managed), validation (slug regex, ≤60-char description, required sections, size caps), path hardening for `references/`, dangerous-pattern scanner (block/flag verdicts), dedup helper
- [ ] tools: `skills_list` / `skill_view` (read; bumps `last_used`), `skill_manage` (create/patch/edit/delete/write_file/remove_file, `scope: global|project`; write-gated; per-session cap)
- [ ] end-of-turn creation-trigger prompt (5+ tool calls succeeded / user correction / error→working path / non-trivial workflow) with embedded authoring rules + dedup-before-create + ≤2 staged writes/session
- [ ] approval gate `skills.write_approval` (default true): staging under `<data>/pending/skills/`, `/skills pending|diff|approve|reject`
- [ ] REPL invocation: `/skill-name` loads SKILL.md; `/skills list`
- [ ] L0 budget: skills_list in system prompt capped (~3k tokens / 40 skills, evict by `last_used`); activation respects L1 budget
- [ ] tests: fake-LLM scripted skill_manage flows, gate on/off, patch ambiguity, path traversal, frontmatter validation retry, dedup rejection, session cap, scanner block/flag, project-store write

Acceptance:
- [ ] scripted 5+ tool-call task → agent proposes a skill → staged → approved → `skills_list` shows it next session
- [ ] "Remember how I fixed the Ollama ROCm env issue" → skill created, recalled and followed later
- [ ] user correction → existing skill patched via `skill_manage patch`, never applied without approval
- [ ] duplicate skill proposal merged into the existing skill; ≤2 staged writes per session enforced
- [ ] `rm -rf /` skill blocked at write; "ignore previous instructions" skill loads with a visible warning
- [ ] project-scoped skill in `.yagent/skills/` available in any session on that repo
- [ ] `write_approval: false` writes immediately; 100-skill store stays under the L0 cap (40 most recently used)

## M4 — Codebase index

**Goal**: L4 — semantic code search over the workspace.

Tasks:
- [ ] `internal/index`: walker (`.gitignore`-aware, size/binary filters), tree-sitter chunker (Go first, then python/ts/js; line-window fallback), content-hash incremental re-embed
- [ ] `index_repo` (background, progress to ui) and `index_search` tools
- [ ] per-turn index retrieval injection (top-6, 2000-token budget, `path:start-end` prefixes)
- [ ] tests: chunker on real files of each supported language; hash-skip on re-index; search relevance on a fixture repo

Acceptance:
- [ ] index this very repo; "where is tool validation implemented?" → `index_search` returns the right chunk without any grep
- [ ] edit one file → re-index → only that file re-embedded (log lines prove it)

## M5 — Web tools

**Goal**: research capability.

Tasks:
- [ ] `web_search` against SearXNG (`format=json`; document the settings.yml requirement); fallback: configurable Brave API key via env
- [ ] `web_fetch`: GET → HTML→markdown extraction → 16 KiB cap; 15s timeout; redirect limit
- [ ] system-prompt guidance: cite URLs when using web results
- [ ] tests: fake SearXNG + fake HTML pages (no network in tests)

Acceptance:
- [ ] "Research whether llama.cpp supports ROCm on gfx1031 and summarize with sources" → searches, fetches ≥2 pages, answer contains URLs

## M6 — TUI + polish

**Goal**: daily-driver quality.

Tasks:
- [ ] bubbletea TUI: streaming pane, tool-call cards (args, diff, approval buttons), status line (model, tokens, iteration)
- [ ] structured logging (`slog`, `--debug` flag, log file under data dir)
- [ ] config validation with helpful errors; `yagent doctor` (server reachable? models pulled? GPU backend active?)
- [ ] tiny eval harness: `testdata/evals/*.yaml` golden tasks (M2–M5 acceptance flows, scripted with fake server) run in CI/`go test`
- [ ] README quickstart verified from scratch on a clean checkout

Acceptance:
- [ ] full M2–M5 acceptance suite passes through the TUI build
- [ ] `yagent doctor` correctly diagnoses: server down, model missing, bad config

## M7 — Orchestration (optional; only if M1–M6 show a real need)

Subagent primitive: spawn a child `Agent` with a narrowed system prompt and tool subset (e.g., read-only "researcher"), run to completion, return its final message as a tool result to the parent. Requires evidence (eval failures) that the single loop is the bottleneck before starting.

---

## Working agreements

- One milestone = one working state of the binary; don't leave `main` broken between tasks
- If a design doc and reality disagree while implementing, fix the doc in the same commit
- If you must add a dependency beyond the approved list in `AGENTS.md`, write down why (commit message + update `AGENTS.md`)
- Track model-specific quirks (tool-call format failures, context limits) in `docs/models.md` as you discover them
