package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/Mechres/Yagent/internal/index"
	"github.com/Mechres/Yagent/internal/llm"
)

// ---------- code_slice ----------

// codeSliceTool extracts one declaration's exact source span (with its doc
// comment) from a file via tree-sitter — reading one symbol instead of a whole
// file keeps context lean on large modules.
type codeSliceTool struct {
	ws string
}

type codeSliceArgs struct {
	Path   string `json:"path"`
	Symbol string `json:"symbol"`
}

var codeSliceSchema = fnSchema("code_slice", "read just one declaration (function, type, method) from a source file — its exact body plus any doc comment — without loading the whole file. Far cheaper than fs_read on large modules. Use when you only need to inspect a specific symbol.",
	map[string]any{
		"path":   strProp("path to the source file, relative to the workspace"),
		"symbol": strProp("the exact declaration name to extract"),
	},
	[]string{"path", "symbol"})

func (t *codeSliceTool) Schema() llm.ToolSchema { return codeSliceSchema }
func (t *codeSliceTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *codeSliceTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a codeSliceArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Path == "" || a.Symbol == "" {
		return "", validationErrorf(`both "path" and "symbol" are required`)
	}
	full, err := resolvePath(t.ws, a.Path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(t.ws, full)
	if err != nil {
		return "", err
	}
	text, ok, err := index.SliceSymbol(t.ws, rel, a.Symbol)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if !ok {
		return fmt.Sprintf("symbol %q not found as a top-level declaration in %s (use index_search or code_references to locate it)", a.Symbol, a.Path), nil
	}
	return capResult(text, maxResultBytes), nil
}
