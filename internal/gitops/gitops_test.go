package gitops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupRepo initializes a git repo with identity and a committed baseline file.
func setupRepo(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	must(t, ws, "init", "-q")
	must(t, ws, "config", "user.name", "Test")
	must(t, ws, "config", "user.email", "test@example.com")
	write(t, ws, "base.txt", "base\n")
	must(t, ws, "add", "base.txt")
	must(t, ws, "commit", "-q", "-m", "baseline")
	return ws
}

func write(t *testing.T, ws, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func must(t *testing.T, ws string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = ws
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestIsRepoAndCommitDirty(t *testing.T) {
	ws := setupRepo(t)
	if !IsRepo(ws) {
		t.Fatal("IsRepo false in a repo")
	}
	if IsRepo(t.TempDir()) {
		t.Fatal("IsRepo true outside a repo")
	}
	// dirty state: modify base + add a new file
	write(t, ws, "base.txt", "base\nchanged\n")
	write(t, ws, "new.txt", "new\n")
	if !Dirty(ws) {
		t.Fatal("Dirty false with uncommitted changes")
	}
	// CommitDirty snapshots it
	hash, err := CommitDirty(ws, "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Fatal("empty commit hash")
	}
	if Dirty(ws) {
		t.Fatal("tree still dirty after CommitDirty")
	}
	// second call with nothing dirty -> "" and no error
	hash2, err := CommitDirty(ws, "again")
	if err != nil || hash2 != "" {
		t.Fatalf("CommitDirty on clean tree = %q, %v; want empty", hash2, err)
	}
}

func TestAgentCommitAndRevertN(t *testing.T) {
	ws := setupRepo(t)
	// agent makes a change
	write(t, ws, "new.txt", "agent work\n")
	if _, err := AgentCommit(ws, "add feature"); err != nil {
		t.Fatal(err)
	}
	// a second agent turn
	write(t, ws, "new.txt", "agent work 2\n")
	if _, err := AgentCommit(ws, "fix"); err != nil {
		t.Fatal(err)
	}

	commits, err := AgentCommits(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 {
		t.Fatalf("agent commits = %d, want 2 (%+v)", len(commits), commits)
	}
	if !strings.Contains(commits[0].Subject, "fix") {
		t.Errorf("newest commit subject = %q", commits[0].Subject)
	}

	// revert the last agent commit -> new.txt back to "agent work"
	reverted, err := RevertN(ws, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(reverted) != 1 || !strings.Contains(reverted[0], "fix") {
		t.Errorf("reverted = %v", reverted)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "new.txt"))
	if string(data) != "agent work\n" {
		t.Errorf("after revert new.txt = %q, want 'agent work\\n'", data)
	}
	// the revert is itself an agent commit; the reverted one stays in history
	// (git revert adds a commit rather than rewriting), so the log now shows
	// add-feature + fix + undo = 3 agent commits.
	commits, _ = AgentCommits(ws)
	if len(commits) != 3 {
		t.Errorf("after revert commits = %d, want 3 (add-feature + fix + undo)", len(commits))
	}
}

func TestRevertNRespectsUserCommitsBetween(t *testing.T) {
	ws := setupRepo(t)
	write(t, ws, "a.txt", "agent a\n")
	if _, err := AgentCommit(ws, "turn a"); err != nil {
		t.Fatal(err)
	}
	// user commits something in between (not agent-marked)
	write(t, ws, "user.txt", "user work\n")
	must(t, ws, "add", "user.txt")
	must(t, ws, "commit", "-q", "-m", "user stuff")
	write(t, ws, "b.txt", "agent b\n")
	if _, err := AgentCommit(ws, "turn b"); err != nil {
		t.Fatal(err)
	}

	// revert only agent commits (both), skipping the user commit
	reverted, err := RevertN(ws, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(reverted) != 2 {
		t.Errorf("reverted %d, want 2 (%v)", len(reverted), reverted)
	}
	// user work preserved
	data, _ := os.ReadFile(filepath.Join(ws, "user.txt"))
	if string(data) != "user work\n" {
		t.Errorf("user.txt clobbered: %q", data)
	}
	// agent files reverted to absent
	if _, err := os.Stat(filepath.Join(ws, "a.txt")); err == nil {
		t.Error("a.txt should be gone after revert")
	}
}

func TestNoIdentitySkipsCommit(t *testing.T) {
	// Isolate from any global git identity so the guard actually fires.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	ws := t.TempDir()
	must(t, ws, "init", "-q")
	write(t, ws, "x.txt", "x\n")
	if _, err := AgentCommit(ws, "no identity"); err == nil {
		t.Fatal("expected error when git identity is unset")
	}
}

func TestChangedFiles(t *testing.T) {
	ws := setupRepo(t)
	write(t, ws, "base.txt", "changed\n")
	write(t, ws, "untracked.txt", "new\n")
	files := ChangedFiles(ws)
	joined := strings.Join(files, " ")
	if !strings.Contains(joined, "base.txt") || !strings.Contains(joined, "untracked.txt") {
		t.Errorf("ChangedFiles = %v", files)
	}
}

func TestHeadAndDiffSince(t *testing.T) {
	ws := setupRepo(t)
	baseline := Head(ws)
	if baseline == "" {
		t.Fatal("empty HEAD in a repo")
	}
	// no changes yet -> empty diff/stat
	if stat := DiffStat(ws, baseline); stat != "" {
		t.Errorf("DiffStat on clean = %q", stat)
	}
	// make a change and commit it as agent work
	write(t, ws, "base.txt", "base\n+ new line\n")
	if _, err := AgentCommit(ws, "edit base"); err != nil {
		t.Fatal(err)
	}
	stat := DiffStat(ws, baseline)
	if !strings.Contains(stat, "base.txt") {
		t.Errorf("DiffStat missing base.txt: %q", stat)
	}
	diff := DiffSince(ws, baseline)
	if !strings.Contains(diff, "+ new line") {
		t.Errorf("DiffSince missing the added line: %q", diff)
	}
}
