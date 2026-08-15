package web

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Paper is one scholarly result (arxiv / pubmed / semantic-scholar).
type Paper struct {
	Title    string
	Authors  []string
	Year     int
	Venue    string
	Abstract string
	URL      string
	DOI      string
}

// paperUserAgent identifies yagent to the scholarly APIs. arXiv's export API
// requires a descriptive User-Agent and throttles by IP; Go's default
// "Go-http-client/1.1" is the classic trigger for 429s.
const paperUserAgent = "Mozilla/5.0 (X11; Linux x86_64) Yagent"

// PaperSource is a scholarly index the paper search queries.
type PaperSource interface {
	Name() string
	// SearchPapers queries for papers matching query, returning at most k
	// results. sinceYear, when > 0, restricts to papers published in or after
	// that year (recency filter).
	SearchPapers(ctx context.Context, query string, k, sinceYear int) ([]Paper, error)
}

// arXiv searches the keyless arXiv Atom API (export.arxiv.org). No API key,
// generous rate limits; returns papers with abstract, authors and the abs/
// landing URL the research gate can fetch.
type arXiv struct {
	http     *http.Client
	endpoint string // overridable in tests
}

func (a *arXiv) Name() string { return "arxiv" }

// NewArxivSource builds an arXiv paper source bound to an explicit http client
// and endpoint (used by tests and the eval harness to run paper search
// against a fake server).
func NewArxivSource(httpClient *http.Client, endpoint string) *arXiv {
	return &arXiv{http: httpClient, endpoint: endpoint}
}

