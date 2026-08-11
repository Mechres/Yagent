package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	tsbash "github.com/tree-sitter/tree-sitter-bash/bindings/go"
	tsc "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tscpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tscss "github.com/tree-sitter/tree-sitter-css/bindings/go"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tshtml "github.com/tree-sitter/tree-sitter-html/bindings/go"
	tsjava "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tspy "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tsrust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tstypescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Chunk limits from docs/PLAN.md M4.
const (
	// maxChunkChars caps a single chunk; larger declarations are split on
	// line windows.
	maxChunkChars = 1200
	// fallbackWindow is the line-window size for unsupported files and
	// files that fail to parse.
	fallbackWindow = 80
	// minDeclChars groups adjacent tiny declarations into one chunk so the
	// index doesn't fill with one-line stubs.
	minDeclChars = 60
	// maxFileBytes caps files the walker will index.
	maxFileBytes = 512 << 10
)

// Symbol is one top-level declaration's identity (for symbol-aware search).
type Symbol struct {
	Path string
	Name string
	Kind string // friendly kind: function, type, namespace, ...
	Line int    // 1-based
}

// SupportedSourceExt reports whether a file extension is structurally
// chunkable (has a tree-sitter grammar).
func SupportedSourceExt(ext string) bool {
	lower := strings.ToLower(ext)
	for i := range languages {
		for _, e := range languages[i].ext {
			if lower == e {
				return true
			}
		}
	}
	return false
}

// SymbolsForFile returns the declaration symbols of rel within workspace.
func SymbolsForFile(workspace, rel string) ([]Symbol, error) {
	content, err := os.ReadFile(filepath.Join(workspace, rel))
	if err != nil {
		return nil, err
	}
	return symbolsFor(rel, string(content)), nil
}

// symbolsFor extracts top-level declaration symbols (name, kind, line) from a
// supported language's source. Returns nil for unsupported files or when no
// declarations are found.
func symbolsFor(path, content string) []Symbol {
	lang := languageFor(path)
	if lang == nil {
		return nil
	}
	return symbolsFromDecls(path, parseDecls(path, content, lang))
}

// declName extracts the identifier a declaration node names, via the grammar's
// "name" field or the first identifier-like descendant.
func declName(n *sitter.Node, content string) string {
	if name := n.ChildByFieldName("name"); name != nil {
		return content[name.StartByte():name.EndByte()]
	}
	var found string
	var walk func(*sitter.Node)
	walk = func(x *sitter.Node) {
		if found != "" {
			return
		}
		switch x.Kind() {
		case "identifier", "type_identifier", "field_identifier", "type_name",
			"class_name", "function_name", "method_name":
			found = content[x.StartByte():x.EndByte()]
			return
		}
		for i := uint(0); i < x.NamedChildCount(); i++ {
			walk(x.NamedChild(i))
		}
	}
	walk(n)
	return found
}

// friendlyKind maps a grammar node kind to a coarse symbol kind.
func friendlyKind(kind string) string {
	switch kind {
	case "function_declaration", "function_item", "method_declaration",
		"method_definition", "generator_function_declaration", "arrow_function":
		return "function"
	case "struct_item", "struct_specifier", "class_declaration", "class_specifier",
		"record_declaration", "impl_item", "interface_declaration", "trait_item",
		"enum_declaration", "enum_item", "union_specifier", "type_alias_declaration",
		"type_item", "typedef_declaration":
		return "type"
	case "namespace_definition", "mod_item":
		return "namespace"
	default:
		return kind
	}
}

// Chunk is one indexed unit of source text.
type Chunk struct {
	Path      string
	StartLine int // 1-based, inclusive
	EndLine   int // 1-based, inclusive
	Content   string
}

// language binds a tree-sitter grammar to the file extensions and the node
// kinds that count as top-level declarations.
type language struct {
	name string
	ext  []string
	lang *sitter.Language
	decl map[string]bool
}

