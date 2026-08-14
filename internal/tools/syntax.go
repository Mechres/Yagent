package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Mechres/Yagent/internal/index"
	"gopkg.in/yaml.v3"
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

// preflightImports checks a modified Go or Python source for identifiers used
// as package qualifiers without a matching import — the single most common
// greenfield slip a 7B-9B model makes (deepseek review #5). It returns a
// remediation note (NOT a block; the compile gate owns rejection) so the model
// fixes the import before burning a diagnostics round-trip. Returns "" when the
// file is not Go/Python or no missing import is detected.
func preflightImports(path, content string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return missingGoImports(content)
	case ".py":
		return missingPyImports(content)
	}
	return ""
}

// goStdlib is a compact set of the standard library packages a small model most
// often references without importing. The scanner only flags these (never
// third-party or workspace packages) so it never false-positives.
var goStdlib = map[string]bool{
	"fmt": true, "os": true, "strings": true, "strconv": true, "sort": true,
	"time": true, "math": true, "errors": true, "io": true, "log": true,
	"path/filepath": true, "encoding/json": true, "net/http": true, "bytes": true,
	"context": true, "regexp": true, "reflect": true, "sync": true, "unicode": true,
	"math/rand": true, "flag": true, "bufio": true, "crypto/sha256": true,
}

// missingGoImports finds `pkg.Identifier(` usages whose package isn't imported.
// It strips comments and string literals first so `fmt.Println` inside a string
// or comment is never flagged (false-positive safety).
func missingGoImports(content string) string {
	imported := map[string]bool{}
	// collect imports: `"fmt"`, `"path/filepath"`, or aliased `f "fmt"`
	for _, ln := range strings.Split(content, "\n") {
		trim := strings.TrimSpace(ln)
		if !strings.HasPrefix(trim, "import") {
			continue
		}
		// single-line import
		if m := regexp.MustCompile(`"([a-z0-9_/.-]+)"`).FindAllStringSubmatch(trim, -1); len(m) > 0 {
			for _, mm := range m {
				imported[mm[1]] = true
			}
		}
	}
	// multi-line import blocks
	re := regexp.MustCompile(`(?m)^\s*([a-zA-Z_][a-zA-Z0-9_]*\s+)?"([a-z0-9_/.-]+)"`)
	for _, m := range re.FindAllStringSubmatch(content, -1) {
		pkg := m[2]
		imported[pkg] = true
		// alias f "fmt" imports as "fmt" for usage matching
		if m[1] != "" {
			imported[pkg] = true
		}
	}
	// strip line comments and strings before scanning usages
	clean := stripGoNoise(content)
	var missing []string
	for pkg := range goStdlib {
		base := pkg
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if imported[pkg] || imported[base] {
			continue
		}
		// `pkg.` or `base.` followed by an identifier
		pat := regexp.MustCompile(`\b(` + regexp.QuoteMeta(base) + `)\.\s*[A-Z][A-Za-z0-9_]*\s*\(`)
		if pat.MatchString(clean) {
			missing = append(missing, fmt.Sprintf(`"%s"`, pkg))
		}
	}
	if len(missing) == 0 {
		return ""
	}
	sort.Strings(missing)
	return fmt.Sprintf("NOTE: %s is/are referenced but not imported — add the import to prevent a compile error", strings.Join(missing, ", "))
}

// stripGoNoise removes Go line comments and double-quoted string literals so
// the import scanner never flags text inside strings or comments.
func stripGoNoise(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			// skip a double-quoted string (no escapes handled beyond \" )
			b.WriteByte(' ')
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			continue
		}
		if s[i] == '/' && i+1 < len(s) && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' {
				b.WriteByte(' ')
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// missingPyImports finds top-level `import x` / `from x import y` statements
// whose module is referenced but never imported (stdlib modules a small model
// often omits).
func missingPyImports(content string) string {
	imported := map[string]bool{}
	re := regexp.MustCompile(`(?m)^\s*(?:from\s+([a-zA-Z0-9_.]+)\s+import|import\s+([a-zA-Z0-9_.]+))`)
	for _, m := range re.FindAllStringSubmatch(content, -1) {
		if m[2] != "" {
			// `import os` / `import os, sys` binds the module names themselves.
			for _, part := range strings.Split(m[2], ",") {
				imported[strings.TrimSpace(strings.Split(part, " ")[0])] = true
			}
		}
		// `from os import getenv` binds only getenv, NOT os — so os.getenv(...)
		// is still a NameError. Deliberately not added to imported.
	}
	// strip strings + # comments
	clean := stripPyNoise(content)
	var missing []string
	for _, mod := range []string{"os", "sys", "math", "json", "random", "datetime", "re", "time", "collections", "itertools", "subprocess", "pathlib", "typing", "argparse", "shutil", "csv", "hashlib"} {
		if imported[mod] {
			continue
		}
		pat := regexp.MustCompile(`\b` + regexp.QuoteMeta(mod) + `\.[A-Za-z_]`)
		if pat.MatchString(clean) {
			missing = append(missing, mod)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	sort.Strings(missing)
	return fmt.Sprintf("NOTE: %s is/are referenced but not imported — add the import to prevent a NameError", strings.Join(missing, ", "))
}

// importNote returns the missing-import remediation note for a written file
// (prepended with a newline), or "" when nothing is missing. Non-blocking: the
// write happened; this just saves the model a failed diagnostics round-trip.
func importNote(path, content string) string {
	if msg := preflightImports(path, content); msg != "" {
		return "\n" + msg
	}
	return ""
}

// stripPyNoise removes Python # comments and string literals (single/double
// quotes) so the import scanner ignores text inside strings.
func stripPyNoise(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && (i == 0 || s[i-1] != '\\') {
			for i < len(s) && s[i] != '\n' {
				b.WriteByte(' ')
				i++
			}
			continue
		}
		if s[i] == '"' || s[i] == '\'' {
			q := s[i]
			b.WriteByte(' ')
			i++
			for i < len(s) && s[i] != q {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// preflightStructured validates YAML/JSON content before it hits disk. The
// agent regularly writes config/playbook/skill-frontmatter/export files; a
// malformed one breaks the NEXT reload with a cryptic parse error. Returns ""
// for non-structured files and valid content.
func preflightStructured(path, content string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(content), &node); err != nil {
			line := 1
			if yerr, ok := err.(*yaml.TypeError); ok && len(yerr.Errors) > 0 {
				return fmt.Sprintf("the change would introduce a YAML problem: %s — fix the edit; the file was NOT modified", yerr.Errors[0])
			}
			return fmt.Sprintf("the change would introduce a YAML parse error at line %d: %v — fix the edit; the file was NOT modified", line, err)
		}
	case ".json":
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(content), &raw); err != nil {
			return fmt.Sprintf("the change would introduce a JSON error: %v — fix the edit; the file was NOT modified", err)
		}
	}
	return ""
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
