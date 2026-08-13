// Package index implements M4: a workspace code index — gitignore-aware
// walker, tree-sitter structural chunking (line-window fallback), content-hash
// incremental re-embedding, and hybrid (vector + FTS5) semantic search. Chunks
// and content hashes live in the same SQLite file as sessions/memories.
package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/Mechres/Yagent/internal/llm"
)

// indexSchema stores per-file content hashes and per-chunk vectors.
const indexSchema = `
CREATE TABLE IF NOT EXISTS index_files (
    path        TEXT PRIMARY KEY,
    hash        TEXT NOT NULL,
    indexed_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS index_chunks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    path        TEXT NOT NULL,
    start_line  INTEGER NOT NULL,
    end_line    INTEGER NOT NULL,
    content     TEXT NOT NULL,
    vector      BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_index_chunks_path ON index_chunks(path);
CREATE VIRTUAL TABLE IF NOT EXISTS index_chunks_fts USING fts5(content);
CREATE TABLE IF NOT EXISTS index_symbols (
    id    INTEGER PRIMARY KEY AUTOINCREMENT,
    path  TEXT NOT NULL,
    name  TEXT NOT NULL,
    kind  TEXT NOT NULL,
    line  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_symbols_name ON index_symbols(name, kind);
CREATE TABLE IF NOT EXISTS index_calls (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    path    TEXT NOT NULL,
    line    INTEGER NOT NULL,
    callee  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_calls_callee ON index_calls(callee);
`

// Summary reports what one Index pass did.
type Summary struct {
	Files      int // files indexed or re-indexed
	Chunks     int // chunks embedded
	Skipped    int // files unchanged since the last pass
	StaleFiles int // previously indexed files no longer present
	ChunksDone int // stale chunks removed
	Duration   time.Duration
}

// Store is the codebase index for one workspace.
type Store struct {
	mu         sync.Mutex // serializes Index() and OnProgress access
	db         *sql.DB
	workspace  string
	embedModel string
	client     *llm.Client
	skipRel    string // data dir relative to the workspace, when nested
	OnProgress func(string)
}

// Open opens (creating if needed) the index store for workspace, sharing the
// SQLite file under dataDir with sessions/memories.
func Open(workspace, dataDir, embedURL, embedModel string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dataDir, err)
	}
	dsn := "file:" + filepath.Join(dataDir, "sessions.db") + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open index db: %w", err)
	}
	if _, err := db.Exec(indexSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init index schema: %w", err)
	}
	ws, err := filepath.Abs(workspace)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("workspace: %w", err)
	}
	ws = filepath.Clean(ws)
	store := &Store{
		db: db, workspace: ws,
		embedModel: embedModel, client: llm.NewClient(embedURL, embedModel),
	}
	if dataAbs, err := filepath.Abs(dataDir); err == nil {
		if rel, err := filepath.Rel(ws, filepath.Clean(dataAbs)); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			store.skipRel = filepath.ToSlash(rel)
		}
	}
	return store, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// SetBearerToken applies Bearer auth to embedding requests (cloud endpoints).
func (s *Store) SetBearerToken(token string) {
	if token != "" {
		s.client.BearerToken = token
	}
}

