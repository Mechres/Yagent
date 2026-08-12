package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceDiagnosticsDetectGo(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotName, gotArgs string
	tool := &diagnosticsTool{
		ws:       ws,
		lookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			gotName, gotArgs = name, strings.Join(args, " ")
			return []byte("vet: no issues"), nil
		},
	}
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "go" || gotArgs != "vet ./..." {
		t.Errorf("cmd = %s %s, want go vet ./...", gotName, gotArgs)
	}
	if !strings.Contains(res, "no issues") {
		t.Errorf("result = %q", res)
	}
}

func TestWorkspaceDiagnosticsNoProject(t *testing.T) {
	tool := &diagnosticsTool{ws: t.TempDir(), lookPath: func(s string) (string, error) { return s, nil }}
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "no diagnostics configured") {
		t.Errorf("result = %q", res)
	}
}

func TestWorkspaceDiagnosticsCommandError(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &diagnosticsTool{
		ws:       ws,
		lookPath: func(s string) (string, error) { return s, nil },
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("go vet failed: syntax error")
		},
	}
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "go vet failed") {
		t.Errorf("result = %q", res)
	}
}
