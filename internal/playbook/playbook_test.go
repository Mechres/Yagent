package playbook

import (
	"os"
	"path/filepath"
	"testing"
)

func writePlaybook(t *testing.T, ws, name, content string) {
	t.Helper()
	dir := Dir(ws)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAndList(t *testing.T) {
	ws := t.TempDir()
	writePlaybook(t, ws, "audit", `name: security-audit
description: run a security audit
phases:
  - goal: "Inspect the repo for security issues."
    rounds: 4
    tools: [fs_read, grep, glob, code_references]
    success: "no critical findings remain"
  - goal: "Write the findings to AUDIT.md"
    tools: [fs_write, fs_read]
`)

	names := List(ws)
	if len(names) != 1 || names[0] != "audit" {
		t.Fatalf("List = %v", names)
	}
	pb, err := Load(ws, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if pb.Name != "security-audit" || len(pb.Phases) != 2 {
		t.Errorf("playbook = %+v", pb)
	}
	if pb.Phases[0].Rounds != 4 || len(pb.Phases[0].Tools) != 4 {
		t.Errorf("phase 0 = %+v", pb.Phases[0])
	}
	if pb.Phases[1].Rounds != 0 { // default rounds handled at run time
		t.Errorf("phase 1 rounds = %d, want 0", pb.Phases[1].Rounds)
	}
}

func TestSuccessPredicates(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws, "pkg/a.go"), []byte("func helper() {}\n"), 0o644)

	// passing checks
	checks := []Check{
		{FileContains: &FileAssert{Path: "pkg/a.go", Text: "helper"}},
		{FileExists: "pkg/a.go"},
		{FileNotContains: &FileAssert{Path: "pkg/a.go", Text: "TODOnever"}},
	}
	for _, c := range checks {
		if fails := c.Evaluate(ws); len(fails) != 0 {
			t.Errorf("check should pass: %+v -> %v", c, fails)
		}
	}
	// failing checks
	if fails := (Check{FileContains: &FileAssert{Path: "pkg/a.go", Text: "absent"}}).Evaluate(ws); len(fails) != 1 {
		t.Errorf("missing text should fail: %v", fails)
	}
	if fails := (Check{FileExists: "nope.go"}).Evaluate(ws); len(fails) != 1 {
		t.Errorf("missing file should fail: %v", fails)
	}
	if fails := (Check{FileNotContains: &FileAssert{Path: "pkg/a.go", Text: "helper"}}).Evaluate(ws); len(fails) != 1 {
		t.Errorf("present text under file_not_contains should fail: %v", fails)
	}

	// a playbook phase with checks parses and reports HasChecks
	writePlaybook(t, ws, "checks", `name: checks
phases:
  - goal: "write the file"
    checks:
      - file_contains: {path: out.txt, text: done}
      - file_exists: out.txt
`)
	pb, err := Load(ws, "checks")
	if err != nil {
		t.Fatal(err)
	}
	if !pb.Phases[0].HasChecks() || len(pb.Phases[0].Checks) != 2 {
		t.Errorf("phase checks = %+v", pb.Phases[0].Checks)
	}

	// a phase with a tests: predicate parses and is detected as having checks
	writePlaybook(t, ws, "tdd", `name: tdd
phases:
  - goal: "implement and verify"
    checks:
      - tests: physics
      - diagnostics: true
`)
	pb2, err := Load(ws, "tdd")
	if err != nil {
		t.Fatal(err)
	}
	ch := pb2.Phases[0].Checks
	if len(ch) != 2 || !pb2.Phases[0].HasChecks() {
		t.Fatalf("tdd phase checks = %+v", ch)
	}
	if ch[0].TestsPass != "physics" {
		t.Errorf("TestsPass = %q, want physics", ch[0].TestsPass)
	}
	if !ch[1].DiagnosticsPass {
		t.Errorf("DiagnosticsPass = false, want true")
	}
}

func TestLoadValidation(t *testing.T) {
	ws := t.TempDir()
	writePlaybook(t, ws, "empty", "name: empty\nphases: []\n")
	if _, err := Load(ws, "empty"); err == nil {
		t.Error("empty phases should fail")
	}
	writePlaybook(t, ws, "nogoal", "name: nogoal\nphases:\n  - rounds: 3\n")
	if _, err := Load(ws, "nogoal"); err == nil {
		t.Error("phase without a goal should fail")
	}
	if _, err := Load(ws, "../escape"); err == nil {
		t.Error("path traversal should fail")
	}
	if _, err := Load(ws, "missing"); err == nil {
		t.Error("missing playbook should fail")
	}
}
