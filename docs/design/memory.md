# Memory

Four layers, each with a single job. L1/L2 ship in M3, L3 in M3, L4 in M4.

```
L1 working context   in-memory []Message + token counter      (agent pkg)
L2 session history   SQLite: every message, resumable         (memory pkg)
L3 semantic memory   embeddings of facts/summaries, chromem   (memory pkg)
L4 codebase index    tree-sitter chunks + embeddings          (index pkg)
```

## L1 — Working context

The slice of messages actually sent to the model, assembled per `agent-loop.md`. Budgeted by heuristic token count (`len/4`). When over budget: oldest 50% of history (excluding system/tool-schema) is summarized and replaced by the running summary.

Summarization prompt (dedicated, no tools): *"Condense this conversation segment into ≤400 words. Preserve: decisions made, file paths touched, errors encountered, user preferences, open tasks. Drop: pleasantries, repeated code, verbose tool output."*

## L2 — Session store (SQLite, modernc.org/sqlite)

```sql
CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,        -- uuid
    repo_path   TEXT NOT NULL,
    title       TEXT,                    -- auto-generated from first user msg
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT NOT NULL REFERENCES sessions(id),
    role        TEXT NOT NULL,           -- user|assistant|tool
    content     TEXT NOT NULL,
    tool_call_id TEXT,                   -- for tool results
    tool_calls   TEXT,                   -- json, for assistant msgs
    tokens_est  INTEGER,
    created_at  INTEGER NOT NULL
);
CREATE INDEX idx_messages_session ON messages(session_id, id);

CREATE TABLE summaries (
    session_id  TEXT PRIMARY KEY REFERENCES sessions(id),
    summary     TEXT NOT NULL,
    covers_until INTEGER NOT NULL        -- last message id included
);
```

CLI: `yagent chat` (new), `yagent chat --continue <id>`, `yagent sessions`.

## L3 — Semantic long-term memory

Purpose: recall across sessions — user preferences, project decisions, past findings, reusable facts.

- **Storage**: chromem-go collection `memories`, persisted to disk. Embeddings: `nomic-embed-text` via `llm.Embed` (OpenAI-compat endpoint).
- **Writes** (two paths):
  1. Explicit: model calls the `memory_save` tool when it judges something worth keeping (the tool description must say *what* is worth keeping: preferences, decisions, gotchas — not code, not chit-chat).
  2. Implicit: at session end (or on `--continue` resume of an old session), a background job summarizes the session and embeds the summary.
- **Reads**: every turn, embed the user input, top-5 by cosine similarity, filter score < 0.35, inject under the memory budget (1000 tok). Deduplicate against the current session's own messages.
- **Schema**: `{id, text, source: "tool"|"summary", session_id, created_at}`.

## L4 — Codebase index

Specified in `tools.md` (`index_search`) and built in M4. Retrieval at turn start: embed user input → top-6 chunks → inject under the index budget (2000 tok), each chunk prefixed with `path:start-end`.

- **Chunking**: tree-sitter per-language — split on top-level declarations (functions, types, classes); fall back to ~80-line windows for unsupported file types. Max chunk ~1200 chars.
- **Freshness**: re-embed only files whose content hash changed (hash stored in SQLite).
- **Scope**: respect `.gitignore`; skip binaries, lock files, files > 512 KiB.

## Interface sketch

```go
// internal/memory
type Store interface {
    NewSession(ctx context.Context, repoPath string) (*Session, error)
    Append(ctx context.Context, sessionID string, msg Message) error
    History(ctx context.Context, sessionID string) ([]Message, error)
    Summary(ctx context.Context, sessionID string) (string, error)
    SetSummary(ctx context.Context, sessionID string, s string, untilMsgID int64) error
    Remember(ctx context.Context, text, source, sessionID string) error
    Recall(ctx context.Context, query string, k int) ([]Memory, error)
}
```

## Rules

- The model never sees L3/L4 raw — only the budgeted injections, clearly delimited.
- Memory writes are never silent to the user: `memory_save` calls show up in the UI like any tool call.
- All memory is local (SQLite + chromem files under the data dir). Deleting the data dir must be a complete, working "forget everything".
- No memory of tool *outputs* beyond what summarization keeps — raw outputs are too big and too low-value.
