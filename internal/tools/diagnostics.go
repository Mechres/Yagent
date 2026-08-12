package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Mechres/Yagent/internal/llm"
)

// ---------- workspace_diagnostics ----------

// diagnosticsTool detects the project type and runs the matching static
// checker (go vet, tsc --noEmit, cargo check, ruff/compileall). Read-only: the
// commands are fixed by the tool, never supplied by the model, so it needs no
// approval gate yet gives the agent a first-class self-healing loop after edits.
type diagnosticsTool struct {
	ws       string
	lookPath func(string) (string, error) // injectable in tests; nil = exec.LookPath
	runCmd   func(ctx context.Context, name string, args ...string) ([]byte, error)
}

var diagnosticsSchema = fnSchema("workspace_diagnostics", "run the project's static checker (go vet ./..., tsc --noEmit, cargo check, ruff check . or a python syntax check) based on the detected language. Read-only and fast — use it after edits to verify a change compiles, or to find existing errors before starting work.",
	map[string]any{}, []string{})

const (
	diagnosticsTimeout   = 120 * time.Second
	diagnosticsMaxOutput = 24 << 10
)

func (t *diagnosticsTool) Schema() llm.ToolSchema { return diagnosticsSchema }
func (t *diagnosticsTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *diagnosticsTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return "", err
	}
	name, args, ok := detectDiagnostics(t.ws, t.lookPath)
	if !ok {
		return "no diagnostics configured for this project (recognized markers: go.mod, package.json+tsconfig, Cargo.toml, pyproject.toml/requirements.txt)", nil
	}
	out, err := t.run(ctx, name, args...)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return capResult(string(out), diagnosticsMaxOutput), nil
}

// run executes the diagnostic command with a timeout, killing the whole process
// group so a hung compiler can't stall the turn.
func (t *diagnosticsTool) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if t.runCmd != nil {
		return t.runCmd(ctx, name, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, diagnosticsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = t.ws
	cmd.Env = scrubEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("diagnostics timed out after %s", diagnosticsTimeout)
		}
		return nil, ctx.Err()
	case <-done:
		if errBuf.Len() > 0 {
			out.WriteString("\nstderr:\n" + errBuf.String())
		}
		return out.Bytes(), nil
	}
}

// detectDiagnostics picks a checker from the workspace's marker files. lookPath
// is used to verify the checker exists (falling back to a lighter check); nil
// means exec.LookPath.
func detectDiagnostics(ws string, lookPath func(string) (string, error)) (name string, args []string, ok bool) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	has := func(rel string) bool {
		_, err := os.Stat(filepath.Join(ws, rel))
		return err == nil
	}
	switch {
	case has("go.mod"):
		if _, err := lookPath("go"); err == nil {
			return "go", []string{"vet", "./..."}, true
		}
	case has("Cargo.toml"):
		if _, err := lookPath("cargo"); err == nil {
			return "cargo", []string{"check"}, true
		}
	case has("package.json"):
		if has("tsconfig.json") {
			if _, err := lookPath("npx"); err == nil {
				return "npx", []string{"tsc", "--noEmit"}, true
			}
		} else if _, err := lookPath("npx"); err == nil {
			// eslint only when a config is present (otherwise it errors)
			for _, cfg := range []string{"eslint.config.js", "eslint.config.mjs", "eslint.config.cjs", ".eslintrc", ".eslintrc.json", ".eslintrc.js"} {
				if has(cfg) {
					return "npx", []string{"eslint", "."}, true
				}
			}
		}
	case has("pyproject.toml") || has("requirements.txt") || has("setup.py"):
		if _, err := lookPath("ruff"); err == nil {
			return "ruff", []string{"check", "."}, true
		}
		if _, err := lookPath("python3"); err == nil {
			return "python3", []string{"-m", "compileall", "-q", "."}, true
		}
	}
	return "", nil, false
}
