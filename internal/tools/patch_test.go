package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yagent/internal/undo"
)

func TestFSPatchAppliesUnifiedDiff(t *testing.T) {
	ws, reg := fakeWorkspace(t)
	writeFile(t, ws, "a.go", "package pkg\n\nfunc old() int {\n    return 1\n}\n")
	writeFile(t, ws, "b.go", "package pkg\n\nfunc keep() string {\n    return \"x\"\n}\n")

	patch := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -3,3 +3,3 @@
 func old() int {
-    return 1
+    return 2
 }
`
	got := execTool(t, reg, "fs_patch", map[string]any{"patch": patch})
	if !strings.Contains(got, "patched 1 file") || !strings.Contains(got, "a.go") {
		t.Fatalf("fs_patch = %q", got)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "a.go"))
	if !strings.Contains(string(data), "return 2") || strings.Contains(string(data), "return 1") {
		t.Errorf("a.go not patched: %q", data)
	}
	// b.go untouched
	b, _ := os.ReadFile(filepath.Join(ws, "b.go"))
	if !strings.Contains(string(b), "return \"x\"") {
		t.Error("b.go changed unexpectedly")
	}
	// path traversal rejected
	got = execTool(t, reg, "fs_patch", map[string]any{"patch": "--- a/../evil\n+++ b/../evil\n@@ -1 +1 @@\n-x\n+y\n"})
	if !strings.Contains(got, "validation-error") {
		t.Errorf("traversal patch = %q", got)
	}
	// context mismatch fails cleanly
	got = execTool(t, reg, "fs_patch", map[string]any{"patch": `--- a/b.go
+++ b/b.go
@@ -1,2 +1,2 @@
 package WRONG
`})
	if !strings.Contains(got, "does not match") {
		t.Errorf("mismatch = %q", got)
	}
}

func TestFSPatchMultiFileAndUndo(t *testing.T) {
	ws := t.TempDir()
	ub := undo.New()
	reg := NewRegistry(ws, Options{Undo: ub, SkillsWriteApproval: true})
	writeFile(t, ws, "one.txt", "alpha\nbeta\ngamma\n")
	writeFile(t, ws, "two.txt", "one\ntwo\nthree\n")

	patch := `--- a/one.txt
+++ b/one.txt
@@ -2,1 +2,1 @@
-beta
+BETA
--- a/two.txt
+++ b/two.txt
@@ -1,3 +1,3 @@
-one
-two
-three
+ONE
+TWO
+THREE
`
	ub.StartTurn()
	got := execTool(t, reg, "fs_patch", map[string]any{"patch": patch})
	ub.EndTurn()
	if !strings.Contains(got, "2 file(s)") {
		t.Fatalf("fs_patch = %q", got)
	}
	// /undo reverts both
	if !ub.CanUndo() {
		t.Fatal("no turn to undo")
	}
	if _, err := ub.UndoLastTurn(); err != nil {
		t.Fatal(err)
	}
	one, _ := os.ReadFile(filepath.Join(ws, "one.txt"))
	if !strings.Contains(string(one), "beta") {
		t.Errorf("one.txt not reverted: %q", one)
	}
}
