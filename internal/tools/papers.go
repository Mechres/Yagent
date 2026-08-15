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
	// Since restricts to papers published in or after this year (recency
	// filter; 0 = no filter).
	Since int `json:"since,omitempty"`
}

var paperSearchSchema = fnSchema("paper_search", "search scholarly databases (arXiv, PubMed, Semantic Scholar) for research papers matching the query; returns title, authors, year, venue, abstract and URL per paper. Use it for academic/research questions, then web_fetch the paper's HTML page (e.g. the arxiv abs/ URL, or arxiv.org/html/<ID> / ar5iv.labs.arxiv.org/html/<ID> for the full body) for the full text",
	map[string]any{
		"query": strProp("the research topic or paper query"),
		"k":     intProp("max results, default 5, max 10 (optional)"),
		"since": intProp("only papers from this year onward, e.g. 2023 (optional; 0 = no filter)"),
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
	if a.Since < 0 || a.Since > 2100 {
		return "", validationErrorf("since must be a year between 0 and 2100")
	}
	if t.client == nil {
		return "error: paper search is not configured", nil
	}
	papers, err := t.client.SearchPapers(ctx, a.Query, a.K, a.Since)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(papers) == 0 {
		return "no papers found", nil
	}
	var b strings.Builder
	if a.Since > 0 {
		b.WriteString(fmt.Sprintf("[papers since %d]\n", a.Since))
	}
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
