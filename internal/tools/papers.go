package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/web"
)

// ---------- paper_search ----------

type paperSearchTool struct {
	client *web.Client
}

type paperSearchArgs struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

var paperSearchSchema = fnSchema("paper_search", "search scholarly databases (arXiv, PubMed, Semantic Scholar) for research papers matching the query; returns title, authors, year, venue, abstract and URL per paper. Use it for academic/research questions, then web_fetch the paper's HTML page (e.g. the arxiv abs/ URL) for the full text",
	map[string]any{
		"query": strProp("the research topic or paper query"),
		"k":     intProp("max results, default 5, max 10 (optional)"),
	},
	[]string{"query"})

func (t *paperSearchTool) Schema() llm.ToolSchema { return paperSearchSchema }
func (t *paperSearchTool) Risk() RiskLevel        { return RiskReadOnly }

func (t *paperSearchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a paperSearchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", validationErrorf(`argument "query" is required`)
	}
	if a.K <= 0 {
		a.K = 5
	}
	if a.K > 10 {
		return "", validationErrorf("k must be at most 10")
	}
	if t.client == nil {
		return "error: paper search is not configured", nil
	}
	papers, err := t.client.SearchPapers(ctx, a.Query, a.K)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(papers) == 0 {
		return "no papers found", nil
	}
	var b strings.Builder
	for i, p := range papers {
		fmt.Fprintf(&b, "%d. %s\n", i+1, p.Title)
		if len(p.Authors) > 0 {
			fmt.Fprintf(&b, "   authors: %s\n", strings.Join(p.Authors, ", "))
		}
		var meta []string
		if p.Year > 0 {
			meta = append(meta, fmt.Sprintf("%d", p.Year))
		}
		if p.Venue != "" {
			meta = append(meta, p.Venue)
		}
		if len(meta) > 0 {
			fmt.Fprintf(&b, "   %s\n", strings.Join(meta, " · "))
		}
		if p.Abstract != "" {
			fmt.Fprintf(&b, "   abstract: %s\n", oneLineCap(p.Abstract, 300))
		}
		if p.URL != "" {
			fmt.Fprintf(&b, "   url: %s\n", p.URL)
		}
		if p.DOI != "" {
			fmt.Fprintf(&b, "   doi: %s\n", p.DOI)
		}
	}
	return capResult(WrapUntrusted("paper_search for "+a.Query, b.String()), maxResultBytes), nil
}

// oneLineCap collapses whitespace and caps the length of a one-line string.
func oneLineCap(s string, max int) string {
	one := strings.Join(strings.Fields(s), " ")
	if len(one) > max {
		return one[:max] + "…"
	}
	return one
}
