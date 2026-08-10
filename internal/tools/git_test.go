package tools

import (
	"os/exec"
	"strings"
	"testing"
)

func gitCmd(t *testing.T, ws string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = ws
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func initGitRepo(t *testing.T) (ws string, reg *Registry) {
	t.Helper()
	ws, reg = fakeWorkspace(t)
	gitCmd(t, ws, "init", "-q")
	gitCmd(t, ws, "config", "user.email", "test@example.com")
	gitCmd(t, ws, "config", "user.name", "Test")
	writeFile(t, ws, "app.go", "package main\n")
	gitCmd(t, ws, "add", "app.go")
	gitCmd(t, ws, "commit", "-q", "-m", "initial")
	return ws, reg
}

func TestGitStatus(t *testing.T) {
	ws, reg := initGitRepo(t)

	if got := execTool(t, reg, "git_status", map[string]any{}); !strings.Contains(got, "working tree clean") {
		t.Errorf("git_status clean = %q", got)
	}
	writeFile(t, ws, "app.go", "package main\n// change\n")
	got := execTool(t, reg, "git_status", map[string]any{})
	if !strings.Contains(got, " M app.go") {
		t.Errorf("git_status dirty = %q", got)
	}
}

func TestGitDiff(t *testing.T) {
	ws, reg := initGitRepo(t)
	writeFile(t, ws, "app.go", "package main\n// new line\n")

	got := execTool(t, reg, "git_diff", map[string]any{})
	if !strings.Contains(got, "+// new line") {
		t.Errorf("git_diff = %q", got)
	}
	if got := execTool(t, reg, "git_diff", map[string]any{"path": "nope.go"}); !strings.Contains(got, "no changes") {
		t.Errorf("git_diff path filter = %q", got)
	}
}

func TestGitLog(t *testing.T) {
	_, reg := initGitRepo(t)
	got := execTool(t, reg, "git_log", map[string]any{"n": 5})
	if !strings.Contains(got, "initial") {
		t.Errorf("git_log = %q", got)
	}
	if got := execTool(t, reg, "git_log", map[string]any{"n": 999}); !strings.Contains(got, "validation-error") {
		t.Errorf("git_log n cap = %q", got)
	}
}

func TestGitNotARepo(t *testing.T) {
	_, reg := fakeWorkspace(t) // no git init
	for _, name := range []string{"git_status", "git_diff", "git_log"} {
		got := execTool(t, reg, name, map[string]any{})
		if !strings.Contains(got, "error:") {
			t.Errorf("%s in non-repo = %q", name, got)
		}
	}
}
