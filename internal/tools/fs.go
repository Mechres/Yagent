package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/undo"
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

type fsReadTool struct {
	ws string
	// cache dedups repeated full reads of an unchanged file: the hash of the
	// last read is kept so a second read of the same content returns a marker
	// instead of re-injecting the whole file into context (Gemini review #6).
	mu    sync.Mutex
	cache map[string]string
}

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
	resolved := ""
	data, err := os.ReadFile(path)
	if err != nil && os.IsNotExist(err) {
		// P6 — small models often drop the extension (fs_read {path:"README"});
		// correct it when exactly one file matches, saving a wasted turn.
		if fixed, ok := fuzzyResolve(t.ws, a.Path); ok {
			if data, err = os.ReadFile(fixed); err == nil {
				path = fixed
				resolved = a.Path
			}
		}
	}
	if err != nil {
		return errorClass("missing_path", true, []string{"glob"}, fmt.Sprintf("%v — the file does not exist", err)), nil
	}
	if isBinary(data) {
		return fmt.Sprintf("error: %s is a binary file; use grep or shell tools instead", a.Path), nil
	}
	// Dedup: a repeated full read of an unchanged file returns a marker instead
	// of the whole content (small models re-read files they already have).
	if a.Offset == 0 && a.Limit == 0 {
		if marker, ok := t.dedupMarker(path, data); ok {
			return marker, nil
		}
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
	if resolved != "" {
		fmt.Fprintf(&b, "note: path %q was resolved to %s\n", resolved, filepath.Base(path))
	}
	start := 1
	if a.Offset > 0 {
		start = a.Offset + 1
	}
	for i, line := range lines {
		fmt.Fprintf(&b, "%6d: %s\n", start+i, line)
	}
	return capResult(b.String(), maxResultBytes), nil
}

// dedupMarker returns a cached marker when the file's content matches the last
// full read this session (Gemini review #6 — token saver). It records the hash
// on first read; a repeat full read of unchanged content yields the marker.
// The marker never claims the earlier content is still in history (pruning may
// have removed it, and telling a small model to "reuse it" invites
// hallucination) — it states the file is unchanged and offers a line-range
// re-read, which is the safe way to fetch the content again.
func (t *fsReadTool) dedupMarker(path string, data []byte) (string, bool) {
	h := fmt.Sprintf("%x", sha256.Sum256(data))
	t.mu.Lock()
	if t.cache == nil {
		t.cache = map[string]string{}
	}
	prev, seen := t.cache[path]
	t.cache[path] = h
	t.mu.Unlock()
	if !seen || prev != h {
		return "", false
	}
	return fmt.Sprintf("[cached] %s is unchanged since the earlier read this session (%d bytes). If you need its contents again, read it with fs_read {path, offset, limit}; otherwise treat the file as already known.", filepath.Base(path), len(data)), true
}

// fuzzyResolve corrects a small-model path slip like "README" when the real
// file is README.md: it scans the workspace for files whose basename starts
// with the given basename and corrects ONLY when exactly one file matches and
// the original had no extension. Returns the resolved path, or "", false.
func fuzzyResolve(ws, p string) (string, bool) {
	base := filepath.Base(p)
	if base == "" || base == "." || strings.Contains(base, ".") {
		return "", false // never guess when an extension is already present
	}
	var matches []string
	_ = filepath.WalkDir(ws, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipRefactorDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, base) && name != base && strings.Contains(name, ".") {
			matches = append(matches, path)
		}
		return nil
	})
	if len(matches) == 1 {
		return matches[0], true
	}
	return "", false
}

func isBinary(data []byte) bool {
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	return strings.IndexByte(string(probe), 0) >= 0
}

// ---------- fs_write ----------

type fsWriteTool struct {
	ws        string
	undo      *undo.Buffer
	skillsDir string // when set, writes under it are self-gated (SKILL.md)
}

type fsWriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *fsWriteTool) Schema() llm.ToolSchema { return fsWriteSchema }
func (t *fsWriteTool) Risk() RiskLevel        { return RiskWrite }

// SelfGatedFor reports whether this write targets the skills store — a
// SKILL.md written by the model is governed by the skills gate (apply vs
// stage), not the generic y/n prompt, mirroring skill_manage.
func (t *fsWriteTool) SelfGatedFor(args json.RawMessage) bool {
	return pathInSkillsDir(t.ws, t.skillsDir, args)
}

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
	if msg := preflightSyntax(a.Path, a.Content); msg != "" {
		return "error: " + msg, nil
	}
	if msg := preflightStructured(a.Path, a.Content); msg != "" {
		return "error: " + msg, nil
	}
	// Record the undo entry only after preflight passes: a rejected write
	// never touched disk, so it must not leave a phantom undo entry (finding
	// #5, 2026-08-13 — /undo would "revert" a write that never happened and
	// could re-write a stale version on the next undo).
	if t.undo != nil {
		// nil Old records "did not exist" so undo can delete the file.
		if oldErr == nil {
			t.undo.Record(path, old)
		} else {
			t.undo.Record(path, nil)
		}
	}
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

