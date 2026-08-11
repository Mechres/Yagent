package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yagent/internal/llm"
	"yagent/internal/skills"
)

func testModel(t *testing.T) *tuiModel {
	t.Helper()
	sk, err := skills.Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &tuiModel{env: &chatEnv{sk: sk}, input: newInput()}
}

func TestTabCyclesCommands(t *testing.T) {
	m := testModel(t)
	m.input.SetValue("/")
	m.completeCommand()
	first := m.input.Value()
	if first == "/" || !strings.HasPrefix(first, "/") {
		t.Fatalf("first completion = %q", first)
	}
	m.completeCommand()
	second := m.input.Value()
	if second == first {
		t.Errorf("tab did not cycle: %q", second)
	}
	// an exact command must cycle to a different one (regression: stuck on
	// the first match)
	m.input.SetValue("/exit")
	m.completeCommand()
	if m.input.Value() == "/exit" {
		t.Errorf("tab stuck on /exit, should cycle to the next command")
	}
}

func TestSlashMatchesIncludeSkills(t *testing.T) {
	m := testModel(t)
	if _, err := m.env.sk.Apply(skills.Op{
		Action: skills.ActionCreate, Name: "smoke-test",
		Content: "---\nname: smoke-test\ndescription: smoke test skill\n---\n## When to Use\nx\n## Procedure\ny\n",
	}); err != nil {
		t.Fatal(err)
	}
	m.input.SetValue("/smoke")
	matches := m.slashMatches()
	if len(matches) != 1 || matches[0] != "/smoke-test" {
		t.Errorf("matches = %v, want [/smoke-test]", matches)
	}
}

func TestRenderApprovalDiff(t *testing.T) {
	d := renderApprovalDiff("line one\nold line\nline three", "line one\nnew line\nline three")
	if !strings.Contains(d, "- old line") || !strings.Contains(d, "+ new line") {
		t.Errorf("diff missing changes: %q", d)
	}
	if !strings.Contains(d, "line one") {
		t.Errorf("unchanged line missing: %q", d)
	}
}

func TestFsApprovalDiff(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("old content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_edit",
		Arguments: []byte(`{"path":"a.txt","old_string":"old content","new_string":"new content"}`)}}
	d := fsApprovalDiff(ws, edit)
	if !strings.Contains(d, "- old content") || !strings.Contains(d, "+ new content") {
		t.Errorf("fs_edit diff = %q", d)
	}
	// path traversal -> no diff
	evil := llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_edit",
		Arguments: []byte(`{"path":"../evil","old_string":"a","new_string":"b"}`)}}
	if d := fsApprovalDiff(ws, evil); d != "" {
		t.Errorf("traversal should yield no diff, got %q", d)
	}
	// non-fs tool -> no diff
	other := llm.ToolCall{Function: llm.ToolCallFunction{Name: "shell_exec", Arguments: []byte(`{"command":"ls"}`)}}
	if d := fsApprovalDiff(ws, other); d != "" {
		t.Errorf("non-fs tool should yield no diff, got %q", d)
	}
}
