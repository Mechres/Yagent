package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Fetch caps.
const (
	// maxFetchSource bounds how much of the page is read from the network.
	maxFetchSource = 512 << 10
	// maxFetchText is the default cap on the extracted text returned to the
	// model (configurable via web_search.max_fetch_kib; see Client.MaxFetchBytes).
	maxFetchText = 32 << 10
)

// Fetch GETs a URL (15s timeout, redirect limit 5) and returns a readable
// Markdown rendering of the page, capped at the client's MaxFetchBytes.
// Scripts, styles, nav and other chrome are stripped. Repeated fetches of the
// same URL within the cache TTL are served from cache (no network).
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

// maxOut returns the effective extracted-text cap for this client.
func (c *Client) maxOut() int {
	if c != nil && c.MaxFetchBytes > 0 {
		return c.MaxFetchBytes
	}
	return maxFetchText
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
	// PDFs (arXiv papers, datasheets, docs) are the one binary format a
	// research agent hits constantly. web_fetch cannot extract their text —
	// fail with a readable reason instead of returning binary garbage, so the
	// model knows to look for the HTML/abstract version.
	if strings.Contains(strings.ToLower(contentType), "application/pdf") || bytes.HasPrefix(body, []byte("%PDF-")) {
		return "", fmt.Errorf("fetch %s: page is a PDF (binary document) — web_fetch cannot extract PDF text. Look for the HTML/abstract page of this document (e.g. an arxiv abs/ URL or the documentation HTML page) and fetch that instead", rawURL)
	}
	if strings.Contains(contentType, "text/html") || looksLikeHTML(body) {
		return htmlToMarkdown(string(body), c.maxOut()), nil
	}
	return truncateText(string(body), c.maxOut()), nil
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

// htmlToMarkdown renders HTML as Markdown, preserving the structure the flat
// text renderer destroyed: headings, lists, code blocks, tables, blockquotes
// and links. A structured page is both cheaper for the model to read and keeps
// links/citations intact for research.
func htmlToMarkdown(source string, maxOut int) string {
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return "(could not parse page)"
	}
	md := &mdBuilder{b: &strings.Builder{}}
	md.walk(doc)
	out := collapseBlank(strings.TrimSpace(md.b.String()))
	return truncateText(out, maxOut)
}

// mdBuilder is a stateful HTML→Markdown walker.
type mdBuilder struct {
	b         *strings.Builder
	listDepth int
	inPre     bool
	inTable   bool
}

// nl writes a single newline unless the buffer already ends in one.
func (m *mdBuilder) nl() {
	s := m.b.String()
	if s == "" {
		return
	}
	if !strings.HasSuffix(s, "\n") {
		m.b.WriteByte('\n')
	}
}

// blank writes a blank line (two newlines) unless already present.
func (m *mdBuilder) blank() {
	m.nl()
	m.nl()
}

