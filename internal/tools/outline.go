package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"yagent/internal/index"
	"yagent/internal/llm"
)

// code_outline lists a file's or package's declarations as signatures (name +
// kind + line) without bodies, letting the agent survey a package in a few
// hundred tokens instead of reading it all.
type codeOutlineTool struct{ ws string }

type codeOutlineArgs struct {
	Path string `json:"path"`
}

var codeOutlineSchema = fnSchema("code_outline", "list the declarations (functions, types, namespaces) of a source file or a whole directory as name/kind/line signatures — no bodies. Use it to survey a package layout cheaply before drilling into files with fs_read.",
	map[string]any{
		"path": strProp("a source file or directory relative to the workspace"),
	},
	[]string{"path"})

func (t *codeOutlineTool) Schema() llm.ToolSchema { return codeOutlineSchema }
func (t *codeOutlineTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *codeOutlineTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a codeOutlineArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Path == "" {
		return "", validationErrorf(`argument "path" is required`)
	}
	full, err := resolvePath(t.ws, a.Path)
	if err != nil {
		return "", err
	}

	var files []string
	if fi, err := os.Stat(full); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	} else if !fi.IsDir() {
		files = []string{a.Path}
	} else {
		_ = filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if index.SupportedSourceExt(filepath.Ext(p)) {
				if rel, err := filepath.Rel(t.ws, p); err == nil {
					files = append(files, filepath.ToSlash(rel))
				}
			}
			return nil
		})
	}
	if len(files) == 0 {
		return "no source files found", nil
	}
	sort.Strings(files)

	var b strings.Builder
	for _, rel := range files {
		syms, err := index.SymbolsForFile(t.ws, rel)
		if err != nil {
			continue
		}
		if len(syms) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s\n", rel)
		for _, s := range syms {
			fmt.Fprintf(&b, "  %d [%s] %s\n", s.Line, s.Kind, s.Name)
		}
	}
	if b.Len() == 0 {
		return "no declarations found", nil
	}
	return capResult(b.String(), maxResultBytes), nil
}
