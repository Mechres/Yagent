package tools

import (
	"context"
	"errors"
	"os"
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
