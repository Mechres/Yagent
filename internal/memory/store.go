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
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/scrub"
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
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(content);
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
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "sessions.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open session db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	st := &Store{db: db, dir: dir}
	if err := st.backfillFTS(); err != nil {
		db.Close()
		return nil, fmt.Errorf("backfill search index: %w", err)
	}
	return st, nil
}

// CountSessions reports how many sessions are stored (doctor storage audit).
func (s *Store) CountSessions() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	return n, err
}

// backfillFTS indexes messages written before the FTS table existed. It reads
// every row into memory first, then writes inside one transaction, so it never
// holds a read cursor while inserting (which would deadlock the rollback
// journal across pooled connections).
func (s *Store) backfillFTS() error {
	var msgs, fts int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgs); err != nil {
		return err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&fts); err != nil {
		return err
	}
	if msgs <= fts {
		return nil
	}
	rows, err := s.db.Query(`SELECT id, content FROM messages`)
	if err != nil {
		return err
	}
	type msgRow struct {
		id      int64
		content string
	}
	var data []msgRow
	for rows.Next() {
		var r msgRow
		if err := rows.Scan(&r.id, &r.content); err != nil {
			rows.Close()
			return err
		}
		data = append(data, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range data {
		if _, err := tx.Exec(`INSERT INTO messages_fts (rowid, content) VALUES (?, ?)`, r.id, scrub.Text(r.content)); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	if _, err := s.db.ExecContext(ctx, `INSERT INTO messages_fts (rowid, content) VALUES (?, ?)`,
		id, scrub.Text(msg.Content)); err != nil {
		return 0, fmt.Errorf("index message: %w", err)
	}

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

// SessionTitle returns a session's auto-generated title ("" when unset).
func (s *Store) SessionTitle(ctx context.Context, sessionID string) (string, error) {
	var title string
	err := s.db.QueryRowContext(ctx, `SELECT title FROM sessions WHERE id = ?`, sessionID).Scan(&title)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return title, nil
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

// MessageHit is one full-text search result across sessions.
type MessageHit struct {
	MessageID int64
	SessionID string
	Title     string
	Role      string
	Snippet   string
}

// CountMessages reports how many messages a session has.
func (s *Store) CountMessages(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}

// DeleteIfEmpty removes a session (and its messages) when it has no messages —
// used at chat teardown so opening and closing the TUI/REPL without talking
// doesn't leave an empty session row.
func (s *Store) DeleteIfEmpty(ctx context.Context, sessionID string) error {
	n, err := s.CountMessages(ctx, sessionID)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return s.DeleteSession(ctx, sessionID)
}

// DeleteSession removes a session and all its messages/summary (used by the
// TUI session browser).
func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM messages_fts WHERE rowid IN (SELECT id FROM messages WHERE session_id = ?)`,
		sessionID); err != nil {
		return fmt.Errorf("delete session search rows: %w", err)
	}
	for _, q := range []string{
		`DELETE FROM messages WHERE session_id = ?`,
		`DELETE FROM summaries WHERE session_id = ?`,
		`DELETE FROM sessions WHERE id = ?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, sessionID); err != nil {
			return fmt.Errorf("delete session: %w", err)
		}
	}
	return nil
}

// SearchMessages runs a full-text search over all stored messages (FTS5),
// newest first, capped at limit.
func (s *Store) SearchMessages(ctx context.Context, query string, limit int) ([]MessageHit, error) {
	q, ok := ftsQuery(query)
	if !ok {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, s.id, s.title, m.role, snippet(messages_fts, 0, '[', ']', '…', 12)
		 FROM messages_fts f
		 JOIN messages m ON m.id = f.rowid
		 JOIN sessions s ON s.id = m.session_id
		 WHERE messages_fts MATCH ?
		 ORDER BY f.rank LIMIT ?`, q, limit)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()
	var out []MessageHit
	for rows.Next() {
		var h MessageHit
		if err := rows.Scan(&h.MessageID, &h.SessionID, &h.Title, &h.Role, &h.Snippet); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// RenderMarkdown renders a session's transcript as Markdown for export.
func (s *Store) RenderMarkdown(ctx context.Context, sessionID string) (string, error) {
	history, err := s.History(ctx, sessionID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Yagent session %s\n\n", sessionID)
	for _, m := range history {
		switch m.Role {
		case "user":
			fmt.Fprintf(&b, "## User\n\n%s\n\n", m.Content)
		case "assistant":
			body := m.Content
			if body == "" {
				body = "(tool calls)"
			}
			fmt.Fprintf(&b, "## Assistant\n\n%s\n\n", body)
		case "tool":
			fmt.Fprintf(&b, "<details><summary>tool result</summary>\n\n```\n%s\n```\n\n</details>\n\n", m.Content)
		}
	}
	return b.String(), nil
}

// RenderHTML renders a session's transcript as a standalone HTML page (inline
// styling, content escaped, no JS, no network — a shareable archive).
func (s *Store) RenderHTML(ctx context.Context, sessionID string) (string, error) {
	history, err := s.History(ctx, sessionID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Yagent session %s</title>
<style>
body{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;max-width:64rem;margin:2rem auto;padding:0 1rem;color:#d4d4d8;background:#1b1d23;line-height:1.55;}
h1{color:#e4e4e7;font-size:1.25rem;}
.msg{margin:1.25rem 0;padding:.75rem 1rem;border-radius:6px;white-space:pre-wrap;word-break:break-word;}
.user{background:#2a2d36;border-left:3px solid #7aa2f7;}
.assistant{background:#24272e;border-left:3px solid #9ece6a;}
.tool{background:#1f2229;border-left:3px solid #5b6270;color:#a1a6b4;font-size:.9em;}
.role{display:block;font-weight:bold;margin-bottom:.4rem;color:#a1a6b4;}
</style></head>
<body><h1>Yagent session %s</h1>`, sessionID, sessionID)
	for _, m := range history {
		switch m.Role {
		case "user":
			fmt.Fprintf(&b, `<div class="msg user"><span class="role">User</span>%s</div>`, html.EscapeString(m.Content))
		case "assistant":
			body := m.Content
			if body == "" {
				body = "(tool calls)"
			}
			fmt.Fprintf(&b, `<div class="msg assistant"><span class="role">Assistant</span>%s</div>`, html.EscapeString(body))
		case "tool":
			fmt.Fprintf(&b, `<div class="msg tool"><span class="role">tool result</span>%s</div>`, html.EscapeString(m.Content))
		}
	}
	b.WriteString(`</body></html>`)
	return b.String(), nil
}

// ftsQuery builds a safe FTS5 MATCH string from the query's word tokens.
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
