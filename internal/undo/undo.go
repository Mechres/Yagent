// Package undo provides an in-memory, per-turn file-write buffer so the user
// can revert a turn's fs_write/fs_edit changes with /undo. It is a safety net,
// not a full VCS: backups live only for the current session.
package undo

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Entry records one file write: its path and the content it replaced (nil
// means the file did not exist and should be deleted on undo).
type Entry struct {
	Path string
	Old  []byte
}

// Buffer accumulates write entries per turn and can revert the last turn.
type Buffer struct {
	mu      sync.Mutex
	turns   [][]Entry
	current []Entry
}

// New returns an empty buffer.
func New() *Buffer { return &Buffer{} }

// StartTurn begins a new turn bucket (call before running a user turn).
func (b *Buffer) StartTurn() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.current = nil
}

// EndTurn closes the current turn bucket (call after a user turn completes).
func (b *Buffer) EndTurn() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.current) > 0 {
		b.turns = append(b.turns, b.current)
	}
	b.current = nil
}

// Record logs the content a file write is about to replace.
func (b *Buffer) Record(path string, old []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.current = append(b.current, Entry{Path: path, Old: old})
}

// CanUndo reports whether there is a completed turn to revert.
func (b *Buffer) CanUndo() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.turns) > 0
}

// UndoLastTurn reverts the most recent completed turn's file writes in reverse
// order (entries whose file did not exist are deleted) and returns what was
// reverted.
func (b *Buffer) UndoLastTurn() ([]Entry, error) {
	b.mu.Lock()
	if len(b.turns) == 0 {
		b.mu.Unlock()
		return nil, fmt.Errorf("nothing to undo")
	}
	turn := b.turns[len(b.turns)-1]
	b.turns = b.turns[:len(b.turns)-1]
	b.mu.Unlock()

	for i := len(turn) - 1; i >= 0; i-- {
		e := turn[i]
		if e.Old == nil {
			_ = os.Remove(e.Path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(e.Path), 0o755); err != nil {
			return turn, err
		}
		if err := os.WriteFile(e.Path, e.Old, 0o644); err != nil {
			return turn, err
		}
	}
	return turn, nil
}
