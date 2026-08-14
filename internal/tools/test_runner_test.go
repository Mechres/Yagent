package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestRunnerTool(t *testing.T, ws string) *testRunnerTool {
	t.Helper()
	return &testRunnerTool{
		ws:       ws,
		lookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
	}
}

func TestTestRunnerGoSymbolScope(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(ws, "pkg"), 0o755)
	os.WriteFile(filepath.Join(ws, "pkg/helper.go"), []byte("package pkg\n\nfunc helper() int { return 1 }\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "pkg/helper_test.go"), []byte("package pkg\n\nfunc TestHelper(t *testing.T) { _ = helper() }\n"), 0o644)

	var gotCmd string
	tool := newTestRunnerTool(t, ws)
	tool.runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		gotCmd = name + " " + strings.Join(args, " ")
		return []byte("ok  \tgithub.com/x/pkg\t0.001s\n"), nil
	}
	res, err := tool.Execute(context.Background(), []byte(`{"scope":"symbol","path":"pkg/helper.go","symbol":"helper"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotCmd, "go test ./pkg") || !strings.Contains(gotCmd, "Test") {
		t.Errorf("cmd = %q, want go test ./pkg -run Test...", gotCmd)
	}
	if !strings.Contains(res, "ok") {
		t.Errorf("result = %q", res)
	}
}

func TestTestRunnerGoFailurePruned(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newTestRunnerTool(t, ws)
	tool.runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(`--- FAIL: TestHelper (0.00s)
    helper_test.go:9: got 2 want 1
FAIL
FAIL	github.com/x	0.100s
`), nil
	}
	res, err := tool.Execute(context.Background(), []byte(`{"scope":"package"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "FAIL") || !strings.Contains(res, "helper_test.go:9") {
		t.Errorf("failure not surfaced: %q", res)
	}
}

func TestTestRunnerPythonFileScope(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "requirements.txt"), []byte("pytest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(ws, "app"), 0o755)
	os.WriteFile(filepath.Join(ws, "app/test_util.py"), []byte("def test_x():\n    pass\n"), 0o644)

	var gotCmd string
	tool := newTestRunnerTool(t, ws)
	tool.runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		gotCmd = name + " " + strings.Join(args, " ")
		return []byte("1 passed\n"), nil
	}
	res, err := tool.Execute(context.Background(), []byte(`{"scope":"file","path":"app/test_util.py"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotCmd, "pytest app/test_util.py") {
		t.Errorf("cmd = %q, want pytest app/test_util.py", gotCmd)
	}
	if !strings.Contains(res, "1 passed") {
		t.Errorf("result = %q", res)
	}
}

func TestTestRunnerRust(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "Cargo.toml"), []byte("[package]\nname = \"game\"\nversion = \"0.1.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotCmd string
	tool := newTestRunnerTool(t, ws)
	tool.runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		gotCmd = name + " " + strings.Join(args, " ")
		return []byte("test result: ok. 1 passed; 0 failed\n"), nil
	}
	// symbol scope -> cargo test -- <symbol>
	res, err := tool.Execute(context.Background(), []byte(`{"scope":"symbol","symbol":"physics"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotCmd, "cargo test -- physics") {
		t.Errorf("symbol cmd = %q, want cargo test -- physics", gotCmd)
	}
	if !strings.Contains(res, "1 passed") {
		t.Errorf("result = %q", res)
	}
	// package scope -> cargo test
	gotCmd = ""
	res2, err := tool.Execute(context.Background(), []byte(`{"scope":"package"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotCmd, "cargo test") || strings.Contains(gotCmd, "-- physics") {
		t.Errorf("package cmd = %q", gotCmd)
	}
	if !strings.Contains(res2, "1 passed") {
		t.Errorf("package result = %q", res2)
	}
}

func TestTestRunnerValidation(t *testing.T) {
	ws := t.TempDir()
	tool := newTestRunnerTool(t, ws)
	// unknown scope
	if _, err := tool.Execute(context.Background(), []byte(`{"scope":"bogus"}`)); err == nil {
		t.Error("bad scope not rejected")
	}
	// symbol scope without symbol
	if _, err := tool.Execute(context.Background(), []byte(`{"scope":"symbol"}`)); err == nil {
		t.Error("symbol missing not rejected")
	}
	// no project
	tool2 := newTestRunnerTool(t, ws)
	if res, err := tool2.Execute(context.Background(), []byte(`{"scope":"package"}`)); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(res, "no test framework") {
		t.Errorf("no project = %q", res)
	}
}
