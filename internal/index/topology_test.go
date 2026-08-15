package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildTopology(t *testing.T) {
	ws := t.TempDir()
	writeTopo := func(rel, content string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTopo("go.mod", "module example.com/acme\n\ngo 1.22\n")
	writeTopo("cmd/yagent/main.go", "package main\n\nimport (\n\t\"example.com/acme/internal/agent\"\n\t\"example.com/acme/internal/ui\"\n)\n\nfunc main() { _ = agent.New; _ = ui.X }\n")
	writeTopo("internal/agent/agent.go", "package agent\n\nimport (\n\t\"example.com/acme/internal/llm\"\n\t\"example.com/acme/internal/tools\"\n)\n\nfunc New() {}\n")
	writeTopo("internal/llm/llm.go", "package llm\n\nfunc X() {}\n")
	writeTopo("internal/tools/tools.go", "package tools\n\nfunc X() {}\n")
	writeTopo("internal/ui/tui.go", "package ui\n\nimport \"example.com/acme/internal/llm\"\n\nfunc X() {}\n")
	// a .gitignore-ignored dir must be excluded
	writeTopo("vendor/dep/dep.go", "package dep\n\nimport \"fmt\"\n")
	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("vendor/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	topo, err := BuildTopology(ws)
	if err != nil {
		t.Fatalf("BuildTopology: %v", err)
	}
	if topo.Module != "example.com/acme" {
		t.Errorf("module = %q", topo.Module)
	}
	got := topo.Render()
	for _, want := range []string{
		"cmd/yagent -> internal/agent, internal/ui",
		"internal/agent -> internal/llm, internal/tools",
		"internal/ui -> internal/llm",
		"entry: cmd/yagent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Render missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "vendor") {
		t.Errorf("vendor leaked into topology:\n%s", got)
	}
	// llm and tools have no local imports
	if !strings.Contains(got, "internal/llm (no local imports)") {
		t.Errorf("leaf package missing:\n%s", got)
	}
}

func TestTopologyExternalImportsExcluded(t *testing.T) {
	ws := t.TempDir()
	writeTopo := func(rel, content string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTopo("go.mod", "module example.com/acme\n")
	writeTopo("a/a.go", "package a\n\nimport \"github.com/foo/bar\"\n")
	topo, err := BuildTopology(ws)
	if err != nil {
		t.Fatal(err)
	}
	if got := topo.Render(); strings.Contains(got, "foo/bar") {
		t.Errorf("external import leaked: %s", got)
	}
}

func TestOrderByDeps(t *testing.T) {
	// AGY #5: upstream (leaf) packages must sort before the callers that
	// import them, so a model fixes the definition before the call sites.
	ws := t.TempDir()
	writeTopo := func(rel, content string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTopo("go.mod", "module example.com/acme\n")
	writeTopo("internal/api/types.go", "package api\n")
	writeTopo("internal/service/svc.go", "package service\n\nimport \"example.com/acme/internal/api\"\n")
	writeTopo("cmd/server/main.go", "package main\n\nimport \"example.com/acme/internal/service\"\n")

	topo, err := BuildTopology(ws)
	if err != nil {
		t.Fatal(err)
	}
	order := topo.OrderByDeps(map[string]bool{
		"cmd/server":       true,
		"internal/service": true,
		"internal/api":     true,
	})
	want := []string{"internal/api", "internal/service", "cmd/server"}
	if len(order) != len(want) {
		t.Fatalf("OrderByDeps = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("OrderByDeps[%d] = %q, want %q (full: %v)", i, order[i], want[i], order)
		}
	}
	// A subset that keeps the chain: with the intermediate package absent the
	// dependency edge is lost (cmd/server no longer imports anything in the
	// set), so ordering falls back to lexical — verify the chain holds when
	// service is present.
	order2 := topo.OrderByDeps(map[string]bool{"cmd/server": true, "internal/service": true, "internal/api": true})
	if order2[0] != "internal/api" || order2[2] != "cmd/server" {
		t.Errorf("subset OrderByDeps = %v, want api first, cmd/server last", order2)
	}
}
