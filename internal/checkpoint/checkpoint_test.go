package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSaveRestoreRoundTrip(t *testing.T) {
	ws := t.TempDir()
	write(t, filepath.Join(ws, "a.txt"), "one")
	write(t, filepath.Join(ws, "sub", "b.txt"), "two")
	// excluded: .git and .yagent state
	write(t, filepath.Join(ws, ".git", "HEAD"), "ref")
	write(t, filepath.Join(ws, ".yagent", "config.yaml"), "keep")

	dir, err := Save(ws, "goal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("snapshot missing a.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Error(".git must not be snapshotted")
	}
	if _, err := os.Stat(filepath.Join(dir, ".yagent")); err == nil {
		t.Error(".yagent must not be snapshotted")
	}

	// mutate the workspace, then restore
	write(t, filepath.Join(ws, "a.txt"), "changed")
	write(t, filepath.Join(ws, "stray.txt"), "extra")
	if err := os.RemoveAll(filepath.Join(ws, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ws, "goal"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "a.txt"))
	if string(data) != "one" {
		t.Errorf("a.txt after restore = %q", data)
	}
	if _, err := os.Stat(filepath.Join(ws, "stray.txt")); err == nil {
		t.Error("stray file should be removed by restore")
	}
	if _, err := os.Stat(filepath.Join(ws, "sub", "b.txt")); err != nil {
		t.Errorf("nested file missing after restore: %v", err)
	}
	// .git/.yagent survive restore untouched
	if _, err := os.Stat(filepath.Join(ws, ".git", "HEAD")); err != nil {
		t.Error(".git should survive restore")
	}
	if _, err := os.Stat(filepath.Join(ws, ".yagent", "config.yaml")); err != nil {
		t.Error(".yagent should survive restore")
	}
}

func TestListAndDelete(t *testing.T) {
	ws := t.TempDir()
	write(t, filepath.Join(ws, "f"), "x")
	if _, err := Save(ws, "goal"); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(ws, "manual"); err != nil {
		t.Fatal(err)
	}
	names := List(ws)
	if len(names) != 2 {
		t.Fatalf("List = %v", names)
	}
	if err := Delete(ws, "manual"); err != nil {
		t.Fatal(err)
	}
	if names := List(ws); len(names) != 1 || names[0] != "goal" {
		t.Errorf("after delete = %v", names)
	}
	// restoring a missing checkpoint errors
	if err := Restore(ws, "nope"); err == nil {
		t.Error("restore of missing checkpoint should error")
	}
}

func TestPruneKeepsRecentNamed(t *testing.T) {
	// Retention: user-named checkpoints beyond the most recent `keep` are
	// pruned, while the fixed "goal" snapshot is always kept.
	ws := t.TempDir()
	write(t, filepath.Join(ws, "f"), "x")
	base := time.Now()
	for _, name := range []string{"goal", "one", "two", "three", "four"} {
		if _, err := Save(ws, name); err != nil {
			t.Fatal(err)
		}
		// age the named snapshots so mtime ordering is deterministic
		_ = os.Chtimes(filepath.Join(ws, dirName, name), base, base.Add(time.Duration(len(name))*time.Second))
	}
	pruned, err := Prune(ws, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 2 { // "one" and "two" are oldest named
		t.Errorf("pruned = %v, want 2 oldest", pruned)
	}
	names := List(ws)
	// goal always kept + the two newest named remain
	if len(names) != 3 {
		t.Errorf("after prune List = %v, want 3 (goal + 2 newest)", names)
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["goal"] {
		t.Error("goal snapshot must never be pruned")
	}
	for _, keep := range []string{"three", "four"} {
		if !found[keep] {
			t.Errorf("newest named %q was pruned (want kept)", keep)
		}
	}
}

func TestPruneNoCheckpoints(t *testing.T) {
	pruned, err := Prune(t.TempDir(), 5)
	if err != nil || len(pruned) != 0 {
		t.Errorf("Prune on empty ws = %v, %v; want 0, nil", pruned, err)
	}
}

func TestRestoreCleansStagingAndPreservesState(t *testing.T) {
	// Crash-safe Restore (2026-08-16): a successful restore must leave no
	// leftover staging dir and must preserve .git/.yagent (agent state).
	ws := t.TempDir()
	write(t, filepath.Join(ws, "a.txt"), "v1")
	write(t, filepath.Join(ws, ".git", "HEAD"), "ref")
	write(t, filepath.Join(ws, ".yagent", "config.yaml"), "keep")

	if _, err := Save(ws, "goal"); err != nil {
		t.Fatal(err)
	}
	// mutate, then restore back to the "goal" snapshot
	write(t, filepath.Join(ws, "a.txt"), "changed")
	if err := Restore(ws, "goal"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "a.txt"))
	if string(data) != "v1" {
		t.Errorf("a.txt after restore = %q; want v1", data)
	}
	if _, err := os.Lstat(filepath.Join(ws, dirName, ".restore-staging")); err == nil {
		t.Error(".restore-staging was left behind after a successful restore")
	}
	if _, err := os.Stat(filepath.Join(ws, ".git", "HEAD")); err != nil {
		t.Error(".git must survive restore")
	}
	if _, err := os.Stat(filepath.Join(ws, ".yagent", "config.yaml")); err != nil {
		t.Error(".yagent must survive restore")
	}
}

func TestRestoreLeavesWorkspaceIntactOnError(t *testing.T) {
	// A restore that cannot proceed (missing snapshot) must not touch the
	// live tree. This guards the crash-safety property: live entries are only
	// removed after a full copy of the snapshot is staged.
	ws := t.TempDir()
	write(t, filepath.Join(ws, "important.txt"), "do-not-lose")
	if err := Restore(ws, "ghost"); err == nil {
		t.Fatal("restore of a missing snapshot should error")
	}
	if data, err := os.ReadFile(filepath.Join(ws, "important.txt")); err != nil || string(data) != "do-not-lose" {
		t.Errorf("workspace was modified by a failed restore (data=%q err=%v)", data, err)
	}
	if _, err := os.Lstat(filepath.Join(ws, dirName, ".restore-staging")); err == nil {
		t.Error("staging dir leaked after a failed restore")
	}
}

func TestRestoreDeleteRejectTraversal(t *testing.T) {
	// Adversarial-QA findings #13/#14 (2026-08-13): a checkpoint name with
	// path separators/traversal must be rejected before any filesystem action.
	// Restore(ws, "../..") previously resolved to the workspace itself and
	// wiped it; Delete(ws, "../../../victim") removed a directory outside ws.
	ws := t.TempDir()
	write(t, filepath.Join(ws, "important.txt"), "keep me")
	write(t, filepath.Join(ws, ".yagent", "checkpoints", "goal", "snap.txt"), "snap")

	for _, bad := range []string{"../..", "a/../..", "..", ".", "a\\b", "x/y"} {
		if err := Restore(ws, bad); err == nil {
			t.Errorf("Restore(%q) accepted a traversal name", bad)
		}
		if err := Delete(ws, bad); err == nil {
			t.Errorf("Delete(%q) accepted a traversal name", bad)
		}
	}

	// The workspace must be untouched after all rejected calls.
	if _, err := os.Stat(filepath.Join(ws, "important.txt")); err != nil {
		t.Errorf("workspace file was affected: %v", err)
	}
}
