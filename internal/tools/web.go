package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/web"
)

// ---------- web_search ----------

type webSearchTool struct {
	client *web.Client
}

type webSearchArgs struct {
	Query   string   `json:"query"`
	Queries []string `json:"queries,omitempty"`
	K       int      `json:"k,omitempty"`
}

var webSearchSchema = fnSchema("web_search", "search the web for the query (or multiple queries, run in parallel) and return ranked results (title, url, snippet); use it for questions about things outside the workspace — then web_fetch the most promising pages. Pass multiple distinct queries via 'queries' to cover several angles of a topic in one call",
	map[string]any{
		"query":   strProp("search query"),
		"queries": arrayProp("multiple distinct queries to search in parallel (optional)"),
		"k":       intProp("max results per query, default 5, max 8 (optional)"),
	},
	[]string{})

func (t *webSearchTool) Schema() llm.ToolSchema { return webSearchSchema }
func (t *webSearchTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *webSearchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a webSearchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	queries := a.Queries
	if strings.TrimSpace(a.Query) != "" {
		queries = append([]string{a.Query}, queries...)
	}
	if len(queries) == 0 {
		return "", validationErrorf(`argument "query" (or "queries") is required`)
	}
	if len(queries) > 8 {
		return "", validationErrorf("queries must have at most 8 entries")
	}
	k := a.K
	if k <= 0 {
		k = 5
	}
	if k > 8 {
		return "", validationErrorf("k must be at most 8")
	}
	if t.client == nil {
		return "error: web search is not configured", nil
	}
	if len(queries) == 1 {
		out := t.searchOne(ctx, queries[0], k, "")
		return capResult(WrapUntrusted("web_search for "+queries[0], out), maxResultBytes), nil
	}
	// Multiple queries: fan out in parallel (each is an independent search, the
	// cache memoizes repeats) and combine the per-query result lists in order.
	results := make([]string, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			results[i] = t.searchOne(ctx, q, k, fmt.Sprintf("[query %d: %s]", i+1, q))
		}(i, q)
	}
	wg.Wait()
	return capResult(WrapUntrusted("web_search for "+strings.Join(queries, " | "), strings.Join(results, "\n")), maxResultBytes), nil
}

// searchOne runs a single query and renders its results with an optional header.
func (t *webSearchTool) searchOne(ctx context.Context, query string, k int, header string) string {
	hitsBefore := t.client.CacheHits()
	results, err := t.client.Search(ctx, query, k)
	if err != nil {
		return fmt.Sprintf("%serror: %v", headerPrefix(header), err)
	}
	cached := t.client.CacheHits() > hitsBefore
	if len(results) == 0 {
		return fmt.Sprintf("%sno results found", headerPrefix(header))
	}
	var b strings.Builder
	if cached {
		b.WriteString("[cached results]\n")
	}
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, r.Title, r.URL, r.Snippet)
	}
	return headerPrefix(header) + b.String()
}

func headerPrefix(header string) string {
	if header == "" {
		return ""
	}
	return header + "\n"
}

// ---------- web_fetch ----------

type webFetchTool struct {
	client *web.Client
}

type webFetchArgs struct {
	URL string `json:"url"`
}

var webFetchSchema = fnSchema("web_fetch", "fetch a URL and return its readable Markdown text (scripts/nav stripped, links preserved as [text](url)); use it on promising web_search results to extract the actual content. PDFs are rejected with a hint to find the HTML/abstract version",
	map[string]any{"url": strProp("absolute http(s) URL to fetch")},
	[]string{"url"})

func (t *webFetchTool) Schema() llm.ToolSchema { return webFetchSchema }
func (t *webFetchTool) Risk() RiskLevel        { return RiskReadOnly }

// WrapUntrusted marks content that came from outside the workspace (web pages,
// fetched repos, search snippets) as DATA, not commands. Delimiters + the
// system prompt's rule make a "ignore previous instructions" injection on a
// fetched page unable to silently take over the model. Deterministic, applied
// at the tool boundary so it survives regardless of how the result is used.
func WrapUntrusted(source, content string) string {
	return "<untrusted data from " + source + ">\n" + content + "\n</untrusted>"
}

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
	wrapped := WrapUntrusted(a.URL, text)
	if t.client.CacheHits() > hitsBefore {
		return "[cached page]\n" + wrapped, nil
	}
	return wrapped, nil
}
