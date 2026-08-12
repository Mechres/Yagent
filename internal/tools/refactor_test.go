package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/undo"
)

func TestRefactorRename(t *testing.T) {
	ws := t.TempDir()
	ub := undo.New()
	writeFile(t, ws, "a.go", "package pkg\nfunc helper() int { return helper() } // helper is the helper\n")
	writeFile(t, ws, "b.go", "package pkg\nvar x = helper()\n")
	writeFile(t, ws, "sub/c.go", "package pkg\nfunc helperWrapper() { helper() }\n")
	// must survive: helperX is a different identifier
	writeFile(t, ws, "d.go", "package pkg\nfunc helperX() {}\n")

	tool := &refactorTool{ws: ws, undo: ub}
	res, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"old_name": "helper", "new_name": "assist"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "renamed helper -> assist") {
		t.Errorf("result = %q", res)
	}

	content, _ := os.ReadFile(filepath.Join(ws, "a.go"))
	if !strings.Contains(string(content), "assist()") || strings.Contains(string(content), "helper") {
		t.Errorf("a.go = %q", content)
	}
	content, _ = os.ReadFile(filepath.Join(ws, "b.go"))
	if !strings.Contains(string(content), "assist()") {
		t.Errorf("b.go = %q", content)
	}
	content, _ = os.ReadFile(filepath.Join(ws, "sub/c.go"))
	if !strings.Contains(string(content), "assist()") {
		t.Errorf("sub/c.go = %q", content)
	}
	content, _ = os.ReadFile(filepath.Join(ws, "d.go"))
	if !strings.Contains(string(content), "helperX") {
		t.Errorf("d.go = %q (helperX must survive)", content)
	}

	// /undo reverts the whole rename (entries commit on EndTurn)
	ub.EndTurn()
	if !ub.CanUndo() {
		t.Fatal("nothing to undo")
	}
	if _, err := ub.UndoLastTurn(); err != nil {
		t.Fatal(err)
	}
	content, _ = os.ReadFile(filepath.Join(ws, "a.go"))
	if !strings.Contains(string(content), "helper()") {
		t.Errorf("a.go not restored: %q", content)
	}
}

func TestRefactorNotFoundAndValidation(t *testing.T) {
	tool := &refactorTool{ws: t.TempDir()}
	res, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"old_name": "zzz_nope", "new_name": "yyy"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "not found") {
		t.Errorf("result = %q", res)
	}
	if _, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"old_name": "", "new_name": "x"})); err == nil {
		t.Error("empty old_name should fail")
	}
	if _, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"old_name": "a b", "new_name": "c"})); err == nil {
		t.Error("non-identifier old_name should fail")
	}
	if _, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"old_name": "x", "new_name": "x"})); err == nil {
		t.Error("same names should fail")
	}
}

func TestRefactorSkipsBuildDirs(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "main.go", "package main\nfunc oldFn() {}\n")
	writeFile(t, ws, "vendor/dep/dep.go", "package dep\nfunc oldFn() {}\n")
	tool := &refactorTool{ws: ws}
	res, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"old_name": "oldFn", "new_name": "newFn"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res, "vendor") {
		t.Errorf("vendor dir must be skipped: %q", res)
	}
	content, _ := os.ReadFile(filepath.Join(ws, "vendor/dep/dep.go"))
	if !strings.Contains(string(content), "oldFn") {
		t.Error("vendor file was rewritten")
	}
}
