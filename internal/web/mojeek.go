package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Mojeek searches www.mojeek.com (independent index, no API key). Results are
// <a class="ob"> anchors with <p class="s"> snippets. Unofficial scraping:
// Mojeek may serve a JS challenge from datacenter IPs; from a home connection
// it generally works without one.
type Mojeek struct {
	http     *http.Client
	endpoint string // overridable in tests
}

const mojeekEndpoint = "https://www.mojeek.com/search?q="

func (m *Mojeek) Name() string { return "mojeek" }

func (m *Mojeek) Search(ctx context.Context, query string, k int) ([]Result, error) {
	endpoint := m.endpoint
	if endpoint == "" {
		endpoint = mojeekEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+url.QueryEscape(query), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) Yagent")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mojeek request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mojeek returned %s", resp.Status)
	}
	doc, err := html.Parse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse mojeek page: %w", err)
	}
	results := parseMojeek(doc)
	if k > 0 && k < len(results) {
		results = results[:k]
	}
	return results, nil
}

// parseMojeek collects <a class="ob"> (title + href) and <p class="s">
// snippets, pairing each snippet with the current result.
func parseMojeek(doc *html.Node) []Result {
	var out []Result
	var cur *Result
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			class := attr(n, "class")
			switch {
			case n.Data == "a" && strings.Contains(class, "ob"):
				if cur != nil {
					out = append(out, *cur)
				}
				cur = &Result{Title: textContent(n), URL: attr(n, "href")}
			case n.Data == "p" && strings.Contains(class, "s"):
				if cur != nil {
					cur.Snippet = textContent(n)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}
