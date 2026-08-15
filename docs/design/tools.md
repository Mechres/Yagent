# Tools

Tools are the agent's hands. With small local models, **fewer, sharper tools beat many fuzzy ones**. Ship this set and resist adding more until M6.

## Risk levels & approval

| Level | Meaning | Approval |
|---|---|---|
| `ReadOnly` | no side effects (read file, grep, git status, web fetch) | none; may run in parallel |
| `Write` | modifies workspace files (`fs_write`, `fs_edit`) | prompt unless `--yes` flag / config `approval: auto` |
| `Destructive` | shell exec, git mutations, anything outside workspace | **always prompt**, no override |

The UI implements `Approver`; prompts show a mutation-specific summary plus the command or filesystem diff and wait for y/n. Multi-hunk `fs_patch` review supports individual decisions, `a` to accept all remaining hunks, and `x` to reject them. A denied call returns `error: user denied this action` as the tool result so the model can adapt. The TUI also records an inspectable `/tools` timeline with arguments, result, and elapsed time; its compact transcript rows never echo successful tool output.

## Tool registry (M2 set)

| Name | Risk | Args (JSON schema) | Notes |
|---|---|---|---|
| `fs_read` | RO | `path` (req), `offset`, `limit` | line-numbered output; 2000-line cap; binary detection |
| `fs_write` | Write | `path` (req), `content` (req) | creates dirs; shows diff if file exists; workspace-scoped |
| `fs_edit` | Write | `path` (req), `old_string` (req), `new_string` (req) | exact match, must be unique in file; on 0 or >1 matches return a precise error the model can fix |
| `glob` | RO | `pattern` (req), `path` | `**/*.go` style; cap 200 results |
| `grep` | RO | `pattern` (req), `path`, `include` | regex via stdlib `regexp` over file walk; cap 100 matches |
| `shell_exec` | Destructive | `command` (req), `timeout_sec` | pipes through `sh -c`; stdout+stderr captured, cap 32 KiB; default timeout 30s (max 300); env scrubbed of secrets (`*_TOKEN`, `*_KEY`, `*_SECRET`). `shell.sandbox: bwrap` (Linux) wraps commands in bubblewrap: workspace writable, system read-only, private `/tmp`, no network, `--die-with-parent`. Fails loudly if bubblewrap isn't installed — never silently runs unsandboxed. |
| `shell_bg` / `shell_logs` / `shell_kill` | M6.19 | Write/RO/Destructive | run a command in the background (`shell_bg` returns a job id), inspect its accumulated tail output (`shell_logs`), terminate it (`shell_kill`); all jobs are killed at session end. For dev servers and long-running commands. |
| `fs_patch` | M6.19 | Write | apply a multi-file unified git diff in one call (context verified; paths workspace-scoped; changes recorded in the /undo buffer) |
| `code_outline` | M6.19 | RO | list a file/directory's declarations as `line [kind] name` signatures (no bodies) via tree-sitter |
| `subagent` | M7 v1 | RO | delegate a self-contained, context-heavy subtask to an isolated read-only child agent (own context, returns a summary) |
| `git_status` | RO | none | porcelain output |
| `git_diff` | RO | `path`, `staged` | cap 32 KiB |
| `git_log` | RO | `n` (≤50) | oneline |

**Workspace scoping**: all fs/glob/grep/shell tools resolve paths against the workspace root (cwd at start). Any resolved path escaping the root → hard error, and it bumps the risk to Destructive for `fs_write`/`fs_edit` if ever allowed by config.

## Tool registry (later milestones)

| Name | Milestone | Risk | Purpose |
|---|---|---|---|
| `memory_save` | M3 | Write | store a fact/decision/preference (see `memory.md`) |
| `memory_search` | M3 | RO | semantic search over long-term memory |
| `index_repo` | M4 | Write | (re)index the workspace; runs in background |
| `index_search` | M4 | RO | semantic code search; returns `path:start-end` + snippet |
| `web_search` | M5 | RO | DuckDuckGo HTML by default (`html.duckduckgo.com/html/?q=`; no key, unofficial scraping — structure can change, rate-limits); Mojeek (`www.mojeek.com/search?q=`, independent index, may serve a JS challenge from datacenter IPs) or SearXNG JSON (`format: json` in settings.yml) via `web_search.provider`; hosted **LangSearch** (`web_search.provider: langsearch` + `web_search.langsearch_api_key`, free key from langsearch.com) joins the fallback chain when a key is set; top-8 results: title, url, snippet. A `queries` array (up to 8) runs the searches concurrently in one call (v0.1.80). Results cached per session (10 min TTL, 64 entries) |
| `web_fetch` | M5 | RO | GET url → HTML→**Markdown** (headings/lists/code/tables/links preserved; scripts/nav/footer stripped via `x/net/html`) → cap `web_search.max_fetch_kib` (default 32 KiB); PDFs rejected with a "find the HTML version" error; 15s timeout; redirect limit 5; no POSTs ever |
| `paper_search` | v0.1.81 | RO | scholarly search (arXiv + PubMed keyless; Semantic Scholar when `web_search.semanticscholar_api_key` is set; enabled by `web_search.papers: true`) → per-paper title, authors, year, venue, abstract, url, doi |
| `research_note` | v0.1.80 | RO | record one verified research finding (fact + source URL) into the TASK STATE ledger, where it survives budget pruning (research mode) |
| `consult` | M6.13 | RO | ask a configured "advisor" for guidance or a second opinion. Two backends: a remote OpenAI-compatible server (`consult.server_url`/`consult.model`, optional `consult.api_key` for cloud endpoints like Gemini/OpenRouter) or an installed terminal AI app run as a subprocess (`consult.cmd`, e.g. `[claude, -p]`, prompt appended as the final arg); 60s timeout |

