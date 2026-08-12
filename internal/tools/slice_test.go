package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodeSlice(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.go", `package demo

// helper multiplies two ints.
func helper(x, y int) int {
    return x * y
}

func other() int { return 1 }
`)
	reg := NewRegistry(ws, Options{})

	res := execTool(t, reg, "code_slice", map[string]any{"path": "a.go", "symbol": "helper"})
	if !strings.Contains(res, "helper") || !strings.Contains(res, "x * y") {
		t.Errorf("code_slice = %q", res)
	}
	if strings.Contains(res, "other()") {
		t.Errorf("code_slice leaked sibling declarations: %q", res)
	}

	// unknown symbol
	if res := execTool(t, reg, "code_slice", map[string]any{"path": "a.go", "symbol": "nope"}); !strings.Contains(res, "not found") {
		t.Errorf("unknown symbol = %q", res)
	}
	// unsupported file
	writeFile(t, ws, "notes.md", "# hi")
	if res := execTool(t, reg, "code_slice", map[string]any{"path": "notes.md", "symbol": "x"}); !strings.Contains(res, "not found") {
		t.Errorf("unsupported file = %q", res)
	}
	// validation
	if res := execTool(t, reg, "code_slice", map[string]any{}); !strings.Contains(res, "validation-error") {
		t.Errorf("empty args = %q", res)
	}
}

func TestPreflightBlocksBrokenEdit(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.go", "package demo\n\nfunc f() int {\n    return 1\n}\n")
	reg := NewRegistry(ws, Options{})

	// an edit that breaks Go syntax is blocked BEFORE writing
	res := execTool(t, reg, "fs_edit", map[string]any{
		"path": "a.go", "old_string": "return 1", "new_string": "return 1 2",
	})
	if !strings.Contains(res, "syntax error") {
		t.Fatalf("broken edit not blocked: %q", res)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "a.go"))
	if strings.Contains(string(data), "return 1 2") {
		t.Errorf("broken edit was written to disk: %q", data)
	}

	// a valid edit still applies
	res2 := execTool(t, reg, "fs_edit", map[string]any{
		"path": "a.go", "old_string": "return 1", "new_string": "return 2",
	})
	if !strings.Contains(res2, "edited") {
		t.Errorf("valid edit blocked: %q", res2)
	}
	data, _ = os.ReadFile(filepath.Join(ws, "a.go"))
	if !strings.Contains(string(data), "return 2") {
		t.Errorf("valid edit not applied: %q", data)
	}

	// fs_write of broken Go is blocked and leaves no file
	res3 := execTool(t, reg, "fs_write", map[string]any{
		"path": "bad.go", "content": "package main\nfunc main( {\n",
	})
	if !strings.Contains(res3, "syntax error") {
		t.Fatalf("broken write not blocked: %q", res3)
	}
	if _, err := os.Stat(filepath.Join(ws, "bad.go")); !os.IsNotExist(err) {
		t.Error("broken write left a file behind")
	}

	// non-source files are never pre-flighted
	res4 := execTool(t, reg, "fs_write", map[string]any{"path": "notes.md", "content": "# anything ` goes"})
	if !strings.Contains(res4, "wrote") {
		t.Errorf("markdown write blocked: %q", res4)
	}
}
