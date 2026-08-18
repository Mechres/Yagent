package tools

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/Mechres/Yagent/internal/llm"
)

// OutcomeStatus describes what happened to a model-issued tool call.
type OutcomeStatus string

const (
	OutcomeSucceeded OutcomeStatus = "succeeded"
	OutcomeFailed    OutcomeStatus = "failed"
	OutcomeDenied    OutcomeStatus = "denied"
	OutcomeSkipped   OutcomeStatus = "skipped"
)

// Presentation is UI-neutral metadata for rendering a tool event. It carries
// semantic categories and safe targets so clients do not need to parse TUI
// strings or infer whether a result is a diff, search, web, or failure card.
type Presentation struct {
	Kind     string   `json:"kind"` // read, search, terminal, diff, web, memory, approval, generic
	Summary  string   `json:"summary,omitempty"`
	Target   string   `json:"target,omitempty"`
	Targets  []string `json:"targets,omitempty"`
	Diff     bool     `json:"diff,omitempty"`
	Approval bool     `json:"approval,omitempty"`
	Failure  bool     `json:"failure,omitempty"`
}

// ToolOutcome is the stable event contract emitted after a tool call is
// resolved. Result remains the exact model-visible text; Presentation is the
// display-oriented projection for UIs and future clients.
type ToolOutcome struct {
	CallID       string          `json:"call_id,omitempty"`
	Name         string          `json:"name"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	Risk         RiskLevel       `json:"risk"`
	Status       OutcomeStatus   `json:"status"`
	Result       string          `json:"result,omitempty"`
	Elapsed      time.Duration   `json:"elapsed_ns,omitempty"`
	Presentation Presentation    `json:"presentation"`
}

// NewToolOutcome builds the common event projection used by the agent.
func NewToolOutcome(call llm.ToolCall, risk RiskLevel, status OutcomeStatus, result string, elapsed time.Duration) ToolOutcome {
	return ToolOutcome{
		CallID: call.ID, Name: call.Function.Name,
		Arguments: append(json.RawMessage(nil), call.Function.Arguments...),
		Risk:      risk, Status: status, Result: result, Elapsed: elapsed,
		Presentation: PresentToolResult(call.Function.Name, call.Function.Arguments, risk, status, result),
	}
}

// PresentToolResult classifies common tools without inspecting UI strings.
func PresentToolResult(name string, args json.RawMessage, risk RiskLevel, status OutcomeStatus, result string) Presentation {
	p := Presentation{Kind: "generic", Failure: status == OutcomeFailed}
	switch {
	case name == "fs_read" || name == "glob" || name == "grep" || strings.HasPrefix(name, "index_") || strings.HasPrefix(name, "code_") || strings.HasPrefix(name, "git_"):
		p.Kind = "read"
		if name == "glob" || name == "grep" || name == "index_search" {
			p.Kind = "search"
		}
	case name == "web_search" || name == "web_fetch" || name == "paper_search":
		p.Kind = "web"
	case name == "shell_exec" || name == "shell_bg" || name == "shell_logs" || name == "shell_kill":
		p.Kind = "terminal"
	case name == "fs_write" || name == "fs_edit" || name == "fs_patch" || name == "fs_refactor":
		p.Kind, p.Diff = "diff", true
	case name == "memory_save" || name == "memory_search" || name == "research_note":
		p.Kind = "memory"
	}
	if status == OutcomeDenied {
		p.Kind, p.Approval = "approval", true
	}
	var fields struct {
		Path  string `json:"path"`
		Patch string `json:"patch"`
	}
	if json.Unmarshal(args, &fields) == nil && fields.Path != "" {
		p.Target = fields.Path
	}
	if fields.Patch != "" && p.Target == "" {
		p.Summary = "unified diff"
	}
	if p.Summary == "" {
		p.Summary = firstLine(result)
	}
	if risk == RiskReadOnly && p.Kind == "generic" {
		p.Kind = "read"
	}
	return p
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
