package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/undo"
)

// fs_patch applies a unified git diff to the workspace. Small models emit
// diff blocks naturally, and it is multi-file in one call. Changes are
// recorded in the undo buffer so /undo reverts them.
type fsPatchTool struct {
	ws   string
	undo *undo.Buffer
}

type fsPatchArgs struct {
	Patch string `json:"patch"`
}

var fsPatchSchema = fnSchema("fs_patch", "apply a unified git diff (patch) to the workspace in one call — multi-file edits at once. The patch must be a standard unified diff (--- a/path, +++ b/path, @@ -l,c +l,c @@). Prefer this over multiple fs_edit calls. Changes can be undone with /undo.",
	map[string]any{
		"patch": strProp("the unified diff text to apply"),
	},
	[]string{"patch"})

func (t *fsPatchTool) Schema() llm.ToolSchema { return fsPatchSchema }
func (t *fsPatchTool) Risk() RiskLevel        { return RiskWrite }

func (t *fsPatchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a fsPatchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Patch) == "" {
		return "", validationErrorf(`argument "patch" is required`)
	}
	files, err := parseUnifiedDiff(a.Patch)
	if err != nil {
		return "", validationErrorf("invalid diff: %v", err)
	}
	if len(files) == 0 {
		return "", validationErrorf("the patch contains no file hunks")
	}
	var changed []string
	for _, f := range files {
		var data []byte
		full, err := resolvePath(t.ws, f.path)
		if err != nil {
			return "", err
		}
		data, err = os.ReadFile(full)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Sprintf("error: %v", err), nil
		}
		lines := []string{}
		if len(data) > 0 {
			lines = strings.Split(string(data), "\n")
		}
		patched, err := applyHunks(lines, f.hunks)
		if err != nil {
			return fmt.Sprintf("error: %s: %v", f.path, err), nil
		}
		out := strings.Join(patched, "\n")
		if out == string(data) {
			continue // no effective change
		}
		if t.undo != nil {
			t.undo.Record(full, data)
		}
		if msg := preflightSyntax(f.path, out); msg != "" {
			return fmt.Sprintf("error: %s: %s", f.path, msg), nil
		}
		if msg := preflightSymbols(f.path, string(data), out); msg != "" {
			return fmt.Sprintf("error: %s: %s", f.path, msg), nil
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
		if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
		changed = append(changed, f.path)
	}
	if len(changed) == 0 {
		return "no changes applied (the patch matched nothing)", nil
	}
	return fmt.Sprintf("patched %d file(s): %s", len(changed), strings.Join(changed, ", ")), nil
}

// diffFile is one file's hunks from a patch.
type diffFile struct {
	path  string
	hunks []diffHunk
}

type diffHunk struct {
	oldStart int // 1-based line in the original
	newStart int // 1-based line in the result
	lines    []diffLine
}

type diffLine struct {
	kind byte // ' ', '-', '+'
	text string
}

// parseUnifiedDiff parses a standard unified diff into per-file hunks.
func parseUnifiedDiff(patch string) ([]diffFile, error) {
	var files []diffFile
	var cur *diffFile
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			files = append(files, diffFile{path: pathFromDiffGit(l)})
			cur = &files[len(files)-1]
		case strings.HasPrefix(l, "--- "):
			path := diffPath(strings.TrimPrefix(l, "--- "))
			// A new file block begins with ---; a bare diff --git header
			// leaves cur pathless until this line arrives.
			if cur == nil || len(cur.hunks) > 0 || cur.path != "" {
				files = append(files, diffFile{path: path})
				cur = &files[len(files)-1]
			} else {
				cur.path = path
			}
		case strings.HasPrefix(l, "+++ "):
			// The +++ line names the real path for new files (/dev/null).
			if cur != nil && (cur.path == "" || cur.path == "/dev/null") {
				cur.path = diffPath(strings.TrimPrefix(l, "+++ "))
			}
		case strings.HasPrefix(l, "@@ "):
			oldStart, newStart, err := parseHunkStart(l)
			if err != nil {
				return nil, err
			}
			if cur == nil {
				return nil, fmt.Errorf("hunk appears before any file header")
			}
			cur.hunks = append(cur.hunks, diffHunk{oldStart: oldStart, newStart: newStart})
		case cur != nil && len(cur.hunks) > 0:
			h := &cur.hunks[len(cur.hunks)-1]
			switch {
			case strings.HasPrefix(l, " "):
				h.lines = append(h.lines, diffLine{' ', l[1:]})
			case strings.HasPrefix(l, "-"):
				h.lines = append(h.lines, diffLine{'-', l[1:]})
			case strings.HasPrefix(l, "+"):
				h.lines = append(h.lines, diffLine{'+', l[1:]})
			case l == `\ No newline at end of file`:
				// ignore
			}
		}
	}
	return files, nil
}

