package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/undo"
)

func TestFSPatchRejectsOutOfRangeHunk(t *testing.T) {
	// Adversarial-QA finding #1 (2026-08-13): a hunk whose start line exceeds
	// the file length with only additions previously panicked in applyHunks
	// ("slice bounds out of range"). It must now return a structured error.
	ws := t.TempDir()
	writeFile(t, ws, "a.txt", "line1\nline2\nline3\n")
	reg := NewRegistry(ws, Options{})
	got := execTool(t, reg, "fs_patch", map[string]any{
		"patch": "--- a/a.txt\n+++ b/a.txt\n@@ -9999,0 +1,1 @@\n+injected\n",
	})
	if strings.Contains(got, "patched") {
		t.Fatalf("out-of-range hunk was applied: %q", got)
	}
	if !strings.Contains(got, "9999") {
		t.Errorf("error should mention the offending hunk start: %q", got)
	}
	// file untouched
	data, _ := os.ReadFile(filepath.Join(ws, "a.txt"))
	if !strings.Contains(string(data), "line3") || strings.Contains(string(data), "injected") {
		t.Errorf("file was modified by a bad hunk: %q", data)
	}
}

func TestFSPatchAtomicPreflightAllBeforeWrite(t *testing.T) {
	// Deterministic version of the atomicity guarantee: run fs_patch over two
	// Go files where the second has a preflight-blocked change (introduces a
	// syntax error via a bad edit). File A must NOT be modified.
	ws := t.TempDir()
	writeFile(t, ws, "a.go", "package pkg\n\nfunc A() int {\n    return 1\n}\n")
	writeFile(t, ws, "b.go", "package pkg\n\nfunc B() int {\n    return 2\n}\n")

	// a.go: valid change (adds a comment). b.go: invalid (drops a brace, which
	// tree-sitter flags as a syntax error).
	diff := `--- a/a.go
+++ b/a.go
@@ -1,2 +1,3 @@
 package pkg
+
+// updated
--- a/b.go
+++ b/b.go
@@ -3,3 +3,2 @@
 func B() int {
-    return 2
 }
`
	reg := NewRegistry(ws, Options{})
	got := execTool(t, reg, "fs_patch", map[string]any{"patch": diff})
	if strings.Contains(got, "patched") {
		t.Fatalf("patch with a bad file was reported as applied: %q", got)
	}
	// atomicity: a.go must be UNCHANGED (the whole patch failed)
	dataA, _ := os.ReadFile(filepath.Join(ws, "a.go"))
	if strings.Contains(string(dataA), "// updated") {
		t.Errorf("a.go was modified even though b.go failed preflight — non-atomic patch:\n%s", dataA)
	}
	// b.go unchanged too
	dataB, _ := os.ReadFile(filepath.Join(ws, "b.go"))
	if !strings.Contains(string(dataB), "return 2") {
		t.Errorf("b.go was modified: %s", dataB)
	}
	// the error names the failing file
	if !strings.Contains(got, "b.go") {
		t.Errorf("error should name the failing file: %q", got)
	}
}

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

func TestRebuildPatchFiltersHunks(t *testing.T) {
	patch := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,3 +1,3 @@
 func old() int {
-    return 1
+    return 2
 }
@@ -10,1 +10,1 @@
-func keep() {}
+func keep2() {}
`
	hunks, err := PatchHunks(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 2 {
		t.Fatalf("hunks = %d", len(hunks))
	}
	// keep only the first hunk
	filtered, err := RebuildPatch(patch, []bool{true, false})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(filtered, "keep2") {
		t.Errorf("skipped hunk leaked into filtered patch: %q", filtered)
	}
	if !strings.Contains(filtered, "return 2") {
		t.Errorf("kept hunk missing: %q", filtered)
	}
	// keep only the second hunk: its oldStart must shift (-3) to match the file
	filtered2, err := RebuildPatch(patch, []bool{false, true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(filtered2, "return 1") {
		t.Errorf("first hunk leaked: %q", filtered2)
	}
	if !strings.Contains(filtered2, "@@ -10,1 +10,1 @@") {
		t.Errorf("kept hunk header wrong: %q", filtered2)
	}
}
