package index

import (
	"context"
	"encoding/binary"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Hybrid retrieval weights mirror L3 memory (docs/design/memory.md): vector
// cosine and FTS5 keyword share the load, so code search stays usable even
// with a weak embedder — for code, keyword overlap is a strong signal.
const (
	idxVecWeight = 0.4
	idxFtsWeight = 0.3
	idxImpWeight = 0.0 // chunks have no importance metadata
	idxRecWeight = 0.3 // recency is meaningful for code (recently indexed)

	idxRecencyHalflifeHours = 24.0 * 30 // 30 days

	minChunkCosine = 0.30
	idxVecPoolSize = 25
	idxFtsPoolSize = 25
)

// Result is one matching chunk, rendered as path:start-end.
type Result struct {
	Path      string
	StartLine int
	EndLine   int
	Content   string
	Score     float64
}

// Search hybrid-ranks indexed chunks against a natural-language query:
//
//	score = 0.4·norm(cosine) + 0.3·norm(bm25) + 0.3·recency
//
// Candidates are the union of the vector pool (cosine ≥ minChunkCosine, top
// idxVecPoolSize) and the FTS5 keyword pool. O(N) full scan over chunk
// vectors; fine at personal-workspace scale.
func (s *Store) Search(ctx context.Context, query string, k int) ([]Result, error) {
	if k <= 0 {
		k = 5
	}
	qvec, err := s.embed(ctx, query)
	if err != nil {
		return nil, err
	}
	all, err := s.loadChunks(ctx, qvec)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	byID := make(map[int64]*chunkRow, len(all))
	for _, c := range all {
		byID[c.id] = c
	}

	cands := map[int64]*chunkRow{}
	for _, c := range topChunks(all, idxVecPoolSize) {
		cands[c.id] = c
	}
	for _, h := range s.ftsTop(ctx, query, idxFtsPoolSize) {
		if c, ok := byID[h.id]; ok {
			c.bm25 = h.bm25
			c.hasBM25 = true
			cands[c.id] = c
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}

	list := make([]*chunkRow, 0, len(cands))
	for _, c := range cands {
		list = append(list, c)
	}
	minCos, maxCos := bounds(list, func(c *chunkRow) float64 { return c.cosine })
	minBM, maxBM := bounds(list, func(c *chunkRow) float64 { return c.bm25 })
	now := float64(time.Now().Unix())
	for _, c := range list {
		normCos := 1.0
		if maxCos > minCos {
			normCos = (c.cosine - minCos) / (maxCos - minCos)
		}
		ftsScore := 0.0
		if c.hasBM25 {
			if maxBM > minBM {
				ftsScore = (maxBM - c.bm25) / (maxBM - minBM)
			} else {
				ftsScore = 1.0
			}
		}
		ageHours := (now - float64(c.indexedAt)) / 3600.0
		c.hybrid = idxVecWeight*normCos + idxFtsWeight*ftsScore +
			idxRecWeight*recency(ageHours)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].hybrid > list[j].hybrid })
	if k > len(list) {
		k = len(list)
	}
	out := make([]Result, 0, k)
	for _, c := range list[:k] {
		out = append(out, Result{
			Path: c.path, StartLine: c.startLine, EndLine: c.endLine,
			Content: c.content, Score: c.hybrid,
		})
	}
	return out, nil
}

type chunkRow struct {
	id        int64
	path      string
	startLine int
	endLine   int
	content   string
	vector    []float32
	cosine    float64
	bm25      float64
	hasBM25   bool
	hybrid    float64
	indexedAt int64
}

type ftsHit struct {
	id   int64
	bm25 float64
}

func (s *Store) loadChunks(ctx context.Context, qvec []float32) ([]*chunkRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.path, c.start_line, c.end_line, c.content, c.vector, f.indexed_at
		 FROM index_chunks c JOIN index_files f ON f.path = c.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*chunkRow
	for rows.Next() {
		var c chunkRow
		var vec []byte
		if err := rows.Scan(&c.id, &c.path, &c.startLine, &c.endLine, &c.content, &vec, &c.indexedAt); err != nil {
			return nil, err
		}
		if len(vec) < 4 || len(vec)%4 != 0 {
			continue
		}
		c.vector = bytesToF32(vec)
		c.cosine = cosineF32(qvec, c.vector)
		out = append(out, &c)
	}
	return out, rows.Err()
}

func (s *Store) ftsTop(ctx context.Context, query string, limit int) []ftsHit {
	q, ok := ftsQuery(query)
	if !ok {
		return nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, bm25(index_chunks_fts) FROM index_chunks_fts
		 WHERE index_chunks_fts MATCH ? ORDER BY bm25(index_chunks_fts) LIMIT ?`, q, limit)
	if err != nil {
		return nil
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

func (s *Store) embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := s.client.Embed(ctx, s.embedModel, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, err
	}
	return vecs[0], nil
}

func topChunks(all []*chunkRow, n int) []*chunkRow {
	pool := make([]*chunkRow, 0, len(all))
	for _, c := range all {
		if c.cosine >= minChunkCosine {
			pool = append(pool, c)
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].cosine > pool[j].cosine })
	if len(pool) > n {
		pool = pool[:n]
	}
	return pool
}

func bounds(list []*chunkRow, f func(*chunkRow) float64) (float64, float64) {
	lo, hi := f(list[0]), f(list[0])
	for _, c := range list[1:] {
		if v := f(c); v < lo {
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

func recency(ageHours float64) float64 {
	return math.Pow(0.5, ageHours/idxRecencyHalflifeHours)
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

func isBinary(data []byte) bool {
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	return strings.IndexByte(string(probe), 0) >= 0
}
