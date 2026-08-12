package index

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// SyntaxError is one tree-sitter ERROR/MISSING node in a source file.
type SyntaxError struct {
	Line int    // 1-based
	Col  int    // 1-based
	Text string // a short preview of the offending source
}

// SyntaxErrors parses content for a supported language (from the file's
// extension) and returns the ERROR/MISSING nodes. Returns nil for unsupported
// files or clean content. Used to pre-flight edits before they touch disk.
func SyntaxErrors(rel, content string) []SyntaxError {
	lang := languageFor(rel)
	if lang == nil {
		return nil
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang.lang); err != nil {
		return nil
	}
	tree := parser.Parse([]byte(content), nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()
	var out []SyntaxError
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n.IsError() || n.IsMissing() {
			pos := n.StartPosition()
			out = append(out, SyntaxError{
				Line: int(pos.Row) + 1,
				Col:  int(pos.Column) + 1,
				Text: nodePreview(content, n),
			})
			return
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())
	return out
}

// nodePreview renders a short single-line snippet of the offending node.
func nodePreview(content string, n *sitter.Node) string {
	s := content[n.StartByte():n.EndByte()]
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return s
}

// SliceSymbol returns the exact source span of a top-level declaration (name,
// body, and any attached doc comment) from a supported-language file — the
// surgical read behind the code_slice tool. ok=false when the file is
// unsupported or the symbol isn't a top-level declaration there.
func SliceSymbol(workspace, rel, symbol string) (string, bool, error) {
	content, err := os.ReadFile(filepath.Join(workspace, rel))
	if err != nil {
		return "", false, err
	}
	lang := languageFor(rel)
	if lang == nil {
		return "", false, nil
	}
	decls, _, tree := parseDeclsWithTree(rel, string(content), lang)
	defer tree.Close()
	for _, d := range decls {
		if d.name == symbol {
			return d.text, true, nil
		}
	}
	return "", false, nil
}

// ExportedSymbols returns the names of a source file's exported top-level
// declarations (public API surface): functions, methods, types. It is the
// symbol-delta source for the diff_semantic write guardrail — a file edit must
// never silently drop a public symbol. Returns nil for unsupported files or
// languages without a tree-sitter grammar.
func ExportedSymbols(rel, content string) []string {
	lang := languageFor(rel)
	if lang == nil {
		return nil
	}
	decls := parseDecls(rel, content, lang)
	var out []string
	for _, d := range decls {
		if exportedName(lang.name, d.name) {
			out = append(out, d.name)
		}
	}
	return out
}

// exportedName reports whether a declaration name is externally visible in a
// language. Heuristic per grammar family: Go exports uppercase initial
// identifiers; Python has no hard keyword so we conservatively keep every
// non-dunder top-level name; JavaScript/TypeScript export whatever `export`
// marks, which we cannot cheaply distinguish here, so we treat all top-level
// names as public (the guardrail errs on the safe side).
func exportedName(langName, name string) bool {
	if name == "" {
		return false
	}
	switch langName {
	case "go":
		r, _ := utf8.DecodeRuneInString(name)
		return unicode.IsUpper(r)
	case "python":
		return !strings.HasPrefix(name, "__") && !strings.HasPrefix(name, "_")
	default:
		return true
	}
}
