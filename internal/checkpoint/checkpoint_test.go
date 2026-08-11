package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
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
