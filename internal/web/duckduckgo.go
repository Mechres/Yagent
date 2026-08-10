package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// DuckDuckGo scrapes html.duckduckgo.com (no API key, no server). Unofficial:
// the HTML structure can change and DDG rate-limits heavy use — fine for a
// personal agent's light queries.
type DuckDuckGo struct {
	http     *http.Client
	endpoint string // overridable in tests
}

const ddgEndpoint = "https://html.duckduckgo.com/html/?q="

func (d *DuckDuckGo) Name() string { return "duckduckgo" }

func (d *DuckDuckGo) Search(ctx context.Context, query string, k int) ([]Result, error) {
	endpoint := d.endpoint
	if endpoint == "" {
		endpoint = ddgEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+url.QueryEscape(query), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) Yagent")
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckduckgo returned %s", resp.Status)
	}
	doc, err := html.Parse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse duckduckgo page: %w", err)
	}
	results := parseDDG(doc)
	if k > 0 && k < len(results) {
		results = results[:k]
	}
	return results, nil
}

// parseDDG walks the result list: each <a class="result__a"> starts a result,
// the following <a class="result__snippet"> fills its snippet.
func parseDDG(doc *html.Node) []Result {
	var out []Result
	var cur *Result
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			switch class := attr(n, "class"); {
			case strings.Contains(class, "result__a"):
				if cur != nil {
					out = append(out, *cur)
				}
				cur = &Result{Title: textContent(n), URL: ddgTarget(attr(n, "href"))}
			case strings.Contains(class, "result__snippet"):
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

// ddgTarget resolves a result link: DDG wraps targets in //duckduckgo.com/l/
// ?uddg=<urlencoded>; anything else is returned as-is (relative // fixed up).
func ddgTarget(href string) string {
	if href == "" {
		return ""
	}
	if u, err := url.Parse(href); err == nil {
		if target := u.Query().Get("uddg"); target != "" {
			return target
		}
	}
	if strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	return href
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
