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
// a small model's greenfield output actually has. An optional `steps` probe
// raises the bar to *behaves*: each step runs the program with its own stdin
// and asserts the output contains the expected text (e.g. a todo app: add an
// item, then a fresh `list` run must show it — catching dead persistence).
type smokeTool struct {
	ws       string
	lookPath func(string) (string, error) // injectable in tests; nil = exec.LookPath
	buildCmd func(ctx context.Context, name string, args ...string) ([]byte, error)
}

var smokeSchema = fnSchema("runtime_smoke", "build and briefly run the program the agent just wrote, feeding it minimal input, to prove it doesn't crash (panic, segfault, assertion, or immediate exit with no output). Optional `steps` — a list of {args, input, expect} probes — assert the program *behaves*: each step runs the program with the given argv and stdin and the output must contain the expected text (fresh process per step, so state persists via files). Use it after fs_write/fs_edit/fs_patch in codegen mode, alongside workspace_diagnostics, before declaring a program complete.",
	map[string]any{
		"steps": map[string]any{"type": "array", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"args":   arrayProp("command-line arguments for this run (empty = none)"),
				"input":  strProp("stdin to feed this run (default: minimal q/newlines)"),
				"expect": strProp("text the program output must contain for this step to pass"),
			},
		}, "description": "optional behavioral assertions: [{args, input, expect}, ...]"},
	}, []string{})

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
	var args struct {
		Steps []smokeStep `json:"steps"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return "", err
	}
	return t.smoke(ctx, args.Steps)
}

// smoke determines build+run plan for the workspace and executes it. With steps
// it runs a behavioral probe: each step launches a fresh process (state
// persists via files on disk), feeds the step's stdin, and asserts the output
// contains the expected text.
func (t *smokeTool) smoke(ctx context.Context, steps []smokeStep) (string, error) {
	plan, ok := t.buildPlan(ctx)
	if !ok {
		return "runtime_smoke: no smoke runner for this project. The program could not be executed, so its runtime safety is unverified.", nil
	}
	if plan.clean != nil {
		defer plan.clean()
	}
	if len(steps) > 0 {
		// Strict steps: every step must assert real output, otherwise a model
		// could game the gate with [{"input":"x"}] (no expect -> always PASS).
		// An assertion is expected text that could actually be present.
		hasAssertion := false
		for _, s := range steps {
			if s.Expect != "" {
				hasAssertion = true
				break
			}
		}
		if !hasAssertion {
			return "runtime_smoke FAIL: steps must assert expected output — every behavioral step needs a non-empty \"expect\" (text the program must print/display). A smoke run with no expectations proves nothing.", nil
		}
		return t.probe(ctx, plan, steps)
	}
	res := t.runSmoke(ctx, plan, nil, smokeScriptedInput)
	if res.skip {
		return "runtime_smoke: no smoke runner for this project.", nil
	}
	if !res.ok {
		return fmt.Sprintf("runtime_smoke FAIL: %s\n%s", res.detail, res.status), nil
	}
	return fmt.Sprintf("runtime_smoke PASS: %s\n%s", res.status, res.detail), nil
}

// probe runs each behavioral step and reports PASS only when every step's
// output contains its expected text (and no step crashed).
func (t *smokeTool) probe(ctx context.Context, plan runPlan, steps []smokeStep) (string, error) {
	for i, step := range steps {
		input := step.Input
		if input == "" {
			input = smokeScriptedInput
		}
		res := t.runSmoke(ctx, plan, step.Args, input)
		if res.skip {
			return "runtime_smoke FAIL: no smoke runner for this project.", nil
		}
		if !res.ok {
			return fmt.Sprintf("runtime_smoke FAIL: step %d crashed\n%s\n%s", i+1, res.detail, res.status), nil
		}
		if step.Expect != "" && !strings.Contains(res.detail, step.Expect) {
			return fmt.Sprintf("runtime_smoke FAIL: step %d output missing %q\n%s", i+1, step.Expect, capResult(res.detail, smokeMaxOutput)), nil
		}
	}
	return fmt.Sprintf("runtime_smoke PASS: all %d behavioral step(s) produced the expected output", len(steps)), nil
}

// smokeStep is one behavioral assertion.
type smokeStep struct {
	Args   []string
	Input  string
	Expect string
}

// smokeResult is the deterministic verdict + evidence.
type smokeResult struct {
	skip   bool   // no smoke runner for this project type
	ok     bool   // survived the run window without crashing
	detail string // crash reason or a sample of stdout
	status string // short summary for the model ("ran ~3s, exited 0")
}

// runPlan describes how to execute the generated program.
type runPlan struct {
	bin   string // compiled binary, or a script path to run with cmd
	cmd   string // interpreter for scripts ("", "python3", "node")
	ws    string // working dir (defaults to t.ws)
	clean func()
	// jsEntry, when js is set, is the .js file a node DOM shim should load.
	js bool
}

// buildPlan returns the runnable plan for the workspace, or ok=false when no
// smoke runner applies. Go/C++/Rust are compiled; Python runs directly; a
// browser JS game runs under a node DOM shim (canvas + document stubs).
func (t *smokeTool) buildPlan(ctx context.Context) (runPlan, bool) {
	if t.ws == "" {
		return runPlan{}, false
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
			return runPlan{}, false
		}
		// A library package (no func main) builds to a non-executable archive —
		// running it would fail with permission denied and mislead the model
		// into "sandbox" rabbit holes. Detect and skip: no main = nothing to
		// execute, so smoke is not applicable (the compile gate covers it).
		fi, err := os.Stat(tmp)
		if err != nil || fi.Mode()&0o111 == 0 {
			_ = os.Remove(tmp)
			return runPlan{}, false
		}
		return runPlan{bin: tmp, clean: func() { _ = os.Remove(tmp) }}, true
	case has("Cargo.toml"):
		// cargo build then the debug binary.
		if err := t.build(ctx, "cargo", "build", "--quiet"); err != nil {
			return runPlan{}, false
		}
		bin := filepath.Join(t.ws, "target", "debug")
		if !hasCargoBinary(bin) {
			return runPlan{}, false
		}
		return runPlan{bin: bin}, true
	case hasCProject(t.ws):
		sources, _ := cSources(t.ws)
		if len(sources) == 0 {
			return runPlan{}, false
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
				return runPlan{}, false
			}
		}
		return runPlan{bin: tmp, clean: func() { _ = os.Remove(tmp) }}, true
	case hasPythonEntry(t.ws):
		// python3 <entry>.py directly.
		return runPlan{cmd: "python3", bin: pythonEntry(t.ws)}, true
	case t.jsEntry() != "":
		// Browser JS: run under a node DOM shim so canvas games execute.
		// Require node; otherwise the browser game can't be verified headless.
		lp := t.lookPath
		if lp == nil {
			lp = exec.LookPath
		}
		if _, err := lp("node"); err != nil {
			return runPlan{}, false
		}
		plan := runPlan{cmd: "node", bin: t.jsEntry(), js: true}
		plan.clean = t.writeJSShim()
		return plan, true
	}
	return runPlan{}, false
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
func hasPythonEntry(ws string) bool { return pythonEntry(ws) != "" }

// pythonEntry returns the .py entry point to run (main.py preferred, else the
// first .py in the workspace root), or "" when none exists.
func pythonEntry(ws string) string {
	if _, err := os.Stat(filepath.Join(ws, "main.py")); err == nil {
		return filepath.Join(ws, "main.py")
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".py") {
			return filepath.Join(ws, e.Name())
		}
	}
	return ""
}

// jsEntry finds a browser-side .js entry point: a root-level main.js/app.js/
// game.js, else the first root .js file that is not a module bundle. Returns ""
// when none exists (nothing to smoke).
func (t *smokeTool) jsEntry() string {
	prefer := []string{"main.js", "app.js", "game.js", "index.js"}
	for _, name := range prefer {
		p := filepath.Join(t.ws, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	entries, err := os.ReadDir(t.ws)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".js") {
			return filepath.Join(t.ws, e.Name())
		}
	}
	return ""
}

// runSmoke executes the runnable target with the given args+stdin and judges
// survival: did it crash, or did it survive (clean exit or alive to timeout)?
func (t *smokeTool) runSmoke(ctx context.Context, plan runPlan, args []string, input string) smokeResult {
	var (
		cmd *exec.Cmd
		dir = plan.ws
	)
	if dir == "" {
		dir = t.ws
	}
	switch {
	case plan.js:
		// node <shim.js> <entry.js> — the shim stubs document/canvas, loads the
		// entry, dispatches scripted keys, and dumps DOM state at exit.
		shim := filepath.Join(t.ws, ".yagent-smoke-shim.js")
		cmd = exec.CommandContext(ctx, "node", shim, plan.bin)
		cmd.Env = append(scrubEnv(os.Environ()), "YAGENT_SMOKE_KEYS="+strings.Join(args, "|"))
	case plan.cmd != "":
		cmd = exec.CommandContext(ctx, plan.cmd, append([]string{plan.bin}, args...)...)
	default:
		cmd = exec.CommandContext(ctx, plan.bin, args...)
	}
	cmd.Dir = dir
	cmd.Env = scrubEnv(os.Environ())
	if plan.js {
		cmd.Env = append(cmd.Env, "YAGENT_SMOKE_KEYS="+strings.Join(args, "|"))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = strings.NewReader(input)
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
	// The shim prints a ----YAGENT-DOM---- marker line carrying the observable
	// DOM state (score text etc.); merge it into the judged output so steps
	// can assert on displayed text, not just console logs.
	if plan.js {
		if dom := extractDOMState(out.Bytes()); dom != "" {
			output += "\n[dom] " + dom
		}
	}
	if msg, crashed := smokeCrashReason(waitErr, elapsed, output); crashed {
		return smokeResult{ok: false, detail: msg, status: fmt.Sprintf("crashed after ~%s", elapsed.Round(time.Millisecond))}
	}
	// Survived: either exited cleanly or kept running (an interactive loop)
	// until the timeout — both prove the program doesn't blow up on first input.
	sample := capResult(output, smokeMaxOutput)
	verb := "ran"
	if waitErr != nil && waitErr != context.DeadlineExceeded && waitErr != context.Canceled {
		verb = "exited"
	}
	return smokeResult{ok: true, detail: sample, status: fmt.Sprintf("%s ~%s (exit %v)", verb, elapsed.Round(time.Millisecond), exitCode(waitErr))}
}

// writeJSShim writes the node DOM shim into the workspace and returns a cleanup.
// The shim stubs document/canvas, loads the entry with REAL timers (so
// setTimeout/requestAnimationFrame loops advance), dispatches the scripted
// arrow keys over a short window, then exits — dumping the captured DOM text
// state so steps can assert on displayed score/messages.
func (t *smokeTool) writeJSShim() func() {
	path := filepath.Join(t.ws, ".yagent-smoke-shim.js")
	shim := `const listeners = {};
const texts = {};
const rafCallbacks = [];
global.document = {
  getElementById: (id) => ({
    _text: "",
    set textContent(v) { this._text = String(v); texts[id] = String(v); },
    get textContent() { return this._text; },
    style: {},
    width: 0, height: 0,
    addEventListener: (ev, fn) => { (listeners[ev] = listeners[ev] || []).push(fn); },
    getContext: () => new Proxy({}, { get: () => () => {}, set: () => true }),
  }),
  addEventListener: (ev, fn) => { (listeners[ev] = listeners[ev] || []).push(fn); },
  createElement: () => ({ style: {}, textContent: "", appendChild: () => {} }),
  body: { appendChild: () => {} },
};
global.window = {
  onload: null,
  addEventListener: (ev, fn) => { (listeners[ev] = listeners[ev] || []).push(fn); },
  innerWidth: 800, innerHeight: 600,
};
global.requestAnimationFrame = (fn) => { rafCallbacks.push(fn); return rafCallbacks.length; };
const entry = process.argv[2];
require(entry);
if (typeof window.onload === "function") window.onload();
if (listeners.load) listeners.load.forEach((f) => f());
const keys = (process.env.YAGENT_SMOKE_KEYS || "").split("|").filter(Boolean);
const raf = () => { const fns = rafCallbacks.splice(0); fns.forEach((f) => f(0)); };
let step = 0;
const iv = setInterval(() => {
  if (step < keys.length) {
    const k = keys[step];
    if (listeners.keydown) listeners.keydown.forEach((h) => h({ key: k }));
  }
  raf();
  step++;
  if (step >= 40) { clearInterval(iv); console.log("----YAGENT-DOM---- " + JSON.stringify(texts)); process.exit(0); }
}, 10);
`
	_ = os.WriteFile(path, []byte(shim), 0o644)
	return func() { _ = os.Remove(path) }
}

// extractDOMState pulls the shim's DOM-state marker out of the captured output.
func extractDOMState(out []byte) string {
	for _, ln := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(ln, "----YAGENT-DOM---- ") {
			return strings.TrimPrefix(ln, "----YAGENT-DOM---- ")
		}
	}
	return ""
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
		"nil pointer", "fatal error",
		// node/js crash signatures (cloud models ship browser code more often).
		"typeerror", "referenceerror", "syntaxerror", "is not a function", "is not defined",
		"cannot read properties", "cannot read property", "is undefined",
	} {
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
