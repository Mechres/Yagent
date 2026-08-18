package tools

import (
	"encoding/json"
	"testing"

	"github.com/Mechres/Yagent/internal/llm"
)

func TestPresentToolResult(t *testing.T) {
	tests := []struct {
		name   string
		status OutcomeStatus
		want   string
		check  func(Presentation) bool
	}{
		{"read", OutcomeSucceeded, "read", func(p Presentation) bool { return p.Target == "README.md" }},
		{"search", OutcomeSucceeded, "search", func(p Presentation) bool { return !p.Failure }},
		{"write", OutcomeSucceeded, "diff", func(p Presentation) bool { return p.Diff && p.Target == "main.go" }},
		{"denied", OutcomeDenied, "approval", func(p Presentation) bool { return p.Approval }},
		{"web", OutcomeFailed, "web", func(p Presentation) bool { return p.Failure }},
	}
	args := map[string]json.RawMessage{
		"read":   json.RawMessage(`{"path":"README.md"}`),
		"search": json.RawMessage(`{"query":"tool validation"}`),
		"write":  json.RawMessage(`{"path":"main.go","content":"package main"}`),
		"denied": json.RawMessage(`{"path":"main.go"}`),
		"web":    json.RawMessage(`{"url":"https://example.com"}`),
	}
	names := map[string]string{"read": "fs_read", "search": "grep", "write": "fs_write", "denied": "fs_write", "web": "web_fetch"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := PresentToolResult(names[tt.name], args[tt.name], RiskWrite, tt.status, "error: failed\nmore")
			if p.Kind != tt.want || !tt.check(p) {
				t.Fatalf("presentation = %+v, want kind %q", p, tt.want)
			}
		})
	}
}

func TestNewToolOutcomeCopiesArguments(t *testing.T) {
	callArgs := json.RawMessage(`{"path":"a.txt"}`)
	o := NewToolOutcome(llm.ToolCall{ID: "c1", Function: llm.ToolCallFunction{Name: "fs_read", Arguments: callArgs}}, RiskReadOnly, OutcomeSucceeded, "ok", 2)
	callArgs[0] = 'x'
	if string(o.Arguments) != `{"path":"a.txt"}` {
		t.Fatalf("outcome arguments alias caller memory: %s", o.Arguments)
	}
	if o.Presentation.Kind != "read" || o.Status != OutcomeSucceeded {
		t.Fatalf("outcome = %+v", o)
	}
}
