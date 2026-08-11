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
			start, err := parseHunkStart(l)
			if err != nil {
				return nil, err
			}
			if cur == nil {
				return nil, fmt.Errorf("hunk appears before any file header")
			}
			cur.hunks = append(cur.hunks, diffHunk{oldStart: start})
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

func parseHunkStart(l string) (int, error) {
	// @@ -12,5 +12,5 @@
	fields := strings.Fields(l)
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed hunk header %q", l)
	}
	old := strings.TrimPrefix(fields[1], "-")
	if i := strings.IndexByte(old, ','); i >= 0 {
		old = old[:i]
	}
	n, err := strconv.Atoi(old)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("malformed hunk start %q", l)
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
