package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSliceSymbol(t *testing.T) {
	ws := t.TempDir()
	rel := "a.go"
	content := `package demo

// helper doubles x.
func helper(x int) int {
    return x * 2
}

func other() int { return 1 }
`
	if err := os.WriteFile(filepath.Join(ws, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok, err := SliceSymbol(ws, rel, "helper")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !strings.Contains(got, "doubles x") || !strings.Contains(got, "x * 2") || strings.Contains(got, "other") {
		t.Errorf("SliceSymbol = %q (%v)", got, ok)
	}
	if _, ok, _ := SliceSymbol(ws, rel, "nope"); ok {
		t.Error("unknown symbol should not resolve")
	}
}

func TestSyntaxErrors(t *testing.T) {
	clean := `package demo

func f() int {
    return 1
}
`
	if errs := SyntaxErrors("a.go", clean); len(errs) != 0 {
		t.Errorf("clean file has errors: %+v", errs)
	}
	broken := `package demo

func f() int {
    return 1 2
}
`
	errs := SyntaxErrors("a.go", broken)
	if len(errs) == 0 {
		t.Fatal("broken file reported no syntax errors")
	}
	if errs[0].Line == 0 || errs[0].Col == 0 {
		t.Errorf("error position missing: %+v", errs[0])
	}
	// unsupported extension -> nil (no false positives)
	if errs := SyntaxErrors("notes.md", "anything ` here"); len(errs) != 0 {
		t.Errorf("markdown should not be parsed: %+v", errs)
	}
}
