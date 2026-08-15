package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakearXiv serves a minimal arXiv Atom feed.
func fakearXiv(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>http://arxiv.org/abs/2601.14277v1</id>
    <title>Which Quantization Should I Use? A Unified Evaluation</title>
    <published>2026-01-11T18:52:37Z</published>
    <author><name>Alice Example</name></author>
    <author><name>Bob Sample</name></author>
    <summary>Quantization reduces model precision to lower memory use.</summary>
  </entry>
</feed>`))
	}))
}

func TestArxivSearch(t *testing.T) {
	ts := fakearXiv(t)
	defer ts.Close()
	a := &arXiv{http: ts.Client(), endpoint: ts.URL + "/api/query"}
	papers, err := a.SearchPapers(context.Background(), "llama.cpp", 5, 0)
	if err != nil {
		t.Fatalf("SearchPapers: %v", err)
	}
	if len(papers) != 1 {
		t.Fatalf("papers = %d, want 1", len(papers))
	}
	p := papers[0]
	if !strings.Contains(p.Title, "Quantization") {
		t.Errorf("title = %q", p.Title)
	}
	if p.URL != "http://arxiv.org/abs/2601.14277v1" {
		t.Errorf("url = %q", p.URL)
	}
	if len(p.Authors) != 2 || p.Authors[0] != "Alice Example" {
		t.Errorf("authors = %v", p.Authors)
	}
	if p.Year != 2026 {
		t.Errorf("year = %d", p.Year)
	}
	if !strings.Contains(p.Abstract, "Quantization reduces") {
		t.Errorf("abstract = %q", p.Abstract)
	}
}

func TestArxivHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer ts.Close()
	a := &arXiv{http: ts.Client(), endpoint: ts.URL + "/api/query"}
	if _, err := a.SearchPapers(context.Background(), "x", 5, 0); err == nil {
		t.Error("arxiv 500 should error")
	}
}

func TestArxivRecencyFilter(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("search_query")
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(`<feed xmlns="http://www.w3.org/2005/Atom"></feed>`))
	}))
	defer ts.Close()
	a := &arXiv{http: ts.Client(), endpoint: ts.URL + "/api/query"}
	if _, err := a.SearchPapers(context.Background(), "llama quantization", 5, 2024); err != nil {
		t.Fatal(err)
	}
	// the recency filter must AND a submittedDate range onto the query
	if !strings.Contains(gotQuery, "submittedDate:[20240101000000 TO 99991231235959]") {
		t.Errorf("arxiv recency filter missing from query: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "all:llama AND all:quantization") {
		t.Errorf("arxiv term conjunction lost: %q", gotQuery)
	}
}

// fakePubMed serves esearch + esummary.
func fakePubMed(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "esearch"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"esearchresult": map[string]any{"idlist": []string{"42600735"}},
			})
		case strings.Contains(r.URL.Path, "esummary"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{
					"42600735": map[string]any{
						"title":       "Large language models in medicine",
						"pubdate":     "2025 Jan 15",
						"source":      "Nature Medicine",
						"elocationid": "10.1038/s41591-025-00001",
						"authors":     []map[string]string{{"name": "Carol Doctor"}},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestPubMedSearch(t *testing.T) {
	ts := fakePubMed(t)
	defer ts.Close()
	p := &PubMed{http: ts.Client(), base: ts.URL}
	papers, err := p.SearchPapers(context.Background(), "llm inference", 5, 0)
	if err != nil {
		t.Fatalf("SearchPapers: %v", err)
	}
	if len(papers) != 1 {
		t.Fatalf("papers = %d, want 1", len(papers))
	}
	p0 := papers[0]
	if p0.Title != "Large language models in medicine" || p0.Venue != "Nature Medicine" || p0.Year != 2025 {
		t.Errorf("paper = %+v", p0)
	}
	if !strings.HasPrefix(p0.URL, "https://pubmed.ncbi.nlm.nih.gov/42600735") {
		t.Errorf("url = %q", p0.URL)
	}
}

func TestPubMedRecencyFilter(t *testing.T) {
	var gotTerm string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTerm = r.URL.Query().Get("term")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"esearchresult": map[string]any{"idlist": []string{}}})
	}))
	defer ts.Close()
	p := &PubMed{http: ts.Client(), base: ts.URL}
	if _, err := p.SearchPapers(context.Background(), "llm inference", 5, 2023); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotTerm, `"2023/01/01"[dp]`) {
		t.Errorf("pubmed recency filter missing: %q", gotTerm)
	}
}

// fakeScholar serves Semantic Scholar Graph search.
func fakeScholar(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("x-api-key") == "" {
			t.Log("scholar request had no api key")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"title":       "Efficient quantized inference",
					"abstract":    "We compress transformers.",
					"year":        2024,
					"venue":       "ICML",
					"url":         "https://example.com/paper",
					"externalIds": map[string]string{"DOI": "10.1234/abc"},
					"authors":     []map[string]string{{"name": "Dana Researcher"}},
				},
			},
		})
	}))
}

func TestSemanticScholarSearch(t *testing.T) {
	ts := fakeScholar(t)
	defer ts.Close()
	s := &SemanticScholar{http: ts.Client(), base: ts.URL, key: "testkey"}
	papers, err := s.SearchPapers(context.Background(), "quantized inference", 5, 0)
	if err != nil {
		t.Fatalf("SearchPapers: %v", err)
	}
	if len(papers) != 1 {
		t.Fatalf("papers = %d, want 1", len(papers))
	}
	p := papers[0]
	if p.Title != "Efficient quantized inference" || p.Year != 2024 || p.Venue != "ICML" || p.DOI != "10.1234/abc" {
		t.Errorf("paper = %+v", p)
	}
}

func TestSemanticScholarRecencyFilter(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer ts.Close()
	s := &SemanticScholar{http: ts.Client(), base: ts.URL}
	if _, err := s.SearchPapers(context.Background(), "quantized inference", 5, 2022); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "year:2022-"+itoa(currentYear())) {
		t.Errorf("semantic scholar recency filter missing: %q", gotQuery)
	}
}

func TestSemanticScholarRateLimited(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()
	s := &SemanticScholar{http: ts.Client(), base: ts.URL}
	_, err := s.SearchPapers(context.Background(), "x", 5, 0)
	if err == nil || !strings.Contains(err.Error(), "rate-limited") {
		t.Errorf("429 should produce a rate-limit error, got %v", err)
	}
}

// fakeLangSearch serves a Bing-compatible response.
func fakeLangSearch(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer testkey" {
			t.Errorf("missing bearer header")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": map[string]any{
				"webPages": map[string]any{
					"value": []map[string]any{
						{"name": "Result one", "url": "https://example.com/1", "snippet": "first snippet"},
						{"name": "Result two", "url": "https://example.com/2", "snippet": "second snippet"},
					},
				},
			},
		})
	}))
}

func TestLangSearchProvider(t *testing.T) {
	ts := fakeLangSearch(t)
	defer ts.Close()
	l := &LangSearch{http: ts.Client(), key: "testkey", url: ts.URL}
	res, err := l.Search(context.Background(), "query", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 || res[0].Title != "Result one" || res[0].URL != "https://example.com/1" {
		t.Errorf("results = %+v", res)
	}
	// k caps
	res2, _ := l.Search(context.Background(), "query", 1)
	if len(res2) != 1 {
		t.Errorf("k=1 gave %d results", len(res2))
	}
}

func TestLangSearchInFallbackChain(t *testing.T) {
	c, err := New(Config{Provider: "duckduckgo", LangSearchKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range c.providers {
		names[p.Name()] = true
	}
	if !names["langsearch"] {
		t.Errorf("langsearch not in fallback chain: %v", names)
	}
	// as a primary, it requires the key
	if _, err := New(Config{Provider: "langsearch"}); err == nil {
		t.Error("langsearch primary without key should fail")
	}
	c2, err := New(Config{Provider: "langsearch", LangSearchKey: "k"})
	if err != nil || c2.ProviderName() != "langsearch" {
		t.Errorf("langsearch primary = %v / %v", c2, err)
	}
}

// countingPaperSource counts calls so the parallel fan-out is provable.
type countingPaperSource struct {
	calls  atomic.Int64
	papers []Paper
	err    error
}

func (f *countingPaperSource) Name() string { return "counting" }
func (f *countingPaperSource) SearchPapers(ctx context.Context, q string, k, sinceYear int) ([]Paper, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.papers, nil
}

func TestSearchPapersMergesAndDedups(t *testing.T) {
	a := &countingPaperSource{papers: []Paper{{Title: "Paper A", URL: "https://example.com/a"}, {Title: "Paper B", URL: "https://example.com/b"}}}
	b := &countingPaperSource{papers: []Paper{{Title: "Paper A dup", URL: "https://example.com/a"}}}
	c := &Client{http: defaultHTTP(), paperSrcs: []PaperSource{a, b}}
	papers, err := c.SearchPapers(context.Background(), "query", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(papers) != 2 {
		t.Errorf("merged = %d, want 2 (deduped by URL)", len(papers))
	}
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Errorf("calls = %d/%d, want both 1", a.calls.Load(), b.calls.Load())
	}
}

func TestSearchPapersFallsBackOnError(t *testing.T) {
	bad := &countingPaperSource{err: errors.New("source down")}
	good := &countingPaperSource{papers: []Paper{{Title: "Good", URL: "https://example.com/good"}}}
	c := &Client{http: defaultHTTP(), paperSrcs: []PaperSource{bad, good}}
	papers, err := c.SearchPapers(context.Background(), "query", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(papers) != 1 || papers[0].Title != "Good" {
		t.Errorf("papers = %+v", papers)
	}
}

func TestSearchPapersNoSources(t *testing.T) {
	c := &Client{http: defaultHTTP()}
	if _, err := c.SearchPapers(context.Background(), "query", 5, 0); err == nil {
		t.Error("no paper sources should error")
	}
}

func TestSetPaperSrcsConfig(t *testing.T) {
	// Papers off -> no sources
	c, _ := New(Config{Provider: "duckduckgo"})
	if len(c.paperSources()) != 0 {
		t.Error("papers disabled should have no sources")
	}
	// Papers on -> arxiv + pubmed
	c, _ = New(Config{Provider: "duckduckgo", Papers: true})
	if len(c.paperSources()) != 2 {
		t.Errorf("papers on = %d sources, want 2 (arxiv+pubmed)", len(c.paperSources()))
	}
	// + scholar key
	c, _ = New(Config{Provider: "duckduckgo", Papers: true, SemanticScholarKey: "k"})
	if len(c.paperSources()) != 3 {
		t.Errorf("papers+scholar = %d sources, want 3", len(c.paperSources()))
	}
}