func (a *arXiv) SearchPapers(ctx context.Context, query string, k, sinceYear int) ([]Paper, error) {
	endpoint := a.endpoint
	if endpoint == "" {
		endpoint = "https://export.arxiv.org/api/query"
	}
	// arXiv's query syntax: "all:term AND all:term". Splitting the words makes
	// a phrase like "llama.cpp quantization" a conjunction instead of one
	// exact-phrase match, so a topical query actually finds papers.
	terms := strings.Fields(query)
	var q string
	if len(terms) == 0 {
		q = "all:empty"
	} else {
		parts := make([]string, 0, len(terms))
		for _, t := range terms {
			parts = append(parts, "all:"+t)
		}
		q = strings.Join(parts, " AND ")
	}
	// Recency filter: restrict to submissions from the given year onward.
	if sinceYear > 0 {
		lo := fmt.Sprintf("%04d0101000000", sinceYear)
		q += " AND submittedDate:[" + lo + " TO 99991231235959]"
	}
	u := endpoint + "?search_query=" + url.QueryEscape(q) + "&start=0&max_results=" + itoa(k)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build arxiv request: %w", err)
	}
	req.Header.Set("User-Agent", paperUserAgent)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("arxiv request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("arxiv rate-limited (429) — wait a few seconds and retry")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("arxiv returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read arxiv response: %w", err)
	}
	var feed struct {
		Entries []struct {
			Title     string `xml:"title"`
			ID        string `xml:"id"`
			Summary   string `xml:"summary"`
			Published string `xml:"published"`
			Authors   []struct {
				Name string `xml:"name"`
			} `xml:"author"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, fmt.Errorf("parse arxiv response: %w", err)
	}
	papers := make([]Paper, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		p := Paper{Title: strings.TrimSpace(e.Title), Abstract: oneLine(e.Summary)}
		if id := strings.TrimSpace(e.ID); id != "" {
			p.URL = id
			if abs := strings.Index(id, "/abs/"); abs >= 0 {
				p.DOI = id[abs+len("/abs/"):] // arxiv id
			}
		}
		if len(e.Authors) > 0 {
			p.Authors = make([]string, 0, len(e.Authors))
			for _, au := range e.Authors {
				if n := strings.TrimSpace(au.Name); n != "" {
					p.Authors = append(p.Authors, n)
				}
			}
		}
		if y, ok := parseYear(e.Published); ok {
			p.Year = y
		}
		papers = append(papers, p)
	}
	if k > 0 && k < len(papers) {
		papers = papers[:k]
	}
	return papers, nil
}

// PubMed searches the keyless E-utilities API: esearch for PMIDs, then esummary
// for the titles/abstracts. No API key needed for light use (NCBI rate-limits).
type PubMed struct {
	http      *http.Client
	base      string // overridable in tests
	esearchF  string
	esummaryF string
}

func (p *PubMed) Name() string { return "pubmed" }

func (p *PubMed) baseURL() string {
	if p.base != "" {
		return p.base
	}
	return "https://eutils.ncbi.nlm.nih.gov/entrez/eutils"
}

func (p *PubMed) SearchPapers(ctx context.Context, query string, k, sinceYear int) ([]Paper, error) {
	ids, err := p.esearch(ctx, query, k, sinceYear)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return p.esummary(ctx, ids)
}

func (p *PubMed) esearch(ctx context.Context, query string, k, sinceYear int) ([]string, error) {
	term := query
	if sinceYear > 0 {
		// PubMed date-range syntax: "2023/01/01"[dp] restricts to publication
		// dates on or after that day.
		term += fmt.Sprintf(` AND "%04d/01/01"[dp]`, sinceYear)
	}
	u := p.baseURL() + "/esearch.fcgi?db=pubmed&retmode=json&retmax=" + itoa(k) + "&term=" + url.QueryEscape(term)
	var out struct {
		Result struct {
			IDList []string `json:"idlist"`
		} `json:"esearchresult"`
	}
	if err := p.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	return out.Result.IDList, nil
}

func (p *PubMed) esummary(ctx context.Context, ids []string) ([]Paper, error) {
	u := p.baseURL() + "/esummary.fcgi?db=pubmed&retmode=json&id=" + strings.Join(ids, ",")
	var out struct {
		Result map[string]struct {
			Title   string `json:"title"`
			Pubdate string `json:"pubdate"`
			Source  string `json:"source"`
			DOI     string `json:"elocationid"`
			Authors []struct {
				Name string `json:"name"`
			} `json:"authors"`
		} `json:"result"`
	}
	if err := p.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	papers := make([]Paper, 0, len(ids))
	for _, id := range ids {
		r, ok := out.Result[id]
		if !ok {
			continue
		}
		p := Paper{Title: strings.TrimSpace(r.Title), Venue: r.Source, DOI: r.DOI}
		if y, ok := parseYear(r.Pubdate); ok {
			p.Year = y
		}
		for _, au := range r.Authors {
			if n := strings.TrimSpace(au.Name); n != "" {
				p.Authors = append(p.Authors, n)
			}
		}
		p.URL = "https://pubmed.ncbi.nlm.nih.gov/" + id + "/"
		papers = append(papers, p)
	}
	return papers, nil
}

func (p *PubMed) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build pubmed request: %w", err)
	}
	req.Header.Set("User-Agent", paperUserAgent)
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("pubmed request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("pubmed rate-limited (429) — wait a few seconds and retry")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pubmed returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read pubmed response: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse pubmed response: %w", err)
	}
	return nil
}

// SemanticScholar searches the Semantic Scholar Graph API (api.semanticscholar.org).
// Keyless use is rate-limited (429); with an API key it is practical.
type SemanticScholar struct {
	http *http.Client
	key  string
	base string // overridable in tests
}

func (s *SemanticScholar) Name() string { return "semantic-scholar" }

func (s *SemanticScholar) baseURL() string {
	if s.base != "" {
		return s.base
	}
	return "https://api.semanticscholar.org/graph/v1"
}

func (s *SemanticScholar) SearchPapers(ctx context.Context, query string, k, sinceYear int) ([]Paper, error) {
	q := query
	if sinceYear > 0 {
		q += " year:" + itoa(sinceYear) + "-" + itoa(currentYear())
	}
	u := s.baseURL() + "/paper/search?query=" + url.QueryEscape(q) +
		"&limit=" + itoa(k) + "&fields=title,abstract,year,venue,url,externalIds,authors.name"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build semanticscholar request: %w", err)
	}
	req.Header.Set("User-Agent", paperUserAgent)
	if s.key != "" {
		req.Header.Set("x-api-key", s.key)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("semanticscholar request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("semantic scholar rate-limited (429) — it requires an api key for sustained use")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("semantic scholar returned %s", resp.Status)
	}
	var out struct {
		Data []struct {
			Title       string            `json:"title"`
			Abstract    string            `json:"abstract"`
			Year        int               `json:"year"`
			Venue       string            `json:"venue"`
			URL         string            `json:"url"`
			ExternalIDs map[string]string `json:"externalIds"`
			Authors     []struct {
				Name string `json:"name"`
			} `json:"authors"`
		} `json:"data"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read semanticscholar response: %w", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse semanticscholar response: %w", err)
	}
	papers := make([]Paper, 0, len(out.Data))
	for _, d := range out.Data {
		p := Paper{Title: strings.TrimSpace(d.Title), Abstract: oneLine(d.Abstract), Year: d.Year, Venue: d.Venue, URL: d.URL, DOI: d.ExternalIDs["DOI"]}
		for _, au := range d.Authors {
			if n := strings.TrimSpace(au.Name); n != "" {
				p.Authors = append(p.Authors, n)
			}
		}
		papers = append(papers, p)
	}
	return papers, nil
}