type fsEditTool struct {
	ws        string
	undo      *undo.Buffer
	skillsDir string
}

type fsEditArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func (t *fsEditTool) Schema() llm.ToolSchema { return fsEditSchema }
func (t *fsEditTool) Risk() RiskLevel        { return RiskWrite }

// SelfGatedFor reports whether this edit targets the skills store.
func (t *fsEditTool) SelfGatedFor(args json.RawMessage) bool {
	return pathInSkillsDir(t.ws, t.skillsDir, args)
}

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
	resolved := ""
	data, err := os.ReadFile(path)
	if err != nil && os.IsNotExist(err) {
		// P6 — small models often drop the extension; correct it when exactly
		// one file matches.
		if fixed, ok := fuzzyResolve(t.ws, a.Path); ok {
			if data, err = os.ReadFile(fixed); err == nil {
				path = fixed
				resolved = a.Path
			}
		}
	}
	if err != nil {
		return fmt.Sprintf("error: %v; re-read the file first with fs_read", err), nil
	}
	old := string(data)
	n := strings.Count(old, a.OldString)
	switch n {
	case 0:
		// P6 — whitespace soft-normalization: small models emit tabs where the
		// file has spaces (or vice versa). When the whitespace-normalized
		// old_string lands at exactly one span, auto-align to the on-disk text
		// and apply instead of failing.
		if start, end, ok := whitespaceNormalizedMatch(old, a.OldString); ok {
			aligned := old[start:end]
			// Re-indent new_string lines with the on-disk span's leading
			// whitespace pattern (file tabs stay tabs even though the model
			// wrote spaces).
			indents := leadingWS(aligned)
			reindented := applyIndents(a.NewString, indents)
			newContent := old[:start] + reindented + old[end:]
			if msg := preflightSyntax(a.Path, newContent); msg != "" {
				return "error: " + msg, nil
			}
			if msg := preflightStructured(a.Path, newContent); msg != "" {
				return "error: " + msg, nil
			}
			if msg := preflightSymbols(a.Path, old, newContent); msg != "" {
				return "error: " + msg, nil
			}
			if t.undo != nil {
				t.undo.Record(path, data)
			}
			if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
				return fmt.Sprintf("error: %v", err), nil
			}
			return fmt.Sprintf("[auto-aligned whitespace indentation]\nedited %s:\n%s", a.Path, simpleDiff(aligned, reindented, 100)), nil
		}
		return errorClass("old_string_not_found", true, []string{"fs_read"}, fmt.Sprintf("old_string not found in %s; re-read the file and copy the exact text%s", a.Path, nearestLineHint(old, a.OldString))), nil
	case 1:
		// proceed
	default:
		return errorClass("ambiguous_match", true, []string{"fs_read"}, fmt.Sprintf("old_string matches %d times in %s%s", n, a.Path, matchLines(old, a.OldString))), nil
	}
	newContent := strings.Replace(old, a.OldString, a.NewString, 1)
	if msg := preflightSyntax(a.Path, newContent); msg != "" {
		return "error: " + msg, nil
	}
	if msg := preflightStructured(a.Path, newContent); msg != "" {
		return "error: " + msg, nil
	}
	if msg := preflightSymbols(a.Path, old, newContent); msg != "" {
		return "error: " + msg, nil
	}
	if t.undo != nil {
		t.undo.Record(path, data)
	}
	if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if resolved != "" {
		return fmt.Sprintf("note: path %q was resolved to %s\nedited %s:\n%s", resolved, filepath.Base(path), a.Path, simpleDiff(old, newContent, 100)), nil
	}
	return fmt.Sprintf("edited %s:\n%s", a.Path, simpleDiff(old, newContent, 100)), nil
}

// pathInSkillsDir reports whether the write/edit args target a path inside the
// skills store (the project dir .yagent/skills or the configured skillsDir). A
// model writing a SKILL.md there is governed by the skills gate, so the
// generic y/n approval is skipped. skillsDirs is a "|"-joined list of roots.
func pathInSkillsDir(ws, skillsDirs string, raw json.RawMessage) bool {
	if skillsDirs == "" {
		return false
	}
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return false
	}
	full, err := resolvePath(ws, a.Path)
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(full)
	if err != nil {
		return false
	}
	for _, dir := range strings.Split(skillsDirs, "|") {
		if dir == "" {
			continue
		}
		absSkill, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absSkill, absTarget)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// matchLines returns " at lines N, M, K" for every occurrence of target in
// content (1-based), so an ambiguous_match error tells the model exactly where
// each match is instead of forcing a blind re-read (agy #4). Works for both
// single-line and multi-line targets.
func matchLines(content, target string) string {
	if target == "" || content == "" {
		return ""
	}
	var nums []int
	offset := 0
	for {
		idx := strings.Index(content[offset:], target)
		if idx < 0 {
			break
		}
		matchPos := offset + idx
		lineNum := 1 + strings.Count(content[:matchPos], "\n")
		nums = append(nums, lineNum)
		offset = matchPos + len(target)
	}
	if len(nums) == 0 {
		return ""
	}
	strs := make([]string, len(nums))
	for i, n := range nums {
		strs[i] = fmt.Sprintf("%d", n)
	}
	return " at lines " + strings.Join(strs, ", ")
}

