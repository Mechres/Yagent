package memory

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/scrub"
)

// Hybrid retrieval weights (docs/design/memory.md L3): vector cosine and FTS5
// keyword share the load so recall survives a mediocre embedder, plus
// importance and recency metadata.
const (
	hybridVecWeight = 0.4
	hybridFtsWeight = 0.3
	hybridImpWeight = 0.2
	hybridRecWeight = 0.1

	// recencyHalflifeHours halves a memory's recency score every 7 days.
	recencyHalflifeHours = 168.0

	// minRecallScore drops memories whose vector similarity is below this
	// from the vector pool (keyword candidates are exempt).
	minRecallScore = 0.35

	vecPoolSize = 50
	ftsPoolSize = 50
)

// memSchema is the L3 store: one SQLite file with the sessions (sessions.db),
// the memories rows and an FTS5 keyword index. Pure Go — no chromem, no ANN.
const memSchema = `
CREATE TABLE IF NOT EXISTS memories (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    text        TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT '',
    session_id  TEXT NOT NULL DEFAULT '',
    importance  REAL NOT NULL DEFAULT 0.5,
    created_at  INTEGER NOT NULL,
    vector      BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memories_created ON memories(created_at);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(text);
CREATE TABLE IF NOT EXISTS always_on_snapshots (
    kind       TEXT PRIMARY KEY,
    text       TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
`

const (
	MaxProjectFactsChars = 4000
	MaxUserProfileChars  = 2000
)

// Snapshot is a bounded always-on memory value. Unlike semantic memories it
// is injected on every turn and is never embedded or ranked.
type Snapshot struct {
	Kind      string
	Text      string
	UpdatedAt int64
}

// Memory is one recalled semantic memory (L3).
type Memory struct {
	ID        string
	Text      string
	Source    string // "tool" | "summary"
	SessionID string
	Score     float64 // hybrid score 0..1
}

// VectorStore is the L3 semantic memory: SQLite-backed hybrid retrieval
// (vector cosine + FTS5 keyword + importance + recency).
type VectorStore struct {
	db         *sql.DB
	dir        string
	embedModel string
	client     *llm.Client
}

// OpenVectorStore opens (creating if needed) the personal SQLite-backed memory
// store under dir, sharing sessions.db with the L2 Store.
func OpenVectorStore(dir, embedURL, embedModel string) (*VectorStore, error) {
	return openAt(filepath.Join(dir, "sessions.db"), dir, embedURL, embedModel)
}

// OpenProjectVectorStore opens a project-scoped memory store (committed to the
// repo at <project>/.yagent/memory/memory.db) shared by a team.
func OpenProjectVectorStore(dir, embedURL, embedModel string) (*VectorStore, error) {
	return openAt(filepath.Join(dir, "memory.db"), dir, embedURL, embedModel)
}

func openAt(dbPath, dir, embedURL, embedModel string) (*VectorStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	dsn := "file:" + dbPath + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open memory db: %w", err)
	}
	if _, err := db.Exec(memSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init memory schema: %w", err)
	}
	return &VectorStore{db: db, dir: dir, embedModel: embedModel, client: llm.NewClient(embedURL, embedModel)}, nil
}

// Close releases the database handle.
func (v *VectorStore) Close() error { return v.db.Close() }

// SetBearerToken applies Bearer auth to embedding requests (cloud endpoints).
func (v *VectorStore) SetBearerToken(token string) {
	if token != "" {
		v.client.BearerToken = token
	}
}

// Dir returns the store's data directory.
func (v *VectorStore) Dir() string { return v.dir }

// Snapshot returns one always-on value, or an empty value when unset.
func (v *VectorStore) Snapshot(ctx context.Context, kind string) (Snapshot, error) {
	var out Snapshot
	err := v.db.QueryRowContext(ctx, `SELECT kind, text, updated_at FROM always_on_snapshots WHERE kind = ?`, kind).
		Scan(&out.Kind, &out.Text, &out.UpdatedAt)
	if err == sql.ErrNoRows {
		return Snapshot{Kind: kind}, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("get always-on snapshot: %w", err)
	}
	return out, nil
}

// ReplaceSnapshot atomically replaces one bounded always-on value.
func (v *VectorStore) ReplaceSnapshot(ctx context.Context, kind, text string) error {
	limit := MaxProjectFactsChars
	if kind == "user_profile" {
		limit = MaxUserProfileChars
	}
	text = strings.TrimSpace(scrub.Text(text))
	if text == "" {
		return fmt.Errorf("snapshot text is required")
	}
	if len(text) > limit {
		return fmt.Errorf("%s exceeds %d character cap", kind, limit)
	}
	if kind != "project_facts" && kind != "user_profile" {
		return fmt.Errorf("unknown snapshot kind %q", kind)
	}
	_, err := v.db.ExecContext(ctx, `INSERT INTO always_on_snapshots(kind, text, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(kind) DO UPDATE SET text = excluded.text, updated_at = excluded.updated_at`, kind, text, time.Now().Unix())
	return err
}

