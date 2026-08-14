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
)

// ---------- runtime_smoke ----------

// smokeTool builds the generated program (when a build step exists) and runs it
// with scripted stdin for a short window, reporting whether it survives or
// crashes (panic / segfault / assertion / non-zero exit with no output). It is
// the codegen-mode companion to workspace_diagnostics: diagnostics prove the
// code *compiles*, smoke proves it *runs without crashing* — the two failures
// a small model's greenfield output actually has.
type smokeTool struct {
	ws       string
	lookPath func(string) (string, error) // injectable in tests; nil = exec.LookPath
	buildCmd func(ctx context.Context, name string, args ...string) ([]byte, error)
}

var smokeSchema = fnSchema("runtime_smoke", "build and briefly run the program the agent just wrote, feeding it minimal input, to prove it doesn't crash (panic, segfault, assertion, or immediate exit with no output). Deterministic — use it after fs_write/fs_edit/fs_patch in codegen mode, alongside workspace_diagnostics, before declaring a program complete.",
	map[string]any{}, []string{})

const (
	smokeTimeout   = 12 * time.Second
	smokeMaxOutput = 4 << 10
)

// smokeScriptedInput is fed to the program on stdin. It is a few moves/quits so
// an interactive loop advances (and doesn't stall the process group).
var smokeScriptedInput = "q\nx\n\n\n0\n0\n\n\n\n"

func (t *smokeTool) Schema() llm.ToolSchema { return smokeSchema }
func (t *smokeTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *smokeTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return "", err
	}
	return t.smoke(ctx)
}

// smokeResult is the deterministic verdict + evidence.
type smokeResult struct {
	skip   bool   // no smoke runner for this project type
	ok     bool   // survived the run window without crashing
	detail string // crash reason or a sample of stdout
	status string // short summary for the model ("ran ~3s, exited 0")
}

// Smoke determines build+run plan for the workspace and executes it.
func (t *smokeTool) smoke(ctx context.Context) (string, error) {
	bin, clean, ok := t.buildPlan(ctx)
	if !ok {
		return "runtime_smoke: no smoke runner for this project (supported: Go, C/C++, Python; a JS/TS project needs a browser DOM). The program could not be executed, so its runtime safety is unverified.", nil
	}
	if bin != "" {
		if clean != nil {
			defer clean()
		}
	}
	res := t.runSmoke(ctx, bin)
	if res.skip {
		return "runtime_smoke: no smoke runner for this project.", nil
	}
	if !res.ok {
		return fmt.Sprintf("runtime_smoke FAIL: %s\n%s", res.detail, res.status), nil
	}
	return fmt.Sprintf("runtime_smoke PASS: %s\n%s", res.status, res.detail), nil
}

// buildPlan returns the runnable binary path (or "" to run a script directly),
// a cleanup func, and whether smoke is supported at all.
func (t *smokeTool) buildPlan(ctx context.Context) (bin string, clean func(), ok bool) {
	if t.ws == "" {
		return "", nil, false
	}
	has := func(rel string) bool {
		_, err := os.Stat(filepath.Join(t.ws, rel))
		return err == nil
	}
	switch {
	case has("go.mod"):
		tmp := filepath.Join(os.TempDir(), fmt.Sprintf("yagent-smoke-%d", time.Now().UnixNano()))
		// -o to a temp binary, then run it; the module's own dir stays clean.
		if err := t.build(ctx, "go", "build", "-o", tmp, "."); err != nil {
			return "", nil, false
		}
		return tmp, func() { _ = os.Remove(tmp) }, true
	case has("Cargo.toml"):
		// cargo build then the debug binary.
		if err := t.build(ctx, "cargo", "build", "--quiet"); err != nil {
			return "", nil, false
		}
		bin := filepath.Join(t.ws, "target", "debug")
		if !hasCargoBinary(bin) {
			return "", nil, false
		}
		return bin, nil, true
	case hasCProject(t.ws):
		sources, _ := cSources(t.ws)
		if len(sources) == 0 {
			return "", nil, false
		}
		tmp := filepath.Join(os.TempDir(), fmt.Sprintf("yagent-smoke-%d", time.Now().UnixNano()))
		var compiler string
		if hasCppSources(t.ws) {
			compiler = "g++"
		} else {
			compiler = "gcc"
		}
		// Plain build first; a terminal game usually links ncurses, so retry
		// with -lncurses when the first attempt fails (no link command in the
		// generated code to tell us otherwise).
		if err := t.build(ctx, compiler, "-o", tmp, strings.Join(sources, " ")); err != nil {
			if err2 := t.build(ctx, compiler, "-o", tmp, strings.Join(sources, " "), "-lncurses"); err2 != nil {
				return "", nil, false
			}
		}
		return tmp, func() { _ = os.Remove(tmp) }, true
	case hasPythonEntry(t.ws):
		// python3 <entry>.py directly.
		return "", nil, true
	}
	return "", nil, false
}

