# Memory

Four layers, each with a single job. L1/L2 ship in M3, L3 in M3, L4 in M4.

```
L1 working context   in-memory []Message + token counter      (agent pkg)
L2 session history   SQLite: every message, resumable         (memory pkg)
L3 semantic memory   SQLite hybrid: vectors + FTS5 + weights  (memory pkg)
L4 codebase index    tree-sitter chunks + embeddings          (index pkg)
```

## L1 — Working context

The slice of messages actually sent to the model, assembled per `agent-loop.md`. Budgeted by accurate token counts from the server tokenizer (`llm.Client.CountTokens`; `len/4` fallback). When over budget: old tool outputs are first collapsed to one-line `[tool output concealed; N lines hidden]` markers (user/assistant turns kept), then the oldest 50% of history (excluding system/tool-schema) is summarized and replaced by the running summary. Automatic and manual compaction choose message boundaries that never split an assistant tool call from its tool results. Manual `/compact` retains the latest prior user exchange alongside the current turn and protects the first exchange as an anchor in the ledger.

Summarization prompt (dedicated, no tools) requests stable sections: *Goal, Constraints, Progress, Decisions, Files, Next Steps,* and *Critical Context*. It is capped at 400 words and must preserve decisions, paths, errors, preferences, open tasks, and unresolved constraints without inventing facts.

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

**Storage**: global memory uses SQLite tables `memories` (text, `source`,
`session_id`, `importance`, `created_at`, float32 vector BLOB) plus an FTS5
virtual table `memories_fts` in the same `sessions.db` file as L2. A
repository-scoped store lives at `.yagent/memory/memory.db`; recall merges the
global and project scopes. This is pure Go with no chromem/ANN dependency;
vectors are O(N)-scanned, which is appropriate at this scale. Embeddings come
from the configured server (`embedding_server_url`, defaulting to `server_url`).

**Hybrid retrieval** (modeled on Hermes/Mnemosyne's approach): candidates = vector pool (cosine ≥ 0.35, top 50) ∪ FTS5 keyword pool (top 50), scored

```
score = 0.4·norm(cosine) + 0.3·norm(bm25) + 0.2·importance + 0.1·recency
```

with cosine and bm25 normalized within the candidate set and recency decaying by a 168 h halflife. The keyword axis keeps recall useful even when the embedding model is weak (e.g. a chat model serving as embedder) — a memory that matches the words but not the vector still surfaces.

**Writes** (two paths):
1. Explicit: the model calls the self-gated `memory_save` (optional
   `importance` 0–1, default 0.5; `scope: global|project`) when it judges
   something worth keeping: preferences, decisions, and durable gotchas—not
   code dumps or chit-chat.
2. Implicit: at session end (or on `--continue` resume of an old session), a background job summarizes the session and stores the summary (importance 0.5).

**Reads**: every turn, embed the user input, hybrid top-5, inject under the memory budget (1000 tok). Deduplicate against the current session's own messages. Recalled memories are framed as **user facts** (`- user fact: ...` with an explicit "attribute these to the USER" header) and `memory_save` instructs the model to phrase facts in the third person (`"the user's name is Ada"`, never `"my name is ..."`) — otherwise a first-person fact gets attributed to the assistant and recall fails.

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

Specified in `tools.md` (`index_repo`/`index_search`) and built in M4 (`internal/index`). Retrieval at turn start: embed user input → top-6 chunks → inject under the index budget (2000 tok), each chunk prefixed with `path:start-end`.

- **Chunking**: tree-sitter for Go, Python, JavaScript/TypeScript/TSX, Rust,
  C/C++, Java, Bash, HTML, and CSS—split on top-level declarations with doc
  comments attached. Unsupported file types use line windows. Chunks are
  capped at ~1200 characters / 80 lines.
- **Freshness**: sha256 content hash per file stored in SQLite; re-embed only files whose hash changed; files that disappear are pruned.
- **Scope**: gitignore-aware (nested `.gitignore` files, negation, dir-only rules), skip hidden files, binaries, lock files and files > 512 KiB.
- **Search**: hybrid like L3 — candidates = vector pool (cosine ≥ 0.30, top 25) ∪ FTS5 keyword pool; score `0.4·cos + 0.3·bm25 + 0.3·recency`, so keyword overlap rescues weak embeddings.

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
- Memory is local: global state is under the data directory and project memory
  is under `.yagent/memory/`. Removing the applicable store is a complete,
  working "forget everything" for that scope.
- No memory of tool *outputs* beyond what summarization keeps — raw outputs are too big and too low-value.