## Execution rules

1. **Validate args** into a typed struct per tool. Missing/invalid → error tool result, model retries (max 3).
2. **ReadOnly tools in the same batch run concurrently** (goroutines + WaitGroup). Others sequential.
3. **Every result truncated** per its cap, with an explicit truncation marker.
4. **Errors are data**: exit codes, stderr, missing files all go back as tool-result text. The loop never dies because a tool failed.
5. Tool descriptions are written for a 12B model: imperative, one example in the description if the arg format is non-obvious, no jargon.

## fs_edit algorithm (the tool models fail most)

```
1. read file
2. count occurrences of old_string
   0 → error: "old_string not found; re-read the file and copy the exact text"
   >1 → error: "old_string matches N times; include more surrounding context"
   1 → replace, write, return unified diff (cap 100 lines)
```

No fuzzy matching in v1 — exact-match + good error feedback is more reliable with small models than fuzzy patching. Revisit only with evidence.

## Tool set additions (M6 → v0.1.16)

Tools added since the M2 registry above; same contract (typed args, capped
results, errors-as-data), with two newer conventions:

- **Error envelopes** — high-value failures carry `[class=… retryable=…
  suggest=…]` markers the model can act on (`missing_path`→glob,
  `old_string_not_found`→fs_read, `ambiguous_match`, `timeout`).
- **Deterministic guardrails** — pre-flight tree-sitter syntax validation
  blocks a write that would break source; the verify-don't-trust barrier runs
  `workspace_diagnostics` before "done" when a write went unverified; the
  fs_read dedup cache returns `[cached]` markers for unchanged re-reads.
  `fs_patch` is atomic (preflights all files before writing any); `fs_write`/
  `fs_edit`/`fs_patch` append a missing-import note for Go/Python; high-output
  read tools offload the full result to `.yagent/scratch/` and return a pointer.

| Name | Risk | Notes |
|---|---|---|
| `workspace_diagnostics` | RO | detects the project (go.mod/Cargo.toml/package.json+tsconfig/py) and runs `go vet`/`cargo check`/`tsc --noEmit`/eslint/`ruff`/compileall, 120s timeout, fixed commands (no approval) |
| `runtime_smoke` | RO | codegen-mode companion: builds and briefly runs the generated program (Go/C/C++/Cargo/Python + a node DOM shim for browser JS) with scripted stdin, reports PASS or FAIL (panic/segfault/assertion/JS TypeError/silent non-zero exit). Optional `steps: [{args, input, expect}]` assert the program *behaves* — fresh process per step, output or DOM state must contain the expected text (catches dead persistence); steps with no expect are refused; a library package (no main) is skipped, not failed |
| `code_slice` | RO | one declaration's exact span (body + doc comment) via tree-sitter, ~80% cheaper than fs_read on large modules |
| `code_references` | RO | call graph: who calls a symbol → `path:line` (from the index) |
| `fs_refactor` | Write | word-boundary symbol rename across source files (build/vendored dirs + binaries skipped), undo-aware, approval-gated |
| `clarify` | RO | asks the user a question with optional choices (REPL numbered prompt / TUI modal); the pick returns as tool data — only offered when the UI wires `SetAskUser` |
| `plan` | RO | lightweight plan-approval gate: steps shown, user approves/revises, returned as `plan approved` / `plan rejected: <feedback>` |
| `consult` | RO | advisor model (`consult.*` config) or terminal AI app (`consult.cmd`) |
| `memory_save` / `memory_search` | RO | L3 semantic memory (`scope: global\|project`) |
| `index_repo` / `index_search` | RO | build + search the tree-sitter code index (`symbol:`/`type:` exact lookups) |
| `skill_view` / `skills_list` / `skill_manage` | RO / RO / Self-gated | procedural memory; writes gated (staged or applied per `skills.write_approval`) |
| `scratch_write` / `scratch_read` | Write / RO | confined to `.yagent/scratch/` (the one write tool read-only subagents get) |
| `subagent` | RO | parallel `tasks[]`, per-child `tools[]` subsets, preset `role:` profiles, shared scratchpad |

## Adding a tool (checklist)

1. Implement `tools.Tool` (Schema/Risk/Execute) in `internal/tools/<name>.go`
2. Register in `tools.NewRegistry(...)`
3. Typed args struct + validation errors worded for model self-correction
4. Result cap chosen and enforced
5. Risk level set honestly (when in doubt: higher)
6. Table-driven tests with a fake workspace (`t.TempDir()`); shell tool tested with `true`/`false`/echo only
7. One line in the system prompt's tool guidance if usage is non-obvious
