package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"yagent/internal/jobs"
	"yagent/internal/llm"
)

// ---------- shell_bg ----------

type shellBgTool struct {
	jobs *jobs.Registry
}

type shellBgArgs struct {
	Command string `json:"command"`
}

var shellBgSchema = fnSchema("shell_bg", "start a command as a background job (e.g. a dev server like 'go run .' or a long test suite) and return its job id; inspect output later with shell_logs, stop it with shell_kill",
	map[string]any{"command": strProp("shell command to run in the background")},
	[]string{"command"})

func (t *shellBgTool) Schema() llm.ToolSchema { return shellBgSchema }
func (t *shellBgTool) Risk() RiskLevel        { return RiskWrite }

func (t *shellBgTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a shellBgArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Command == "" {
		return "", validationErrorf(`argument "command" is required`)
	}
	if t.jobs == nil {
		return "error: background jobs are not configured for this session", nil
	}
	job, err := t.jobs.Start(a.Command)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return fmt.Sprintf("started job %s: %s", job.ID, a.Command), nil
}

// ---------- shell_logs ----------

type shellLogsTool struct {
	jobs *jobs.Registry
}

type shellLogsArgs struct {
	ID string `json:"id"`
}

var shellLogsSchema = fnSchema("shell_logs", "return the accumulated output of a background job (tail, capped at 32 KiB)",
	map[string]any{"id": strProp("job id from shell_bg")},
	[]string{"id"})

func (t *shellLogsTool) Schema() llm.ToolSchema { return shellLogsSchema }
func (t *shellLogsTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *shellLogsTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a shellLogsArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.ID == "" {
		return "", validationErrorf(`argument "id" is required`)
	}
	if t.jobs == nil {
		return "error: background jobs are not configured for this session", nil
	}
	job, ok := t.jobs.Get(a.ID)
	if !ok {
		return fmt.Sprintf("error: unknown job %q", a.ID), nil
	}
	status := "running"
	if !job.Alive() {
		status = "exited"
	}
	logs := job.Logs(32 << 10)
	if logs == "" {
		logs = "(no output yet)"
	}
	return fmt.Sprintf("[%s] %s\n%s", status, a.ID, logs), nil
}

// ---------- shell_kill ----------

type shellKillTool struct {
	jobs *jobs.Registry
}

type shellKillArgs struct {
	ID string `json:"id"`
}

var shellKillSchema = fnSchema("shell_kill", "terminate a background job started with shell_bg",
	map[string]any{"id": strProp("job id to stop")},
	[]string{"id"})

func (t *shellKillTool) Schema() llm.ToolSchema { return shellKillSchema }
func (t *shellKillTool) Risk() RiskLevel        { return RiskDestructive }

func (t *shellKillTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a shellKillArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.ID == "" {
		return "", validationErrorf(`argument "id" is required`)
	}
	if t.jobs == nil {
		return "error: background jobs are not configured for this session", nil
	}
	if err := t.jobs.Kill(a.ID); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return fmt.Sprintf("killed job %s", a.ID), nil
}
