package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
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

// ---------- code_impact ----------

// codeImpactTool computes the change radius of a symbol or file before an edit:
// which files call it (downstream consumers), which packages depend on its
// package, and which test files cover it. Deterministic (zero LLM calls) so a
// small model knows the blast radius without guessing.
type codeImpactTool struct {
	store *index.Store
	ws    string
}

type codeImpactArgs struct {
	Path   string `json:"path"`
	Symbol string `json:"symbol,omitempty"`
}

var codeImpactSchema = fnSchema("code_impact", "compute the change radius of a symbol or file BEFORE editing: who calls it (call-graph), which packages depend on its package (import DAG), and which test files cover it. Use it when about to change a function/type/interface to see every consumer that might break — deterministic, no LLM cost. Requires the code index (run index_repo first)",
	map[string]any{
		"path":   strProp("the source file you plan to edit, relative to the workspace"),
		"symbol": strProp("an optional symbol name in that file to narrow the impact to callers of just that declaration"),
	},
	[]string{"path"})

func (t *codeImpactTool) Schema() llm.ToolSchema { return codeImpactSchema }
func (t *codeImpactTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *codeImpactTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a codeImpactArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Path == "" {
		return "", validationErrorf(`argument "path" is required`)
	}
	if t.store == nil {
		return "error: code index is not configured for this session", nil
	}

	// Downstream callers: the whole file, or just one symbol in it.
	callers := map[string][]string{} // path -> "line:callee"
	if a.Symbol != "" {
		for _, r := range t.store.References(ctx, a.Symbol) {
			callers[r.Path] = append(callers[r.Path], fmt.Sprintf("%d:%s", r.Line, a.Symbol))
		}
	} else {
		syms, err := index.SymbolsForFile(t.ws, a.Path)
		if err == nil {
			for _, s := range syms {
				for _, r := range t.store.References(ctx, s.Name) {
					callers[r.Path] = append(callers[r.Path], fmt.Sprintf("%d:%s", r.Line, s.Name))
				}
			}
		}
	}

	// Package-level dependents from the import DAG.
	topo, err := index.BuildTopology(t.ws)
	myPkg := packageDirOf(a.Path)
	var dependents []string
	if err == nil && myPkg != "" {
		for dir, deps := range topo.Packages {
			for _, d := range deps {
				if d == myPkg && dir != myPkg {
					dependents = append(dependents, dir)
				}
			}
		}
		sort.Strings(dependents)
	}

	// Test files covering the touched file's package and each caller package.
	tests := testFilesFor(t.ws, a.Path, callers)

	var b strings.Builder
	if a.Symbol != "" {
		fmt.Fprintf(&b, "impact of %s in %s:\n", a.Symbol, a.Path)
	} else {
		fmt.Fprintf(&b, "impact of editing %s:\n", a.Path)
	}
	if len(callers) == 0 && len(dependents) == 0 && len(tests) == 0 {
		b.WriteString("  no consumers found — likely safe to edit\n")
	}
	if len(callers) > 0 {
		fmt.Fprintf(&b, "  %d direct caller file(s):\n", len(callers))
		paths := make([]string, 0, len(callers))
		for p := range callers {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Fprintf(&b, "    %s (%s)\n", p, strings.Join(callers[p], ", "))
		}
	}
	if len(dependents) > 0 {
		fmt.Fprintf(&b, "  %d package(s) import this package:\n", len(dependents))
		for _, d := range dependents {
			fmt.Fprintf(&b, "    %s\n", d)
		}
	}
	if len(tests) > 0 {
		fmt.Fprintf(&b, "  %d test file(s) covering the touched packages:\n", len(tests))
		for _, tc := range tests {
			fmt.Fprintf(&b, "    %s\n", tc)
		}
	}
	return capResult(b.String(), maxResultBytes), nil
}

// packageDirOf returns the slash-separated directory of a workspace-relative
// path ("" for root-level files and empty paths).
func packageDirOf(rel string) string {
	idx := strings.LastIndexByte(rel, '/')
	if idx < 0 {
		return ""
	}
	return rel[:idx]
}

// testFilesFor lists _test.go / test_*.py files in the touched file's package
// and every caller's package, deduplicated.
func testFilesFor(ws, touched string, callers map[string][]string) []string {
	dirs := map[string]bool{}
	if d := packageDirOf(touched); d != "" {
		dirs[d] = true
	}
	for p := range callers {
		if d := packageDirOf(p); d != "" {
			dirs[d] = true
		}
	}
	seen := map[string]bool{}
	var out []string
	for d := range dirs {
		matches, _ := filepath.Glob(filepath.Join(ws, filepath.FromSlash(d), "*_test.go"))
		for _, m := range matches {
			rel := filepath.ToSlash(m)
			if i := strings.Index(rel, "/"); i >= 0 {
				rel = rel[i+1:]
			}
			if rel = strings.TrimPrefix(rel, ws+"/"); !seen[rel] {
				seen[rel] = true
				out = append(out, rel)
			}
		}
		py, _ := filepath.Glob(filepath.Join(ws, filepath.FromSlash(d), "test_*.py"))
		for _, m := range py {
			rel := strings.TrimPrefix(filepath.ToSlash(m), ws+"/")
			if !seen[rel] {
				seen[rel] = true
				out = append(out, rel)
			}
		}
	}
	sort.Strings(out)
	return out
}
