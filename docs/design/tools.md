# Tools

Tools are the agent's hands. With small local models, **fewer, sharper tools beat many fuzzy ones**. Ship this set and resist adding more until M6.

## Risk levels & approval

| Level | Meaning | Approval |
|---|---|---|
| `ReadOnly` | no side effects (read file, grep, git status, web fetch) | none; may run in parallel |
| `Write` | modifies workspace files (`fs_write`, `fs_edit`) | prompt unless `--yes` flag / config `approval: auto` |
| `Destructive` | shell exec, git mutations, anything outside workspace | **always prompt**, no override |

The ui implements `Approver`; prompts show the tool name, args (command/diff), and wait for y/n. A denied call returns `error: user denied this action` as the tool result so the model can adapt.

## Tool registry (M2 set)

| Name | Risk | Args (JSON schema) | Notes |
|---|---|---|---|
| `fs_read` | RO | `path` (req), `offset`, `limit` | line-numbered output; 2000-line cap; binary detection |
| `fs_write` | Write | `path` (req), `content` (req) | creates dirs; shows diff if file exists; workspace-scoped |
| `fs_edit` | Write | `path` (req), `old_string` (req), `new_string` (req) | exact match, must be unique in file; on 0 or >1 matches return a precise error the model can fix |
| `glob` | RO | `pattern` (req), `path` | `**/*.go` style; cap 200 results |
| `grep` | RO | `pattern` (req), `path`, `include` | regex via stdlib `regexp` over file walk; cap 100 matches |
| `shell_exec` | Destructive | `command` (req), `timeout_sec` | pipes through `sh -c`; stdout+stderr captured, cap 32 KiB; default timeout 30s (max 300); env scrubbed of secrets (`*_TOKEN`, `*_KEY`, `*_SECRET`). `shell.sandbox: bwrap` (Linux) wraps commands in bubblewrap: workspace writable, system read-only, private `/tmp`, no network, `--die-with-parent`. Fails loudly if bubblewrap isn't installed — never silently runs unsandboxed. |
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
| `web_search` | M5 | RO | DuckDuckGo HTML by default (`html.duckduckgo.com/html/?q=`; no key, unofficial scraping — structure can change, rate-limits); Mojeek (`www.mojeek.com/search?q=`, independent index, may serve a JS challenge from datacenter IPs) or SearXNG JSON (`format: json` in settings.yml) via `web_search.provider`; top-8 results: title, url, snippet |
| `web_fetch` | M5 | RO | GET url → HTML→text (strip scripts/nav/footer via `x/net/html`) → cap 16 KiB; 15s timeout; redirect limit 5; no POSTs ever |
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

## Adding a tool (checklist)

1. Implement `tools.Tool` (Schema/Risk/Execute) in `internal/tools/<name>.go`
2. Register in `tools.NewRegistry(...)`
3. Typed args struct + validation errors worded for model self-correction
4. Result cap chosen and enforced
5. Risk level set honestly (when in doubt: higher)
6. Table-driven tests with a fake workspace (`t.TempDir()`); shell tool tested with `true`/`false`/echo only
7. One line in the system prompt's tool guidance if usage is non-obvious
