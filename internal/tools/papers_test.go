package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/web"
)

// fakePaperSource returns canned papers (no network) so the tool's wiring and
// formatting are testable offline. The source parsing itself is covered by the
// web package tests.
type fakePaperSource struct {
	papers []web.Paper
}

func (f *fakePaperSource) Name() string { return "fake" }
func (f *fakePaperSource) SearchPapers(ctx context.Context, q string, k, sinceYear int) ([]web.Paper, error) {
	if k > 0 && k < len(f.papers) {
		return f.papers[:k], nil
	}
	return f.papers, nil
}

func TestPaperSearchTool(t *testing.T) {
	c, err := web.New(web.Config{Provider: "duckduckgo"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetPaperSources([]web.PaperSource{&fakePaperSource{papers: []web.Paper{
		{
			Title:    "Quantization for llama.cpp",
			Authors:  []string{"Alice Example"},
			Year:     2026,
			Venue:    "arXiv",
			Abstract: "Quantization lowers memory use for LLM inference.",
			URL:      "https://arxiv.org/abs/2601.14277v1",
			DOI:      "2601.14277",
		},
	}}})
	reg := NewRegistry(t.TempDir(), Options{Web: c, Papers: true})

	if got := execTool(t, reg, "paper_search", map[string]any{"query": "llama.cpp quantization"}); !strings.Contains(got, "Quantization for llama.cpp") {
		t.Errorf("paper_search title missing: %q", got)
	}
	if got := execTool(t, reg, "paper_search", map[string]any{"query": "llama.cpp quantization"}); !strings.Contains(got, "arxiv.org/abs/2601.14277v1") {
		t.Errorf("paper_search url missing: %q", got)
	}
	if got := execTool(t, reg, "paper_search", map[string]any{"query": "llama.cpp quantization"}); !strings.Contains(got, "Alice Example") || !strings.Contains(got, "2026") {
		t.Errorf("paper_search authors/year missing: %q", got)
	}
	// untrusted wrapper
	if got := execTool(t, reg, "paper_search", map[string]any{"query": "x"}); !strings.Contains(got, "<untrusted data from paper_search for x>") {
		t.Errorf("paper_search missing untrusted wrapper: %q", got)
	}
	// validation
	if got := execTool(t, reg, "paper_search", map[string]any{"query": ""}); !strings.Contains(got, "validation-error") {
		t.Errorf("paper_search empty = %q", got)
	}
	if got := execTool(t, reg, "paper_search", map[string]any{"query": "x", "k": 99}); !strings.Contains(got, "validation-error") {
		t.Errorf("paper_search k=99 = %q", got)
	}
	if got := execTool(t, reg, "paper_search", map[string]any{"query": "x", "since": -1}); !strings.Contains(got, "validation-error") {
		t.Errorf("paper_search since=-1 = %q", got)
	}
	if got := execTool(t, reg, "paper_search", map[string]any{"query": "x", "since": 9999}); !strings.Contains(got, "validation-error") {
		t.Errorf("paper_search since=9999 = %q", got)
	}
	// since is passed through to the sources
	if got := execTool(t, reg, "paper_search", map[string]any{"query": "x", "since": 2024}); !strings.Contains(got, "[papers since 2024]") {
		t.Errorf("paper_search since=2024 = %q", got)
	}
	// not registered without Papers
	reg2 := NewRegistry(t.TempDir(), Options{Web: c})
	if _, ok := reg2.Get("paper_search"); ok {
		t.Error("paper_search should not be registered without Papers")
	}
	// no paper sources configured -> structured error
	c2, _ := web.New(web.Config{Provider: "duckduckgo"})
	reg3 := NewRegistry(t.TempDir(), Options{Web: c2, Papers: true})
	if got := execTool(t, reg3, "paper_search", map[string]any{"query": "x"}); !strings.Contains(got, "no paper sources") {
		t.Errorf("paper_search without sources = %q", got)
	}
}

func TestPaperSearchNotConfigured(t *testing.T) {
	reg := NewRegistry(t.TempDir(), Options{})
	if _, ok := reg.Get("paper_search"); ok {
		t.Error("paper_search should not be registered when web is nil")
	}
}
