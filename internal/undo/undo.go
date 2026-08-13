// Package undo provides an in-memory, per-turn file-write buffer so the user
// can revert a turn's fs_write/fs_edit changes with /undo. It is a safety net,
// not a full VCS: backups live only for the current session.
package undo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// Turns returns one summary line per completed turn (most recent first), for
// the /undo list command (proposal #6, 2026-08-13).
func (b *Buffer) Turns() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.turns))
	for i := len(b.turns) - 1; i >= 0; i-- {
		turn := b.turns[i]
		var paths []string
		for _, e := range turn {
			paths = append(paths, e.Path)
		}
		out = append(out, fmt.Sprintf("turn %d: %d file(s): %s", i+1, len(turn), strings.Join(paths, ", ")))
	}
	return out
}

// Count returns how many completed turns can be undone.
func (b *Buffer) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.turns)
}

// UndoN reverts the n most recent completed turns (all of them when n <= 0 or
// n >= len(turns)) and returns every reverted entry. Unlike UndoLastTurn it
// never leaves a partial state: entries are only popped after all writes
// succeed.
func (b *Buffer) UndoN(n int) ([]Entry, error) {
	b.mu.Lock()
	if len(b.turns) == 0 {
		b.mu.Unlock()
		return nil, fmt.Errorf("nothing to undo")
	}
	if n <= 0 || n > len(b.turns) {
		n = len(b.turns)
	}
	target := len(b.turns) - n
	revert := b.turns[target:] // all turns from target to end (oldest→newest)
	// Flatten oldest→newest so writes go back in reverse application order.
	var flat []Entry
	for _, turn := range revert {
		flat = append(flat, turn...)
	}
	b.mu.Unlock()

	// Apply in reverse of the original write order (newest write first).
	for i := len(flat) - 1; i >= 0; i-- {
		e := flat[i]
		if e.Old == nil {
			_ = os.Remove(e.Path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(e.Path), 0o755); err != nil {
			return flat, err
		}
		if err := os.WriteFile(e.Path, e.Old, 0o644); err != nil {
			return flat, err
		}
	}
	// All writes succeeded: now drop the reverted turns from the stack.
	b.mu.Lock()
	b.turns = b.turns[:target]
	b.mu.Unlock()
	return flat, nil
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
