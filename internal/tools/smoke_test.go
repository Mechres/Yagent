package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSmokeCrashReason(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		out    string
		wantOk bool // true = survived
	}{
		{"clean exit", nil, "hello world\n", true},
		{"interactive loop alive to timeout", context.DeadlineExceeded, "menu:\n> ", true},
		{"go panic", nil, "panic: index out of range\n", false},
		{"segfault marker", nil, "Segmentation fault (core dumped)\n", false},
		{"cpp assertion", nil, "Assertion `__n < this->size()' failed.\n", false},
		{"silent non-zero exit", errors.New("exit status 1"), "", false},
		{"runtime error", nil, "runtime error: invalid memory address\n", false},
		{"fatal error", nil, "fatal error: concurrent map writes\n", false},
	}
	for _, c := range cases {
		msg, crashed := smokeCrashReason(c.err, 0, c.out)
		if crashed == c.wantOk {
			t.Errorf("%s: crashed=%v wantOk=%v (msg=%q)", c.name, crashed, c.wantOk, msg)
		}
	}
}

func TestSmokeToolGoBuildAndRun(t *testing.T) {
	// A real Go program (no injection): builds with `go build` and runs to
	// completion. This proves the end-to-end build+run path works.
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module smoke\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte(`package main

import "fmt"

func main() {
	for i := 0; i < 3; i++ {
		fmt.Println("tick", i)
	}
}
`), 0o644)

	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "runtime_smoke PASS") {
		t.Errorf("expected PASS, got: %s", res)
	}
}

func TestSmokeToolGoCrashes(t *testing.T) {
	// The model's classic greenfield failure: compiles clean, panics on run.
	// The smoke gate must catch it deterministically.
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module smoke\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte(`package main

func main() {
	var s []int
	_ = s[5]
}
`), 0o644)

	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "runtime_smoke FAIL") {
		t.Errorf("expected FAIL (panic), got: %s", res)
	}
	if !strings.Contains(strings.ToLower(res), "panic") {
		t.Errorf("FAIL should cite the crash marker, got: %s", res)
	}
}

func TestSmokeToolNoRunner(t *testing.T) {
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "README.md"), []byte("nothing to smoke\n"), 0o644)
	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "no smoke runner") {
		t.Errorf("expected no-runner message, got: %s", res)
	}
}

func TestSmokeToolBuildFailure(t *testing.T) {
	// A Go file that doesn't compile -> no smoke runner result; the compile
	// gate (workspace_diagnostics) owns that failure, smoke stays silent.
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module smoke\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc broken(\n"), 0o644)
	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	// Build failure must NOT be reported as a crash (it isn't a runtime issue);
	// it should degrade to a no-runner message (unverifiable), leaving the
	// compile gate to report the actual error.
	if strings.Contains(res, "runtime_smoke FAIL") {
		t.Errorf("build failure misreported as smoke crash: %s", res)
	}
}

func TestSmokeToolLibraryPackageSkips(t *testing.T) {
	// A Go LIBRARY package (no func main) builds to a non-executable archive.
	// Running it would fail with permission denied and mislead the model into
	// a "sandbox" rabbit hole (found live 2026-08-14). Smoke must skip — the
	// compile gate covers the library; there is nothing to execute.
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module lib\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644)
	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "no smoke runner") {
		t.Errorf("library package should skip smoke (no main), got: %s", res)
	}
	if strings.Contains(res, "runtime_smoke FAIL") {
		t.Errorf("library package must not FAIL smoke: %s", res)
	}
}

func TestSmokeToolPythonRuns(t *testing.T) {
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "main.py"), []byte(`print("py ok")`), 0o644)
	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "runtime_smoke PASS") {
		t.Errorf("expected PASS, got: %s", res)
	}
}

