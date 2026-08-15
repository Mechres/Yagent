package index

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// Topology is a compact package-dependency map of a workspace: which packages
// exist, what each one imports (local packages only), and the entry points.
type Topology struct {
	Module      string              // Go module path ("" when not a Go module)
	Packages    map[string][]string // package dir -> sorted local import dirs
	EntryPoints []string            // dirs that contain a main function / entry script
}

// importQueries extracts import-path strings per language. Patterns that fail
// to compile for a grammar are skipped gracefully.
var importQueries = map[string]string{
	"go": `
(import_declaration (import_spec path: (interpreted_string_literal) @imp))
(import_spec path: (interpreted_string_literal) @imp)
`,
	"python": `
(import_statement (dotted_name) @imp)
(import_from_statement module_name: (dotted_name) @imp)
`,
	"javascript": `
(import_statement source: (string) @imp)
(call_expression function: (identifier) @fn (arguments (string) @imp) (#eq? @fn "require"))
`,
	"typescript": `
(import_statement source: (string) @imp)
`,
	"rust": `
(use_declaration argument: (use_tree) @imp)
`,
	"java": `
(import_declaration) @imp
`,
	"c": `
(preproc_include path: (string_literal) @imp)
(preproc_include path: (system_lib_string) @imp)
`,
	"cpp": `
(preproc_include path: (string_literal) @imp)
(preproc_include path: (system_lib_string) @imp)
`,
}

// Topology scans workspace once and builds a package-level import DAG. It does
// not embed anything — it is a cheap structural read for architectural
// questions (code_topology tool). Packages are top-level directories that
// contain source files; imports are resolved to local packages when they match
// a known directory (Go module path prefix, relative ./.., or a bare dir).
func BuildTopology(ws string) (*Topology, error) {
	module := ""
	if data, err := os.ReadFile(filepath.Join(ws, "go.mod")); err == nil {
		for _, ln := range strings.Split(string(data), "\n") {
			if f := strings.Fields(ln); len(f) >= 2 && f[0] == "module" {
				module = f[1]
				break
			}
		}
	}

	files, err := walkSourceFiles(ws)
	if err != nil {
		return nil, err
	}

	// Pass 1: the full set of package dirs that contain source (all imports
	// must resolve against the complete set, not the walk prefix).
	hasSource := map[string]bool{}
	for _, rel := range files {
		hasSource[packageDir(rel)] = true
	}

	// Pass 2: per-package local import dirs.
	pkgImports := map[string]map[string]bool{}
	for _, rel := range files {
		dir := packageDir(rel)
		if pkgImports[dir] == nil {
			pkgImports[dir] = map[string]bool{}
		}
		for _, imp := range importsOf(ws, rel) {
			if local := resolveLocalImport(imp, dir, module, hasSource); local != "" {
				pkgImports[dir][local] = true
			}
		}
	}

	t := &Topology{Module: module, Packages: map[string][]string{}}
	for dir, deps := range pkgImports {
		if !hasSource[dir] {
			continue
		}
		var list []string
		for d := range deps {
			list = append(list, d)
		}
		sort.Strings(list)
		t.Packages[dir] = list
	}

	// Entry points: Go main packages; top-level main.py / index.js / main.ts.
	for dir := range hasSource {
		if entry, ok := isEntryPoint(ws, dir); ok && entry {
			t.EntryPoints = append(t.EntryPoints, dir)
		}
	}
	sort.Strings(t.EntryPoints)
	return t, nil
}

// Render formats the topology as a compact ASCII DAG.
func (t *Topology) Render() string {
	if t == nil || len(t.Packages) == 0 {
		return "no source files found"
	}
	var b strings.Builder
	if t.Module != "" {
		fmt.Fprintf(&b, "module %s\n", t.Module)
	}
	dirs := make([]string, 0, len(t.Packages))
	for d := range t.Packages {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		deps := t.Packages[d]
		if len(deps) == 0 {
			fmt.Fprintf(&b, "%s (no local imports)\n", d)
			continue
		}
		fmt.Fprintf(&b, "%s -> %s\n", d, strings.Join(deps, ", "))
	}
	if len(t.EntryPoints) > 0 {
		fmt.Fprintf(&b, "entry: %s\n", strings.Join(t.EntryPoints, ", "))
	}
	return b.String()
}

// packageDir is the directory owning a file, as the package identifier
// (""-rooted: "cmd/yagent", "internal/agent"; "." for root files).
func packageDir(rel string) string {
	idx := strings.LastIndexByte(rel, '/')
	if idx < 0 {
		return "."
	}
	return rel[:idx]
}

