package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yagent/internal/web"
)

// fakeWebServer serves SearXNG JSON search results and a target article.
func fakeWebServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{
					{"title": "ROCm setup guide", "url": "https://example.com/rocm", "content": "How to run llama.cpp on ROCm."},
					{"title": "Second", "url": "https://example.com/2", "content": "More content."},
				},
			})
		case "/rocm":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><h1>ROCm guide</h1><p>Use HSA_OVERRIDE_GFX_VERSION=10.3.0 for gfx1031.</p></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestWebTools(t *testing.T) {
	ts := fakeWebServer(t)
	defer ts.Close()

	client, err := web.New(web.Config{Provider: "searxng", SearxngURL: ts.URL})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	reg := NewRegistry(t.TempDir(), Options{Web: client})

	if got := execTool(t, reg, "web_search", map[string]any{"query": "rocmsetup"}); !strings.Contains(got, "example.com/rocm") || !strings.Contains(got, "ROCm setup guide") {
		t.Errorf("web_search = %q", got)
	}
	if got := execTool(t, reg, "web_search", map[string]any{"query": ""}); !strings.Contains(got, "validation-error") {
		t.Errorf("web_search empty = %q", got)
	}
	if got := execTool(t, reg, "web_fetch", map[string]any{"url": ts.URL + "/rocm"}); !strings.Contains(got, "gfx1031") {
		t.Errorf("web_fetch = %q", got)
	}
	if got := execTool(t, reg, "web_fetch", map[string]any{"url": ""}); !strings.Contains(got, "validation-error") {
		t.Errorf("web_fetch empty = %q", got)
	}
	// no results
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer empty.Close()
	client2, _ := web.New(web.Config{Provider: "searxng", SearxngURL: empty.URL})
	reg2 := NewRegistry(t.TempDir(), Options{Web: client2})
	if got := execTool(t, reg2, "web_search", map[string]any{"query": "x"}); !strings.Contains(got, "no results found") {
		t.Errorf("web_search empty result = %q", got)
	}
}
