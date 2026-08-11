package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Mechres/Yagent/internal/llm"
)

// subagentTool delegates a self-contained subtask to an isolated read-only
// child agent (M7). Multiple tasks run in parallel subagents.
type subagentTool struct {
	ws  string
	run func(ctx context.Context, task, workspace string) (string, error)
}

type subagentArgs struct {
	Task  string   `json:"task"`
	Tasks []string `json:"tasks,omitempty"`
}

var subagentSchema = fnSchema("subagent", "delegate a self-contained, context-heavy subtask (e.g. research across several web pages, auditing a diff, exploring a large directory) to an isolated read-only subagent. It runs in its own context and returns a concise summary — use it to keep long investigations out of the main conversation. Provide either 'task' or 'tasks' (an array, run in parallel).",
	map[string]any{
		"task":  strProp("the subtask to complete; be specific about what to return"),
		"tasks": strProp("multiple subtasks to run in parallel subagents (optional)"),
	},
	[]string{})

func (t *subagentTool) Schema() llm.ToolSchema { return subagentSchema }
func (t *subagentTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *subagentTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a subagentArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Task == "" && len(a.Tasks) == 0 {
		return "", validationErrorf(`either "task" or "tasks" is required`)
	}
	if t.run == nil {
		return "error: subagents are not configured for this session", nil
	}
	if len(a.Tasks) > 0 {
		return t.runParallel(ctx, a.Tasks)
	}
	answer, err := t.run(ctx, a.Task, t.ws)
	if err != nil {
		return fmt.Sprintf("error: subagent failed: %v", err), nil
	}
	return capResult(answer, maxResultBytes), nil
}

// runParallel runs multiple subtasks in isolated subagents concurrently and
// combines the summaries in order.
func (t *subagentTool) runParallel(ctx context.Context, tasks []string) (string, error) {
	results := make([]string, len(tasks))
	var wg sync.WaitGroup
	for i, tk := range tasks {
		wg.Add(1)
		go func(i int, tk string) {
			defer wg.Done()
			r, err := t.run(ctx, tk, t.ws)
			if err != nil {
				r = "error: " + err.Error()
			}
			results[i] = r
		}(i, tk)
	}
	wg.Wait()
	var b strings.Builder
	for i, tk := range tasks {
		fmt.Fprintf(&b, "### %s\n%s\n\n", tk, results[i])
	}
	return capResult(b.String(), maxResultBytes), nil
}
