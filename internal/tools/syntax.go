package tools

import (
	"fmt"
	"path/filepath"
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
