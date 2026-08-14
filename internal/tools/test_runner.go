package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Mechres/Yagent/internal/llm"
)

// ---------- test_runner ----------

// testRunnerTool executes targeted unit tests (Go / Python / JS-TS) scoped to
// a package, file, or single symbol, and prunes the output to failures plus a
// concise summary. Complements workspace_diagnostics (which only compiles):
// this is the semantic "did my change break the logic" loop.
type testRunnerTool struct {
	ws       string
	lookPath func(string) (string, error)
	runCmd   func(ctx context.Context, name string, args ...string) ([]byte, error)
}

type testRunnerArgs struct {
	Scope  string `json:"scope"`            // package | file | symbol
	Path   string `json:"path,omitempty"`   // file or package dir (relative)
	Symbol string `json:"symbol,omitempty"` // required for scope=symbol
}

var testRunnerSchema = fnSchema("test_runner", "run the unit tests affected by a change, scoped to a package, a file, or one symbol, and report only failures + a summary (passing tests are collapsed). Go/Python/JS-TS. Use it after an edit when workspace_diagnostics passes but you need to know whether the logic still holds — it runs ONLY the targeted tests, not the whole suite",
	map[string]any{
		"scope":  strProp("what to run: 'package' (default; the dir containing path or the whole workspace), 'file' (only the tests in the given file), or 'symbol' (only tests matching the symbol name)"),
		"path":   strProp("the file or directory to test, relative to the workspace (optional for scope=package: defaults to the whole project)"),
		"symbol": strProp("the function/class name to match tests against (required for scope=symbol)"),
	},
	[]string{"scope"})

const testRunnerTimeout = 120 * time.Second

func (t *testRunnerTool) Schema() llm.ToolSchema { return testRunnerSchema }
func (t *testRunnerTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *testRunnerTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a testRunnerArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	switch a.Scope {
	case "package", "file", "symbol":
	case "":
		a.Scope = "package"
	default:
		return "", validationErrorf(`scope must be "package", "file" or "symbol"`)
	}
	if a.Scope == "symbol" && a.Symbol == "" {
		return "", validationErrorf(`scope=symbol requires the "symbol" argument`)
	}
	if a.Scope == "file" && a.Path == "" {
		return "", validationErrorf(`scope=file requires the "path" argument`)
	}

	name, args, _, err := t.detectTestCommand(a)
	if err != nil {
		return "no test framework configured for this project (recognized: go test, cargo test, pytest, vitest/jest)", nil
	}
	out, err := t.run(ctx, name, args...)
	if err != nil {
		// go test / pytest exit non-zero on failures — that is the result,
		// not a crash. Return the (pruned) output so the model sees it.
		pruned := pruneTestOutput(string(out))
		if strings.TrimSpace(pruned) == "" {
			pruned = fmt.Sprintf("tests failed: %v", err)
		}
		return capResult(pruned, diagnosticsMaxOutput), nil
	}
	return capResult(pruneTestOutput(string(out)), diagnosticsMaxOutput), nil
}

