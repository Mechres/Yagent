package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/philippgille/chromem-go"
)

const (
	vectorCollection = "memories"
	// minRecallScore drops memories whose cosine similarity to the query is
	// below 0.35 (memory.md L3: "filter score < 0.35").
	minRecallScore = 0.35
)

// Memory is one recalled semantic memory (L3).
type Memory struct {
	ID        string
	Text      string
	Source    string // "tool" | "summary"
	SessionID string
	Score     float64
}

// VectorStore is the L3 semantic memory: a chromem collection persisted to
// disk, embedding through the configured OpenAI-compatible endpoint.
type VectorStore struct {
	db    *chromem.DB
	col   *chromem.Collection
	embed chromem.EmbeddingFunc
	dir   string
	path  string
}

// OpenVectorStore opens (creating if needed) the persistent vector store
// under dir, using serverURL/embedModel for embeddings.
func OpenVectorStore(dir, serverURL, embedModel string) (*VectorStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "chromem")
	db, err := chromem.NewPersistentDB(path, false)
	if err != nil {
		return nil, fmt.Errorf("open chromem: %w", err)
	}
	embedFunc := chromem.NewEmbeddingFuncOpenAICompat(serverURL, "", embedModel, nil)
	col, err := db.GetOrCreateCollection(vectorCollection, nil, embedFunc)
	if err != nil {
		return nil, fmt.Errorf("get memories collection: %w", err)
	}
	return &VectorStore{db: db, col: col, embed: embedFunc, dir: dir, path: path}, nil
}

// Close releases the store. chromem persists synchronously on every write,
// so there is nothing to flush.
func (v *VectorStore) Close() error { return nil }

// Dir returns the store's data directory.
func (v *VectorStore) Dir() string { return v.dir }

// Save stores one memory (fact, preference, decision) with its embedding.
func (v *VectorStore) Save(ctx context.Context, text, source, sessionID string) error {
	if text == "" {
		return fmt.Errorf("memory text is required")
	}
	id, err := newID()
	if err != nil {
		return err
	}
	metadata := map[string]string{
		"source":     source,
		"session_id": sessionID,
		"created_at": fmt.Sprint(time.Now().Unix()),
	}
	doc, err := chromem.NewDocument(ctx, id, metadata, nil, text, v.embed)
	if err != nil {
		return fmt.Errorf("embed memory: %w", err)
	}
	if err := v.col.AddDocument(ctx, doc); err != nil {
		return fmt.Errorf("add memory: %w", err)
	}
	return nil
}

// Search recalls the top-k memories similar to query, ordered best-first,
// dropping results below the relevance threshold.
func (v *VectorStore) Search(ctx context.Context, query string, k int) ([]Memory, error) {
	if k <= 0 {
		k = 5
	}
	// chromem requires nResults <= document count.
	if n := v.col.Count(); k > n {
		k = n
	}
	if k == 0 {
		return nil, nil
	}
	results, err := v.col.Query(ctx, query, k, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("query memories: %w", err)
	}
	out := make([]Memory, 0, len(results))
	for _, r := range results {
		if float64(r.Similarity) < minRecallScore {
			continue
		}
		out = append(out, Memory{
			ID:        r.ID,
			Text:      r.Content,
			Source:    r.Metadata["source"],
			SessionID: r.Metadata["session_id"],
			Score:     float64(r.Similarity),
		})
	}
	return out, nil
}

// Count reports how many memories are stored.
func (v *VectorStore) Count() int { return v.col.Count() }