// todoApp returns a small Go todo CLI: add <text> persists to todos.json and
// list prints it back. Broken persists but never loads on a fresh run — the
// exact dead-persistence bug from the live test drive (v0.1.58).
const workingTodo = `package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type Todo struct {
	ID   int    ` + "`json:\"id\"`" + `
	Text string ` + "`json:\"text\"`" + `
}

func load() []Todo {
	var ts []Todo
	if b, err := os.ReadFile("todos.json"); err == nil {
		_ = json.Unmarshal(b, &ts)
	}
	return ts
}

func save(ts []Todo) {
	b, _ := json.Marshal(ts)
	_ = os.WriteFile("todos.json", b, 0o644)
}

func main() {
	ts := load()
	switch os.Args[1] {
	case "add":
		ts = append(ts, Todo{ID: len(ts) + 1, Text: os.Args[2]})
		save(ts)
		fmt.Println("Added:", os.Args[2])
	case "list":
		for _, t := range ts {
			fmt.Printf("%d %s\n", t.ID, t.Text)
		}
	}
}
`

const brokenTodo = `package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type Todo struct {
	ID   int    ` + "`json:\"id\"`" + `
	Text string ` + "`json:\"text\"`" + `
}

var ts []Todo

func save(ts []Todo) {
	b, _ := json.Marshal(ts)
	_ = os.WriteFile("todos.json", b, 0o644)
}

func main() {
	// BUG: never calls load(), so a fresh ` + "`list`" + ` run sees nothing —
	// the v0.1.58 test-drive failure.
	switch os.Args[1] {
	case "add":
		ts = append(ts, Todo{ID: len(ts) + 1, Text: os.Args[2]})
		save(ts)
		fmt.Println("Added:", os.Args[2])
	case "list":
		for _, t := range ts {
			fmt.Printf("%d %s\n", t.ID, t.Text)
		}
	}
}
`