func pathFromDiffGit(l string) string {
	parts := strings.Fields(l)
	for _, p := range parts {
		if strings.HasPrefix(p, "b/") {
			return strings.TrimPrefix(p, "b/")
		}
	}
	return ""
}

// diffPath strips the a/ or b/ prefix git uses in patch headers.
func diffPath(p string) string {
	if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
		return p[2:]
	}
	return p
}

func parseHunkStart(l string) (oldStart, newStart int, err error) {
	// @@ -12,5 +12,5 @@
	fields := strings.Fields(l)
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("malformed hunk header %q", l)
	}
	oldStart, err = parseStartField(fields[1])
	if err != nil {
		return 0, 0, err
	}
	newStart, err = parseStartField(fields[2])
	if err != nil {
		return 0, 0, err
	}
	return oldStart, newStart, nil
}

// parseStartField parses "-12,5" / "+12,5" into the start line.
func parseStartField(f string) (int, error) {
	f = strings.TrimLeft(f, "-+")
	if i := strings.IndexByte(f, ','); i >= 0 {
		f = f[:i]
	}
	n, err := strconv.Atoi(f)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("malformed hunk start %q", f)
	}
	return n, nil
}

// applyHunks applies hunks to fileLines (bottom-up so positions stay valid),
// verifying context lines match.
func applyHunks(fileLines []string, hunks []diffHunk) ([]string, error) {
	for hi := len(hunks) - 1; hi >= 0; hi-- {
		h := hunks[hi]
		var expected, added []string
		for _, dl := range h.lines {
			switch dl.kind {
			case ' ', '-':
				expected = append(expected, dl.text)
			case '+':
				added = append(added, dl.text)
			}
		}
		// verify the context/removed lines match at the stated position
		for k, exp := range expected {
			idx := h.oldStart - 1 + k
			if idx >= len(fileLines) || fileLines[idx] != exp {
				return nil, fmt.Errorf("hunk at line %d does not match the file content", h.oldStart)
			}
		}
		before := fileLines[:h.oldStart-1]
		after := fileLines[h.oldStart-1+len(expected):]
		out := make([]string, 0, len(before)+len(added)+len(after))
		out = append(out, before...)
		out = append(out, added...)
		out = append(out, after...)
		fileLines = out
	}
	return fileLines, nil
}

// PatchHunk is one reviewable hunk (used by the TUI's per-hunk approval).
type PatchHunk struct {
	File  string
	Index int // index within its file
	Start int // original line of the hunk header
	Lines []string
}

// PatchHunks splits a unified diff into reviewable hunks in file order.
func PatchHunks(patch string) ([]PatchHunk, error) {
	files, err := parseUnifiedDiff(patch)
	if err != nil {
		return nil, err
	}
	var out []PatchHunk
	for _, f := range files {
		for i, h := range f.hunks {
			lines := make([]string, 0, len(h.lines)+1)
			lines = append(lines, fmt.Sprintf("@@ -%d", h.oldStart))
			for _, dl := range h.lines {
				lines = append(lines, string(dl.kind)+dl.text)
			}
			out = append(out, PatchHunk{File: f.path, Index: i, Start: h.oldStart, Lines: lines})
		}
	}
	return out, nil
}

// RebuildPatch returns a unified diff containing only the hunks whose global
// index is in keep. Hunk headers are recomputed so the filtered patch still
// applies cleanly. Indexes follow PatchHunks order (0-based).
func RebuildPatch(patch string, keep []bool) (string, error) {
	files, err := parseUnifiedDiff(patch)
	if err != nil {
		return "", err
	}
	var keptFiles []diffFile
	gi := 0
	for _, f := range files {
		nf := diffFile{path: f.path}
		for _, h := range f.hunks {
			// Hunk headers are always relative to the original file, so a
			// kept hunk keeps its oldStart/newStart unchanged.
			if gi < len(keep) && keep[gi] {
				nf.hunks = append(nf.hunks, h)
			}
			gi++
		}
		if len(nf.hunks) > 0 {
			keptFiles = append(keptFiles, nf)
		}
	}
	var b strings.Builder
	for _, f := range keptFiles {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", f.path, f.path)
		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", f.path, f.path)
		for _, h := range f.hunks {
			var oldCount, newCount int
			for _, dl := range h.lines {
				switch dl.kind {
				case '-', ' ':
					oldCount++
				case '+':
					newCount++
				}
			}
			fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.oldStart, oldCount, h.newStart, newCount)
			for _, dl := range h.lines {
				b.WriteByte(dl.kind)
				b.WriteString(dl.text)
				b.WriteByte('\n')
			}
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
