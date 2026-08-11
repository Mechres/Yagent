package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mechres/Yagent/internal/index"
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

func TestCodeOutline(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "pkg/one.go", `package pkg

// Add returns a + b.
func Add(a, b int) int { return a + b }

type Store struct{ data []string }

func (s *Store) Get(i int) string { return s.data[i] }
`)
	writeFile(t, ws, "pkg/README.md", "not code\n")
	reg := NewRegistry(ws, Options{SkillsWriteApproval: true})
	got := execTool(t, reg, "code_outline", map[string]any{"path": "pkg"})
	if !strings.Contains(got, "Add") || !strings.Contains(got, "[function]") || !strings.Contains(got, "Store") {
		t.Errorf("code_outline = %q", got)
	}
	if strings.Contains(got, "README") {
		t.Error("non-source file included")
	}
	// single file
	got = execTool(t, reg, "code_outline", map[string]any{"path": "pkg/one.go"})
	if !strings.Contains(got, "Store") || !strings.Contains(got, "Get") {
		t.Errorf("single file outline = %q", got)
	}
	if got := execTool(t, reg, "code_outline", map[string]any{"path": ""}); !strings.Contains(got, "validation-error") {
		t.Errorf("empty path = %q", got)
	}
}

func TestSubagentParallel(t *testing.T) {
	var mu sync.Mutex
	active := 0
	max := 0
	tool := &subagentTool{ws: t.TempDir(), run: func(ctx context.Context, task, ws string, toolset []string) (string, error) {
		mu.Lock()
		active++
		if active > max {
			max = active
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return "SUMMARY " + task, nil
	}}
	res, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"tasks": []string{"a", "b", "c", "d"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "SUMMARY a") || !strings.Contains(res, "SUMMARY d") {
		t.Errorf("parallel result = %q", res)
	}
	if max < 2 {
		t.Errorf("subagents did not run in parallel (max concurrent = %d)", max)
	}
	// validation
	if _, err := tool.Execute(ctx(), argsJSON(t, map[string]any{})); err == nil {
		t.Error("empty args should fail")
	}
}

func TestSubagentToolSet(t *testing.T) {
	var mu sync.Mutex
	var gotTools []string
	tool := &subagentTool{ws: t.TempDir(), run: func(ctx context.Context, task, ws string, toolset []string) (string, error) {
		mu.Lock()
		gotTools = append(gotTools, toolset...)
		mu.Unlock()
		return "SUMMARY " + task, nil
	}}
	// single task + tool subset
	if _, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"task": "x", "tools": []string{"web_search", "web_fetch"}})); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotTools, []string{"web_search", "web_fetch"}) {
		t.Errorf("tool subset not passed through: %v", gotTools)
	}
	// parallel tasks share the subset
	gotTools = nil
	if _, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"tasks": []string{"a", "b"}, "tools": []string{"fs_read"}})); err != nil {
		t.Fatal(err)
	}
	for _, tk := range gotTools {
		if tk != "fs_read" {
			t.Errorf("parallel tool subset = %v", gotTools)
			break
		}
	}
	// no tools -> nil (full default set)
	gotTools = nil
	if _, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"task": "x"})); err != nil {
		t.Fatal(err)
	}
	if gotTools != nil {
		t.Errorf("empty tools should be nil, got %v", gotTools)
	}
}

func TestRegistryRestrict(t *testing.T) {
	reg := NewRegistry(t.TempDir(), Options{})
	full := reg.Names()
	if len(full) == 0 {
		t.Fatal("expected a non-empty tool registry")
	}
	sub, err := reg.Restrict([]string{"grep", "fs_read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Names()) != 2 {
		t.Errorf("restricted registry = %v", sub.Names())
	}
	if _, ok := sub.Get("grep"); !ok {
		t.Error("grep missing from restricted registry")
	}
	if _, ok := sub.Get("fs_write"); ok {
		t.Error("fs_write should not be in a read-only subagent subset")
	}
	// unknown tool -> validation error naming what IS available
	if _, err := reg.Restrict([]string{"nope"}); err == nil {
		t.Error("restricting to an unknown tool should fail")
	}
	// subagents build the child registry read-only, so destructive tools
	// can't be requested into a subset there
	ro := NewRegistry(t.TempDir(), Options{ReadOnly: true})
	if _, err := ro.Restrict([]string{"shell_exec"}); err == nil {
		t.Error("restricting a read-only registry to shell_exec should fail")
	}
}