// RemoveSnapshot removes one always-on value. Removing an absent value is
// successful, which makes explicit cleanup idempotent.
func (v *VectorStore) RemoveSnapshot(ctx context.Context, kind string) error {
	if kind != "project_facts" && kind != "user_profile" {
		return fmt.Errorf("unknown snapshot kind %q", kind)
	}
	_, err := v.db.ExecContext(ctx, `DELETE FROM always_on_snapshots WHERE kind = ?`, kind)
	return err
}

func (v *VectorStore) embedOne(ctx context.Context, text string) ([]float32, error) {
	vecs, err := v.client.Embed(ctx, v.embedModel, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("embed returned %d vectors for 1 input", len(vecs))
	}
	return vecs[0], nil
}

// Save stores one memory (fact, preference, decision, summary) with its
// embedding and keyword index. importance (0..1) biases recall; 0 uses the
// default 0.5. The text is redacted before it is embedded and persisted.
func (v *VectorStore) Save(ctx context.Context, text, source, sessionID string, importance float64) error {
	if text == "" {
		return fmt.Errorf("memory text is required")
	}
	if importance <= 0 {
		importance = 0.5
	}
	if importance > 1 {
		importance = 1
	}
	text = scrub.Text(text)
	vec, err := v.embedOne(ctx, text)
	if err != nil {
		return fmt.Errorf("embed memory: %w", err)
	}
	res, err := v.db.ExecContext(ctx,
		`INSERT INTO memories (text, source, session_id, importance, created_at, vector) VALUES (?, ?, ?, ?, ?, ?)`,
		text, source, sessionID, importance, time.Now().Unix(), f32ToBytes(vec))
	if err != nil {
		return fmt.Errorf("insert memory: %w", err)
	}
	id, _ := res.LastInsertId()
	if _, err := v.db.ExecContext(ctx, `INSERT INTO memories_fts (rowid, text) VALUES (?, ?)`, id, text); err != nil {
		return fmt.Errorf("index memory: %w", err)
	}
	return nil
}

// Search recalls the top-k memories for query, hybrid-ranked:
//
//	score = 0.4·norm(cosine) + 0.3·norm(bm25) + 0.2·importance + 0.1·recency
//
// Candidates are the union of the vector pool (cosine ≥ minRecallScore, top
// vecPoolSize) and the FTS5 keyword pool (top ftsPoolSize), so a memory that
// matches the words but not the embedding still surfaces. O(N) full scan over
// vectors — fine at this scale, no ANN index needed.
func (v *VectorStore) Search(ctx context.Context, query string, k int) ([]Memory, error) {
	if k <= 0 {
		k = 5
	}
	// Zero-count short-circuit (agy #1): an empty store needs no embedding
	// request or SlotLock acquisition — clean-slate projects would otherwise
	// spend a GPU round-trip on every turn for nothing.
	if v.Count() == 0 {
		return nil, nil
	}
	qvec, err := v.embedOne(ctx, query)
	if err != nil {
		return nil, err
	}
	all, err := v.loadAll(ctx, qvec)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	byID := make(map[int64]*memRow, len(all))
	for _, m := range all {
		byID[m.id] = m
	}

	// Candidate set: vector pool ∪ keyword pool.
	cands := map[int64]*memRow{}
	for _, m := range topVec(all, vecPoolSize) {
		cands[m.id] = m
	}
	for _, h := range v.ftsTop(ctx, query, ftsPoolSize) {
		if m, ok := byID[h.id]; ok {
			m.bm25 = h.bm25
			m.hasBM25 = true
			cands[m.id] = m
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}

	list := make([]*memRow, 0, len(cands))
	for _, m := range cands {
		list = append(list, m)
	}
	// Normalize cosine and bm25 within the candidate set so no component
	// dominates; non-keyword candidates score 0 on the keyword axis.
	minCos, maxCos := bounds(list, func(m *memRow) float64 { return m.cosine })
	minBM, maxBM := bounds(list, func(m *memRow) float64 { return m.bm25 })
	now := float64(time.Now().Unix())
	for _, m := range list {
		normCos := 1.0
		if maxCos > minCos {
			normCos = (m.cosine - minCos) / (maxCos - minCos)
		}
		ftsScore := 0.0
		if m.hasBM25 {
			if maxBM > minBM {
				ftsScore = (maxBM - m.bm25) / (maxBM - minBM)
			} else {
				ftsScore = 1.0
			}
		}
		ageHours := (now - float64(m.createdAt)) / 3600.0
		m.hybrid = hybridVecWeight*normCos + hybridFtsWeight*ftsScore +
			hybridImpWeight*m.importance + hybridRecWeight*recency(ageHours)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].hybrid > list[j].hybrid })
	if k > len(list) {
		k = len(list)
	}
	out := make([]Memory, 0, k)
	for _, m := range list[:k] {
		out = append(out, Memory{
			ID:        fmt.Sprint(m.id),
			Text:      m.text,
			Source:    m.source,
			SessionID: m.sessionID,
			Score:     m.hybrid,
		})
	}
	return out, nil
}