// detectTestCommand picks the test invocation for the scope and project type.
func (t *testRunnerTool) detectTestCommand(a testRunnerArgs) (name string, args []string, target string, err error) {
	if t.lookPath == nil {
		t.lookPath = exec.LookPath
	}
	has := func(rel string) bool {
		_, err := os.Stat(filepath.Join(t.ws, rel))
		return err == nil
	}
	switch {
	case has("go.mod"):
		dir := "./..."
		switch a.Scope {
		case "file":
			d := filepath.Dir(a.Path)
			tests := testNamesIn(filepath.Join(t.ws, a.Path))
			if d == "." {
				dir = "."
			} else {
				dir = "./" + filepath.ToSlash(d)
			}
			if len(tests) > 0 {
				return "go", []string{"test", dir, "-run", strings.Join(tests, "|"), "-count=1"}, a.Path, nil
			}
			return "go", []string{"test", dir, "-count=1"}, a.Path, nil
		case "symbol":
			dir = packageArg(a.Path)
			return "go", []string{"test", dir, "-run", symbolTestRegex(a.Symbol), "-count=1"}, a.Symbol, nil
		default:
			if a.Path != "" {
				dir = packageArg(a.Path)
			}
			return "go", []string{"test", dir, "-count=1"}, dir, nil
		}
	case has("Cargo.toml"):
		// Rust: cargo test (agy #3) — completes the edit-verify loop for Rust
		// projects (diagnostics already runs cargo check).
		if _, err := t.lookPath("cargo"); err != nil {
			return "", nil, "", err
		}
		switch a.Scope {
		case "symbol":
			return "cargo", []string{"test", "--", a.Symbol}, a.Symbol, nil
		case "file":
			// cargo has no per-file target; run the package tests.
			return "cargo", []string{"test"}, a.Path, nil
		default:
			return "cargo", []string{"test"}, "", nil
		}
	case has("pyproject.toml") || has("requirements.txt") || has("setup.py") || has("pytest.ini"):
		if _, err := t.lookPath("python3"); err != nil {
			return "", nil, "", err
		}
		switch a.Scope {
		case "file":
			return "python3", []string{"-m", "pytest", a.Path, "-q"}, a.Path, nil
		case "symbol":
			dir := "."
			if a.Path != "" {
				dir = a.Path
			}
			return "python3", []string{"-m", "pytest", dir, "-k", a.Symbol, "-q"}, a.Symbol, nil
		default:
			dir := "."
			if a.Path != "" {
				dir = a.Path
			}
			return "python3", []string{"-m", "pytest", dir, "-q"}, dir, nil
		}
	case has("package.json"):
		// vitest first, then jest.
		if _, err := t.lookPath("npx"); err == nil {
			switch a.Scope {
			case "file":
				return "npx", []string{"vitest", "run", a.Path}, a.Path, nil
			case "symbol":
				return "npx", []string{"vitest", "run", a.Path, "-t", a.Symbol}, a.Symbol, nil
			default:
				dir := "."
				if a.Path != "" {
					dir = a.Path
				}
				return "npx", []string{"vitest", "run", dir}, dir, nil
			}
		}
	}
	return "", nil, "", fmt.Errorf("no framework")
}

// packageArg maps a path (file or dir) to a Go package selector.
func packageArg(path string) string {
	if path == "" {
		return "./..."
	}
	d := path
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		d = filepath.Dir(path)
	}
	if d == "." || d == "" {
		return "."
	}
	return "./" + filepath.ToSlash(d)
}

// symbolTestRegex turns a symbol into a go test -run regex matching the
// conventional Test<Symbol> family (TestHelper, Test_Helper, TestMyHelper…).
func symbolTestRegex(symbol string) string {
	esc := regexp.QuoteMeta(symbol)
	return `(^Test(?:[A-Z0-9_]*_)??` + esc + `)|(^Example` + esc + `)`
}

// testNamesIn extracts func Test* names from a Go test file (best-effort).
var testFuncRe = regexp.MustCompile(`func (Test[A-Za-z0-9_]+)\(`)

func testNamesIn(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range testFuncRe.FindAllSubmatch(data, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// run executes the test command with a timeout, killing the process group.
func (t *testRunnerTool) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if t.runCmd != nil {
		return t.runCmd(ctx, name, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, testRunnerTimeout)
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
			return out.Bytes(), fmt.Errorf("tests timed out after %s", testRunnerTimeout)
		}
		return out.Bytes(), ctx.Err()
	case <-done:
		if errBuf.Len() > 0 {
			out.WriteString("\nstderr:\n" + errBuf.String())
		}
		return out.Bytes(), nil
	}
}

// pruneTestOutput collapses a full test run into failures + a summary: keeps
// ok/Fail headers, the pytest/vitest summary line, and failure detail lines;
// drops per-test verbose RUN/PASS lines.
func pruneTestOutput(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	sawFailure := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		switch {
		case strings.HasPrefix(t, "ok ") || strings.HasPrefix(t, "FAIL") ||
			strings.HasPrefix(t, "--- FAIL") || strings.HasPrefix(t, "PASS") ||
			strings.HasPrefix(t, "=== ") && strings.Contains(t, "FAIL"):
			out = append(out, ln)
			sawFailure = sawFailure || strings.HasPrefix(t, "FAIL") || strings.HasPrefix(t, "--- FAIL")
		case strings.HasPrefix(t, "--- PASS"):
			// collapse passing tests
			continue
		case strings.HasPrefix(t, "=== RUN"):
			continue
		case sawFailure:
			// failure detail lines (file:line, messages) — keep a few
			if strings.Contains(ln, ".go:") || strings.Contains(ln, ".py:") ||
				strings.Contains(ln, ".ts:") || strings.Contains(ln, ".js:") ||
				strings.HasPrefix(ln, " ") {
				out = append(out, ln)
			}
		case strings.Contains(t, "passed") || strings.Contains(t, "failed"):
			// pytest / vitest summary lines
			out = append(out, ln)
		}
	}
	if len(out) == 0 {
		return "all tests passed"
	}
	return strings.Join(out, "\n")
}
