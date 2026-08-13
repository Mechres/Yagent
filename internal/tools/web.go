package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/web"
)

// ---------- web_search ----------

type webSearchTool struct {
	client *web.Client
}

type webSearchArgs struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

var webSearchSchema = fnSchema("web_search", "search the web for the query and return ranked results (title, url, snippet); use it for questions about things outside the workspace — then web_fetch the most promising pages",
	map[string]any{
		"query": strProp("search query"),
		"k":     intProp("max results, default 5, max 8 (optional)"),
	},
	[]string{"query"})

func (t *webSearchTool) Schema() llm.ToolSchema { return webSearchSchema }
func (t *webSearchTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *webSearchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a webSearchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Query == "" {
		return "", validationErrorf(`argument "query" is required`)
	}
	if a.K <= 0 {
		a.K = 5
	}
	if a.K > 8 {
		return "", validationErrorf("k must be at most 8")
	}
	if t.client == nil {
		return "error: web search is not configured", nil
	}
	hitsBefore := t.client.CacheHits()
	results, err := t.client.Search(ctx, a.Query, a.K)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	cached := t.client.CacheHits() > hitsBefore
	if len(results) == 0 {
		return "no results found", nil
	}
	var b strings.Builder
	if cached {
		b.WriteString("[cached results]\n")
	}
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, r.Title, r.URL, r.Snippet)
	}
	return capResult(b.String(), maxResultBytes), nil
}

// ---------- web_fetch ----------

type webFetchTool struct {
	client *web.Client
}

type webFetchArgs struct {
	URL string `json:"url"`
}

var webFetchSchema = fnSchema("web_fetch", "fetch a URL and return its readable text (scripts/nav stripped, capped at 16 KiB); use it on promising web_search results to extract the actual content",
	map[string]any{"url": strProp("absolute http(s) URL to fetch")},
	[]string{"url"})

func (t *webFetchTool) Schema() llm.ToolSchema { return webFetchSchema }
func (t *webFetchTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *webFetchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a webFetchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.URL == "" {
		return "", validationErrorf(`argument "url" is required`)
	}
	if t.client == nil {
		return "error: web fetch is not configured", nil
	}
	hitsBefore := t.client.CacheHits()
	text, err := t.client.Fetch(ctx, a.URL)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if t.client.CacheHits() > hitsBefore {
		return "[cached page]\n" + text, nil
	}
	return text, nil
}