// resolveLocalImport maps an import string to a local package dir, or "" when
// it is external (a third-party dependency, stdlib, or unmatched path).
func resolveLocalImport(imp, fromDir, module string, hasSource map[string]bool) string {
	imp = strings.Trim(imp, `"'`)
	if imp == "" {
		return ""
	}
	// Relative imports (./../x) resolve relative to the importing package.
	if strings.HasPrefix(imp, "./") || strings.HasPrefix(imp, "../") {
		joined := filepath.ToSlash(filepath.Join(fromDir, imp))
		if hasSource[joined] {
			return joined
		}
		return ""
	}
	// Go module path prefix: github.com/me/module/internal/x -> internal/x.
	if module != "" && strings.HasPrefix(imp, module) {
		rest := strings.TrimPrefix(imp, module)
		rest = strings.TrimPrefix(rest, "/")
		if rest == "" {
			return "."
		}
		if hasSource[rest] {
			return rest
		}
		return ""
	}
	// Python/JS: a bare top-level name that is also a local source dir.
	if hasSource[imp] {
		return imp
	}
	// Fall back to the directory segment (import "sub/thing" may name a file).
	dir := packageDir(imp)
	if hasSource[dir] {
		return dir
	}
	return ""
}

// importsOf extracts import strings from one source file.
func importsOf(ws, rel string) []string {
	lang := languageFor(rel)
	if lang == nil {
		return nil
	}
	src, ok := importQueries[lang.name]
	if !ok || src == "" {
		return nil
	}
	content, err := os.ReadFile(filepath.Join(ws, rel))
	if err != nil {
		return nil
	}
	parser := tree_sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang.lang); err != nil {
		return nil
	}
	tree := parser.Parse(content, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	q, qerr := tree_sitter.NewQuery(lang.lang, src)
	if qerr != nil {
		return nil
	}
	defer q.Close()

	seen := map[string]bool{}
	var out []string
	cursor := tree_sitter.NewQueryCursor()
	defer cursor.Close()
	matches := cursor.Matches(q, tree.RootNode(), content)
	for {
		m := matches.Next()
		if m == nil {
			break
		}
		for _, cap := range m.Captures {
			text := strings.TrimSpace(cap.Node.Utf8Text(content))
			text = strings.Trim(text, `"'`)
			if text == "" || seen[text] {
				continue
			}
			seen[text] = true
			out = append(out, text)
		}
	}
	return out
}

// isEntryPoint reports whether a package dir contains an entry script/main.
func isEntryPoint(ws, dir string) (bool, bool) {
	if dir == "." {
		return false, false
	}
	base := filepath.Join(ws, filepath.FromSlash(dir))
	for _, name := range []string{"main.go", "main.py", "main.js", "main.ts", "index.js", "index.ts"} {
		if _, err := os.Stat(filepath.Join(base, name)); err == nil {
			return true, true
		}
	}
	return false, false
}

// walkSourceFiles lists source files in ws respecting .gitignore (reuses the
// index walker semantics without needing a Store).
func walkSourceFiles(ws string) ([]string, error) {
	var files []string
	m := &gitignoreMatcher{}
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if data, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err == nil {
			m.push(parseGitignore(rel, string(data)))
			defer m.pop()
		}
		for _, e := range entries {
			name := e.Name()
			relPath := filepath.ToSlash(filepath.Join(rel, name))
			if name == ".git" || name == ".yagent" {
				continue
			}
			if m.ignored(relPath, e.IsDir()) {
				continue
			}
			if e.IsDir() {
				if err := walk(filepath.Join(dir, name), relPath); err != nil {
					return err
				}
				continue
			}
			if strings.HasPrefix(name, ".") {
				continue
			}
			full := filepath.Join(dir, name)
			info, err := e.Info()
			if err != nil || info.Size() > maxFileBytes {
				continue
			}
			if lockFiles[filepath.ToSlash(name)] {
				continue
			}
			data, err := os.ReadFile(full)
			if err != nil || isBinary(data) {
				continue
			}
			if SupportedSourceExt(strings.ToLower(filepath.Ext(name))) {
				files = append(files, relPath)
			}
		}
		return nil
	}
	if err := walk(ws, ""); err != nil {
		return nil, err
	}
	return files, nil
}

// OrderByDeps returns the given package dirs sorted so that upstream packages
// (those importing nothing local) come first and downstream callers last. It
// follows the import DAG: a package appears only after every package it (directly
// or transitively) imports that is also in the set. Unrelated packages keep a
// stable lexical order. Used by the dependency-ranked fix hint so a model edits
// the upstream definition before the callers that break against it.
func (t *Topology) OrderByDeps(dirs map[string]bool) []string {
	if t == nil {
		return nil
	}
	depth := map[string]int{}
	var visit func(d string, visiting map[string]bool) int
	visit = func(d string, visiting map[string]bool) int {
		if n, ok := depth[d]; ok {
			return n
		}
		if visiting[d] {
			return 0 // cycle guard — treat as depth 0 rather than recursing forever
		}
		visiting[d] = true
		max := 0
		for _, dep := range t.Packages[d] {
			if !dirs[dep] {
				continue
			}
			if n := visit(dep, visiting); n > max {
				max = n
			}
		}
		delete(visiting, d)
		depth[d] = max + 1
		return depth[d]
	}
	for d := range dirs {
		visit(d, map[string]bool{})
	}
	ordered := make([]string, 0, len(dirs))
	for d := range dirs {
		ordered = append(ordered, d)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		di, dj := depth[ordered[i]], depth[ordered[j]]
		if di != dj {
			return di < dj // shallower (more upstream) first
		}
		return ordered[i] < ordered[j]
	})
	return ordered
}
