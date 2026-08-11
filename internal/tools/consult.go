package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"yagent/internal/llm"
)

// consultTool asks a second, configured "advisor" model for guidance.
type consultTool struct{ client *llm.Client }

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
	if t.client == nil {
		return "error: the consult advisor is not configured (set consult.server_url and consult.model)", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	prompt := a.Question
	if a.Context != "" {
		prompt += "\n\nContext:\n" + a.Context
	}
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
