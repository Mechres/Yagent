package tools

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestCodeReferences(t *testing.T) {
	ts := indexEmbedServer(t)
	defer ts.Close()
	ws := t.TempDir()
	writeFile(t, ws, "pkg/a.go", `package pkg

func caller() {
	helper(1)
	_ = helper
}
`)
	writeFile(t, ws, "pkg/b.go", `package pkg

func helper(x int) int { return x }
`)
	idx, err := index.Open(ws, t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(ws, Options{Index: idx})
	if got := skillExec(t, reg, "index_repo", map[string]any{}); !strings.Contains(got, "indexed 2 files") {
		t.Fatalf("index_repo = %q", got)
	}
	got := skillExec(t, reg, "code_references", map[string]any{"symbol": "helper"})
	if !strings.Contains(got, "pkg/a.go:4") {
		t.Errorf("code_references = %q", got)
	}
	// validation: missing symbol
	if got := skillExec(t, reg, "code_references", map[string]any{}); !strings.Contains(got, "validation-error") {
		t.Errorf("code_references no symbol = %q", got)
	}
	// no callers
	if got := skillExec(t, reg, "code_references", map[string]any{"symbol": "nope"}); !strings.Contains(got, "no call sites") {
		t.Errorf("code_references no match = %q", got)
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
	tool := &subagentTool{ws: t.TempDir(), run: func(ctx context.Context, task, ws string, toolset []string, role SubagentRole) (string, error) {
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
	tool := &subagentTool{ws: t.TempDir(), run: func(ctx context.Context, task, ws string, toolset []string, role SubagentRole) (string, error) {
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

func TestSubagentRolePreset(t *testing.T) {
	var mu sync.Mutex
	var gotTools []string
	var gotRole SubagentRole
	tool := &subagentTool{ws: t.TempDir(), run: func(ctx context.Context, task, ws string, toolset []string, role SubagentRole) (string, error) {
		mu.Lock()
		gotTools = append([]string(nil), toolset...)
		gotRole = role
		mu.Unlock()
		return "SUMMARY " + task, nil
	}}

	// a known role applies its default tool subset and is passed through
	if _, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"task": "audit", "role": "architect"})); err != nil {
		t.Fatal(err)
	}
	arch, ok := RoleByName("architect")
	if !ok {
		t.Fatal("architect role missing")
	}
	mu.Lock()
	gotTools1 := append([]string(nil), gotTools...)
	gotRole1 := gotRole
	mu.Unlock()
	if gotRole1.Name != "architect" || gotRole1.Prompt != arch.Prompt || gotRole1.Temperature != arch.Temperature {
		t.Errorf("role not passed through: %+v", gotRole1)
	}
	if !reflect.DeepEqual(gotTools1, arch.Tools) {
		t.Errorf("role default tools = %v, want %v", gotTools1, arch.Tools)
	}

	// an explicit tools slice overrides the role default
	if _, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"task": "x", "role": "auditor", "tools": []string{"grep"}})); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotTools2 := append([]string(nil), gotTools...)
	mu.Unlock()
	if !reflect.DeepEqual(gotTools2, []string{"grep"}) {
		t.Errorf("explicit tools should override role default: %v", gotTools2)
	}
}

func TestSubagentUnknownRole(t *testing.T) {
	tool := &subagentTool{ws: t.TempDir(), run: func(ctx context.Context, task, ws string, toolset []string, role SubagentRole) (string, error) {
		return "ok", nil
	}}
	_, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"task": "x", "role": "wizard"}))
	if err == nil || !strings.Contains(err.Error(), "unknown subagent role") {
		t.Errorf("unknown role error = %v", err)
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

func TestCompactLines(t *testing.T) {
	in := "error: build failed\n" + strings.Repeat("  FAIL: test_x\n", 5) + "ok\n"
	got := compactLines(in)
	if !strings.Contains(got, "[5×]") || strings.Contains(got, "FAIL: test_x\n  FAIL") {
		t.Errorf("compactLines = %q", got)
	}
	// blank-line runs collapse
	blanks := "a\n\n\n\n\n\nb\n"
	if got := compactLines(blanks); strings.Contains(got, "\n\n\n\n") {
		t.Errorf("blank run not collapsed: %q", got)
	}
	// single lines pass through untouched
	uniq := "one\ntwo\nthree\n"
	if got := compactLines(uniq); got != uniq {
		t.Errorf("unique lines altered: %q", got)
	}
}

func TestGroupErrorCascade(t *testing.T) {
	// A single root cause (undefined: fmt) producing a large cascade of
	// derived errors must collapse to a few signatures with a count.
	cascade := "go vet\n"
	for i := 1; i <= 40; i++ {
		cascade += fmt.Sprintf("internal/agent/agent.go:%d:13: undefined: fmt\n", i)
	}
	cascade += "internal/agent/loop.go:3:5: undefined: fmt\n"
	got := groupErrorCascade(cascade)
	if got == "" {
		t.Fatal("error cascade not grouped")
	}
	if strings.Contains(got, "agent.go:39:") && strings.Contains(got, "agent.go:1:") {
		t.Errorf("cascade kept too many instances: %q", got)
	}
	if !strings.Contains(got, "similar omitted") && !strings.Contains(got, "more error lines") {
		t.Errorf("cascade missing a fold count: %q", got)
	}
	// a single signature repeated: one representative + a count
	single := strings.Repeat("x.go:1:2: undefined: fmt\n", 10)
	got = groupErrorCascade(single)
	if strings.Count(got, "x.go:") != 1 {
		t.Errorf("repeated signature kept %d representatives: %q", strings.Count(got, "x.go:"), got)
	}
	// non-error output passes through untouched
	normal := "hello\nworld\n"
	if got := groupErrorCascade(normal); got != "" {
		t.Errorf("normal output grouped: %q", got)
	}
	// distinct root causes: top 3 kept, the rest folded
	var multi strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&multi, "pkg/a.go:%d:2: undefined: fmt\n", i)
	}
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&multi, "pkg/b.go:%d:3: undefined: os\n", i)
	}
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&multi, "pkg/c.go:%d:4: undefined: log\n", i)
	}
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&multi, "pkg/d.go:%d:5: undefined: net\n", i)
	}
	got = groupErrorCascade(multi.String())
	if !strings.Contains(got, "and 20 more error lines") {
		t.Errorf("4th signature not folded: %q", got)
	}
}

func TestCapResultGroupsCascade(t *testing.T) {
	// capResult applies the cascade group when output would otherwise be huge.
	var cascade strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&cascade, "pkg/f.go:%d:2: undefined: something\n", i)
	}
	out := capResult(cascade.String(), 4096)
	if len(out) > 4096 {
		t.Errorf("capped result too big: %d", len(out))
	}
	if !strings.Contains(out, "similar omitted") && !strings.Contains(out, "more error lines") {
		t.Errorf("capResult did not group: %q", out[:min(len(out), 120)])
	}
}
