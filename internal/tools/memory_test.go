package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/memory"
)

// embedServer is a deterministic /v1/embeddings fake: "tab" → (0,1), else (1,0).
func embedServer(t *testing.T) *httptest.Server {
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
			var one string
			if err2 := json.Unmarshal(req.Input, &one); err2 != nil {
				http.Error(w, "bad input", http.StatusBadRequest)
				return
			}
			inputs = []string{one}
		}
		type item struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]item, 0, len(inputs))
		for i, text := range inputs {
			vec := []float32{1, 0}
			if strings.Contains(text, "tab") {
				vec = []float32{0, 1}
			}
			data = append(data, item{Object: "embedding", Index: i, Embedding: vec})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
}

func TestMemoryTools(t *testing.T) {
	ts := embedServer(t)
	defer ts.Close()

	ws := t.TempDir()
	vs, err := memory.OpenVectorStore(ws, ts.URL, "test-embed")
	if err != nil {
		t.Fatalf("OpenVectorStore: %v", err)
	}
	defer vs.Close()
	reg := NewRegistry(ws, Options{Vectors: vs, SessionID: "sess-1", SkillsWriteApproval: true})

	// save
	if got := execTool(t, reg, "memory_save", map[string]any{"text": "user prefers tabs over spaces"}); !strings.Contains(got, "remembered") {
		t.Errorf("memory_save = %q", got)
	}
	// validation: missing text
	if got := execTool(t, reg, "memory_save", map[string]any{}); !strings.Contains(got, "validation-error") {
		t.Errorf("memory_save no text = %q", got)
	}
	// search recalls it
	if got := execTool(t, reg, "memory_search", map[string]any{"query": "what about tabs?"}); !strings.Contains(got, "tabs") {
		t.Errorf("memory_search = %q", got)
	}
	// search validation: k cap
	if got := execTool(t, reg, "memory_search", map[string]any{"query": "x", "k": 99}); !strings.Contains(got, "validation-error") {
		t.Errorf("memory_search k cap = %q", got)
	}
	// unknown query → no memories
	if got := execTool(t, reg, "memory_search", map[string]any{"query": "deployment stuff"}); !strings.Contains(got, "no memories found") {
		t.Errorf("memory_search miss = %q", got)
	}
	// metadata: session_id recorded
	mem, err := vs.Search(context.Background(), "what about tabs?", 5)
	if err != nil || len(mem) == 0 {
		t.Fatalf("search: %v", err)
	}
	if mem[0].SessionID != "sess-1" || mem[0].Source != "tool" {
		t.Errorf("memory metadata = %+v", mem[0])
	}
}

func TestMemoryToolsUnconfigured(t *testing.T) {
	reg := NewRegistry(t.TempDir(), Options{SkillsWriteApproval: true})
	if got := execTool(t, reg, "memory_save", map[string]any{"text": "x"}); !strings.Contains(got, "not configured") {
		t.Errorf("memory_save unconfigured = %q", got)
	}
	if got := execTool(t, reg, "memory_search", map[string]any{"query": "x"}); !strings.Contains(got, "not configured") {
		t.Errorf("memory_search unconfigured = %q", got)
	}
}
