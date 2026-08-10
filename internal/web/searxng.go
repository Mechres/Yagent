package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// SearXNG searches a self-hosted SearXNG instance via its JSON API
// (requires `format: json` in the instance's settings.yml).
type SearXNG struct {
	baseURL string
	http    *http.Client
}

func (s *SearXNG) Name() string { return "searxng" }

func (s *SearXNG) Search(ctx context.Context, query string, k int) ([]Result, error) {
	u := fmt.Sprintf("%s/search?q=%s&format=json", s.baseURL, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng returned %s", resp.Status)
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read searxng response: %w", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse searxng response: %w", err)
	}
	results := make([]Result, 0, len(out.Results))
	for _, r := range out.Results {
		snippet := strings.TrimSpace(r.Content)
		results = append(results, Result{Title: r.Title, URL: r.URL, Snippet: snippet})
	}
	if k > 0 && k < len(results) {
		results = results[:k]
	}
	return results, nil
}
