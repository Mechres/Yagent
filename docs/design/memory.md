# Memory

Four layers, each with a single job. L1/L2 ship in M3, L3 in M3, L4 in M4.

```
L1 working context   in-memory []Message + token counter      (agent pkg)
L2 session history   SQLite: every message, resumable         (memory pkg)
L3 semantic memory   SQLite hybrid: vectors + FTS5 + weights  (memory pkg)
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

**Storage**: SQLite tables `memories` (text, `source`, `session_id`, `importance`, `created_at`, float32 vector BLOB) plus an FTS5 virtual table `memories_fts` for keyword search — same `sessions.db` file as L2, pure Go, no chromem/ANN (post-M3.5 rewrite; vectors O(N) scanned, fine at this scale). Embeddings come from the configured server (`embedding_server_url`, defaults to `server_url`).

**Hybrid retrieval** (modeled on Hermes/Mnemosyne's approach): candidates = vector pool (cosine ≥ 0.35, top 50) ∪ FTS5 keyword pool (top 50), scored

```
score = 0.4·norm(cosine) + 0.3·norm(bm25) + 0.2·importance + 0.1·recency
```

with cosine and bm25 normalized within the candidate set and recency decaying by a 168 h halflife. The keyword axis keeps recall useful even when the embedding model is weak (e.g. a chat model serving as embedder) — a memory that matches the words but not the vector still surfaces.

**Writes** (two paths):
1. Explicit: model calls `memory_save` (optional `importance` 0–1, default 0.5) when it judges something worth keeping (the tool description must say *what* is worth keeping: preferences, decisions, gotchas — not code, not chit-chat).
2. Implicit: at session end (or on `--continue` resume of an old session), a background job summarizes the session and stores the summary (importance 0.5).

**Reads**: every turn, embed the user input, hybrid top-5, inject under the memory budget (1000 tok). Deduplicate against the current session's own messages.

**Schema**:

```sql
CREATE TABLE memories (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    text        TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT '',
    session_id  TEXT NOT NULL DEFAULT '',
    importance  REAL NOT NULL DEFAULT 0.5,
    created_at  INTEGER NOT NULL,
    vector      BLOB NOT NULL               -- float32 LE
);
CREATE VIRTUAL TABLE memories_fts USING fts5(text);
```

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
- All memory is local (one SQLite file under the data dir). Deleting the data dir must be a complete, working "forget everything".
- No memory of tool *outputs* beyond what summarization keeps — raw outputs are too big and too low-value.
