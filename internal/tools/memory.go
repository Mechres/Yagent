package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"yagent/internal/llm"
	"yagent/internal/memory"
)

// ---------- memory_save ----------

type memorySaveTool struct {
	vectors   *memory.VectorStore
	sessionID string
}

type memorySaveArgs struct {
	Text string `json:"text"`
}

var memorySaveSchema = fnSchema("memory_save", "store a fact worth remembering across sessions: user preferences, project decisions, gotchas, reusable findings. NOT code, NOT chit-chat, NOT tool output.",
	map[string]any{"text": strProp("the fact or preference to remember, one concise sentence")},
	[]string{"text"})

func (t *memorySaveTool) Schema() llm.ToolSchema { return memorySaveSchema }
func (t *memorySaveTool) Risk() RiskLevel        { return RiskWrite }

func (t *memorySaveTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a memorySaveArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Text == "" {
		return "", validationErrorf(`argument "text" is required`)
	}
	if t.vectors == nil {
		return "error: semantic memory is not configured for this session", nil
	}
	if err := t.vectors.Save(ctx, a.Text, "tool", t.sessionID); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return "remembered", nil
}

// ---------- memory_search ----------

type memorySearchTool struct {
	vectors *memory.VectorStore
}

type memorySearchArgs struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

var memorySearchSchema = fnSchema("memory_search", "recall facts from past sessions semantically related to the query; use when the user references something from before",
	map[string]any{
		"query": strProp("what to recall, in natural language"),
		"k":     intProp("max results, default 5, max 10 (optional)"),
	},
	[]string{"query"})

func (t *memorySearchTool) Schema() llm.ToolSchema { return memorySearchSchema }
func (t *memorySearchTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *memorySearchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a memorySearchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Query == "" {
		return "", validationErrorf(`argument "query" is required`)
	}
	if a.K <= 0 {
		a.K = 5
	}
	if a.K > 10 {
		return "", validationErrorf("k must be at most 10")
	}
	if t.vectors == nil {
		return "error: semantic memory is not configured for this session", nil
	}
	memories, err := t.vectors.Search(ctx, a.Query, a.K)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(memories) == 0 {
		return "no memories found", nil
	}
	var b strings.Builder
	for _, m := range memories {
		fmt.Fprintf(&b, "- [%.2f] %s\n", m.Score, m.Text)
	}
	return capResult(b.String(), maxResultBytes), nil
}
