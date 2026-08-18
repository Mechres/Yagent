package grill

import (
	"strings"
	"testing"
)

func TestPromptContainsWorkflowGuardrails(t *testing.T) {
	p := Prompt("replace the config loader")
	for _, want := range []string{
		"clarify tool",
		"CONTEXT.md",
		"docs/adr/",
		"Do not implement source-code changes",
		"at most 8",
		"re-read every artifact",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestOpeningIncludesTopic(t *testing.T) {
	if got := Opening("add caching"); !strings.Contains(got, "add caching") {
		t.Fatalf("opening = %q", got)
	}
}

func TestValidateContext(t *testing.T) {
	if err := ValidateArtifact("CONTEXT.md", "# Glossary\n\n## Widget\nThe project term."); err != nil {
		t.Fatalf("valid context: %v", err)
	}
	if err := ValidateArtifact("CONTEXT.md", "# Glossary\nplain text"); err == nil {
		t.Fatal("context without terms should fail")
	}
	if err := ValidateArtifact("CONTEXT.md", "## Widget\n## Procedure\nDo the thing"); err == nil {
		t.Fatal("procedural context should fail")
	}
}

func TestValidateADR(t *testing.T) {
	valid := "# Choice\n\n## Status\nAccepted\n## Context\nWhy\n## Decision\nWhat\n## Consequences\nTrade-off"
	if err := ValidateArtifact("docs/adr/0001-choice.md", valid); err != nil {
		t.Fatalf("valid ADR: %v", err)
	}
	if err := ValidateArtifact("docs/adr/0001-choice.md", "# Choice\n## Decision\nOnly this"); err == nil {
		t.Fatal("incomplete ADR should fail")
	}
}

func TestArtifactPath(t *testing.T) {
	for name, want := range map[string]bool{
		"CONTEXT.md":               true,
		"docs/adr/0001-choice.md":  true,
		"docs/adr/0001-choice.txt": false,
		"internal/agent/agent.go":  false,
		"docs/adr/../../source.go": false,
		"/tmp/CONTEXT.md":          false,
	} {
		if got := IsArtifactPath(name); got != want {
			t.Errorf("IsArtifactPath(%q) = %v, want %v", name, got, want)
		}
	}
}
