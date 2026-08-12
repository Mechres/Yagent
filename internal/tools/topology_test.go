package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodeTopologyTool(t *testing.T) {
	ws := t.TempDir()
	writeTopoFile := func(rel, content string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTopoFile("go.mod", "module example.com/acme\n\ngo 1.22\n")
	writeTopoFile("internal/agent/agent.go", "package agent\n\nimport \"example.com/acme/internal/llm\"\n\nfunc New() {}\n")
	writeTopoFile("internal/llm/llm.go", "package llm\n\nfunc X() {}\n")

	reg := NewRegistry(ws, Options{})
	res := execTool(t, reg, "code_topology", map[string]any{})
	if !strings.Contains(res, "module example.com/acme") {
		t.Errorf("module missing: %q", res)
	}
	if !strings.Contains(res, "internal/agent -> internal/llm") {
		t.Errorf("import edge missing: %q", res)
	}
	if strings.Contains(res, "validation-error") {
		t.Errorf("unexpected validation error: %q", res)
	}
}
