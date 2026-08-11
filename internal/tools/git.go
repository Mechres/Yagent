package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
)

// git executes a read-only git command in the workspace and returns its
// combined output; a non-zero exit (e.g. "not a git repository") is returned
// as result text so the model sees it. Errors are returned only for context
// cancellation.
func git(ctx context.Context, ws string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = ws
	cmd.Env = scrubEnv(os.Environ())
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Sprintf("error: git %s: %s", strings.Join(args, " "), msg), nil
	}
	return out.String(), nil
}

// ---------- git_status ----------

type gitStatusTool struct{ ws string }

var gitStatusSchema = fnSchema("git_status", "show working tree status in porcelain format; no arguments needed",
	nil, nil)

func (t *gitStatusTool) Schema() llm.ToolSchema { return gitStatusSchema }
func (t *gitStatusTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *gitStatusTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a map[string]any
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	out, err := git(ctx, t.ws, "-C", t.ws, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "working tree clean", nil
	}
	return capResult(out, maxResultBytes), nil
}

// ---------- git_diff ----------

type gitDiffTool struct{ ws string }

type gitDiffArgs struct {
	Path   string `json:"path,omitempty"`
	Staged bool   `json:"staged,omitempty"`
}

var gitDiffSchema = fnSchema("git_diff", "show uncommitted changes (diff); set staged=true for the index diff; optionally limit to one path",
	map[string]any{
		"path":   strProp("limit diff to this path (optional)"),
		"staged": map[string]any{"type": "boolean", "description": "show staged (index) diff instead of working tree (optional)"},
	}, nil)

func (t *gitDiffTool) Schema() llm.ToolSchema { return gitDiffSchema }
func (t *gitDiffTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *gitDiffTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a gitDiffArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	args := []string{"-C", t.ws, "diff"}
	if a.Staged {
		args = append(args, "--staged")
	}
	if a.Path != "" {
		p, err := resolvePath(t.ws, a.Path)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(t.ws, p)
		if err != nil {
			return "", err
		}
		args = append(args, "--", rel)
	}
	out, err := git(ctx, t.ws, args...)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "no changes", nil
	}
	return capResult(out, maxResultBytes), nil
}

// ---------- git_log ----------

type gitLogTool struct{ ws string }

type gitLogArgs struct {
	N int `json:"n,omitempty"`
}

var gitLogSchema = fnSchema("git_log", "show recent commit history in oneline format",
	map[string]any{"n": intProp("number of commits, default 20, max 50 (optional)")}, nil)

func (t *gitLogTool) Schema() llm.ToolSchema { return gitLogSchema }
func (t *gitLogTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *gitLogTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a gitLogArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	n := a.N
	if n == 0 {
		n = 20
	}
	if n < 0 || n > 50 {
		return "", validationErrorf("n must be between 1 and 50")
	}
	out, err := git(ctx, t.ws, "-C", t.ws, "log", "--oneline", "-n", strconv.Itoa(n))
	if err != nil {
		return "", err
	}
	if out == "" {
		return "no commits yet", nil
	}
	return capResult(out, maxResultBytes), nil
}
