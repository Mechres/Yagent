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
//
// Crash-safety: the snapshot is copied into a staging area (under the
// excluded .yagent dir) *before* any live entry is removed. The live tree is
// only cleared once a complete copy of the snapshot exists in staging, so a
// failed or interrupted restore (disk full, permission error, snapshot
// corruption) can never leave the workspace wiped. On any error the staging
// area is removed and the live tree is returned untouched.
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
	// Stage the snapshot first. Staging lives at <ws>/.yagent/.restore-staging
	// (under .yagent but OUTSIDE the checkpoints/ dir), so it is excluded from
	// List/Prune and never surfaces as a checkpoint, and it survives the live
	// tree-removal loop below.
	staging := filepath.Join(ws, ".yagent", ".restore-staging")
	os.RemoveAll(staging)
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		return fmt.Errorf("checkpoint dir: %w", err)
	}
	if err := copyTree(snap, staging); err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("stage checkpoint %q: %w", name, err)
	}
	// Remove everything except .git and .yagent.
	entries, err := os.ReadDir(ws)
	if err != nil {
		os.RemoveAll(staging)
		return err
	}
	for _, e := range entries {
		if saveExcludes[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(ws, e.Name())); err != nil {
			os.RemoveAll(staging)
			return fmt.Errorf("remove %s: %w", e.Name(), err)
		}
	}
	// Move the staged snapshot into the workspace root.
	staged, err := os.ReadDir(staging)
	if err != nil {
		os.RemoveAll(staging)
		return err
	}
	for _, e := range staged {
		if err := os.Rename(filepath.Join(staging, e.Name()), filepath.Join(ws, e.Name())); err != nil {
			os.RemoveAll(staging)
			return fmt.Errorf("restore %s: %w", e.Name(), err)
		}
	}
	return os.RemoveAll(staging)
}

// List returns checkpoint names, newest first. Hidden entries (names
// starting with ".") are skipped — they are internal state (e.g. the
// .restore-staging dir used by Restore), never user snapshots.
func List(ws string) []string {
	dir := filepath.Join(ws, dirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
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

// Prune deletes user-named snapshots beyond the most recent `keep`, newest
// first by modification time. The fixed GoalName snapshot is always kept
// (goal mode overwrites it each round; it never accumulates). Returns the
// pruned names. keep <= 0 means keep all.
func Prune(ws string, keep int) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	dir := filepath.Join(ws, dirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // no checkpoints dir -> nothing to prune
	}
	var named []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() || e.Name() == GoalName {
			continue
		}
		named = append(named, e)
	}
	sort.Slice(named, func(i, j int) bool {
		// newest first by mtime
		fi, _ := named[i].Info()
		fj, _ := named[j].Info()
		return fi.ModTime().After(fj.ModTime())
	})
	var pruned []string
	for _, e := range named[keep:] {
		name := e.Name()
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return pruned, fmt.Errorf("prune checkpoint %s: %w", name, err)
		}
		pruned = append(pruned, name)
	}
	return pruned, nil
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
