package refs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveFileRangeFolderAndGuards(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src", "main.go"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("SECRET=x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := Resolve(ws, "review @file:src/main.go:2-3 and @folder:src and @file:.env and @file:../outside")
	if !strings.Contains(out, "two\nthree") || !strings.Contains(out, "main.go") {
		t.Fatalf("resolved context = %q", out)
	}
	if strings.Contains(out, "SECRET=x") || !strings.Contains(out, "sensitive path blocked") || !strings.Contains(out, "path escapes workspace") {
		t.Fatalf("unsafe references not blocked: %q", out)
	}
}

func TestResolveWithoutReferencesIsEmpty(t *testing.T) {
	if got := Resolve(t.TempDir(), "ordinary question"); got != "" {
		t.Fatalf("Resolve = %q", got)
	}
}
