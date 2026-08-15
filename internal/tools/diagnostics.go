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
		return "no diagnostics configured for this project (recognized markers: go.mod, package.json+tsconfig, Cargo.toml, pyproject.toml/requirements.txt, or C/C++ sources)", nil
	}
	out, err := t.run(ctx, name, args...)
	if err != nil {
		// A non-zero exit (or timeout) is a FAIL, not a crash — the gate must
		// trust the exit status. Prefix the output so DiagnosticsFailed can
		// parse it without guessing from prose alone (GPT sol #1).
		return "[FAIL] " + offloadResult(t.ws, string(out), diagnosticsMaxOutput) + fmt.Sprintf("\n(exited %v)", err), nil
	}
	return "[PASS] " + offloadResult(t.ws, string(out), diagnosticsMaxOutput), nil
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
	case waitErr := <-done:
		if errBuf.Len() > 0 {
			out.WriteString("\nstderr:\n" + errBuf.String())
		}
		if waitErr != nil {
			// A non-zero exit means the check FAILED — surface it so the gate
			// trusts the exit status, not just the output prose (GPT sol #1).
			return out.Bytes(), fmt.Errorf("exit status %d", waitExitCode(waitErr))
		}
		return out.Bytes(), nil
	}
}

// exitCode extracts a numeric exit code from a Wait error (0 when nil).
func waitExitCode(waitErr error) int {
	if waitErr == nil {
		return 0
	}
	if ee, ok := waitErr.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
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
	case hasCProject(ws):
		// C/C++: prefer the project's own build system (CMake build dir or
		// Makefile) — it knows the include paths and the correct compiler, so
		// diagnostics reflect the real build. Bare gcc -fsyntax-only on all
		// sources is only a fallback for projects with no build system at all
		// (it false-errors on real projects that rely on CMake include dirs).
		if cmd, args, ok := cBuildCheck(ws, lookPath); ok {
			return cmd, args, ok
		}
		sources, _ := cSources(ws)
		if len(sources) == 0 {
			return "", nil, false // manifest only; nothing to syntax-check
		}
		if hasCppSources(ws) {
			if _, err := lookPath("g++"); err == nil {
				return "g++", append([]string{"-fsyntax-only"}, sources...), true
			}
		}
		if _, err := lookPath("gcc"); err == nil {
			return "gcc", append([]string{"-fsyntax-only"}, sources...), true
		}
		if _, err := lookPath("cc"); err == nil {
			return "cc", append([]string{"-fsyntax-only"}, sources...), true
		}
	}
	return "", nil, false
}

// cBuildCheck prefers a real build over a bare syntax pass. It looks for, in
// order: an existing cmake build dir (build/, cmake-build-*/), a CMakeLists.txt
// (configure+build into build/), or a Makefile. Returns ok=false when none is
// usable, so the caller falls back to gcc -fsyntax-only.
func cBuildCheck(ws string, lookPath func(string) (string, error)) (string, []string, bool) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	// An existing cmake build dir has a configured Makefile — build it.
	if buildDir, ok := cmakeBuildDir(ws); ok {
		if _, err := lookPath("cmake"); err == nil {
			return "cmake", []string{"--build", buildDir, "-j"}, true
		}
		if _, err := lookPath("make"); err == nil {
			return "make", []string{"-C", buildDir, "-j"}, true
		}
	}
	// CMakeLists.txt but no build dir yet: configure into build/ then build.
	if _, err := os.Stat(filepath.Join(ws, "CMakeLists.txt")); err == nil {
		if _, err := lookPath("cmake"); err == nil {
			buildDir := filepath.Join(ws, "build")
			// Only use it if already configured; otherwise configuring takes a
			// long time and may need flags the tool can't know. Prefer the
			// existing build dir above; here we reconfigure only when build/
			// exists but wasn't caught (e.g. no CMakeCache yet).
			if _, err := os.Stat(filepath.Join(buildDir, "CMakeCache.txt")); err == nil {
				return "cmake", []string{"--build", buildDir, "-j"}, true
			}
		}
	}
	// A plain Makefile.
	if _, err := os.Stat(filepath.Join(ws, "Makefile")); err == nil {
		if _, err := lookPath("make"); err == nil {
			return "make", []string{"-j"}, true
		}
	}
	return "", nil, false
}

// cmakeBuildDir finds an existing configured CMake build directory (build/,
// cmake-build-debug/, cmake-build-release/) with a CMakeCache.
func cmakeBuildDir(ws string) (string, bool) {
	for _, d := range []string{"build", "cmake-build-debug", "cmake-build-release", "cmake-build"} {
		p := filepath.Join(ws, d)
		if _, err := os.Stat(filepath.Join(p, "CMakeCache.txt")); err == nil {
			return d, true
		}
	}
	return "", false
}

// hasCProject reports whether the workspace looks like a C/C++ project: a
// Makefile/CMakeLists, or at least one C/C++ source file, with no higher-level
// manifest (go.mod, Cargo.toml, package.json, pyproject.toml) present.
func hasCProject(ws string) bool {
	for _, m := range []string{"Makefile", "makefile", "CMakeLists.txt", "CMakeLists.txt.in"} {
		if _, err := os.Stat(filepath.Join(ws, m)); err == nil {
			return true
		}
	}
	sources, err := cSources(ws)
	return err == nil && len(sources) > 0
}

// cSources lists .c/.cc/.cpp/.cxx source files under ws (skipping build dirs).
func cSources(ws string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(ws, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		switch ext {
		case ".c", ".cc", ".cpp", ".cxx":
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// hasCppSources reports whether any .cpp/.cc/.cxx file exists (to pick g++).
func hasCppSources(ws string) bool {
	sources, err := cSources(ws)
	if err != nil {
		return false
	}
	for _, s := range sources {
		switch strings.ToLower(filepath.Ext(s)) {
		case ".cc", ".cpp", ".cxx":
			return true
		}
	}
	return false
}
