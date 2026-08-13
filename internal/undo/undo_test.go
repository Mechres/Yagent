package undo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUndoLastTurn(t *testing.T) {
	b := New()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.StartTurn()
	b.Record(path, []byte("old"))
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.EndTurn()

	if !b.CanUndo() {
		t.Fatal("CanUndo should be true after a turn")
	}
	entries, err := b.UndoLastTurn()
	if err != nil || len(entries) != 1 {
		t.Fatalf("UndoLastTurn = %v / %v", entries, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "old" {
		t.Errorf("file = %q, want old", data)
	}
	if _, err := b.UndoLastTurn(); err == nil {
		t.Error("second undo should fail (nothing to undo)")
	}
}

func TestUndoDeletesCreatedFile(t *testing.T) {
	b := New()
	dir := t.TempDir()
	path := filepath.Join(dir, "created.txt")
	b.StartTurn()
	b.Record(path, nil) // did not exist
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.EndTurn()
	if _, err := b.UndoLastTurn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("undo did not delete the created file")
	}
}

func TestUndoListAndUndoN(t *testing.T) {
	b := New()
	dir := t.TempDir()
	// turn 1: write a.txt old -> new1
	a := filepath.Join(dir, "a.txt")
	os.WriteFile(a, []byte("v0"), 0o644)
	b.StartTurn()
	b.Record(a, []byte("v0"))
	os.WriteFile(a, []byte("v1"), 0o644)
	b.EndTurn()
	// turn 2: write a.txt v1 -> v2 and create b.txt
	bb := filepath.Join(dir, "b.txt")
	b.StartTurn()
	b.Record(a, []byte("v1"))
	os.WriteFile(a, []byte("v2"), 0o644)
	b.Record(bb, nil)
	os.WriteFile(bb, []byte("new"), 0o644)
	b.EndTurn()

	// /undo list summary
	turns := b.Turns()
	if len(turns) != 2 {
		t.Fatalf("Turns = %v, want 2", turns)
	}
	if b.Count() != 2 {
		t.Errorf("Count = %d, want 2", b.Count())
	}

	// /undo 1 reverts only the most recent turn -> a.txt=v1, b.txt gone
	if _, err := b.UndoN(1); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(a)
	if string(data) != "v1" {
		t.Errorf("a.txt after undo 1 = %q, want v1", data)
	}
	if _, err := os.Stat(bb); !os.IsNotExist(err) {
		t.Error("b.txt should be deleted after undo 1")
	}
	if b.Count() != 1 {
		t.Errorf("Count after undo 1 = %d, want 1", b.Count())
	}

	// /undo 1 again reverts turn 1 -> a.txt=v0
	if _, err := b.UndoN(1); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(a)
	if string(data) != "v0" {
		t.Errorf("a.txt after undo 1 (turn 1) = %q, want v0", data)
	}
	if b.Count() != 0 {
		t.Errorf("Count after second undo = %d, want 0", b.Count())
	}
	if _, err := b.UndoN(1); err == nil {
		t.Error("undo with nothing left should error")
	}
}

func TestUndoNReversesMultiTurnOrder(t *testing.T) {
	b := New()
	dir := t.TempDir()
	p := filepath.Join(dir, "x.txt")
	// three turns, each appending a version marker
	for i, v := range []string{"v1", "v2", "v3"} {
		prev := ""
		if i > 0 {
			prev = []string{"v1", "v2", "v3"}[i-1]
		}
		os.WriteFile(p, []byte(prev), 0o644)
		b.StartTurn()
		b.Record(p, []byte(prev))
		os.WriteFile(p, []byte(v), 0o644)
		b.EndTurn()
	}
	// /undo 3 (or /undo 99) reverts everything back to the state before turn 1
	if _, err := b.UndoN(99); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "" {
		t.Errorf("x.txt after undo all = %q, want empty (v0)", data)
	}
	if b.Count() != 0 {
		t.Errorf("Count = %d, want 0", b.Count())
	}
}
