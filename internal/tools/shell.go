package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/scrub"
)

// ---------- shell_exec ----------

type shellExecTool struct {
	ws       string
	sandbox  string
	hasBwrap func() bool // injectable in tests; nil = exec.LookPath
}

type shellExecArgs struct {
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
}

const (
	shellDefaultTimeout = 30 * time.Second
	shellMaxTimeout     = 300 * time.Second
	shellMaxOutput      = 32 << 10
)

var shellExecSchema = fnSchema("shell_exec", "run a shell command via sh -c; destructive, requires approval; output capped at 32 KiB",
	map[string]any{
		"command":     strProp("shell command to run"),
		"timeout_sec": intProp("timeout in seconds, default 30, max 300 (optional)"),
	},
	[]string{"command"})

func (t *shellExecTool) Schema() llm.ToolSchema { return shellExecSchema }
func (t *shellExecTool) Risk() RiskLevel        { return RiskDestructive }

func (t *shellExecTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a shellExecArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Command == "" {
		return "", validationErrorf(`argument "command" is required`)
	}
	timeout := shellDefaultTimeout
	if a.TimeoutSec > 0 {
		timeout = time.Duration(a.TimeoutSec) * time.Second
		if timeout > shellMaxTimeout {
			return "", validationErrorf("timeout_sec %d exceeds max %d", a.TimeoutSec, int(shellMaxTimeout.Seconds()))
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if t.sandbox == "bwrap" {
		if !t.bwrapAvailable() {
			return "error: shell.sandbox is bwrap but bubblewrap is not installed (install it, or set shell.sandbox to empty)", nil
		}
		home, _ := os.UserHomeDir()
		args, err := bwrapArgs(t.ws, home, a.Command)
		if err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
		cmd = exec.CommandContext(ctx, "bwrap", args...)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", a.Command)
	}
	cmd.Dir = t.ws
	cmd.Env = scrubEnv(os.Environ())
	// Run the command in its own process group so a timeout can kill
	// descendants too (`sh -c "sleep 5"` forks sleep; CommandContext only
	// SIGKILLs the shell, and the grandchild keeps the stdout pipe open,
	// stalling the wait until it exits on its own).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		// Kill the whole process group (negative pid), not just the direct
		// child, so orphaned grandchildren die and the pipes close.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Sprintf("error: command timed out after %s:\n%s%s", timeout, out.String(), errBuf.String()), nil
		}
		return fmt.Sprintf("error: command canceled:\n%s%s", out.String(), errBuf.String()), nil
	case err := <-done:
		var b strings.Builder
		b.WriteString(out.String())
		if errBuf.Len() > 0 {
			fmt.Fprintf(&b, "stderr:\n%s", errBuf.String())
		}
		if err != nil {
			fmt.Fprintf(&b, "exit status: %v", err)
		}
		if b.Len() == 0 {
			return "(no output)", nil
		}
		return capResult(b.String(), shellMaxOutput), nil
	}
}

func (t *shellExecTool) bwrapAvailable() bool {
	if t.hasBwrap != nil {
		return t.hasBwrap()
	}
	_, err := exec.LookPath("bwrap")
	return err == nil
}

// bwrapArgs builds a bubblewrap command line: the workspace is writable, the
// rest of the system is read-only, /tmp is private, and the network is
// unshared. Common dev caches ($HOME/.cache, $HOME/go) stay writable so builds
// that don't need the network can still run.
func bwrapArgs(workspace, home, command string) ([]string, error) {
	ws, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	args := []string{
		"--die-with-parent", "--new-session",
		"--unshare-pid", "--unshare-net",
		"--tmpfs", "/tmp",
		"--dev", "/dev",
		"--proc", "/proc",
		"--bind", ws, ws,
		"--chdir", ws,
	}
	for _, d := range []string{"/usr", "/usr/local", "/lib", "/lib64", "/bin", "/sbin", "/etc", "/opt"} {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			args = append(args, "--ro-bind", d, d)
		}
	}
	if home != "" {
		if fi, err := os.Stat(home); err == nil && fi.IsDir() {
			args = append(args, "--ro-bind", home, home)
		}
		// Mask sensitive material even though the rest of $HOME stays readable
		// (git/npm config still works): a sandboxed-but-untrusted command must
		// not be able to read SSH keys, cloud credentials or browser profiles.
		// Dirs are replaced with empty tmpfs; files are rebound to /dev/null.
		for _, p := range sensitiveHomePaths(home) {
			fi, err := os.Lstat(p)
			if err != nil {
				continue
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				continue // symlink to an unbound target (e.g. /run/user/...); unreachable in the sandbox
			}
			if fi.IsDir() {
				args = append(args, "--tmpfs", p)
			} else {
				args = append(args, "--ro-bind", "/dev/null", p)
			}
		}
		for _, cache := range []string{filepath.Join(home, ".cache"), filepath.Join(home, "go")} {
			if fi, err := os.Stat(cache); err == nil && fi.IsDir() {
				args = append(args, "--bind", cache, cache)
			}
		}
	}
	args = append(args, "sh", "-c", command)
	return args, nil
}

// sensitiveHomePaths lists $HOME entries that should be hidden from sandboxed
// commands: credential dirs/files and browser profiles.
func sensitiveHomePaths(home string) []string {
	dirs := []string{
		".ssh", ".aws", ".gnupg", ".kube", ".docker", ".password-store",
		".config/gh", ".config/gcloud", ".config/hub",
		".local/share/keyrings", ".local/share/gnupg",
		".mozilla", ".config/google-chrome", ".config/chromium",
		".config/BraveSoftware", ".config/Brave-Browser",
	}
	files := []string{".git-credentials", ".netrc", ".npmrc", ".pypirc", ".curlrc", ".env"}
	var out []string
	for _, rel := range append(dirs, files...) {
		out = append(out, filepath.Join(home, rel))
	}
	return out
}

// scrubEnv drops secret-looking variables (API_TOKEN, GH_PAT, credential
// URLs, SSH keys, ...) from the child environment. Name checks use the same
// heuristics as internal/scrub, plus value checks for unconventionally named
// secrets.
func scrubEnv(env []string) []string {
	kept := env[:0]
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		if scrub.SecretEnv(kv[:eq], kv[eq+1:]) {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}
