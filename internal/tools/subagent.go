package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Mechres/Yagent/internal/llm"
)

// SubagentRole is a preset child-agent profile: a specialized system-prompt
// suffix, a default read-only tool subset, and an optional temperature. Zero
// value means no role.
type SubagentRole struct {
	Name        string
	Prompt      string   // appended to the child system prompt
	Tools       []string // default tool subset when the model gives none
	Temperature float64  // 0 = inherit the parent's sampling
}

// subagentRoles is the built-in role catalog (P2).
var subagentRoles = map[string]SubagentRole{
	"architect": {
		Name:        "architect",
		Prompt:      "You are an ARCHITECT. Focus on design, structure, dependencies and maintainability. Prefer code_outline and code_references over raw reads. Call out coupling, layering and naming issues. End with concrete recommendations.",
		Tools:       []string{"fs_read", "glob", "grep", "index_search", "code_outline", "code_references", "git_status", "git_diff", "git_log"},
		Temperature: 0.4,
	},
	"auditor": {
		Name:        "auditor",
		Prompt:      "You are an AUDITOR. Your job is to find problems: security issues, correctness bugs, race conditions, error paths ignored, secrets or credentials, and risky patterns. Be adversarial and cite path:line for every finding. Report only facts you verified with tools.",
		Tools:       []string{"fs_read", "glob", "grep", "index_search", "code_outline", "code_references", "git_status", "git_diff", "git_log", "web_search", "web_fetch"},
		Temperature: 0.3,
	},
	"test-engineer": {
		Name:        "test-engineer",
		Prompt:      "You are a TEST ENGINEER. Identify what is untested and what could break. Inspect tests and code paths, and propose concrete test cases with the exact functions to exercise. List coverage gaps as path:line.",
		Tools:       []string{"fs_read", "glob", "grep", "index_search", "code_outline", "code_references", "git_status", "git_diff"},
		Temperature: 0.5,
	},
	"docs-writer": {
		Name:        "docs-writer",
		Prompt:      "You are a DOCS WRITER. Read the relevant code and produce clear, accurate documentation (usage, examples, edge cases) as your report. Do not invent APIs — only document what the code actually shows.",
		Tools:       []string{"fs_read", "glob", "grep", "index_search", "code_outline", "git_status", "git_diff"},
		Temperature: 0.6,
	},
}

// RoleByName resolves a preset subagent role (empty name = no role).
func RoleByName(name string) (SubagentRole, bool) {
	r, ok := subagentRoles[name]
	return r, ok
}

// subagentTool delegates a self-contained subtask to an isolated read-only
// child agent (M7). Multiple tasks run in parallel subagents. The optional
// Tools field scopes each child to a tool subset (M7 beyond v2); the optional
// Role field applies a preset profile (P2).
type subagentTool struct {
	ws  string
	run func(ctx context.Context, task, workspace string, tools []string, role SubagentRole) (string, error)
}

type subagentArgs struct {
	Task  string   `json:"task"`
	Tasks []string `json:"tasks,omitempty"`
	Tools []string `json:"tools,omitempty"`
	Role  string   `json:"role,omitempty"`
}

var subagentSchema = fnSchema("subagent", "delegate a self-contained, context-heavy subtask (e.g. research across several web pages, auditing a diff, exploring a large directory) to an isolated read-only subagent. It runs in its own context and returns a concise summary — use it to keep long investigations out of the main conversation. Provide either 'task' or 'tasks' (an array, run in parallel). Optionally pass 'tools' to scope each subagent to a subset of the read-only tools (e.g. [\"web_search\",\"web_fetch\"] for research-only, [\"grep\",\"fs_read\",\"index_search\"] for code exploration); default is the full read-only set. Optionally pass 'role' for a preset profile: architect, auditor, test-engineer, docs-writer (sets a focused system prompt, a default tool subset and sampling).",
	map[string]any{
		"task":  strProp("the subtask to complete; be specific about what to return"),
		"tasks": arrayProp("multiple subtasks to run in parallel subagents (optional)"),
		"tools": arrayProp("restrict each subagent to this subset of read-only tools (optional; default = all read-only tools)"),
		"role":  strProp("preset role: architect, auditor, test-engineer, docs-writer (optional)"),
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
	var role SubagentRole
	if a.Role != "" {
		var ok bool
		role, ok = subagentRoles[a.Role]
		if !ok {
			return "", validationErrorf("unknown subagent role %q (architect | auditor | test-engineer | docs-writer)", a.Role)
		}
	}
	if t.run == nil {
		return "error: subagents are not configured for this session", nil
	}
	tools := a.Tools
	if len(tools) == 0 && len(role.Tools) > 0 {
		tools = role.Tools // role default subset
	}
	if len(a.Tasks) > 0 {
		return t.runParallel(ctx, a.Tasks, tools, role)
	}
	answer, err := t.run(ctx, a.Task, t.ws, tools, role)
	if err != nil {
		return fmt.Sprintf("error: subagent failed: %v", err), nil
	}
	return capResult(answer, maxResultBytes), nil
}

// runParallel runs multiple subtasks in isolated subagents concurrently and
// combines the summaries in order.
func (t *subagentTool) runParallel(ctx context.Context, tasks, tools []string, role SubagentRole) (string, error) {
	results := make([]string, len(tasks))
	var wg sync.WaitGroup
	for i, tk := range tasks {
		wg.Add(1)
		go func(i int, tk string) {
			defer wg.Done()
			r, err := t.run(ctx, tk, t.ws, tools, role)
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
