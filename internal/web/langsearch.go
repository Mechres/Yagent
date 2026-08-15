package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// LangSearch is a hosted web-search API (langsearch.com) that is free for
// individuals and small teams. It needs a dashboard API key (Bearer header).
// The response is Bing-compatible JSON. Opt-in: only used when
// web_search.langsearch_api_key is configured.
type LangSearch struct {
	http *http.Client
	key  string
	url  string // overridable in tests
}

func (l *LangSearch) Name() string { return "langsearch" }

func (l *LangSearch) Search(ctx context.Context, query string, k int) ([]Result, error) {
	endpoint := l.url
	if endpoint == "" {
		endpoint = "https://api.langsearch.com/v1/web-search"
	}
	if k <= 0 || k > 10 {
		k = 10
	}
	body := strings.NewReader(fmt.Sprintf(`{"query":%q,"count":%d,"summary":false}`, query, k))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("build langsearch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.key)
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("langsearch request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("langsearch returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read langsearch response: %w", err)
	}
	var out struct {
		Data struct {
			WebPages struct {
				Value []struct {
					Name    string `json:"name"`
					URL     string `json:"url"`
					Snippet string `json:"snippet"`
				} `json:"value"`
			} `json:"webPages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse langsearch response: %w", err)
	}
	results := make([]Result, 0, len(out.Data.WebPages.Value))
	for _, v := range out.Data.WebPages.Value {
		results = append(results, Result{Title: strings.TrimSpace(v.Name), URL: v.URL, Snippet: oneLine(v.Snippet)})
	}
	if k > 0 && k < len(results) {
		results = results[:k]
	}
	return results, nil
}
