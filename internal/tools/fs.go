package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"yagent/internal/llm"
)

var (
	fsReadSchema = fnSchema("fs_read", "read a text file with line numbers; use offset/limit to page; paths are relative to the workspace",
		map[string]any{"path": strProp("file path relative to workspace"), "offset": intProp("0-based line to start from (optional)"), "limit": intProp("max lines to return (optional)")},
		[]string{"path"})
	fsWriteSchema = fnSchema("fs_write", "create or overwrite a file; creates parent directories; shows a diff when overwriting",
		map[string]any{"path": strProp("file path relative to workspace"), "content": strProp("full new file content")},
		[]string{"path", "content"})
	fsEditSchema = fnSchema("fs_edit", "replace an exact string in a file; old_string must match exactly once; prefer re-reading the file first",
		map[string]any{"path": strProp("file path relative to workspace"), "old_string": strProp("exact text to replace, copied from the file"), "new_string": strProp("replacement text")},
		[]string{"path", "old_string", "new_string"})
	globSchema = fnSchema("glob", "list files matching a glob pattern (supports **); e.g. **/*.go",
		map[string]any{"pattern": strProp("glob pattern"), "path": strProp("directory to search, default workspace root (optional)")},
		[]string{"pattern"})
	grepSchema = fnSchema("grep", "search files for a regular expression; returns file:line: text matches",
		map[string]any{"pattern": strProp("regular expression"), "path": strProp("directory to search, default workspace root (optional)"), "include": strProp("only search files whose name matches this glob (optional)")},
		[]string{"pattern"})
)

// ---------- fs_read ----------

type fsReadTool struct{ ws string }

type fsReadArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

const fsReadMaxLines = 2000

func (t *fsReadTool) Schema() llm.ToolSchema { return fsReadSchema }
func (t *fsReadTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *fsReadTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a fsReadArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	path, err := resolvePath(t.ws, a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if isBinary(data) {
		return fmt.Sprintf("error: %s is a binary file; use grep or shell tools instead", a.Path), nil
	}
	lines := strings.Split(string(data), "\n")
	if a.Offset > 0 {
		if a.Offset > len(lines) {
			return fmt.Sprintf("error: offset %d beyond file (%d lines)", a.Offset, len(lines)), nil
		}
		lines = lines[a.Offset:]
	}
	if a.Limit > 0 && a.Limit < len(lines) {
		lines = lines[:a.Limit]
	}
	if len(lines) > fsReadMaxLines {
		lines = lines[:fsReadMaxLines]
	}
	var b strings.Builder
	start := 1
	if a.Offset > 0 {
		start = a.Offset + 1
	}
	for i, line := range lines {
		fmt.Fprintf(&b, "%6d: %s\n", start+i, line)
	}
	return capResult(b.String(), maxResultBytes), nil
}

func isBinary(data []byte) bool {
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	return strings.IndexByte(string(probe), 0) >= 0
}

// ---------- fs_write ----------

type fsWriteTool struct{ ws string }

type fsWriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *fsWriteTool) Schema() llm.ToolSchema { return fsWriteSchema }
func (t *fsWriteTool) Risk() RiskLevel        { return RiskWrite }

func (t *fsWriteTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a fsWriteArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	path, err := resolvePath(t.ws, a.Path)
	if err != nil {
		return "", err
	}
	old, oldErr := os.ReadFile(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Sprintf("error: create parent dirs: %v", err), nil
	}
	if err := os.WriteFile(path, []byte(a.Content), 0o644); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if oldErr == nil {
		return fmt.Sprintf("wrote %s (%d bytes; overwrote %d bytes)\n%s", a.Path, len(a.Content), len(old),
			simpleDiff(string(old), a.Content, 100)), nil
	}
	return fmt.Sprintf("wrote %s (%d bytes)", a.Path, len(a.Content)), nil
}

// ---------- fs_edit ----------

type fsEditTool struct{ ws string }

type fsEditArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func (t *fsEditTool) Schema() llm.ToolSchema { return fsEditSchema }
func (t *fsEditTool) Risk() RiskLevel        { return RiskWrite }

func (t *fsEditTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a fsEditArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.OldString == "" {
		return "", validationErrorf(`argument "old_string" is required and must not be empty`)
	}
	path, err := resolvePath(t.ws, a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("error: %v; re-read the file first with fs_read", err), nil
	}
	old := string(data)
	n := strings.Count(old, a.OldString)
	switch n {
	case 0:
		return fmt.Sprintf("error: old_string not found in %s; re-read the file and copy the exact text", a.Path), nil
	case 1:
		// proceed
	default:
		return fmt.Sprintf("error: old_string matches %d times in %s; include more surrounding context", n, a.Path), nil
	}
	newContent := strings.Replace(old, a.OldString, a.NewString, 1)
	if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return fmt.Sprintf("edited %s:\n%s", a.Path, simpleDiff(old, newContent, 100)), nil
}

