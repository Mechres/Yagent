package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"yagent/internal/llm"
)

// subagentTool delegates a self-contained subtask to an isolated read-only
// child agent (M7 v1) whose context does not pollute the main conversation.
type subagentTool struct {
	ws  string
	run func(ctx context.Context, task, workspace string) (string, error)
}

type subagentArgs struct {
	Task string `json:"task"`
}

var subagentSchema = fnSchema("subagent", "delegate a self-contained, context-heavy subtask (e.g. research across several web pages, auditing a diff, exploring a large directory) to an isolated read-only subagent. It runs in its own context and returns a concise summary — use it to keep long investigations out of the main conversation.",
	map[string]any{
		"task": strProp("the subtask to complete; be specific about what to return"),
	},
	[]string{"task"})

func (t *subagentTool) Schema() llm.ToolSchema { return subagentSchema }
func (t *subagentTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *subagentTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a subagentArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Task == "" {
		return "", validationErrorf(`argument "task" is required`)
	}
	if t.run == nil {
		return "error: subagents are not configured for this session", nil
	}
	answer, err := t.run(ctx, a.Task, t.ws)
	if err != nil {
		return fmt.Sprintf("error: subagent failed: %v", err), nil
	}
	return capResult(answer, maxResultBytes), nil
}
