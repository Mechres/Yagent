package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
)

// researchWriteTool is the only workspace mutation exposed by the research
// profile. Reports are durable deliverables, but arbitrary source/config/git
// mutations are outside research mode's scope.
type researchWriteTool struct {
	inner Tool
	ws    string
}

func (t *researchWriteTool) Schema() llm.ToolSchema { return t.inner.Schema() }
func (t *researchWriteTool) Risk() RiskLevel        { return RiskWrite }

func (t *researchWriteTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", validationErrorf("invalid arguments: %v", err)
	}
	if !ResearchReportPathAllowed(t.ws, args.Path) {
		return "error: research mode permits writes only under .yagent/research/", nil
	}
	return t.inner.Execute(ctx, raw)
}

// ResearchReportPathAllowed reports whether name is a markdown report below
// the workspace's .yagent/research directory.
func ResearchReportPathAllowed(workspace, name string) bool {
	name = filepath.Clean(strings.TrimSpace(name))
	if name == "." || filepath.IsAbs(name) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(workspace), filepath.Join(filepath.Clean(workspace), name))
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return false
	}
	researchRoot := filepath.Join(".yagent", "research")
	return strings.HasPrefix(filepath.ToSlash(rel), researchRoot+"/") && strings.HasSuffix(rel, ".md")
}

// ResearchProfile returns a shallow registry containing only capabilities
// useful for evidence-based research. MCP tools are deliberately excluded:
// their schemas do not prove that a remote mutation is safe.
func (r *Registry) ResearchProfile() (*Registry, error) {
	allowed := []string{
		"fs_read", "glob", "grep",
		"git_status", "git_diff", "git_log",
		"index_search", "code_outline", "code_slice", "code_topology", "code_references", "code_impact", "code_unused",
		"web_search", "web_fetch", "paper_search",
		"memory_save", "memory_search", "session_search", "research_note", "scratch_read",
		"fs_write",
	}
	nr, err := r.Restrict(allowed)
	if err != nil {
		// Optional capabilities vary by session configuration. Missing tools are
		// omitted rather than making research mode unavailable.
		nr = &Registry{workspace: r.workspace, tools: make(map[string]Tool)}
		for _, name := range allowed {
			if tool, ok := r.tools[name]; ok {
				nr.tools[name] = tool
			}
		}
	}
	if write, ok := nr.tools["fs_write"]; ok {
		nr.tools["fs_write"] = &researchWriteTool{inner: write, ws: r.workspace}
	}
	return nr, nil
}

func (t *researchWriteTool) String() string { return fmt.Sprintf("research writer for %s", t.ws) }
