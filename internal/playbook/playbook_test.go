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
