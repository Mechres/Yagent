package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScratchTools(t *testing.T) {
	ws := t.TempDir()
	reg := NewRegistry(ws, Options{ReadOnly: true}) // subagent-style registry
	if _, ok := reg.Get("scratch_write"); !ok {
		t.Fatal("scratch_write must be available to read-only subagents")
	}
	if _, ok := reg.Get("fs_write"); ok {
		t.Fatal("fs_write must NOT be available to read-only subagents")
	}

	w := execTool(t, reg, "scratch_write", map[string]any{"path": "task-1/api.json", "content": `{"name":"svc"}`})
	if !strings.Contains(w, "saved") {
		t.Errorf("scratch_write = %q", w)
	}
	r := execTool(t, reg, "scratch_read", map[string]any{"path": "task-1/api.json"})
	if !strings.Contains(r, "svc") {
		t.Errorf("scratch_read = %q", r)
	}
	// missing note
	if r := execTool(t, reg, "scratch_read", map[string]any{"path": "nope"}); !strings.Contains(r, "no scratch note") {
		t.Errorf("scratch_read missing = %q", r)
	}
	// escape attempts rejected
	for _, evil := range []string{"../escape", "/abs", "a/../../b"} {
		if got := execTool(t, reg, "scratch_write", map[string]any{"path": evil, "content": "x"}); !strings.Contains(got, "validation-error") {
			t.Errorf("scratch escape %q = %q", evil, got)
		}
	}
	// writes confined to scratch dir
	if _, err := os.Stat(filepath.Join(ws, ".yagent", "scratch", "task-1", "api.json")); err != nil {
		t.Errorf("note not in scratch dir: %v", err)
	}
}
