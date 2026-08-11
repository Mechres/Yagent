package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
)

// scratchRoot is the workspace-relative directory for subagent scratch notes.
const scratchRoot = ".yagent/scratch"

// scratchWriteTool stores a note under <ws>/.yagent/scratch/ for sibling
// subagents to read. It is the ONLY write tool available to subagents: it is
// strictly confined to the scratch dir, so the read-only guarantee still holds
// for everything else.
type scratchWriteTool struct{ ws string }

type scratchArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

var scratchWriteSchema = fnSchema("scratch_write", "save a structured note (interface/API sketch, findings, contract) into the shared subagent scratchpad at .yagent/scratch/<path> so sibling subagents working in parallel can read it. Confined to the scratch dir; use .json for structured notes. Use scratch_read to retrieve.",
	map[string]any{
		"path":    strProp("scratch-relative path, e.g. 'task-1/api.json'"),
		"content": strProp("the note content (JSON or text)"),
	},
	[]string{"path", "content"})

func (t *scratchWriteTool) Schema() llm.ToolSchema { return scratchWriteSchema }
func (t *scratchWriteTool) Risk() RiskLevel        { return RiskWrite }

func (t *scratchWriteTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a scratchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	full, err := t.scratchPath(a.Path)
	if err != nil {
		return "", validationErrorf("path %q escapes the scratch dir", a.Path)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return fmt.Sprintf("saved scratch note %s", a.Path), nil
}

// scratchReadTool reads a sibling subagent's note from the scratchpad.
type scratchReadTool struct{ ws string }

var scratchReadSchema = fnSchema("scratch_read", "read a note from the shared subagent scratchpad (.yagent/scratch/<path>) that a sibling subagent wrote with scratch_write — e.g. an API contract you must implement against.",
	map[string]any{
		"path": strProp("scratch-relative path, e.g. 'task-1/api.json'"),
	},
	[]string{"path"})

func (t *scratchReadTool) Schema() llm.ToolSchema { return scratchReadSchema }
func (t *scratchReadTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *scratchReadTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a scratchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	full, err := t.scratchPath(a.Path)
	if err != nil {
		return "", validationErrorf("path %q escapes the scratch dir", a.Path)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return fmt.Sprintf("no scratch note at %s (has a sibling written it yet?)", a.Path), nil
	}
	return capResult(string(data), maxResultBytes), nil
}

// scratchPath resolves a scratch-relative path under <ws>/.yagent/scratch/,
// rejecting escapes.
func (t *scratchWriteTool) scratchPath(p string) (string, error) { return scratchPath(t.ws, p) }
func (t *scratchReadTool) scratchPath(p string) (string, error)  { return scratchPath(t.ws, p) }

func scratchPath(ws, p string) (string, error) {
	if p == "" || filepath.IsAbs(p) || strings.Contains(p, "..") {
		return "", fmt.Errorf("bad scratch path %q", p)
	}
	root := filepath.Join(ws, scratchRoot)
	abs := filepath.Clean(filepath.Join(root, p))
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the scratch dir", p)
	}
	return abs, nil
}
