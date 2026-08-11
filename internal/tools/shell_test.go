package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellExec(t *testing.T) {
	ws, reg := fakeWorkspace(t)
	writeFile(t, ws, "data.txt", "payload")

	// runs in the workspace dir
	got := execTool(t, reg, "shell_exec", map[string]any{"command": "cat data.txt"})
	if !strings.Contains(got, "payload") {
		t.Errorf("shell_exec cwd = %q", got)
	}
	// non-zero exit is data, not a crash
	if got := execTool(t, reg, "shell_exec", map[string]any{"command": "exit 3"}); !strings.Contains(got, "exit status") {
		t.Errorf("shell_exec exit = %q", got)
	}
	// missing command → validation error
	if got := execTool(t, reg, "shell_exec", map[string]any{}); !strings.Contains(got, "validation-error") {
		t.Errorf("shell_exec no command = %q", got)
	}
	// timeout_sec beyond max → validation error
	if got := execTool(t, reg, "shell_exec", map[string]any{"command": "true", "timeout_sec": 99999}); !strings.Contains(got, "validation-error") {
		t.Errorf("shell_exec bad timeout = %q", got)
	}
}

func TestShellExecTimeout(t *testing.T) {
	_, reg := fakeWorkspace(t)
	start := time.Now()
	got := execTool(t, reg, "shell_exec", map[string]any{"command": "sleep 5", "timeout_sec": 1})
	if !strings.Contains(got, "timed out") {
		t.Errorf("shell_exec timeout = %q", got)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("timeout took too long: %v", time.Since(start))
	}
}

func TestScrubEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"OPENAI_API_KEY=sk-secret",
		"MY_TOKEN=abc",
		"AWS_SECRET_ACCESS_KEY=zzz",
		"HOME=/home/user",
	}
	scrubbed := scrubEnv(env)
	joined := strings.Join(scrubbed, "\n")
	for _, keep := range []string{"PATH=/usr/bin", "HOME=/home/user"} {
		if !strings.Contains(joined, keep) {
			t.Errorf("scrubEnv dropped %q: %v", keep, scrubbed)
		}
	}
	for _, drop := range []string{"OPENAI_API_KEY", "MY_TOKEN", "AWS_SECRET_ACCESS_KEY"} {
		if strings.Contains(joined, drop) {
			t.Errorf("scrubEnv kept %q: %v", drop, scrubbed)
		}
	}
}

func TestShellExecScrubsSecrets(t *testing.T) {
	_, reg := fakeWorkspace(t)
	t.Setenv("YAGENT_TEST_TOKEN", "supersecretvalue123")
	got := execTool(t, reg, "shell_exec", map[string]any{"command": "echo $YAGENT_TEST_TOKEN"})
	if strings.Contains(got, "supersecretvalue123") {
		t.Errorf("secret leaked to shell: %q", got)
	}
}

func TestBwrapArgs(t *testing.T) {
	home := t.TempDir() // exists, so it must be bound read-only
	args, err := bwrapArgs("/tmp/ws", home, "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--unshare-net", "--tmpfs", "/tmp", "--bind", "/tmp/ws", "/tmp/ws", "--chdir", "/tmp/ws", "--ro-bind", home, home, "sh", "-c", "echo hi"} {
		if !strings.Contains(joined, want) {
			t.Errorf("bwrap args missing %q: %s", want, joined)
		}
	}
	// the command must be the final argument
	if args[len(args)-1] != "echo hi" || args[len(args)-2] != "-c" {
		t.Errorf("command not at the end: %v", args[len(args)-3:])
	}
}

func TestShellExecSandboxNotInstalled(t *testing.T) {
	tool := &shellExecTool{ws: t.TempDir(), sandbox: "bwrap", hasBwrap: func() bool { return false }}
	res, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"command": "echo hi"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "bubblewrap is not installed") {
		t.Errorf("missing-bwrap result = %q", res)
	}
}

func TestShellExecSandboxLive(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
	tool := &shellExecTool{ws: t.TempDir(), sandbox: "bwrap"}
	res, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"command": "echo sandboxed-ok"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "sandboxed-ok") {
		t.Errorf("sandboxed shell = %q", res)
	}
	// the sandboxed workspace can be written (the agent's own edits)
	ws := t.TempDir()
	tool = &shellExecTool{ws: ws, sandbox: "bwrap"}
	if _, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"command": "touch probe.txt"})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "probe.txt")); err != nil {
		t.Errorf("workspace write in sandbox failed: %v", err)
	}
}
