package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ctx() context.Context { return context.Background() }

func argsJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

func fakeWorkspace(t *testing.T) (ws string, reg *Registry) {
	t.Helper()
	ws = t.TempDir()
	reg = NewRegistry(ws)
	return ws, reg
}

func writeFile(t *testing.T, ws, path, content string) {
	t.Helper()
	full := filepath.Join(ws, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func execTool(t *testing.T, reg *Registry, name string, args any) string {
	t.Helper()
	tool, ok := reg.Get(name)
	if !ok {
		t.Fatalf("tool %q not registered", name)
	}
	res, err := tool.Execute(ctx(), argsJSON(t, args))
	if err != nil {
		// validation errors are expected in some tests; return them as text
		return "validation-error: " + err.Error()
	}
	return res
}

func TestFSRead(t *testing.T) {
	ws, reg := fakeWorkspace(t)
	writeFile(t, ws, "hello.txt", "one\ntwo\nthree\n")

	if got := execTool(t, reg, "fs_read", map[string]any{"path": "hello.txt"}); !strings.Contains(got, "1: one") || !strings.Contains(got, "3: three") {
		t.Errorf("fs_read = %q", got)
	}
	if got := execTool(t, reg, "fs_read", map[string]any{"path": "hello.txt", "offset": 1, "limit": 1}); !strings.Contains(got, "2: two") || strings.Contains(got, "1: one") {
		t.Errorf("fs_read offset/limit = %q", got)
	}
	if got := execTool(t, reg, "fs_read", map[string]any{"path": "missing.txt"}); !strings.Contains(got, "error:") {
		t.Errorf("fs_read missing = %q", got)
	}
}

func TestFSReadBinaryDetection(t *testing.T) {
	ws, reg := fakeWorkspace(t)
	writeFile(t, ws, "blob.bin", "a\x00b\x00c")
	if got := execTool(t, reg, "fs_read", map[string]any{"path": "blob.bin"}); !strings.Contains(got, "binary") {
		t.Errorf("fs_read binary = %q", got)
	}
}

func TestFSWrite(t *testing.T) {
	ws, reg := fakeWorkspace(t)
	if got := execTool(t, reg, "fs_write", map[string]any{"path": "sub/dir/a.txt", "content": "hello"}); !strings.Contains(got, "wrote sub/dir/a.txt") {
		t.Errorf("fs_write create = %q", got)
	}
	if got := execTool(t, reg, "fs_write", map[string]any{"path": "sub/dir/a.txt", "content": "hello2"}); !strings.Contains(got, "overwrote") {
		t.Errorf("fs_write overwrite = %q", got)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "sub/dir/a.txt"))
	if string(data) != "hello2" {
		t.Errorf("content = %q", data)
	}
}

func TestFSEdit(t *testing.T) {
	ws, reg := fakeWorkspace(t)
	writeFile(t, ws, "doc.txt", "alpha beta alpha\n")

	// 2 matches → precise error
	if got := execTool(t, reg, "fs_edit", map[string]any{"path": "doc.txt", "old_string": "alpha", "new_string": "x"}); !strings.Contains(got, "matches 2 times") {
		t.Errorf("fs_edit ambiguous = %q", got)
	}
	// 0 matches → precise error
	if got := execTool(t, reg, "fs_edit", map[string]any{"path": "doc.txt", "old_string": "gamma", "new_string": "x"}); !strings.Contains(got, "not found") {
		t.Errorf("fs_edit missing = %q", got)
	}
	// 1 match → applies and returns diff
	if got := execTool(t, reg, "fs_edit", map[string]any{"path": "doc.txt", "old_string": "beta", "new_string": "BETA"}); !strings.Contains(got, "-alpha beta alpha") || !strings.Contains(got, "+alpha BETA alpha") {
		t.Errorf("fs_edit ok = %q", got)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "doc.txt"))
	if string(data) != "alpha BETA alpha\n" {
		t.Errorf("content = %q", data)
	}
}

