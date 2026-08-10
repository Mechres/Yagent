package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// fakeDDG serves a minimal html.duckduckgo.com results page.
func fakeDDG(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/html/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>
<div class="result results_links results_links_deep web-result">
  <h2 class="result__title">
    <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Frocmsetup&amp;rut=abc">llama.cpp ROCm setup</a>
  </h2>
  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=...">Set up ROCm for llama.cpp on gfx1031.</a>
</div>
<div class="result">
  <a class="result__a" href="https://direct.example/page">Direct link</a>
  <a class="result__snippet">No redirect here.</a>
</div>
</body></html>`))
	}))
}

func TestDuckDuckGoSearch(t *testing.T) {
	ts := fakeDDG(t)
	defer ts.Close()
	d := &DuckDuckGo{http: ts.Client(), endpoint: ts.URL + "/html/?q="}
	res, err := d.Search(context.Background(), "rocmsetup", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	if res[0].Title != "llama.cpp ROCm setup" || res[0].URL != "https://example.com/rocmsetup" {
		t.Errorf("res[0] = %+v", res[0])
	}
	if !strings.Contains(res[0].Snippet, "gfx1031") {
		t.Errorf("snippet = %q", res[0].Snippet)
	}
	// k caps
	res2, _ := d.Search(context.Background(), "rocmsetup", 1)
	if len(res2) != 1 {
		t.Errorf("k=1 gave %d results", len(res2))
	}
}

func TestParseDDG(t *testing.T) {
	src := `<html><body>
<div class="result">
  <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Frocmsetup&amp;rut=abc">Title One</a>
  <a class="result__snippet">Snippet one.</a>
</div>
<div class="result">
  <a class="result__a" href="https://direct.example/x">Title Two</a>
  <a class="result__snippet">Snippet two.</a>
</div>
</body></html>`
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	res := parseDDG(doc)
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	if res[0].Title != "Title One" || res[0].URL != "https://example.com/rocmsetup" {
		t.Errorf("res[0] = %+v", res[0])
	}
	if res[0].Snippet != "Snippet one." {
		t.Errorf("snippet = %q", res[0].Snippet)
	}
	if res[1].URL != "https://direct.example/x" {
		t.Errorf("res[1].URL = %q", res[1].URL)
	}
}

func TestSearXNGSearch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("expected format=json, got %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "ROCm docs", "url": "https://example.com/rocm", "content": "How to set up ROCm."},
				{"title": "Second", "url": "https://example.com/2", "content": "Another result."},
			},
		})
	}))
	defer ts.Close()
	s := &SearXNG{baseURL: ts.URL, http: ts.Client()}
	res, err := s.Search(context.Background(), "rocmsetup", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 || res[0].Title != "ROCm docs" || res[0].Snippet != "How to set up ROCm." {
		t.Errorf("results = %+v", res)
	}
}

func TestNewConfig(t *testing.T) {
	c, err := New(DefaultConfig())
	if err != nil || c.ProviderName() != "duckduckgo" {
		t.Fatalf("default = %v / %v", c, err)
	}
	if _, err := New(Config{Provider: "searxng"}); err == nil {
		t.Error("searxng without URL should fail")
	}
	c, err = New(Config{Provider: "searxng", SearxngURL: "http://localhost:8080"})
	if err != nil || c.ProviderName() != "searxng" {
		t.Fatalf("searxng = %v / %v", c, err)
	}
	if _, err := New(Config{Provider: "brave"}); err == nil {
		t.Error("unknown provider should fail")
	}
}

func TestFetchStripsChrome(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><script>var x=1;</script><style>body{}</style></head>
<body>
<nav>Skip me</nav>
<header>Also skip</header>
<main>
<h1>Page title</h1>
<p>Useful <a href="https://example.com/target">link text</a> content.</p>
</main>
<footer>Footer noise</footer>
</body></html>`))
	}))
	defer ts.Close()

	c := &Client{http: ts.Client(), fetchTimeout: 0}
	got, err := c.Fetch(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(got, "Page title") || !strings.Contains(got, "Useful") {
		t.Errorf("missing content: %q", got)
	}
	if strings.Contains(got, "Skip me") || strings.Contains(got, "var x=1") || strings.Contains(got, "Footer noise") {
		t.Errorf("chrome not stripped: %q", got)
	}
	if !strings.Contains(got, "https://example.com/target") {
		t.Errorf("link URL not preserved: %q", got)
	}
}

func TestFetchTruncates(t *testing.T) {
	body := "<p>" + strings.Repeat("lorem ipsum dolor ", 4000) + "</p>"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	c := &Client{http: ts.Client(), fetchTimeout: 0}
	got, err := c.Fetch(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) > maxFetchText+40 {
		t.Errorf("output %d bytes exceeds cap %d", len(got), maxFetchText)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncation marker missing")
	}
}

func TestFetchHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer ts.Close()
	c := &Client{http: ts.Client(), fetchTimeout: 0}
	if _, err := c.Fetch(context.Background(), ts.URL); err == nil {
		t.Error("404 should error")
	}
}

func TestMojeekSearch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><ul class="results-standard">
<li><a class="ob" href="https://example.com/one">First result</a><p class="s">Snippet for the first result.</p></li>
<li><a class="ob" href="https://example.com/two">Second result</a><p class="s">Snippet two.</p></li>
</ul></body></html>`))
	}))
	defer ts.Close()
	m := &Mojeek{http: ts.Client(), endpoint: ts.URL + "/search?q="}
	res, err := m.Search(context.Background(), "go programming", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	if res[0].Title != "First result" || res[0].URL != "https://example.com/one" || !strings.Contains(res[0].Snippet, "first result") {
		t.Errorf("res[0] = %+v", res[0])
	}
	// k caps
	res2, _ := m.Search(context.Background(), "x", 1)
	if len(res2) != 1 {
		t.Errorf("k=1 gave %d results", len(res2))
	}
}

func TestNewMojeekConfig(t *testing.T) {
	c, err := New(Config{Provider: "mojeek"})
	if err != nil || c.ProviderName() != "mojeek" {
		t.Fatalf("mojeek = %v / %v", c, err)
	}
}
