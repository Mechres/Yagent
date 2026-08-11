package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"yagent/internal/llm"
	"yagent/internal/memory"
)

// ---------- memory_save ----------

type memorySaveTool struct {
	vectors        *memory.VectorStore
	projectVectors *memory.VectorStore
	sessionID      string
}

type memorySaveArgs struct {
	Text       string  `json:"text"`
	Importance float64 `json:"importance,omitempty"`
	Scope      string  `json:"scope,omitempty"` // global (default) | project
}

var memorySaveSchema = fnSchema("memory_save", "store a fact worth remembering across sessions: user preferences, project decisions, gotchas, reusable findings. Phrase the fact descriptively, in third person about the user (e.g. \"the user's name is Ada\", \"the user prefers tabs\") — never as a first-person quote (\"my name is ...\"). NOT code, NOT chit-chat, NOT tool output.",
	map[string]any{
		"text":       strProp("the fact or preference to remember, one concise third-person sentence"),
		"importance": numProp("how important this fact is for future recall, 0.0-1.0, default 0.5 (optional)"),
		"scope":      strProp("where to store it: global (default, personal) or project (shared with the team via the repo) (optional)"),
	},
	[]string{"text"})

func (t *memorySaveTool) Schema() llm.ToolSchema { return memorySaveSchema }
func (t *memorySaveTool) Risk() RiskLevel        { return RiskWrite }

// SelfGated means memory_save applies automatically: saving a fact to the
// agent's own memory store is low-stakes and reversible in spirit, so it must
// not prompt the user (unlike workspace/destructive writes).
func (t *memorySaveTool) SelfGated() bool { return true }

func (t *memorySaveTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a memorySaveArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Text == "" {
		return "", validationErrorf(`argument "text" is required`)
	}
	if a.Importance < 0 || a.Importance > 1 {
		return "", validationErrorf("importance must be between 0 and 1")
	}
	switch a.Scope {
	case "", "global":
		if t.vectors == nil {
			return "error: semantic memory is not configured for this session", nil
		}
		if err := t.vectors.Save(ctx, a.Text, "tool", t.sessionID, a.Importance); err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
	case "project":
		if t.projectVectors == nil {
			return "error: project memory is not configured for this workspace", nil
		}
		if err := t.projectVectors.Save(ctx, a.Text, "tool", t.sessionID, a.Importance); err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
	default:
		return "", validationErrorf("scope must be global or project")
	}
	return "remembered", nil
}

// ---------- memory_search ----------

type memorySearchTool struct {
	vectors        *memory.VectorStore
	projectVectors *memory.VectorStore
}

type memorySearchArgs struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

var memorySearchSchema = fnSchema("memory_search", "recall facts from past sessions semantically related to the query (personal + project memory); use when the user references something from before",
	map[string]any{
		"query": strProp("what to recall, in natural language"),
		"k":     intProp("max results, default 5, max 10 (optional)"),
	},
	[]string{"query"})

func (t *memorySearchTool) Schema() llm.ToolSchema { return memorySearchSchema }
func (t *memorySearchTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *memorySearchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a memorySearchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Query == "" {
		return "", validationErrorf(`argument "query" is required`)
	}
	if a.K <= 0 {
		a.K = 5
	}
	if a.K > 10 {
		return "", validationErrorf("k must be at most 10")
	}
	if t.vectors == nil && t.projectVectors == nil {
		return "error: semantic memory is not configured for this session", nil
	}
	results, err := searchAll(ctx, a.Query, a.K, t.vectors, t.projectVectors)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(results) == 0 {
		return "no memories found", nil
	}
	var b strings.Builder
	for _, m := range results {
		fmt.Fprintf(&b, "- [%.2f] %s\n", m.Score, m.Text)
	}
	return capResult(b.String(), maxResultBytes), nil
}

// searchAll queries one or more memory stores and merges the top-k by score.
func searchAll(ctx context.Context, query string, k int, stores ...*memory.VectorStore) ([]memory.Memory, error) {
	var merged []memory.Memory
	for _, vs := range stores {
		if vs == nil {
			continue
		}
		ms, err := vs.Search(ctx, query, k)
		if err != nil {
			return nil, err
		}
		merged = append(merged, ms...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if len(merged) > k {
		merged = merged[:k]
	}
	return merged, nil
}
