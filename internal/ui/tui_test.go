package ui

import (
	"strings"
	"testing"

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