// Count reports how many chunks are indexed.
func (s *Store) Count() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM index_chunks`).Scan(&n)
	return n
}

// Index rebuilds the index incrementally: only files whose content hash
// changed since the last pass are re-chunked and re-embedded; unchanged files
// are skipped; files that disappeared are pruned. Serialized so a background
// startup re-index and an explicit index_repo call never interleave.
func (s *Store) Index(ctx context.Context) (Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := time.Now()
	var sum Summary

	files, err := s.walkFiles()
	if err != nil {
		return sum, fmt.Errorf("walk workspace: %w", err)
	}

	current := make(map[string]string, len(files)) // relPath -> sha256 hex
	var newChunks []Chunk
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		content, err := os.ReadFile(filepath.Join(s.workspace, rel))
		if err != nil {
			continue
		}
		h := sha256.Sum256(content)
		hash := hex.EncodeToString(h[:])
		current[rel] = hash

		if old, ok := s.fileHash(rel); ok && old == hash {
			sum.Skipped++
			continue
		}
		if err := s.removeChunks(ctx, rel); err != nil {
			return sum, err
		}
		if err := s.removeSymbols(ctx, rel); err != nil {
			return sum, err
		}
		if err := s.removeRefs(ctx, rel); err != nil {
			return sum, err
		}
		chunks, syms, refs := chunkAndSymbols(rel, string(content))
		if len(chunks) == 0 && len(refs) == 0 {
			continue
		}
		newChunks = append(newChunks, chunks...)
		if err := s.insertSymbols(ctx, syms); err != nil {
			return sum, err
		}
		if err := s.insertRefs(ctx, refs); err != nil {
			return sum, err
		}
		sum.Files++
		s.progressf("indexing %s (%d chunks)", rel, len(chunks))
	}

	if err := s.embedChunks(ctx, newChunks); err != nil {
		return sum, err
	}
	sum.Chunks += len(newChunks)
	for rel, hash := range current {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO index_files (path, hash, indexed_at) VALUES (?, ?, ?)
			 ON CONFLICT(path) DO UPDATE SET hash = excluded.hash, indexed_at = excluded.indexed_at`,
			rel, hash, time.Now().Unix()); err != nil {
			return sum, fmt.Errorf("record file hash: %w", err)
		}
	}

	// Prune files that no longer exist.
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM index_files`)
	if err != nil {
		return sum, fmt.Errorf("list indexed files: %w", err)
	}
	var stale []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			rows.Close()
			return sum, err
		}
		if _, ok := current[rel]; !ok {
			stale = append(stale, rel)
		}
	}
	rows.Close()
	for _, rel := range stale {
		if err := s.removeChunks(ctx, rel); err != nil {
			return sum, err
		}
		if err := s.removeSymbols(ctx, rel); err != nil {
			return sum, err
		}
		if err := s.removeRefs(ctx, rel); err != nil {
			return sum, err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM index_files WHERE path = ?`, rel); err != nil {
			return sum, fmt.Errorf("drop file entry: %w", err)
		}
		sum.StaleFiles++
		s.progressf("removed %s", rel)
	}

	sum.Duration = time.Since(start)
	s.progressf("done: %d files, %d chunks, %d skipped, %d stale removed",
		sum.Files, sum.Chunks, sum.Skipped, sum.StaleFiles)
	return sum, nil
}

func (s *Store) progressf(format string, args ...any) {
	if s.OnProgress != nil {
		s.OnProgress(fmt.Sprintf(format, args...))
	}
}

// SetOnProgress wires the progress sink (thread-safe).
func (s *Store) SetOnProgress(fn func(string)) {
	s.mu.Lock()
	s.OnProgress = fn
	s.mu.Unlock()
}

func (s *Store) fileHash(rel string) (string, bool) {
	var h string
	err := s.db.QueryRow(`SELECT hash FROM index_files WHERE path = ?`, rel).Scan(&h)
	return h, err == nil
}

