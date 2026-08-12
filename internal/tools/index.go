package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mechres/Yagent/internal/index"
	"github.com/Mechres/Yagent/internal/llm"
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
	store.SetOnProgress(t.onProgress)
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
	Query  string `json:"query"`
	K      int    `json:"k,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	Type   string `json:"type,omitempty"`
}

var indexSearchSchema = fnSchema("index_search", "search the workspace code index: hybrid semantic search by query, or exact symbol lookup by name — set 'symbol' to find a declaration by name (optionally filter with 'type': function/type/namespace). Returns path:line ranges",
	map[string]any{
		"query":  strProp("what to find, e.g. 'tool argument validation' (optional when symbol is set)"),
		"k":      intProp("max results, default 5, max 10 (optional)"),
		"symbol": strProp("exact symbol/declaration name to look up, e.g. AssembleContext (optional)"),
		"type":   strProp("filter symbol results by kind: function, type, namespace (optional)"),
	},
	[]string{})

func (t *indexSearchTool) Schema() llm.ToolSchema { return indexSearchSchema }
func (t *indexSearchTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *indexSearchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a indexSearchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Query == "" && a.Symbol == "" {
		return "", validationErrorf(`either "query" or "symbol" is required`)
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
	if a.Symbol != "" {
		return t.symbolLookup(ctx, a)
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

// symbolLookup returns exact-name symbol matches with their surrounding chunk.
func (t *indexSearchTool) symbolLookup(ctx context.Context, a indexSearchArgs) (string, error) {
	symbols, err := t.store.SearchSymbol(ctx, a.Symbol, a.Type)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(symbols) == 0 {
		return fmt.Sprintf("no symbol named %q%sfound (run index_repo first?)", a.Symbol, kindNote(a.Type)), nil
	}
	if len(symbols) > a.K {
		symbols = symbols[:a.K]
	}
	var b strings.Builder
	for _, s := range symbols {
		fmt.Fprintf(&b, "%s:%d [%s] %s\n", s.Path, s.Line, s.Kind, s.Name)
	}
	return capResult(b.String(), maxResultBytes), nil
}

func kindNote(kind string) string {
	if kind == "" {
		return " "
	}
	return " of kind " + kind + " "
}

// ---------- code_topology ----------

// codeTopologyTool renders the workspace's package-level import DAG (module
// path, which packages import which, entry points) without loading files into
// context — a cheap architectural map for large-repo questions.
type codeTopologyTool struct{ ws string }

var codeTopologySchema = fnSchema("code_topology", "render the workspace's package topology as a compact ASCII dependency DAG: the module path, each package directory and which local packages it imports, and entry points (main packages / entry scripts). Use it for architectural questions like 'what are the layers?' or 'what depends on X?' before drilling in with fs_read/code_outline. No index needed; reads import statements directly",
	map[string]any{}, []string{})

func (t *codeTopologyTool) Schema() llm.ToolSchema { return codeTopologySchema }
func (t *codeTopologyTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *codeTopologyTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return "", err
	}
	topo, err := index.BuildTopology(t.ws)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return capResult(topo.Render(), maxResultBytes), nil
}

// ---------- code_references ----------

type codeReferencesTool struct{ store *index.Store }

type codeReferencesArgs struct {
	Symbol string `json:"symbol"`
}

var codeReferencesSchema = fnSchema("code_references", "find every call site of a function by name: returns 'path:line' for each caller (call-graph, from the code index). Pair with index_search symbol lookup: search tells you where X is declared, code_references tells you who calls it",
	map[string]any{
		"symbol": strProp("the function name to find callers of, e.g. AssembleContext"),
	},
	[]string{"symbol"})

func (t *codeReferencesTool) Schema() llm.ToolSchema { return codeReferencesSchema }
func (t *codeReferencesTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *codeReferencesTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a codeReferencesArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Symbol == "" {
		return "", validationErrorf(`argument "symbol" is required`)
	}
	if t.store == nil {
		return "error: code index is not configured for this session", nil
	}
	refs := t.store.References(ctx, a.Symbol)
	if len(refs) == 0 {
		return fmt.Sprintf("no call sites for %q (run index_repo first?)", a.Symbol), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d call site(s) for %s:\n", len(refs), a.Symbol)
	for _, r := range refs {
		fmt.Fprintf(&b, "  %s:%d\n", r.Path, r.Line)
	}
	return capResult(b.String(), maxResultBytes), nil
}
