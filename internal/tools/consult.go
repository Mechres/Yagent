package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/Mechres/Yagent/internal/llm"
)

// consultTool asks a configured "advisor" for guidance: either a second
// OpenAI-compatible server (consult.server_url/model/api_key) or an installed
// terminal AI app run as a subprocess (consult.cmd, e.g. ["claude", "-p"]).
type consultTool struct {
	client *llm.Client
	cmd    []string
}

type consultArgs struct {
	Question string `json:"question"`
	Context  string `json:"context,omitempty"`
}

var consultSchema = fnSchema("consult", "ask a second AI model (the advisor) for guidance or a second opinion — use it when stuck, before a risky change, or to sanity-check a plan. The advisor is a separate model, not you.",
	map[string]any{
		"question": strProp("the question, decision, or plan to check"),
		"context":  strProp("relevant context: file path, error message, code snippet (optional)"),
	},
	[]string{"question"})

func (t *consultTool) Schema() llm.ToolSchema { return consultSchema }
func (t *consultTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *consultTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a consultArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Question == "" {
		return "", validationErrorf(`argument "question" is required`)
	}
	prompt := a.Question
	if a.Context != "" {
		prompt += "\n\nContext:\n" + a.Context
	}
	// CLI advisor first (explicitly configured), then the HTTP advisor.
	if len(t.cmd) > 0 {
		return t.consultCLI(ctx, prompt)
	}
	if t.client == nil {
		return "error: the consult advisor is not configured (set consult.server_url/model/api_key, or consult.cmd for a terminal AI app)", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	msgs := []llm.Message{
		{Role: "system", Content: "You are an experienced engineering advisor to another AI agent. Give concise, actionable guidance. Challenge bad ideas. Answer the question directly, in a few sentences."},
		{Role: "user", Content: prompt},
	}
	resp, err := t.client.ChatStream(ctx, msgs, nil, func(string) {})
	if err != nil {
		return fmt.Sprintf("error: consult failed: %v", err), nil
	}
	return capResult(resp.Message.Content, maxResultBytes), nil
}

// consultCLI runs an installed terminal AI app (e.g. `claude -p`, `cursor-agent`)
// with the prompt appended as the final argument, returning its stdout.
func (t *consultTool) consultCLI(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	args := append(append([]string{}, t.cmd...), prompt)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	cmd.Env = scrubEnv(os.Environ())
	if err := cmd.Run(); err != nil {
		return fmt.Sprintf("error: consult command %q failed: %v\n%s", t.cmd[0], err, errBuf.String()), nil
	}
	if errBuf.Len() > 0 {
		out.WriteString("\nstderr:\n" + errBuf.String())
	}
	return capResult(out.String(), maxResultBytes), nil
}
