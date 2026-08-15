package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
)

// ---------- research_note ----------

type researchNoteTool struct {
	record func(note string)
}

type researchNoteArgs struct {
	Note   string `json:"note"`
	Source string `json:"source,omitempty"`
}

var researchNoteSchema = fnSchema("research_note", "record one verified research finding (the fact + its source URL) into the persistent research ledger. Notes survive context pruning, so a long research session keeps its accumulated facts and their citations. Use it after web_fetch confirms a claim; include the exact URL the fact came from",
	map[string]any{
		"note":   strProp("the finding, stated as a fact with enough detail to stand alone"),
		"source": strProp("the URL this finding came from (optional but strongly encouraged)"),
	},
	[]string{"note"})

func (t *researchNoteTool) Schema() llm.ToolSchema { return researchNoteSchema }
func (t *researchNoteTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *researchNoteTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a researchNoteArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Note) == "" {
		return "", validationErrorf(`argument "note" is required`)
	}
	note := strings.TrimSpace(a.Note)
	if s := strings.TrimSpace(a.Source); s != "" {
		note += " [" + s + "]"
	}
	if t.record != nil {
		t.record(note)
		return fmt.Sprintf("recorded research note (%d characters)", len(note)), nil
	}
	return "error: research ledger is not configured", nil
}
