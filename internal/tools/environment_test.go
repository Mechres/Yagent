package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodeEnvironment(t *testing.T) {
	ws := t.TempDir()
	// a workspace with a cgo file — must be detected as a native binding
	writeFile(t, ws, "pkg/cgo.go", "package pkg\n\n// #cgo LDFLAGS: -lm\nimport \"C\"\n\nfunc X() {}\n")
	reg := NewRegistry(ws, Options{})

	res := execTool(t, reg, "code_environment", map[string]any{})
	if !strings.Contains(res, "toolchain:") {
		t.Errorf("missing toolchain section: %q", res)
	}
	// go is present on this build host
	if !strings.Contains(res, "go") {
		t.Errorf("go missing from toolchain: %q", res)
	}
	if !strings.Contains(res, "env flags:") {
		t.Errorf("missing env flags section: %q", res)
	}
	if !strings.Contains(res, "pkg/cgo.go") {
		t.Errorf("cgo file not detected as a native binding: %q", res)
	}
	// env: scanNativeBindings via the tool on a clean dir
	ws2 := t.TempDir()
	writeFile(t, ws2, "main.go", "package main\n\nfunc main() {}\n")
	reg2 := NewRegistry(ws2, Options{})
	res2 := execTool(t, reg2, "code_environment", map[string]any{})
	if !strings.Contains(res2, "none detected") {
		t.Errorf("clean workspace should report no native bindings: %q", res2)
	}
}

func TestScanNativeBindings(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "src"), 0o755)
	writeFile(t, ws, "src/a.go", "package x\n\n// #cgo CFLAGS: -O2\nimport \"C\"\n")
	writeFile(t, ws, "src/b.rs", "extern \"C\" { fn foo(); }\n")
	writeFile(t, ws, "src/c.txt", "plain text")
	hits, err := scanNativeBindings(ws)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(hits, " ")
	if !strings.Contains(joined, "a.go") || !strings.Contains(joined, "b.rs") {
		t.Errorf("native bindings = %q, want a.go and b.rs", joined)
	}
	if strings.Contains(joined, "c.txt") {
		t.Errorf("plain text file wrongly flagged: %q", joined)
	}
}