func TestGlob(t *testing.T) {
	ws, reg := fakeWorkspace(t)
	writeFile(t, ws, "a/main.go", "")
	writeFile(t, ws, "a/util_test.go", "")
	writeFile(t, ws, "b/deep/x.go", "")
	writeFile(t, ws, "b/readme.md", "")
	writeFile(t, ws, "README.md", "") // root-level: **/ must match it too

	got := execTool(t, reg, "glob", map[string]any{"pattern": "**/*.go"})
	for _, want := range []string{"a/main.go", "a/util_test.go", "b/deep/x.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("glob missing %s in %q", want, got)
		}
	}
	if strings.Contains(got, "readme.md") {
		t.Errorf("glob matched readme.md: %q", got)
	}
	// root-level match regression (real-model run exposed this)
	got = execTool(t, reg, "glob", map[string]any{"pattern": "**/README*"})
	if !strings.Contains(got, "README.md") {
		t.Errorf("glob **/README* missed root README.md: %q", got)
	}
}

func TestGrep(t *testing.T) {
	ws, reg := fakeWorkspace(t)
	writeFile(t, ws, "src/a.go", "func main() {\n// todo: fix\n}\n")
	writeFile(t, ws, "src/b.txt", "nothing here\n")

	got := execTool(t, reg, "grep", map[string]any{"pattern": "todo"})
	if !strings.Contains(got, "src/a.go:2") {
		t.Errorf("grep = %q", got)
	}
	if strings.Contains(got, "b.txt") {
		t.Errorf("grep matched wrong file: %q", got)
	}
	// include filter (basename glob)
	got = execTool(t, reg, "grep", map[string]any{"pattern": "func", "include": "*.go"})
	if !strings.Contains(got, "src/a.go:1") {
		t.Errorf("grep include = %q", got)
	}
	// include with **/ must also match root-level files (regression)
	writeFile(t, ws, "root.txt", "func root\n")
	got = execTool(t, reg, "grep", map[string]any{"pattern": "func", "include": "**/*.txt"})
	if !strings.Contains(got, "root.txt:1") {
		t.Errorf("grep include **/ missed root file = %q", got)
	}
	// invalid regex → validation error
	if got := execTool(t, reg, "grep", map[string]any{"pattern": "(["}); !strings.Contains(got, "validation-error") {
		t.Errorf("grep bad regex = %q", got)
	}
}

func TestWorkspaceScoping(t *testing.T) {
	ws, reg := fakeWorkspace(t)
	escape := filepath.Join(filepath.Dir(ws), "outside.txt")
	if err := os.WriteFile(escape, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := execTool(t, reg, "fs_read", map[string]any{"path": "../outside.txt"}); !strings.Contains(got, "escapes") {
		t.Errorf("fs_read escape = %q", got)
	}
	if got := execTool(t, reg, "fs_write", map[string]any{"path": "/tmp/evil.txt", "content": "x"}); !strings.Contains(got, "validation-error") {
		t.Errorf("fs_write absolute escape = %q", got)
	}
	if got := execTool(t, reg, "fs_read", map[string]any{"path": "../../etc/passwd"}); !strings.Contains(got, "escapes") {
		t.Errorf("fs_read deep escape = %q", got)
	}
}

func TestStrictArgValidation(t *testing.T) {
	ws, reg := fakeWorkspace(t)
	writeFile(t, ws, "doc.txt", "hi\n")
	// unknown field → validation error
	if got := execTool(t, reg, "fs_read", map[string]any{"path": "doc.txt", "bogus_field": 1}); !strings.Contains(got, "validation-error") {
		t.Errorf("fs_read unknown field = %q", got)
	}
	// missing required field
	if got := execTool(t, reg, "fs_read", map[string]any{}); !strings.Contains(got, "validation-error") {
		t.Errorf("fs_read missing path = %q", got)
	}
}

func TestRegistrySchemas(t *testing.T) {
	_, reg := fakeWorkspace(t)
	names := reg.Names()
	if len(names) != 9 {
		t.Fatalf("registry has %d tools: %v", len(names), names)
	}
	schemas := reg.Schemas()
	for i, s := range schemas {
		if s.Function.Name != names[i] {
			t.Errorf("schema %d name %q, want %q (sorted)", i, s.Function.Name, names[i])
		}
		if s.Function.Description == "" || s.Function.Parameters == nil {
			t.Errorf("schema %s missing description/parameters", s.Function.Name)
		}
	}
	if _, ok := reg.Get("nope"); ok {
		t.Error("unknown tool should not be found")
	}
}
