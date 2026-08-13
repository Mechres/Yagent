package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/undo"
)

// ---------- fs_refactor ----------

// refactorTool renames a symbol across the workspace with word-boundary
// matching. It walks source text files (skipping .git/.yagent and common build
// dirs), finds every occurrence of old_name, and rewrites it to new_name. Old
// content is recorded in the undo buffer so /undo reverts the whole rename.
type refactorTool struct {
	ws   string
	undo *undo.Buffer
}

type refactorArgs struct {
	OldName string `json:"old_name"`
	NewName string `json:"new_name"`
}

var refactorSchema = fnSchema("fs_refactor", "rename a symbol (function, type, variable) across the whole workspace with word-boundary matching, rewriting every occurrence in source files (including comments/strings). Records the changes for /undo and requires approval. Prefer this over many manual fs_edit calls for a project-wide rename.",
	map[string]any{
		"old_name": strProp("the exact current symbol name to rename"),
		"new_name": strProp("the new symbol name"),
	},
	[]string{"old_name", "new_name"})

func (t *refactorTool) Schema() llm.ToolSchema { return refactorSchema }
func (t *refactorTool) Risk() RiskLevel        { return RiskWrite }

// skipRefactorDir lists directory names never searched by fs_refactor (build
// artifacts, vendored deps, the agent's own state).
var skipRefactorDir = map[string]bool{
	".git": true, ".yagent": true, "node_modules": true, "vendor": true,
	"target": true, "dist": true, "build": true, ".cache": true, ".venv": true,
}

func (t *refactorTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a refactorArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.OldName == "" || a.NewName == "" {
		return "", validationErrorf(`both "old_name" and "new_name" are required`)
	}
	if a.OldName == a.NewName {
		return "", validationErrorf("old_name and new_name are the same")
	}
	if !identRe.MatchString(a.OldName) || !identRe.MatchString(a.NewName) {
		return "", validationErrorf("names must be identifiers ([A-Za-z_][A-Za-z0-9_]*)")
	}

	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(a.OldName) + `\b`)
	if err != nil {
		return "", validationErrorf("bad rename pattern: %v", err)
	}

	type change struct {
		path string
		old  []byte
		new  []byte
		n    int
	}
	var changes []change
	total := 0
	err = filepath.WalkDir(t.ws, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipRefactorDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if isBinaryPath(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || !re.Match(data) {
			return nil
		}
		n := len(re.FindAll(data, -1))
		if n == 0 {
			return nil
		}
		changes = append(changes, change{path: path, old: data, new: re.ReplaceAll(data, []byte(a.NewName)), n: n})
		total += n
		return nil
	})
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if total == 0 {
		return fmt.Sprintf("symbol %q not found in any workspace source file", a.OldName), nil
	}

	// Pre-flight: every rewritten file must still parse (tree-sitter syntax
	// check). A rename touches many files at once, so a broken rewrite is
	// strictly more dangerous than a single edit — validate ALL of them BEFORE
	// writing any (all-or-nothing). NOTE: the exported-symbol delta guardrail
	// (preflightSymbols) is intentionally NOT applied here — a rename
	// Foo->Bar removes Foo by design, so it would block every public rename.
	for _, c := range changes {
		if msg := preflightSyntax(c.path, string(c.new)); msg != "" {
			return fmt.Sprintf("error: %s: %s", c.path, msg), nil
		}
		if msg := preflightStructured(c.path, string(c.new)); msg != "" {
			return fmt.Sprintf("error: %s: %s", c.path, msg), nil
		}
	}

	// Apply every rewrite, recording the originals for /undo.
	for _, c := range changes {
		if t.undo != nil {
			t.undo.Record(c.path, c.old)
		}
		if err := os.WriteFile(c.path, c.new, 0o644); err != nil {
			return fmt.Sprintf("error: write %s: %v", c.path, err), nil
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "renamed %s -> %s in %d file(s), %d occurrence(s):\n", a.OldName, a.NewName, len(changes), total)
	for _, c := range changes {
		rel, _ := filepath.Rel(t.ws, c.path)
		fmt.Fprintf(&b, "- %s (%d×)\n", filepath.ToSlash(rel), c.n)
	}
	return capResult(b.String(), maxResultBytes), nil
}

// identRe matches Go-style identifiers.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isBinaryPath reports whether the file looks binary (checks the first chunk).
func isBinaryPath(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	buf := make([]byte, 8000)
	n, _ := f.Read(buf)
	return isBinary(buf[:n])
}