// nearestLineHint finds the line in content most similar to the target string
// and returns a " — nearest match at line N: ..." hint, or "" when nothing is
// close enough. Deterministic remediation for small models that botch
// old_string (Gemini review #1): a concrete hint recovers in one turn instead
// of three loops.
func nearestLineHint(content, target string) string {
	bestLine := 0
	bestScore := 0.0
	for i, line := range strings.Split(content, "\n") {
		if s := lineSimilarity(line, target); s > bestScore {
			bestScore = s
			bestLine = i + 1
		}
	}
	if bestLine == 0 || bestScore < 0.5 {
		return ""
	}
	text := strings.TrimSpace(strings.SplitN(content, "\n", bestLine)[bestLine-1])
	if len(text) > 60 {
		text = text[:60] + "…"
	}
	return fmt.Sprintf(" — nearest match at line %d: %q", bestLine, text)
}

// lineSimilarity scores how close a line is to the target: 1.0 for a substring
// match, else 1 - normalized Levenshtein distance.
func lineSimilarity(line, target string) float64 {
	line = strings.TrimSpace(line)
	if line == "" {
		return 0
	}
	if strings.Contains(line, target) || strings.Contains(target, line) {
		return 1.0
	}
	longer := len(line)
	if len(target) > longer {
		longer = len(target)
	}
	if longer == 0 {
		return 0
	}
	return 1.0 - float64(levenshtein(line, target))/float64(longer)
}

// whitespaceNormalizedMatch finds the (unique) span in content whose lines
// match target after normalizing leading whitespace per line. Small models
// often emit tabs where the file has spaces (or vice versa); when the
// normalized old_string lands at exactly one place, fs_edit can auto-align
// instead of burning a retry turn. Returns (start, end) byte offsets and
// ok=false unless the match is unique.
func whitespaceNormalizedMatch(content, target string) (int, int, bool) {
	if target == "" || content == "" {
		return 0, 0, false
	}
	tgt := normalizeLeadingWS(target)
	if tgt == "" {
		return 0, 0, false
	}
	// Split content into lines keeping offsets.
	lines := strings.Split(content, "\n")
	offs := make([]int, len(lines))
	pos := 0
	for i, ln := range lines {
		offs[i] = pos
		pos += len(ln) + 1
	}
	tgtLines := strings.Split(tgt, "\n")
	// Normalize the content lines once.
	normLines := make([]string, len(lines))
	for i, ln := range lines {
		normLines[i] = normalizeLeadingWS(ln)
	}
	matches := []int{}
	for i := 0; i+len(tgtLines) <= len(normLines); i++ {
		ok := true
		for j := range tgtLines {
			if normLines[i+j] != tgtLines[j] {
				ok = false
				break
			}
		}
		if ok {
			matches = append(matches, i)
		}
	}
	if len(matches) != 1 {
		return 0, 0, false
	}
	start := matches[0]
	end := start + len(tgtLines) - 1
	begin := offs[start]
	finish := offs[end] + len(lines[end])
	if begin >= finish {
		return 0, 0, false
	}
	return begin, finish, true
}

// normalizeLeadingWS replaces each line's leading whitespace with a canonical
// single tab-equivalent (a "T" marker), so tab-vs-space indentation slips
// compare equal while the rest of the line stays exact.
func normalizeLeadingWS(s string) string {
	out := make([]string, 0, strings.Count(s, "\n")+1)
	for _, ln := range strings.Split(s, "\n") {
		trimmed := strings.TrimLeft(ln, " \t")
		out = append(out, "T"+trimmed)
	}
	return strings.Join(out, "\n")
}

// leadingWS returns each line's leading whitespace run (spaces/tabs).
func leadingWS(s string) []string {
	lines := strings.Split(s, "\n")
	out := make([]string, len(lines))
	for i, ln := range lines {
		j := 0
		for j < len(ln) && (ln[j] == ' ' || ln[j] == '\t') {
			j++
		}
		out[i] = ln[:j]
	}
	return out
}

// applyIndents re-indents each line of s using the corresponding entry in
// indents (cycling the last one for extra lines), stripping any leading
// whitespace the model emitted first. Preserves on-disk indentation style when
// auto-aligning a whitespace-mismatched edit.
func applyIndents(s string, indents []string) string {
	lines := strings.Split(s, "\n")
	if len(indents) == 0 {
		return s
	}
	for i, ln := range lines {
		trimmed := strings.TrimLeft(ln, " \t")
		idx := i
		if idx >= len(indents) {
			idx = len(indents) - 1
		}
		lines[i] = indents[idx] + trimmed
	}
	return strings.Join(lines, "\n")
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
