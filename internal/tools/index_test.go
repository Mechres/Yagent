package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yagent/internal/index"
)

// indexEmbedServer is a neutral deterministic embedder (same vector for all).
func indexEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var inputs []string
		if err := json.Unmarshal(req.Input, &inputs); err != nil {
			http.Error(w, "expected array", http.StatusBadRequest)
			return
		}
		type item struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]item, 0, len(inputs))
		for i := range inputs {
			data = append(data, item{Object: "embedding", Index: i, Embedding: []float32{1, 0}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
}

func TestIndexTools(t *testing.T) {
	ts := indexEmbedServer(t)
	defer ts.Close()

	ws := t.TempDir()
	writeFile(t, ws, "pkg/tool.go", `package pkg

// validateToolInput checks tool arguments before dispatch.
func validateToolInput(name string) error {
	if name == "" {
		return validationError
	}
	return nil
}
`)
	idx, err := index.Open(ws, t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	reg := NewRegistry(ws, Options{Index: idx})

	// index_repo builds the index
	got := skillExec(t, reg, "index_repo", map[string]any{})
	if !strings.Contains(got, "indexed 1 files") {
		t.Errorf("index_repo = %q", got)
	}
	// incremental pass skips unchanged
	if got := skillExec(t, reg, "index_repo", map[string]any{}); !strings.Contains(got, "1 unchanged skipped") {
		t.Errorf("index_repo 2 = %q", got)
	}
	// index_search finds the right chunk
	got = skillExec(t, reg, "index_search", map[string]any{"query": "tool argument validation"})
	if !strings.Contains(got, "pkg/tool.go") || !strings.Contains(got, "validateToolInput") {
		t.Errorf("index_search = %q", got)
	}
	// validation: empty query, k cap
	if got := skillExec(t, reg, "index_search", map[string]any{"query": ""}); !strings.Contains(got, "validation-error") {
		t.Errorf("index_search empty query = %q", got)
	}
	if got := skillExec(t, reg, "index_search", map[string]any{"query": "x", "k": 99}); !strings.Contains(got, "validation-error") {
		t.Errorf("index_search k cap = %q", got)
	}
}

func TestIndexSearchSymbol(t *testing.T) {
	ts := indexEmbedServer(t)
	defer ts.Close()
	ws := t.TempDir()
	writeFile(t, ws, "pkg/tool.go", `package pkg

// validateToolInput checks tool arguments before dispatch.
func validateToolInput(name string) error {
	return nil
}
`)
	idx, err := index.Open(ws, t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(ws, Options{Index: idx})
	if got := skillExec(t, reg, "index_repo", map[string]any{}); !strings.Contains(got, "indexed 1 files") {
		t.Fatalf("index_repo = %q", got)
	}
	got := skillExec(t, reg, "index_search", map[string]any{"symbol": "validateToolInput"})
	if !strings.Contains(got, "pkg/tool.go:4") || !strings.Contains(got, "function") {
		t.Errorf("symbol lookup = %q", got)
	}
	// kind filter + no match
	if got := skillExec(t, reg, "index_search", map[string]any{"symbol": "validateToolInput", "type": "type"}); !strings.Contains(got, "no symbol") {
		t.Errorf("kind-filtered = %q", got)
	}
	// missing both args
	if got := skillExec(t, reg, "index_search", map[string]any{}); !strings.Contains(got, "validation-error") {
		t.Errorf("no args = %q", got)
	}
}