// SearchPapers runs the configured paper sources in parallel and merges their
// results (deduped by URL), falling back per-source on error. Sources with
// rate limits or failures degrade gracefully — the tool returns whatever the
// working sources found. sinceYear, when > 0, restricts to papers from that
// year onward (recency filter, passed to every source).
func (c *Client) SearchPapers(ctx context.Context, query string, k, sinceYear int) ([]Paper, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is required")
	}
	sources := c.paperSources()
	if len(sources) == 0 {
		return nil, fmt.Errorf("no paper sources configured")
	}
	type srcResult struct {
		src    string
		papers []Paper
		err    error
	}
	results := make([]srcResult, len(sources))
	var wg sync.WaitGroup
	for i, src := range sources {
		wg.Add(1)
		go func(i int, src PaperSource) {
			defer wg.Done()
			ps, err := src.SearchPapers(ctx, query, k, sinceYear)
			results[i] = srcResult{src: src.Name(), papers: ps, err: err}
		}(i, src)
	}
	wg.Wait()
	var merged []Paper
	seen := map[string]bool{}
	var errs []string
	for _, r := range results {
		if r.err != nil {
			slog.Debug("paper source failed", "source", r.src, "error", r.err)
			errs = append(errs, fmt.Sprintf("%s: %v", r.src, r.err))
			continue
		}
		for _, p := range r.papers {
			key := strings.ToLower(strings.TrimSpace(p.URL))
			if key == "" {
				key = strings.ToLower(p.Title)
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, p)
		}
	}
	if len(merged) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("paper search failed: %s", strings.Join(errs, "; "))
	}
	if k > 0 && k < len(merged) {
		merged = merged[:k]
	}
	return merged, nil
}

// paperSources returns the configured paper indexes (nil when none enabled).
func (c *Client) paperSources() []PaperSource {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paperSrcs
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// currentYear returns the local calendar year (for recency-filter ranges).
func currentYear() int {
	return time.Now().Year()
}

// oneLine collapses whitespace so an abstract/snippet reads as a single line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func parseYear(s string) (int, bool) {
	for _, part := range strings.Fields(s) {
		// ISO date: "2026-01-11T18:52:37Z" or "2026-01-11"
		if len(part) >= 4 && allDigits(part[:4]) {
			return atoi(part[:4]), true
		}
		if len(part) == 4 && part[0] >= '1' && part[0] <= '9' && allDigits(part) {
			return atoi(part), true
		}
	}
	return 0, false
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