func TestSmokeBehavioralStepsTodoWorking(t *testing.T) {
	// A todo app with working persistence: `add buy milk` then a FRESH `list`
	// run must show the item — the cross-invocation assertion that catches the
	// dead-persistence bug found in the v0.1.58 live test drive.
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module todo\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte(workingTodo), 0o644)

	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{"steps":[
		{"args":["add","buy milk"],"expect":"Added: buy milk"},
		{"args":["list"],"expect":"buy milk"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "runtime_smoke PASS") {
		t.Errorf("working todo should pass behavioral steps, got: %s", res)
	}
}

func TestSmokeBehavioralStepsTodoBrokenPersistence(t *testing.T) {
	// The broken todo (never loads on a fresh run) compiles and runs but a
	// fresh `list` shows nothing. The behavioral probe must catch it even
	// though a crash-only smoke would PASS.
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module todo\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte(brokenTodo), 0o644)

	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{"steps":[
		{"args":["add","buy milk"],"expect":"Added: buy milk"},
		{"args":["list"],"expect":"buy milk"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "runtime_smoke FAIL") {
		t.Errorf("broken persistence should FAIL behavioral steps, got: %s", res)
	}
	if !strings.Contains(res, "missing \"buy milk\"") {
		t.Errorf("FAIL should cite the missing output, got: %s", res)
	}
}

func TestSmokeBehavioralStepsInteractive(t *testing.T) {
	// A stdin-driven interactive app: each step feeds input, output must
	// contain the expected echo.
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "main.py"), []byte(`while True:
    line = input()
    if line == "q": break
    print("echo", line)
`), 0o644)
	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{"steps":[
		{"input":"hello\n","expect":"echo hello"},
		{"input":"q\n","expect":""}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "runtime_smoke PASS") {
		t.Errorf("interactive echo with steps should pass, got: %s", res)
	}
}

func TestSmokeBehavioralStepsExpectFail(t *testing.T) {
	// The probe asserts output; when the expected text is absent the gate must
	// fail even though the program runs fine (behavioral bug, not a crash).
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "main.py"), []byte(`print("no data here")`), 0o644)
	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{"steps":[
		{"input":"","expect":"the missing thing"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "runtime_smoke FAIL") || !strings.Contains(res, "missing") {
		t.Errorf("expected FAIL citing the missing text, got: %s", res)
	}
}

func TestSmokeStrictStepsRejectsNoAssertion(t *testing.T) {
	// A model gaming the gate: steps with no expect at all would always PASS.
	// Strict validation must refuse them — a probe without assertions proves
	// nothing.
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "main.py"), []byte(`print("hi")`), 0o644)
	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{"steps":[
		{"input":"x"},
		{"input":"y"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "runtime_smoke FAIL") || !strings.Contains(res, "expect") {
		t.Errorf("no-assertion steps must be refused, got: %s", res)
	}
}

func TestSmokeJSGameLoadsAndDOMState(t *testing.T) {
	// A browser JS game (canvas + DOM). The node shim must load it, run its
	// onload, dispatch scripted keys, and expose the score via DOM state so a
	// behavioral step can assert it.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "game.js"), []byte(`
const canvas = document.getElementById("game");
const ctx = canvas.getContext("2d");
const score = document.getElementById("score");
let s = 0;
document.addEventListener("keydown", (e) => {
  if (e.key === "ArrowRight") s++;
  score.textContent = "Score: " + s;
});
window.onload = () => { score.textContent = "Score: 0"; };
`), 0o644)
	tool := &smokeTool{ws: ws}
	// crash-only: must PASS (the game loads and runs under the shim)
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "runtime_smoke PASS") {
		t.Errorf("JS game should pass crash smoke, got: %s", res)
	}
	// behavioral: pressing ArrowRight increments the DOM score
	res, err = tool.Execute(context.Background(), []byte(`{"steps":[
		{"args":["ArrowRight","ArrowRight"],"expect":"Score: 2"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "runtime_smoke PASS") {
		t.Errorf("JS DOM-state expect should pass, got: %s", res)
	}
}

func TestSmokeJSGameThrowsOnLoad(t *testing.T) {
	// A JS game that crashes on load (undefined reference) must FAIL — the
	// shim surfaces the uncaught exception as a crash.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "game.js"), []byte(`
const canvas = document.getElementById("game");
canvas.width = 400;
const missing = null;
missing.x = 1; // TypeError at load
`), 0o644)
	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "runtime_smoke FAIL") {
		t.Errorf("throwing JS game should FAIL smoke, got: %s", res)
	}
}

func TestSmokeJSGamingVector(t *testing.T) {
	// The snake game's growth bug: `list`-style behavior is DOM state; but the
	// KEY vector is a game that dies on first food silently (gameOver(), no
	// throw). A behavioral expect on the score that never reaches 1 catches it.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	ws := t.TempDir()
	// snake that increments the score ONLY when it reaches food at x=40, but a
	// bug makes it die before eating (score stuck at 0).
	os.WriteFile(filepath.Join(ws, "game.js"), []byte(`
const score = document.getElementById("score");
let x = 0, s = 0;
document.addEventListener("keydown", (e) => {
  if (e.key === "ArrowRight") x += 20;
  if (x >= 100) { s++; score.textContent = "Score: " + s; return; }
  if (x > 120) { gameOver(); } // never reached -> bug
});
window.onload = () => { score.textContent = "Score: 0"; };
function gameOver() {}
`), 0o644)
	tool := &smokeTool{ws: ws}
	res, err := tool.Execute(context.Background(), []byte(`{"steps":[
		{"args":["ArrowRight","ArrowRight","ArrowRight","ArrowRight","ArrowRight","ArrowRight"],"expect":"Score: 1"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	// The bug means the score never advances -> FAIL (caught behaviorally).
	if !strings.Contains(res, "runtime_smoke FAIL") {
		t.Errorf("snake-growth bug should FAIL the behavioral probe, got: %s", res)
	}
}
