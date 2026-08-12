package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
)

// ---------- clarify / plan ----------

// askUserFunc prompts the user with a question and optional choices and returns
// the answer (wired by the UI: REPL prints a numbered prompt, TUI opens a
// modal). Nil disables the clarify and plan tools.
type askUserFunc func(ctx context.Context, question string, choices []string) (string, error)

// clarifyTool asks the user a question with optional structured choices when
// the task is ambiguous or a decision matters — instead of the small model
// guessing and burning a turn (Hermes review #1/#5).
type clarifyTool struct {
	ask askUserFunc
}

type clarifyArgs struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
}

var clarifySchema = fnSchema("clarify", "ask the user a question when the task is ambiguous or a decision matters. Returns the user's exact answer as the tool result. Call it instead of guessing whenever instructions are incomplete, conflicting, or a choice would change the work.",
	map[string]any{
		"question": strProp("the concise question for the user"),
		"choices": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "up to 6 suggested answers; the user may also type their own (optional)",
		},
	},
	[]string{"question"})

func (t *clarifyTool) Schema() llm.ToolSchema { return clarifySchema }
func (t *clarifyTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *clarifyTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a clarifyArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Question == "" {
		return "", validationErrorf(`argument "question" is required`)
	}
	if len(a.Choices) > 6 {
		return "", validationErrorf("at most 6 choices")
	}
	if t.ask == nil {
		return "error: the clarify tool is not available in this context (run in the interactive UI)", nil
	}
	answer, err := t.ask(ctx, a.Question, a.Choices)
	if err != nil {
		return "error: " + err.Error(), nil
	}
	return "user answered: " + answer, nil
}

// planTool proposes a step-by-step plan for the user to approve BEFORE any
// side-effecting work (Hermes review #4): a cheap up-front gate instead of a
// wrong first move burning the turn budget.
type planTool struct {
	ask askUserFunc
}

type planArgs struct {
	Steps []string `json:"steps"`
}

var planSchema = fnSchema("plan", "propose a step-by-step plan for a multi-step task (3+ steps or significant side effects) BEFORE executing it: the steps are shown to the user for approval. If approved, execute the plan; if the user asks for revisions, adjust the plan and call plan again. Never start side-effecting work on a large task without an approved plan.",
	map[string]any{
		"steps": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "the ordered steps, each concise and specific",
		},
	},
	[]string{"steps"})

func (t *planTool) Schema() llm.ToolSchema { return planSchema }
func (t *planTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *planTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a planArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if len(a.Steps) == 0 {
		return "", validationErrorf(`argument "steps" is required (a non-empty array)`)
	}
	if t.ask == nil {
		return "error: the plan tool is not available in this context (run in the interactive UI)", nil
	}
	var q strings.Builder
	q.WriteString("The agent proposes this plan:\n")
	for i, s := range a.Steps {
		fmt.Fprintf(&q, "%d. %s\n", i+1, s)
	}
	q.WriteString("Approve it, or revise (your feedback is returned to the agent).")
	answer, err := t.ask(ctx, q.String(), []string{"Approve plan", "Revise — give feedback"})
	if err != nil {
		return "error: " + err.Error(), nil
	}
	if strings.HasPrefix(strings.ToLower(answer), "approve") {
		return "plan approved — execute it now", nil
	}
	return "plan rejected by the user: " + answer, nil
}
