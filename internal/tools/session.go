package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/memory"
)

type sessionSearchTool struct{ store *memory.Store }

type sessionSearchArgs struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

var sessionSearchSchema = fnSchema("session_search", "search historical conversation messages with FTS5; use this to recover exact details from an older session after context compaction or pruning. This is transcript search, not durable semantic memory, and returns bounded snippets with session ids",
	map[string]any{
		"query": strProp("keywords or a short phrase to find in previous conversations"),
		"k":     intProp("maximum matches, default 5, maximum 10 (optional)"),
	}, []string{"query"})

func (t *sessionSearchTool) Schema() llm.ToolSchema { return sessionSearchSchema }
func (t *sessionSearchTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *sessionSearchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a sessionSearchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", validationErrorf(`argument "query" is required`)
	}
	if a.K <= 0 {
		a.K = 5
	}
	if a.K > 10 {
		return "", validationErrorf("k must be at most 10")
	}
	if t.store == nil {
		return "error: session history is not configured", nil
	}
	hits, err := t.store.SearchMessages(ctx, a.Query, a.K)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(hits) == 0 {
		return "no historical messages found", nil
	}
	var b strings.Builder
	for _, h := range hits {
		title := h.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "- session %s [%s] %s: %s\n", h.SessionID, h.Role, title, h.Snippet)
	}
	return capResult(b.String(), maxResultBytes), nil
}