// embedChunks embeds and stores chunks in batches.
func (s *Store) embedChunks(ctx context.Context, chunks []Chunk) error {
	const batch = 8
	for i := 0; i < len(chunks); i += batch {
		end := i + batch
		if end > len(chunks) {
			end = len(chunks)
		}
		texts := make([]string, 0, end-i)
		for _, c := range chunks[i:end] {
			texts = append(texts, c.Content)
		}
		vecs, err := s.client.Embed(ctx, s.embedModel, texts)
		if err != nil {
			return fmt.Errorf("embed chunks: %w", err)
		}
		if len(vecs) != len(texts) {
			return fmt.Errorf("embed returned %d vectors for %d texts", len(vecs), len(texts))
		}
		for j, c := range chunks[i:end] {
			res, err := s.db.ExecContext(ctx,
				`INSERT INTO index_chunks (path, start_line, end_line, content, vector) VALUES (?, ?, ?, ?, ?)`,
				c.Path, c.StartLine, c.EndLine, c.Content, f32ToBytes(vecs[j]))
			if err != nil {
				return fmt.Errorf("insert chunk: %w", err)
			}
			id, _ := res.LastInsertId()
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO index_chunks_fts (rowid, content) VALUES (?, ?)`, id, c.Content); err != nil {
				return fmt.Errorf("index chunk: %w", err)
			}
		}
	}
	return nil
}

// removeChunks deletes a file's chunks from the vector and FTS tables.
func (s *Store) removeChunks(ctx context.Context, rel string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM index_chunks WHERE path = ?`, rel)
	if err != nil {
		return fmt.Errorf("list chunks: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM index_chunks_fts WHERE rowid = ?`, id); err != nil {
			return fmt.Errorf("remove fts chunk: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM index_chunks WHERE path = ?`, rel); err != nil {
		return fmt.Errorf("remove chunk rows: %w", err)
	}
	return nil
}

// insertSymbols stores declaration symbols (already parsed by chunkAndSymbols).
func (s *Store) insertSymbols(ctx context.Context, syms []Symbol) error {
	for _, sym := range syms {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO index_symbols (path, name, kind, line) VALUES (?, ?, ?, ?)`,
			sym.Path, sym.Name, sym.Kind, sym.Line); err != nil {
			return fmt.Errorf("insert symbol: %w", err)
		}
	}
	return nil
}

// removeSymbols deletes a file's symbols.
func (s *Store) removeSymbols(ctx context.Context, rel string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM index_symbols WHERE path = ?`, rel); err != nil {
		return fmt.Errorf("remove symbols: %w", err)
	}
	return nil
}

// insertRefs stores a file's call sites (already parsed by chunkAndSymbols).
func (s *Store) insertRefs(ctx context.Context, refs []CallRef) error {
	for _, r := range refs {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO index_calls (path, line, callee) VALUES (?, ?, ?)`,
			r.Path, r.Line, r.Callee); err != nil {
			return fmt.Errorf("insert call ref: %w", err)
		}
	}
	return nil
}

// removeRefs deletes a file's call sites.
func (s *Store) removeRefs(ctx context.Context, rel string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM index_calls WHERE path = ?`, rel); err != nil {
		return fmt.Errorf("remove call refs: %w", err)
	}
	return nil
}

// References returns every indexed call site of callee, in path/line order.
func (s *Store) References(ctx context.Context, callee string) []CallRef {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, line FROM index_calls WHERE callee = ? ORDER BY path, line`, callee)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CallRef
	for rows.Next() {
		var r CallRef
		if err := rows.Scan(&r.Path, &r.Line); err != nil {
			return out
		}
		r.Callee = callee
		out = append(out, r)
	}
	return out
}

// SymbolResult is one exact-name symbol match.
type SymbolResult struct {
	Path string
	Name string
	Kind string
	Line int
}

// DeadSymbol is an exported top-level declaration with zero in-repo callers —
// a candidate for safe deletion (not proof: interface implementations and
// dynamic dispatch don't produce index_calls, and tests count as callers).
type DeadSymbol struct {
	Path string
	Name string
	Kind string
	Line int
}

// DeadSymbols lists exported symbols with no call sites anywhere in the
// indexed workspace. Test references are indexed too, so a symbol used only by
// tests is NOT dead. This is a candidate list — the caller should present it as
// "safe to delete" candidates, never as truth.
func (s *Store) DeadSymbols(ctx context.Context) []DeadSymbol {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sym.path, sym.name, sym.kind, sym.line
		FROM index_symbols sym
		WHERE NOT EXISTS (
			SELECT 1 FROM index_calls c WHERE c.callee = sym.name
		)
		ORDER BY sym.path, sym.line`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []DeadSymbol
	for rows.Next() {
		var d DeadSymbol
		if err := rows.Scan(&d.Path, &d.Name, &d.Kind, &d.Line); err != nil {
			return out
		}
		out = append(out, d)
	}
	return out
}

// SearchSymbol finds declarations by exact name, optionally filtered by kind.
func (s *Store) SearchSymbol(ctx context.Context, name, kind string) ([]SymbolResult, error) {
	q := `SELECT path, name, kind, line FROM index_symbols WHERE name = ?`
	args := []any{name}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY path, line LIMIT 50`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("search symbols: %w", err)
	}
	defer rows.Close()
	var out []SymbolResult
	for rows.Next() {
		var r SymbolResult
		if err := rows.Scan(&r.Path, &r.Name, &r.Kind, &r.Line); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// lockFiles are skipped even when gitignore doesn't list them.
var lockFiles = map[string]bool{
	"go.sum": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"package-lock.json": true, "Cargo.lock": true, "composer.lock": true,
	"Gemfile.lock": true, "poetry.lock": true,
}

// walkFiles returns the indexable files relative to the workspace: gitignore
// aware, skipping .git/.yagent, the data dir when nested, binaries, oversized
// files and lock files.
func (s *Store) walkFiles() ([]string, error) {
	var files []string

	m := &gitignoreMatcher{}
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if data, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err == nil {
			m.push(parseGitignore(rel, string(data)))
			defer m.pop()
		}
		for _, e := range entries {
			name := e.Name()
			relPath := filepath.ToSlash(filepath.Join(rel, name))
			if name == ".git" || name == ".yagent" || relPath == s.skipRel {
				continue
			}
			if m.ignored(relPath, e.IsDir()) {
				continue
			}
			if e.IsDir() {
				if err := walk(filepath.Join(dir, name), relPath); err != nil {
					return err
				}
				continue
			}
			// Skip hidden files (.gitignore, .env, ...); hidden dirs (.github)
			// are still descended into.
			if strings.HasPrefix(name, ".") {
				continue
			}
			full := filepath.Join(dir, name)
			info, err := e.Info()
			if err != nil || info.Size() > maxFileBytes {
				continue
			}
			if lockFiles[filepath.ToSlash(name)] {
				continue
			}
			data, err := os.ReadFile(full)
			if err != nil || isBinary(data) {
				continue
			}
			files = append(files, relPath)
		}
		return nil
	}
	if err := walk(s.workspace, ""); err != nil {
		return nil, err
	}
	return files, nil
}