// Count reports how many memories are stored.
func (v *VectorStore) Count() int {
	var n int
	_ = v.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n)
	return n
}

// List returns every memory (newest first), for the `yagent memory` CLI.
func (v *VectorStore) List(limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := v.db.Query(
		`SELECT id, text, source, session_id, created_at FROM memories ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		var id int64
		var createdAt int64
		if err := rows.Scan(&id, &m.Text, &m.Source, &m.SessionID, &createdAt); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		m.ID = fmt.Sprint(id)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Delete removes a memory by id. Reports false when the id was not found.
func (v *VectorStore) Delete(id string) (bool, error) {
	res, err := v.db.Exec(`DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete memory: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteAll removes every memory (yagent memory clear).
func (v *VectorStore) DeleteAll() error {
	_, err := v.db.Exec(`DELETE FROM memories`)
	return err
}

// memRow is one decoded memories row plus derived scores.
type memRow struct {
	id         int64
	text       string
	source     string
	sessionID  string
	importance float64
	createdAt  int64
	vector     []float32
	cosine     float64
	bm25       float64
	hasBM25    bool
	hybrid     float64
}

type ftsHit struct {
	id   int64
	bm25 float64
}

func (v *VectorStore) loadAll(ctx context.Context, qvec []float32) ([]*memRow, error) {
	rows, err := v.db.QueryContext(ctx,
		`SELECT id, text, source, session_id, importance, created_at, vector FROM memories`)
	if err != nil {
		return nil, fmt.Errorf("query memories: %w", err)
	}
	defer rows.Close()
	var out []*memRow
	for rows.Next() {
		var m memRow
		var vec []byte
		if err := rows.Scan(&m.id, &m.text, &m.source, &m.sessionID, &m.importance, &m.createdAt, &vec); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		if len(vec) < 4 || len(vec)%4 != 0 {
			continue
		}
		m.vector = bytesToF32(vec)
		m.cosine = cosineF32(qvec, m.vector)
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (v *VectorStore) ftsTop(ctx context.Context, query string, limit int) []ftsHit {
	q, ok := ftsQuery(query)
	if !ok {
		return nil
	}
	rows, err := v.db.QueryContext(ctx,
		`SELECT rowid, bm25(memories_fts) FROM memories_fts
		 WHERE memories_fts MATCH ? ORDER BY bm25(memories_fts) LIMIT ?`, q, limit)
	if err != nil {
		return nil // keyword search is best-effort
	}
	defer rows.Close()
	var out []ftsHit
	for rows.Next() {
		var h ftsHit
		if err := rows.Scan(&h.id, &h.bm25); err != nil {
			return nil
		}
		out = append(out, h)
	}
	return out
}

// topVec returns the poolSize memories with the highest cosine above the
// relevance floor.
func topVec(all []*memRow, n int) []*memRow {
	pool := make([]*memRow, 0, len(all))
	for _, m := range all {
		if m.cosine >= minRecallScore {
			pool = append(pool, m)
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].cosine > pool[j].cosine })
	if len(pool) > n {
		pool = pool[:n]
	}
	return pool
}

func bounds(list []*memRow, f func(*memRow) float64) (float64, float64) {
	lo, hi := f(list[0]), f(list[0])
	for _, m := range list[1:] {
		if v := f(m); v < lo {
			lo = v
		} else if v > hi {
			hi = v
		}
	}
	return lo, hi
}

// ftsQuery builds a safe FTS5 MATCH string from the alphabetic tokens of a
// natural-language query (OR-joined, so any keyword overlap counts).
func ftsQuery(query string) (string, bool) {
	var terms []string
	for _, t := range strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(t) >= 2 {
			terms = append(terms, t)
		}
	}
	if len(terms) == 0 {
		return "", false
	}
	return strings.Join(terms, " OR "), true
}

// recency decays a memory's temporal score from 1 (just now) toward 0.
func recency(ageHours float64) float64 {
	return math.Pow(0.5, ageHours/recencyHalflifeHours)
}

func f32ToBytes(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

func bytesToF32(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

func cosineF32(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