// walk renders one node (and its children) into the buffer.
func (m *mdBuilder) walk(n *html.Node) {
	if n.Type == html.ElementNode && skipTags[n.Data] {
		return
	}
	if n.Type == html.TextNode {
		if m.inPre {
			m.b.WriteString(n.Data)
			return
		}
		if s := strings.TrimSpace(n.Data); s != "" {
			m.b.WriteString(s)
			m.b.WriteByte(' ')
		}
		return
	}
	if n.Type != html.ElementNode {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			m.walk(c)
		}
		return
	}
	switch n.Data {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level, _ := strconv.Atoi(n.Data[1:])
		m.nl()
		m.b.WriteString(strings.Repeat("#", level) + " ")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			m.walk(c)
		}
		m.blank()
	case "p":
		m.nl()
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			m.walk(c)
		}
		m.blank()
	case "br":
		m.b.WriteString("  \n")
	case "pre":
		m.nl()
		m.b.WriteString("```\n")
		m.inPre = true
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			m.walk(c)
		}
		m.inPre = false
		m.b.WriteString("\n```\n")
		m.blank()
	case "code":
		if m.inPre {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				m.walk(c)
			}
			return
		}
		m.b.WriteByte('`')
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			m.walk(c)
		}
		m.b.WriteByte('`')
	case "ul", "ol":
		m.nl()
		m.listDepth++
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			m.walk(c)
		}
		m.listDepth--
		m.nl()
	case "li":
		m.nl()
		marker := "- "
		if n.Parent != nil && n.Parent.Data == "ol" {
			idx := 1
			for s := n.Parent.FirstChild; s != nil && s != n; s = s.NextSibling {
				if s.Type == html.ElementNode && s.Data == "li" {
					idx++
				}
			}
			marker = strconv.Itoa(idx) + ". "
		}
		if m.listDepth > 0 {
			m.b.WriteString(strings.Repeat("  ", m.listDepth-1))
		}
		m.b.WriteString(marker)
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			m.walk(c)
		}
		m.nl()
	case "a":
		href := attr(n, "href")
		text := strings.TrimSpace(m.inline(n))
		switch {
		case href != "" && text != "":
			m.b.WriteString("[" + text + "](" + href + ") ")
		case href != "":
			m.b.WriteString(href + " ")
		default:
			m.b.WriteString(text + " ")
		}
	case "strong", "b":
		if inner := strings.TrimSpace(m.inline(n)); inner != "" {
			m.b.WriteString("**" + inner + "**")
		}
	case "em", "i":
		if inner := strings.TrimSpace(m.inline(n)); inner != "" {
			m.b.WriteString("*" + inner + "*")
		}
	case "blockquote":
		m.nl()
		m.b.WriteString("> ")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			m.walk(c)
		}
		m.blank()
	case "hr":
		m.nl()
		m.b.WriteString("---\n")
		m.nl()
	case "img":
		alt := attr(n, "alt")
		src := attr(n, "src")
		switch {
		case src != "" && alt != "":
			m.b.WriteString("![" + alt + "](" + src + ") ")
		case src != "":
			m.b.WriteString("(image: " + src + ") ")
		default:
			m.b.WriteString(alt + " ")
		}
	case "table":
		m.walkTable(n)
	default:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			m.walk(c)
		}
	}
}

// inline renders a node's children into a fresh string (for inline elements
// like links and emphasis), sharing the list-depth context.
func (m *mdBuilder) inline(n *html.Node) string {
	sub := &mdBuilder{b: &strings.Builder{}, listDepth: m.listDepth, inPre: m.inPre}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sub.walk(c)
	}
	return sub.b.String()
}

// walkTable renders an HTML table as Markdown: a header row, a separator, then
// the body rows. Cells are rendered inline (emphasis, links preserved).
func (m *mdBuilder) walkTable(n *html.Node) {
	m.nl()
	var rows [][]string
	var walkRows func(*html.Node)
	walkRows = func(container *html.Node) {
		for tr := container.FirstChild; tr != nil; tr = tr.NextSibling {
			if tr.Type != html.ElementNode {
				continue
			}
			// html.Parse wraps <tr> in <tbody>/<thead>/<tfoot>.
			if tr.Data == "tbody" || tr.Data == "thead" || tr.Data == "tfoot" {
				walkRows(tr)
				continue
			}
			if tr.Data != "tr" {
				continue
			}
			var cells []string
			for td := tr.FirstChild; td != nil; td = td.NextSibling {
				if td.Type != html.ElementNode || (td.Data != "td" && td.Data != "th") {
					continue
				}
				cells = append(cells, strings.TrimSpace(m.inline(td)))
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
		}
	}
	walkRows(n)
	if len(rows) == 0 {
		return
	}
	cols := len(rows[0])
	row := func(r []string) string {
		if len(r) < cols {
			padded := make([]string, cols)
			copy(padded, r)
			r = padded
		}
		return "| " + strings.Join(r, " | ") + " |"
	}
	m.b.WriteString(row(rows[0]) + "\n")
	m.b.WriteString("|" + strings.Repeat("---|", cols) + "\n")
	for _, r := range rows[1:] {
		m.b.WriteString(row(r) + "\n")
	}
	m.blank()
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
