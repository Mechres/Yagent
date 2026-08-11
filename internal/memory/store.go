// Package memory implements the L2 session store (SQLite) and L3 semantic
// memory (SQLite-backed hybrid retrieval: vector + FTS5 keyword + importance
// + recency), per docs/design/memory.md. All data lives under the data dir;
// deleting it is a complete "forget everything".
package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"yagent/internal/llm"
	"yagent/internal/scrub"
)

// Message is the persisted form of a chat message.
type Message = llm.Message

// Session is a persisted conversation.
type Session struct {
	ID        string
	RepoPath  string
	Title     string
	CreatedAt int64
	UpdatedAt int64
}

// SessionSummary is one row of `yagent sessions`.
type SessionSummary struct {
	ID        string
	Title     string
	CreatedAt int64
	UpdatedAt int64
	Messages  int
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    repo_path   TEXT NOT NULL,
    title       TEXT,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT NOT NULL REFERENCES sessions(id),
    role         TEXT NOT NULL,
    content      TEXT NOT NULL,
    tool_call_id TEXT,
    tool_calls   TEXT,
    tokens_est   INTEGER,
    created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, id);
CREATE TABLE IF NOT EXISTS summaries (
    session_id   TEXT PRIMARY KEY REFERENCES sessions(id),
    summary      TEXT NOT NULL,
    covers_until INTEGER NOT NULL
);
`

// Store is the SQLite-backed session store.
type Store struct {
	db  *sql.DB
	dir string
}

// Open opens (creating if needed) the SQLite store under dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "sessions.db"))
	if err != nil {
		return nil, fmt.Errorf("open session db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db, dir: dir}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Dir returns the store's data directory.
func (s *Store) Dir() string { return s.dir }

// NewSession creates a session row and returns it.
func (s *Store) NewSession(ctx context.Context, repoPath string) (*Session, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	sess := &Session{ID: id, RepoPath: repoPath, CreatedAt: now, UpdatedAt: now}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, repo_path, title, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
		sess.ID, sess.RepoPath, sess.CreatedAt, sess.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return sess, nil
}

// Append persists one message and returns its row id. The session title is
// auto-set from the first user message when missing. Content is redacted
// (scrubbed of likely secrets) before it is written to disk.
func (s *Store) Append(ctx context.Context, sessionID string, msg Message) (int64, error) {
	var toolCalls []byte
	if len(msg.ToolCalls) > 0 {
		var err error
		toolCalls, err = json.Marshal(msg.ToolCalls)
		if err != nil {
			return 0, fmt.Errorf("marshal tool_calls: %w", err)
		}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (session_id, role, content, tool_call_id, tool_calls, tokens_est, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sessionID, msg.Role, scrub.Text(msg.Content), nullIfEmpty(msg.ToolCallID), nullIfEmpty(scrub.Text(string(toolCalls))),
		len(msg.Content)/4, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	id, _ := res.LastInsertId()

	if msg.Role == "user" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE sessions SET title = COALESCE(NULLIF(title, ''), ?) WHERE id = ?`,
			truncateTitle(scrub.Text(msg.Content)), sessionID); err != nil {
			return 0, fmt.Errorf("set session title: %w", err)
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, time.Now().Unix(), sessionID)
	if err != nil {
		return 0, fmt.Errorf("touch session: %w", err)
	}
	return id, nil
}

// History returns all messages of a session, oldest first.
func (s *Store) History(ctx context.Context, sessionID string) ([]Message, error) {
	return s.historyAfter(ctx, sessionID, 0)
}

// HistoryAfter returns messages with id > minID (used on --continue to skip
// messages already covered by the running summary).
func (s *Store) HistoryAfter(ctx context.Context, sessionID string, minID int64) ([]Message, error) {
	return s.historyAfter(ctx, sessionID, minID)
}

func (s *Store) historyAfter(ctx context.Context, sessionID string, minID int64) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, content, tool_call_id, tool_calls FROM messages
		 WHERE session_id = ? AND id > ? ORDER BY id`, sessionID, minID)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var toolCallID, toolCalls sql.NullString
		if err := rows.Scan(&m.Role, &m.Content, &toolCallID, &toolCalls); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.ToolCallID = toolCallID.String
		if toolCalls.Valid && toolCalls.String != "" {
			if err := json.Unmarshal([]byte(toolCalls.String), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("unmarshal tool_calls: %w", err)
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListSessions returns all sessions, newest first, with message counts.
func (s *Store) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sess.id, sess.title, sess.created_at, sess.updated_at,
		        (SELECT COUNT(*) FROM messages m WHERE m.session_id = sess.id)
		 FROM sessions sess ORDER BY sess.updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var ss SessionSummary
		if err := rows.Scan(&ss.ID, &ss.Title, &ss.CreatedAt, &ss.UpdatedAt, &ss.Messages); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// Summary returns the running summary and the last message id it covers.
func (s *Store) Summary(ctx context.Context, sessionID string) (string, int64, error) {
	var summary string
	var until int64
	err := s.db.QueryRowContext(ctx,
		`SELECT summary, covers_until FROM summaries WHERE session_id = ?`, sessionID).
		Scan(&summary, &until)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("get summary: %w", err)
	}
	return summary, until, nil
}

// SetSummary stores the running summary covering messages up to and
// including msgID. The summary is redacted before being written.
func (s *Store) SetSummary(ctx context.Context, sessionID, summary string, msgID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO summaries (session_id, summary, covers_until) VALUES (?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET summary = excluded.summary, covers_until = excluded.covers_until`,
		sessionID, scrub.Text(summary), msgID)
	if err != nil {
		return fmt.Errorf("set summary: %w", err)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncateTitle(s string) string {
	r := []rune(s)
	if len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
