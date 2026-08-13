// Package checkpoint snapshots a workspace before autonomous goal runs so a
// stray multi-round run can be reverted to its starting state. Snapshots live
// under <workspace>/.yagent/checkpoints/<name>/ and copy the tree minus .git
// and .yagent (the agent's own state).
package checkpoint

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const dirName = ".yagent/checkpoints"

// saveExcludes are workspace entries never copied into a snapshot.
var saveExcludes = map[string]bool{".git": true, ".yagent": true}

// Name is a snapshot identifier.
type Name struct{ Value string }

// GoalName is the fixed snapshot name used for pre-goal captures.
const GoalName = "goal"

// Save copies the workspace tree into <ws>/.yagent/checkpoints/<name>/
// (excluding .git and .yagent). An existing snapshot with the same name is
// replaced.
func Save(ws, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("invalid checkpoint name %q", name)
	}
	dst := filepath.Join(ws, dirName, name)
	if err := os.RemoveAll(dst); err != nil {
		return "", fmt.Errorf("clear old checkpoint: %w", err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", fmt.Errorf("checkpoint dir: %w", err)
	}
	src := filepath.Clean(ws)
	if err := copyTree(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// Restore reverts the workspace to a snapshot: the current tree (minus .git
// and .yagent) is removed and replaced with the snapshot's contents.
func Restore(ws, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	snap := filepath.Join(ws, dirName, name)
	fi, err := os.Stat(snap)
	if err != nil {
		return fmt.Errorf("checkpoint %q not found (run /checkpoint list)", name)
	}
	// A checkpoint is a directory; a file with the same name must not pass
	// (a stray file would be copied over the workspace).
	if !fi.IsDir() {
		return fmt.Errorf("checkpoint %q is not a directory", name)
	}
	// Remove everything except .git and .yagent.
	entries, err := os.ReadDir(ws)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if saveExcludes[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(ws, e.Name())); err != nil {
			return fmt.Errorf("remove %s: %w", e.Name(), err)
		}
	}
	return copyTree(snap, ws)
}

// List returns checkpoint names, newest first.
func List(ws string) []string {
	dir := filepath.Join(ws, dirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

// Delete removes a snapshot.
func Delete(ws, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(ws, dirName, name))
}

// validateName rejects checkpoint names that could escape the checkpoints dir
// (path separators and traversal), mirroring the guard Save has always had.
// Without it, Restore(ws, "../..") resolves to the workspace itself and wipes
// it, and Delete(ws, "../../../victim") removes a directory outside the
// workspace (adversarial-QA findings #13/#14, 2026-08-13).
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("invalid checkpoint name %q", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid checkpoint name %q (must not contain path separators)", name)
	}
	if filepath.Clean(name) != name || name == "." || name == ".." {
		return fmt.Errorf("invalid checkpoint name %q", name)
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		top := strings.SplitN(rel, string(filepath.Separator), 2)[0]
		if saveExcludes[top] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			// Skip symlinks: they may point outside the tree and a broken link
			// would break the restore walk.
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// FormatAge renders a snapshot's age for listings ("" when unavailable).
func FormatAge(ws, name string) string {
	fi, err := os.Stat(filepath.Join(ws, dirName, name))
	if err != nil {
		return ""
	}
	return time.Since(fi.ModTime()).Round(time.Minute).String()
}