// simpleDiff emits a crude -/+ line diff, capped at maxLines.
func simpleDiff(oldS, newS string, maxLines int) string {
	oldLines := strings.Split(oldS, "\n")
	newLines := strings.Split(newS, "\n")
	var b strings.Builder
	max := len(oldLines)
	if len(newLines) > max {
		max = len(newLines)
	}
	emitted := 0
	for i := 0; i < max; i++ {
		if emitted >= maxLines {
			fmt.Fprintf(&b, "... diff truncated (%d lines omitted)\n", max-i)
			break
		}
		var o, n string
		changed := false
		if i < len(oldLines) {
			o = oldLines[i]
			if i >= len(newLines) || o != newLines[i] {
				changed = true
			}
		} else {
			changed = true
		}
		if i < len(newLines) {
			n = newLines[i]
		}
		if changed {
			if i < len(oldLines) {
				fmt.Fprintf(&b, "-%s\n", o)
			}
			if i < len(newLines) {
				fmt.Fprintf(&b, "+%s\n", n)
			}
			emitted++
		}
	}
	if b.Len() == 0 {
		return "(no changes)"
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------- glob ----------

type globTool struct{ ws string }

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

const globMaxResults = 200

func (t *globTool) Schema() llm.ToolSchema { return globSchema }
func (t *globTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *globTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a globArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Pattern == "" {
		return "", validationErrorf(`argument "pattern" is required`)
	}
	base := t.ws
	if a.Path != "" {
		var err error
		base, err = resolvePath(t.ws, a.Path)
		if err != nil {
			return "", err
		}
	}
	re, err := regexp.Compile(globToRegex(a.Pattern))
	if err != nil {
		return "", validationErrorf("invalid glob pattern %q: %v", a.Pattern, err)
	}
	matches := []string{}
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(t.ws, path)
		if err != nil {
			return nil
		}
		if re.MatchString(filepath.ToSlash(rel)) {
			matches = append(matches, rel)
		}
		return nil
	})
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(matches) > globMaxResults {
		matches = matches[:globMaxResults]
	}
	if len(matches) == 0 {
		return "no files match", nil
	}
	return capResult(strings.Join(matches, "\n"), maxResultBytes), nil
}

// globToRegex converts a glob with ** support into an anchored regex.
// "**/" matches zero or more directory levels, so "**/README*" matches
// "README.md" at the workspace root as well as nested copies.
func globToRegex(pattern string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2 // consume "**" and the following "/"
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return b.String()
}

// ---------- grep ----------

type grepTool struct{ ws string }

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Include string `json:"include,omitempty"`
}

const grepMaxMatches = 100

func (t *grepTool) Schema() llm.ToolSchema { return grepSchema }
func (t *grepTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *grepTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a grepArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Pattern == "" {
		return "", validationErrorf(`argument "pattern" is required`)
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "", validationErrorf("invalid regex %q: %v", a.Pattern, err)
	}
	base := t.ws
	if a.Path != "" {
		base, err = resolvePath(t.ws, a.Path)
		if err != nil {
			return "", err
		}
	}
	var includeRe *regexp.Regexp
	if a.Include != "" {
		includeRe, err = regexp.Compile(globToRegex(a.Include))
		if err != nil {
			return "", validationErrorf("invalid include glob %q: %v", a.Include, err)
		}
	}
	var b strings.Builder
	count := 0
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || count >= grepMaxMatches {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if includeRe != nil {
			// Match against the basename ("*.go") or the relative path
			// ("**/README*"); a root-level file must match "**/..." too.
			rel, _ := filepath.Rel(t.ws, path)
			if !includeRe.MatchString(d.Name()) && !includeRe.MatchString(filepath.ToSlash(rel)) {
				return nil
			}
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		rel, _ := filepath.Rel(t.ws, path)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		lineNo := 0
		for sc.Scan() && count < grepMaxMatches {
			lineNo++
			line := sc.Text()
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d: %s\n", filepath.ToSlash(rel), lineNo, strings.TrimSpace(line))
				count++
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if count == 0 {
		return "no matches", nil
	}
	return capResult(b.String(), maxResultBytes), nil
}