var languages = []language{
	{
		name: "go",
		ext:  []string{".go"},
		lang: sitter.NewLanguage(tsgo.Language()),
		decl: map[string]bool{
			"function_declaration": true, "method_declaration": true,
			"type_declaration": true, "var_declaration": true, "const_declaration": true,
		},
	},
	{
		name: "python",
		ext:  []string{".py"},
		lang: sitter.NewLanguage(tspy.Language()),
		decl: map[string]bool{
			"function_definition": true, "class_definition": true, "decorated_definition": true,
		},
	},
	{
		name: "javascript",
		ext:  []string{".js", ".jsx", ".mjs", ".cjs"},
		lang: sitter.NewLanguage(tsjs.Language()),
		decl: map[string]bool{
			"function_declaration": true, "class_declaration": true, "generator_function_declaration": true,
		},
	},
	{
		name: "typescript",
		ext:  []string{".ts", ".mts", ".cts"},
		lang: sitter.NewLanguage(tstypescript.LanguageTypescript()),
		decl: map[string]bool{
			"function_declaration": true, "class_declaration": true, "interface_declaration": true,
			"type_alias_declaration": true, "method_definition": true, "generator_function_declaration": true,
		},
	},
	{
		name: "tsx",
		ext:  []string{".tsx"},
		lang: sitter.NewLanguage(tstypescript.LanguageTSX()),
		decl: map[string]bool{
			"function_declaration": true, "class_declaration": true, "interface_declaration": true,
			"type_alias_declaration": true, "arrow_function": true, "generator_function_declaration": true,
		},
	},
	{
		name: "rust",
		ext:  []string{".rs"},
		lang: sitter.NewLanguage(tsrust.Language()),
		decl: map[string]bool{
			"function_item": true, "struct_item": true, "enum_item": true, "impl_item": true,
			"trait_item": true, "type_item": true, "const_item": true, "static_item": true,
			"mod_item": true, "macro_definition": true,
		},
	},
	{
		name: "c",
		ext:  []string{".c", ".h"},
		lang: sitter.NewLanguage(tsc.Language()),
		decl: map[string]bool{
			"function_definition": true, "struct_specifier": true, "enum_specifier": true,
			"union_specifier": true, "type_definition": true, "declaration": true,
		},
	},
	{
		name: "cpp",
		ext:  []string{".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx"},
		lang: sitter.NewLanguage(tscpp.Language()),
		decl: map[string]bool{
			"function_definition": true, "class_specifier": true, "struct_specifier": true,
			"enum_specifier": true, "union_specifier": true, "namespace_definition": true,
			"using_declaration": true, "template_declaration": true, "alias_declaration": true,
		},
	},
	{
		name: "java",
		ext:  []string{".java"},
		lang: sitter.NewLanguage(tsjava.Language()),
		decl: map[string]bool{
			"method_declaration": true, "class_declaration": true, "interface_declaration": true,
			"enum_declaration": true, "record_declaration": true, "constructor_declaration": true,
		},
	},
	{
		name: "bash",
		ext:  []string{".sh", ".bash"},
		lang: sitter.NewLanguage(tsbash.Language()),
		decl: map[string]bool{
			"function_definition": true,
		},
	},
	{
		name: "html",
		ext:  []string{".html", ".htm", ".xhtml"},
		lang: sitter.NewLanguage(tshtml.Language()),
		decl: map[string]bool{
			"element": true, "style_element": true, "script_element": true,
		},
	},
	{
		name: "css",
		ext:  []string{".css", ".scss"},
		lang: sitter.NewLanguage(tscss.Language()),
		decl: map[string]bool{
			"rule_set": true, "at_rule": true, "media_statement": true,
		},
	},
}

// languageFor returns the grammar for a path, or nil for unsupported files.
func languageFor(path string) *language {
	lower := strings.ToLower(path)
	for i := range languages {
		for _, ext := range languages[i].ext {
			if strings.HasSuffix(lower, ext) {
				return &languages[i]
			}
		}
	}
	return nil
}

// chunkSource splits file content into chunks. Supported languages are split
// on top-level declarations (doc comments attached); unsupported files or
// parse failures fall back to fixed-size line windows.
func chunkSource(path, content string) []Chunk {
	if lang := languageFor(path); lang != nil {
		if chunks := structuralChunks(path, content, lang); len(chunks) > 0 {
			return chunks
		}
	}
	return lineWindowChunks(path, content, fallbackWindow)
}

// structuralChunks splits on top-level declarations. Returns nil when the
// file is empty or yields no declarations (caller falls back to line windows).
func structuralChunks(path, content string, lang *language) []Chunk {
	decls := parseDecls(path, content, lang)
	if len(decls) == 0 {
		return nil
	}
	return chunksFromDecls(path, content, decls)
}

// parseDeclsWithTree parses a supported file once and returns its top-level
// declarations (text, line, name, kind), with adjacent doc comments attached,
// plus the root node and tree so call refs can be extracted from the same
// parse. The caller is responsible for closing the returned tree.
func parseDeclsWithTree(path, content string, lang *language) ([]declInfo, *sitter.Node, *sitter.Tree) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang.lang); err != nil {
		return nil, nil, nil
	}
	tree := parser.Parse([]byte(content), nil)
	if tree == nil {
		return nil, nil, nil
	}
	root := tree.RootNode()

	var kids []*sitter.Node
	for i := uint(0); i < root.NamedChildCount(); i++ {
		kids = append(kids, root.NamedChild(i))
	}
	var out []declInfo
	for i, k := range kids {
		if !lang.decl[k.Kind()] {
			continue
		}
		startByte := int(k.StartByte())
		sp := k.StartPosition()
		startLine := int(sp.Row) + 1
		declLine := startLine
		// Attach adjacent doc-comment runs above the declaration.
		for j := i - 1; j >= 0 && kids[j].Kind() == "comment"; j-- {
			if int(sp.Row)-int(kids[j].EndPosition().Row) > 1 {
				break
			}
			startByte = int(kids[j].StartByte())
			startLine = int(kids[j].StartPosition().Row) + 1
		}
		out = append(out, declInfo{
			startLine: startLine,
			declLine:  declLine,
			text:      content[startByte:k.EndByte()],
			name:      declName(k, content),
			kind:      friendlyKind(k.Kind()),
		})
	}
	return out, root, tree
}