// hasCargoBinary returns the cargo debug binary path or "".
func hasCargoBinary(debugDir string) bool {
	entries, err := os.ReadDir(debugDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			return true
		}
	}
	return false
}

// hasPythonEntry finds a runnable .py entry point (main.py preferred, else the
// only .py in the workspace root).
func hasPythonEntry(ws string) bool {
	if _, err := os.Stat(filepath.Join(ws, "main.py")); err == nil {
		return true
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".py") {
			return true
		}
	}
	return false
}

// runSmoke executes the runnable target with scripted stdin and judges survival.
func (t *smokeTool) runSmoke(ctx context.Context, bin string) smokeResult {
	var (
		cmd *exec.Cmd
		dir = t.ws
	)
	if bin != "" {
		cmd = exec.CommandContext(ctx, bin)
	} else {
		// python3 <entry.py>
		entry := "main.py"
		if _, err := os.Stat(filepath.Join(t.ws, "main.py")); err != nil {
			entries, _ := os.ReadDir(t.ws)
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".py") {
					entry = e.Name()
					break
				}
			}
		}
		cmd = exec.CommandContext(ctx, "python3", entry)
	}
	cmd.Dir = dir
	cmd.Env = scrubEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = strings.NewReader(smokeScriptedInput)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return smokeResult{ok: false, detail: fmt.Sprintf("failed to launch: %v", err)}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ctx2, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()

	var waitErr error
	select {
	case <-ctx2.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		waitErr = ctx2.Err()
	case waitErr = <-done:
	}

	elapsed := time.Since(start)
	output := combineOutput(out.Bytes(), errBuf.Bytes())
	if msg, crashed := smokeCrashReason(waitErr, elapsed, output); crashed {
		return smokeResult{ok: false, detail: msg, status: fmt.Sprintf("crashed after ~%s", elapsed.Round(time.Millisecond))}
	}
	// Survived: either exited cleanly or kept running (an interactive loop)
	// until the timeout — both prove the program doesn't blow up on first input.
	sample := capResult(string(output), smokeMaxOutput)
	verb := "ran"
	if waitErr != nil && waitErr != context.DeadlineExceeded && waitErr != context.Canceled {
		verb = "exited"
	}
	return smokeResult{ok: true, detail: sample, status: fmt.Sprintf("%s ~%s (exit %v)", verb, elapsed.Round(time.Millisecond), exitCode(waitErr))}
}

// smokeCrashReason reports whether the run crashed. A program that exits
// non-zero with no output, or emits a panic/segfault/assertion, is a crash;
// an interactive loop that survives to the timeout is not.
func smokeCrashReason(waitErr error, elapsed time.Duration, output string) (string, bool) {
	if waitErr != nil && waitErr != context.DeadlineExceeded && waitErr != context.Canceled {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				if ws.Signaled() {
					return fmt.Sprintf("killed by signal %v", ws.Signal()), true
				}
			}
		}
	}
	low := strings.ToLower(output)
	for _, marker := range []string{"panic:", "segmentation fault", "segfault", "assertion", "assert failed",
		"aborted", "stack overflow", "runtime error", "uncaught exception", "index out of range",
		"nil pointer", "fatal error"} {
		if strings.Contains(low, marker) {
			return "output contains crash marker \"" + marker + "\"", true
		}
	}
	// Exited non-zero with essentially no output — a silent failure.
	if waitErr != nil && waitErr != context.DeadlineExceeded && waitErr != context.Canceled &&
		strings.TrimSpace(output) == "" {
		return "exited non-zero with no output", true
	}
	return "", false
}

// exitCode formats a Wait error as an exit status for the report.
func exitCode(waitErr error) string {
	if waitErr == nil {
		return "0"
	}
	if ee, ok := waitErr.(*exec.ExitError); ok {
		return fmt.Sprintf("%d", ee.ExitCode())
	}
	if waitErr == context.DeadlineExceeded {
		return "timeout (survived the window)"
	}
	return waitErr.Error()
}

// build runs a build command, returning its combined output or an error.
func (t *smokeTool) build(ctx context.Context, name string, args ...string) error {
	if t.buildCmd != nil {
		_, err := t.buildCmd(ctx, name, args...)
		return err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = t.ws
	cmd.Env = scrubEnv(os.Environ())
	return cmd.Run()
}

// combineOutput merges stdout+stderr for judgment.
func combineOutput(out, errBuf []byte) string {
	var b strings.Builder
	b.Write(out)
	if len(errBuf) > 0 {
		b.WriteString("\nstderr:\n")
		b.Write(errBuf)
	}
	return b.String()
}
