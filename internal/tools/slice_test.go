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

func TestPreflightBlocksExportedDeletion(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "api.go", `package demo

// ExportedFunc is public API.
func ExportedFunc() int {
    return 42
}

func helper() int { return 1 }
`)
	reg := NewRegistry(ws, Options{})

	// deleting an exported function in a targeted edit is blocked
	res := execTool(t, reg, "fs_edit", map[string]any{
		"path": "api.go", "old_string": "func ExportedFunc() int {\n    return 42\n}\n\n", "new_string": "",
	})
	if !strings.Contains(res, "exported symbol") {
		t.Fatalf("exported deletion not blocked: %q", res)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "api.go"))
	if strings.Contains(string(data), "return 42") == false {
		t.Errorf("exported symbol removal was written to disk: %q", data)
	}
	if !strings.Contains(string(data), "ExportedFunc") {
		t.Errorf("ExportedFunc was removed despite the block: %q", data)
	}

	// an unexported (lowercase) function may be removed in the same file
	res2 := execTool(t, reg, "fs_edit", map[string]any{
		"path": "api.go", "old_string": "func helper() int { return 1 }\n", "new_string": "",
	})
	if !strings.Contains(res2, "edited") {
		t.Errorf("unexported deletion blocked: %q", res2)
	}

	// renaming a public function removes the old exported symbol — blocked in
	// a targeted edit (use fs_refactor for renames)
	writeFile(t, ws, "b.go", "package demo\n\nfunc Original() int { return 1 }\n")
	res3 := execTool(t, reg, "fs_edit", map[string]any{
		"path": "b.go", "old_string": "func Original()", "new_string": "func Renamed()",
	})
	if !strings.Contains(res3, "exported symbol") {
		t.Errorf("rename of exported symbol not blocked: %q", res3)
	}

	// fs_write (full rewrite) is NOT subject to the symbol-delta guardrail —
	// the model may intentionally replace the whole file.
	res4 := execTool(t, reg, "fs_write", map[string]any{
		"path": "c.go", "content": "package demo\n\nfunc OnlyOne() int { return 1 }\n",
	})
	if !strings.Contains(res4, "wrote") {
		t.Errorf("fs_write blocked: %q", res4)
	}
}

func TestEditWhitespaceNormalizedMatch(t *testing.T) {
	ws := t.TempDir()
	// file uses TAB indentation
	writeFile(t, ws, "tabs.go", "package demo\n\nfunc f() int {\n\treturn 1\n}\n")
	reg := NewRegistry(ws, Options{})

	// model emits SPACE indentation for the same block
	res := execTool(t, reg, "fs_edit", map[string]any{
		"path": "tabs.go", "old_string": "func f() int {\n    return 1\n}", "new_string": "func f() int {\n    return 2\n}",
	})
	if !strings.Contains(res, "auto-aligned whitespace") {
		t.Fatalf("whitespace fallback not applied: %q", res)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "tabs.go"))
	if !strings.Contains(string(data), "return 2") {
		t.Errorf("edit not applied: %q", data)
	}
	// the on-disk indentation must remain tabs (only the value changed)
	if !strings.Contains(string(data), "\treturn 2") {
		t.Errorf("on-disk indentation not preserved: %q", data)
	}

	// an ambiguous normalized match must NOT auto-apply (two identical bodies)
	writeFile(t, ws, "dup.go", "package demo\n\nfunc a() int {\n\treturn 1\n}\n\nfunc b() int {\n\treturn 1\n}\n")
	res2 := execTool(t, reg, "fs_edit", map[string]any{
		"path": "dup.go", "old_string": "func _() int {\n    return 1\n}", "new_string": "func _() int {\n    return 2\n}",
	})
	if !strings.Contains(res2, "not found") {
		t.Errorf("ambiguous whitespace match auto-applied: %q", res2)
	}
}

func TestPreflightStructuredFiles(t *testing.T) {
	ws := t.TempDir()
	reg := NewRegistry(ws, Options{})

	// a malformed YAML write is blocked and never lands on disk
	res := execTool(t, reg, "fs_write", map[string]any{
		"path": "cfg.yaml", "content": "server_url: http://x\nskills:\n  write_approval: [unclosed\n",
	})
	if !strings.Contains(res, "YAML") {
		t.Fatalf("bad YAML not blocked: %q", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "cfg.yaml")); !os.IsNotExist(err) {
		t.Error("bad YAML file was written")
	}

	// a malformed JSON edit is blocked
	writeFile(t, ws, "data.json", "{\"a\": 1}")
	res2 := execTool(t, reg, "fs_edit", map[string]any{
		"path": "data.json", "old_string": "{\"a\": 1}", "new_string": "{\"a\": }",
	})
	if !strings.Contains(res2, "JSON") {
		t.Fatalf("bad JSON edit not blocked: %q", res2)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "data.json"))
	if !strings.Contains(string(data), "{\"a\": 1}") {
		t.Errorf("bad JSON edit was written: %q", data)
	}

	// valid YAML still writes
	res3 := execTool(t, reg, "fs_write", map[string]any{
		"path": "ok.yaml", "content": "a: 1\nb:\n  - x\n",
	})
	if !strings.Contains(res3, "wrote") {
		t.Errorf("valid YAML blocked: %q", res3)
	}
}
