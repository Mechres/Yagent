package tools

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Mechres/Yagent/internal/index"
)

// preflightSyntax parses a modified source string with tree-sitter and, when it
// would introduce ERROR/MISSING nodes, returns a descriptive message so the
// model can fix the edit BEFORE it touches disk (a deterministic guardrail, not
// prompt hope). Returns "" for unsupported languages and clean content.
func preflightSyntax(path, content string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if !index.SupportedSourceExt(ext) {
		return ""
	}
	errs := index.SyntaxErrors(path, content)
	if len(errs) == 0 {
		return ""
	}
	e := errs[0]
	return fmt.Sprintf("the change would introduce a syntax error at line %d, col %d (%q) — fix the edit; the file was NOT modified", e.Line, e.Col, e.Text)
}

// preflightSymbols is the diff_semantic guardrail: it compares the exported
// top-level declaration surface of a file before and after an edit. If an
// exported symbol that existed before would disappear, the write is blocked —
// a targeted line edit should never silently delete a public function/type.
// Returns "" when the file is unsupported, has no exported symbols, or none
// are lost. fs_edit/fs_patch call it after preflightSyntax.
func preflightSymbols(path, oldContent, newContent string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if !index.SupportedSourceExt(ext) {
		return ""
	}
	before := index.ExportedSymbols(path, oldContent)
	if len(before) == 0 {
		return ""
	}
	afterSet := map[string]bool{}
	for _, s := range index.ExportedSymbols(path, newContent) {
		afterSet[s] = true
	}
	var lost []string
	for _, s := range before {
		if !afterSet[s] {
			lost = append(lost, s)
		}
	}
	if len(lost) == 0 {
		return ""
	}
	sort.Strings(lost)
	return fmt.Sprintf("the change would delete exported symbol(s): %s — restore them; a targeted edit must not remove public API. If the deletion is intentional, split it into its own edit and state so. The file was NOT modified",
		strings.Join(lost, ", "))
}
