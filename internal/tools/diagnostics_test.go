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

func TestWorkspaceDiagnosticsDetectC(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "src"), 0o755)
	if err := os.WriteFile(filepath.Join(ws, "src", "main.c"), []byte("int main(void) { return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotName, gotArgs string
	tool := &diagnosticsTool{
		ws:       ws,
		lookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			gotName, gotArgs = name, strings.Join(args, " ")
			return []byte("no issues"), nil
		},
	}
	res, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "gcc" || !strings.Contains(gotArgs, "-fsyntax-only") {
		t.Errorf("cmd = %s %s, want gcc -fsyntax-only", gotName, gotArgs)
	}
	if !strings.Contains(gotArgs, "main.c") {
		t.Errorf("args missing source file: %s", gotArgs)
	}
	if !strings.Contains(res, "no issues") {
		t.Errorf("result = %q", res)
	}
}

func TestDetectDiagnosticsCppPrefersGpp(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "src"), 0o755)
	os.WriteFile(filepath.Join(ws, "src", "game.cpp"), []byte("int main() { return 0; }\n"), 0o644)
	name, args, ok := detectDiagnostics(ws, func(s string) (string, error) { return "/usr/bin/" + s, nil })
	if !ok {
		t.Fatal("C++ project not detected")
	}
	if name != "g++" {
		t.Errorf("name = %q, want g++ for a .cpp project", name)
	}
	if len(args) < 2 || args[0] != "-fsyntax-only" {
		t.Errorf("args = %v", args)
	}
}

func TestDetectDiagnosticsPrefersBuildSystem(t *testing.T) {
	// A CMake project with an existing configured build dir must use
	// `cmake --build build` (real compile), NOT bare gcc -fsyntax-only which
	// false-errors on missing include dirs (2026-08-13 real-use finding).
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "CMakeLists.txt"), []byte("cmake_minimum_required(VERSION 3.16)\nproject(x)\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "main.cpp"), []byte("int main() { return 0; }\n"), 0o644)
	os.MkdirAll(filepath.Join(ws, "build"), 0o755)
	os.WriteFile(filepath.Join(ws, "build", "CMakeCache.txt"), []byte("x"), 0o644)

	name, args, ok := detectDiagnostics(ws, func(s string) (string, error) { return "/usr/bin/" + s, nil })
	if !ok {
		t.Fatal("CMake project not detected")
	}
	if name != "cmake" || len(args) < 3 || args[0] != "--build" {
		t.Errorf("got %s %v, want cmake --build <dir> -j", name, args)
	}
	// A bare Makefile also wins over the syntax pass.
	ws2 := t.TempDir()
	os.WriteFile(filepath.Join(ws2, "Makefile"), []byte("all:\n"), 0o644)
	os.WriteFile(filepath.Join(ws2, "main.c"), []byte("int main(void) { return 0; }\n"), 0o644)
	name2, _, ok2 := detectDiagnostics(ws2, func(s string) (string, error) { return "/usr/bin/" + s, nil })
	if !ok2 || name2 != "make" {
		t.Errorf("Makefile project = %q (%v), want make", name2, ok2)
	}
}