// declInfo is one parsed top-level declaration. startLine includes any
// attached doc comments; declLine is the declaration's own first line.
type declInfo struct {
	startLine int
	declLine  int
	text      string
	name      string
	kind      string
}

// chunksFromDecls builds chunks from parsed declarations, merging small ones
// and splitting oversized ones.
func chunksFromDecls(path, content string, decls []declInfo) []Chunk {
	// Tiny files become one chunk.
	if len(content) < minDeclChars*2 {
		return []Chunk{{
			Path:      path,
			StartLine: decls[0].startLine,
			EndLine:   decls[len(decls)-1].startLine + strings.Count(decls[len(decls)-1].text, "\n"),
			Content:   content,
		}}
	}

	var chunks []Chunk
	var pending []Chunk
	flush := func() {
		if len(pending) == 0 {
			return
		}
		var b strings.Builder
		for i, c := range pending {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(c.Content)
		}
		chunks = append(chunks, Chunk{
			Path:      path,
			StartLine: pending[0].StartLine,
			EndLine:   pending[len(pending)-1].EndLine,
			Content:   b.String(),
		})
		pending = nil
	}

	for _, d := range decls {
		text := d.text
		startLine := d.startLine
		endLine := startLine + strings.Count(text, "\n")
		if len(text) < minDeclChars {
			pending = append(pending, Chunk{Path: path, StartLine: startLine, EndLine: endLine, Content: text})
			continue
		}
		flush()
		if len(text) > maxChunkChars {
			chunks = append(chunks, splitChunk(path, text, startLine)...)
			continue
		}
		chunks = append(chunks, Chunk{Path: path, StartLine: startLine, EndLine: endLine, Content: text})
	}
	flush()
	return chunks
}

// symbolsFromDecls builds the symbol list from parsed declarations.
func symbolsFromDecls(path string, decls []declInfo) []Symbol {
	var out []Symbol
	for _, d := range decls {
		if d.name == "" {
			continue
		}
		out = append(out, Symbol{Path: path, Name: d.name, Kind: d.kind, Line: d.declLine})
	}
	return out
}

// chunkAndSymbols parses a file once and returns both its chunks and symbols
// (falling back to line windows for unsupported files). Used by Index() to
// avoid a double parse.
func chunkAndSymbols(path, content string) ([]Chunk, []Symbol, []CallRef) {
	lang := languageFor(path)
	if lang == nil {
		return lineWindowChunks(path, content, fallbackWindow), nil, nil
	}
	decls, root, tree := parseDeclsWithTree(path, content, lang)
	defer tree.Close()
	refs := refsFromTree(path, lang, root, []byte(content))
	if len(decls) == 0 {
		return lineWindowChunks(path, content, fallbackWindow), nil, refs
	}
	return chunksFromDecls(path, content, decls), symbolsFromDecls(path, decls), refs
}

// parseDecls extracts declarations, parsing the file once and returning the
// root node so call refs can be extracted from the same parse.
func parseDecls(path, content string, lang *language) []declInfo {
	decls, _, tree := parseDeclsWithTree(path, content, lang)
	if tree != nil {
		tree.Close()
	}
	return decls
}

// splitChunk splits an oversized chunk into windows bounded by both
// fallbackWindow lines and maxChunkChars (whichever hits first).
func splitChunk(path, content string, startLine int) []Chunk {
	return windowChunks(path, content, startLine, fallbackWindow)
}

// lineWindowChunks is the fallback for unsupported or unparsable files.
func lineWindowChunks(path, content string, window int) []Chunk {
	return windowChunks(path, content, 1, window)
}

func windowChunks(path, content string, startLine, window int) []Chunk {
	lines := strings.Split(content, "\n")
	var chunks []Chunk
	start := 0
	for start < len(lines) {
		end := start
		size := 0
		for end < len(lines) && end-start < window {
			lineLen := len(lines[end]) + 1
			if end > start && size+lineLen > maxChunkChars {
				break // keep chunks within the ~1200-char cap
			}
			size += lineLen
			end++
		}
		if end == start {
			end = start + 1 // a single line longer than the cap still gets chunked
		}
		chunks = append(chunks, Chunk{
			Path:      path,
			StartLine: startLine + start,
			EndLine:   startLine + end - 1,
			Content:   strings.Join(lines[start:end], "\n"),
		})
		start = end
	}
	return chunks
}

// displayPath renders the chunk location as path:start-end.
func displayPath(c Chunk) string {
	if c.EndLine == c.StartLine {
		return fmt.Sprintf("%s:%d", c.Path, c.StartLine)
	}
	return fmt.Sprintf("%s:%d-%d", c.Path, c.StartLine, c.EndLine)
}
