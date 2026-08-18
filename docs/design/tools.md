# Tools

Tools are the agent's hands. With small local models, **fewer, sharper tools
beat many fuzzy ones**. The registry below is the current surface; schemas are
also filtered dynamically so a turn sees the tools relevant to its task. A
workspace capability profile suppresses diagnostics, tests, and smoke schemas
when an empty workspace has not selected a stack yet, or when the required
local toolchain is missing; they reappear after a successful scaffold write.

## Risk levels & approval

| Level | Meaning | Approval |
|---|---|---|
| `ReadOnly` | no side effects (read file, grep, git status, web fetch) | none; may run in parallel |
| `Write` | modifies workspace or agent state (`fs_write`, `fs_edit`, `memory_save`) | prompt, except self-gated memory/skill writes; `--yolo` may pre-grant consent for the session |
| `Destructive` | shell execution or terminating a background job | prompt; `--yolo` may pre-grant consent for the session |

The UI implements `Approver`; prompts show a mutation-specific summary plus the command or filesystem diff and wait for y/n. Multi-hunk `fs_patch` review supports individual decisions, `a` to accept all remaining hunks, and `x` to reject them. A denied call returns `error: user denied this action` as the tool result so the model can adapt. The TUI also records an inspectable `/tools` timeline with arguments, result, and elapsed time; its compact transcript rows never echo successful tool output.

Every dispatch also has a UI-neutral `tools.ToolOutcome` projection: call ID,
tool name, risk, status, elapsed time, model-visible result, and semantic
presentation metadata (`read`, `search`, `web`, `terminal`, `diff`,
`approval`, or `memory`). The legacy string callbacks remain available for
the REPL/TUI, while GUI clients can consume the structured event without
parsing terminal text.

## Core tool registry

| Name | Risk | Args (JSON schema) | Notes |
|---|---|---|---|
| `fs_read` | RO | `path` (req), `offset`, `limit` | line-numbered output; 2000-line cap; binary detection |
| `fs_write` | Write | `path` (req), `content` (req) | creates dirs; shows diff if file exists; workspace-scoped |
| `fs_edit` | Write | `path` (req), `old_string` (req), `new_string` (req) | exact match, must be unique in file; on 0 or >1 matches return a precise error the model can fix |
| `fs_patch` | Write | `patch` (req) | atomic multi-file unified-diff apply; syntax, structured-file, and public-symbol preflight run before any file is written |
| `fs_refactor` | Write | `old_name` (req), `new_name` (req) | workspace-wide word-boundary rename; skips build/vendor directories and is undo-aware |
| `glob` | RO | `pattern` (req), `path` | `**/*.go` style; cap 200 results |
| `grep` | RO | `pattern` (req), `path`, `include` | regex via stdlib `regexp` over file walk; cap 100 matches |
| `code_outline` / `code_slice` / `code_topology` | RO | file/symbol-specific | declaration outline, exact declaration read, and local import topology without embedding search |
| `workspace_diagnostics` / `test_runner` / `runtime_smoke` / `code_environment` | RO | scoped where relevant | fixed-command diagnostics, targeted tests, executable behavior checks, and toolchain/native-binding audit |
| `shell_exec` | Destructive | `command` (req), `timeout_sec` | pipes through `sh -c`; stdout+stderr captured, cap 32 KiB; default timeout 30s (max 300); env scrubbed of secrets (`*_TOKEN`, `*_KEY`, `*_SECRET`). `shell.sandbox: bwrap` (Linux) wraps commands in bubblewrap: workspace writable, system read-only, private `/tmp`, no network, `--die-with-parent`. Fails loudly if bubblewrap isn't installed — never silently runs unsandboxed. |
| `shell_bg` / `shell_logs` / `shell_kill` | Write/RO/Destructive | command/job-specific | run a command in the background (`shell_bg` returns a job id), inspect its accumulated tail output (`shell_logs`), or terminate it (`shell_kill`); all jobs are killed at session end. |
| `subagent` | RO | `task` or `tasks` | delegate a context-heavy subtask to an isolated read-only child agent; optional tool subsets and preset roles keep the child scoped |
| `git_status` | RO | none | porcelain output |
| `git_diff` | RO | `path`, `staged` | cap 32 KiB |
| `git_log` | RO | `n` (≤50) | oneline |

