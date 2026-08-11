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
