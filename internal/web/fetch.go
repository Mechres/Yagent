package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Fetch caps.
const (
	// maxFetchSource bounds how much of the page is read from the network.
	maxFetchSource = 512 << 10
	// maxFetchText caps the extracted text returned to the model.
	maxFetchText = 16 << 10
)

// Fetch GETs a URL (15s timeout, redirect limit 5) and returns a readable
// text rendering of the page, capped at maxFetchText. Scripts, styles, nav
// and other chrome are stripped. Repeated fetches of the same URL within the
// cache TTL are served from cache (no network).
func (c *Client) Fetch(ctx context.Context, rawURL string) (string, error) {
	key := "fetch:" + rawURL
	if e, ok := c.cached(key); ok {
		slog.Debug("web fetch cache hit", "url", rawURL)
		return e.value, nil
	}
	text, err := c.fetchNetwork(ctx, rawURL)
	if err != nil {
		return "", err
	}
	c.store(key, cacheEntry{value: text})
	return text, nil
}

// fetchNetwork performs the actual HTTP GET + extraction. Only http(s) URLs
// are fetched — other schemes (file://, gopher://, ...) are rejected before
// any request so a model-provided URL can never touch the local filesystem or
// a non-HTTP service (adversarial-QA finding #2, 2026-08-13).
func (c *Client) fetchNetwork(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("fetch %s: invalid URL: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("fetch %s: unsupported scheme %q (only http/https)", rawURL, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("fetch %s: missing host", rawURL)
	}
	timeout := c.fetchTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) Yagent")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch %s: %s", rawURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchSource))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", rawURL, err)
	}
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/html") || looksLikeHTML(body) {
		return htmlToText(string(body), maxFetchText), nil
	}
	return truncateText(string(body), maxFetchText), nil
}

// looksLikeHTML is a cheap heuristic for pages that omit a Content-Type.
func looksLikeHTML(body []byte) bool {
	head := string(body)
	if len(head) > 1024 {
		head = head[:1024]
	}
	l := strings.ToLower(head)
	return strings.Contains(l, "<html") || strings.Contains(l, "<!doctype html") ||
		strings.Contains(l, "<body")
}

var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "nav": true,
	"header": true, "footer": true, "svg": true, "form": true,
	"iframe": true, "template": true,
}

var blockTags = map[string]bool{
	"p": true, "h1": true, "h2": true, "h3": true, "h4": true,
	"h5": true, "h6": true, "li": true, "div": true, "section": true,
	"article": true, "pre": true, "blockquote": true, "tr": true,
}

// htmlToText renders HTML as plain text, preserving block boundaries.
func htmlToText(source string, maxOut int) string {
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return "(could not parse page)"
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipTags[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			if s := strings.TrimSpace(n.Data); s != "" {
				b.WriteString(s)
				b.WriteByte(' ')
			}
		}
		if n.Type == html.ElementNode {
			if blockTags[n.Data] || n.Data == "br" {
				b.WriteByte('\n')
			}
			if n.Data == "a" {
				if href := attr(n, "href"); href != "" {
					fmt.Fprintf(&b, "[%s] ", href)
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	out := collapseBlank(b.String())
	return truncateText(out, maxOut)
}

// collapseBlank reduces runs of blank lines to a single one.
func collapseBlank(s string) string {
	out := strings.TrimSpace(s)
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return out
}

func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "\n... (truncated)"
}