**Workspace scoping**: filesystem, glob, grep, refactor, patch, scratch, and
index tools are workspace-scoped; escape attempts fail. Shell commands are
confined with bubblewrap by default when it is installed and otherwise fail
closed. `shell.sandbox: unsafe` is an explicit escape hatch that permits paths
outside the workspace.

## Tool registry (later milestones)

| Name | Milestone | Risk | Purpose |
|---|---|---|---|
| `memory_save` | M3 | Write | store a fact/decision/preference (see `memory.md`) |
| `memory_search` | M3 | RO | semantic search over long-term memory |
| `index_repo` | M4 | Write | (re)index the workspace incrementally; unchanged files are skipped |
| `index_search` | M4 | RO | semantic code search; returns `path:start-end` + snippet |
| `web_search` | M5 | RO | DuckDuckGo HTML by default (`html.duckduckgo.com/html/?q=`; no key, unofficial scraping — structure can change, rate-limits); Mojeek (`www.mojeek.com/search?q=`, independent index, may serve a JS challenge from datacenter IPs) or SearXNG JSON (`format: json` in settings.yml) via `web_search.provider`; hosted **LangSearch** (`web_search.provider: langsearch` + `web_search.langsearch_api_key`, free key from langsearch.com) joins the fallback chain when a key is set; top-8 results: title, url, snippet. A `queries` array (up to 8) runs the searches concurrently in one call (v0.1.80). Results cached per session (10 min TTL, 64 entries) |
| `web_fetch` | M5 | RO | GET url → HTML→**Markdown** (headings/lists/code/tables/links preserved; scripts/nav/footer stripped via `x/net/html`) → cap `web_search.max_fetch_kib` (default 32 KiB); PDFs rejected with a "find the HTML version" error; 15s timeout; redirect limit 5; no POSTs ever |
| `paper_search` | v0.1.81 | RO | scholarly search (arXiv + PubMed keyless; Semantic Scholar when `web_search.semanticscholar_api_key` is set; enabled by `web_search.papers: true`) → per-paper title, authors, year, venue, abstract, url, doi |
| `research_note` | v0.1.80 | RO | record one verified research finding (fact + source URL) into the TASK STATE ledger, where it survives budget pruning (research mode) |
| `consult` | M6.13 | RO | ask a configured advisor for guidance or a second opinion; it can use another OpenAI-compatible server or an installed terminal AI app |
| `<server>_<tool>` | M7 | Write by default | an MCP-advertised tool; only names in that server's `read_only_tools` allowlist skip approval |

### Research profile

`--research` and `/research` temporarily replace the normal registry with a
restricted profile. It exposes web/paper search and fetch, filesystem
read/search, read-only code/git inspection, memory, `research_note`, and
`scratch_read`. The only workspace mutation is `fs_write` for `.md` files under
`.yagent/research/`; shell, source edits, git mutations, jobs, subagents, and
MCP tools are unavailable. Dispatch enforces the same boundary even if the
model emits a tool call whose schema was not offered. The original registry is
restored when the research workflow ends.

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

Exact matching remains the primary path. When an exact match fails, a unique
whitespace-normalized match is accepted and re-indented to the file's style;
otherwise the error includes a nearby-match hint and asks the model to re-read.

## Additional tools and guardrails

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
| `code_references` / `code_impact` / `code_unused` | RO | call graph, direct change radius, and dead-symbol candidates from the index |
| `fs_refactor` | Write | word-boundary symbol rename across source files (build/vendored dirs + binaries skipped), undo-aware, approval-gated |
| `clarify` | RO | asks the user a question with optional choices (REPL numbered prompt / TUI modal); the pick returns as tool data — only offered when the UI wires `SetAskUser` |
| `plan` | RO | lightweight plan-approval gate: steps shown, user approves/revises, returned as `plan approved` / `plan rejected: <feedback>` |
| `consult` | RO | advisor model (`consult.*` config) or terminal AI app (`consult.cmd`) |
| `memory_save` / `memory_search` | Write (self-gated) / RO | L3 semantic memory (`scope: global\|project`) |
| `index_repo` / `index_search` | Write / RO | build + search the tree-sitter code index (`symbol:`/`type:` exact lookups) |
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
