package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"yagent/internal/index"
	"yagent/internal/llm"
)

// ---------- index_repo ----------

type indexRepoTool struct {
	store      *index.Store
	onProgress func(string)
}

var indexRepoSchema = fnSchema("index_repo", "build or refresh the workspace code index (incremental: unchanged files are skipped); run it once after starting work in a repo, and again after large edits so index_search sees the new code",
	map[string]any{}, []string{})

func (t *indexRepoTool) Schema() llm.ToolSchema { return indexRepoSchema }
func (t *indexRepoTool) Risk() RiskLevel        { return RiskWrite }

func (t *indexRepoTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if t.store == nil {
		return "error: code index is not configured for this session", nil
	}
	store := t.store
	store.OnProgress = t.onProgress
	sum, err := store.Index(ctx)
	if err != nil {
		return fmt.Sprintf("error: index failed: %v", err), nil
	}
	return fmt.Sprintf("indexed %d files (%d chunks, %d unchanged skipped, %d stale removed) in %s",
		sum.Files, sum.Chunks, sum.Skipped, sum.StaleFiles, sum.Duration.Round(1e6)), nil
}

// ---------- index_search ----------

type indexSearchTool struct{ store *index.Store }

type indexSearchArgs struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

var indexSearchSchema = fnSchema("index_search", "search the workspace code index semantically for the query; returns matching code chunks with their path:line ranges — use it to find where a function, type or concept lives without grep",
	map[string]any{
		"query": strProp("what to find, e.g. 'tool argument validation'"),
		"k":     intProp("max results, default 5, max 10 (optional)"),
	},
	[]string{"query"})

func (t *indexSearchTool) Schema() llm.ToolSchema { return indexSearchSchema }
func (t *indexSearchTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *indexSearchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a indexSearchArgs
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
	if t.store == nil {
		return "error: code index is not configured for this session", nil
	}
	results, err := t.store.Search(ctx, a.Query, a.K)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(results) == 0 {
		return "no matching code found (run index_repo first?)", nil
	}
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%s:%d-%d [%.2f]\n", r.Path, r.StartLine, r.EndLine, r.Score)
		snippet := r.Content
		if len(snippet) > 800 {
			snippet = snippet[:800] + "\n..."
		}
		b.WriteString(snippet + "\n\n")
	}
	return capResult(b.String(), maxResultBytes), nil
}
