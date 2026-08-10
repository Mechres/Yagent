// Package web implements M5: web search (DuckDuckGo HTML by default, SearXNG
// optional) and web fetch (GET → HTML→text). Local-first means the only
// external traffic is these explicit search/fetch calls.
package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Result is one web search hit.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Provider searches the web. Implementations: DuckDuckGo (HTML scrape) and
// SearXNG (JSON). Adding a provider (Mojeek, Brave, ...) is a small struct.
type Provider interface {
	Name() string
	Search(ctx context.Context, query string, k int) ([]Result, error)
}

// Client is the configured search + fetch entry point.
type Client struct {
	provider Provider
	http     *http.Client
	// fetchTimeout bounds a web_fetch GET.
	fetchTimeout time.Duration
}

// Config selects the backend. Provider is "duckduckgo" (default) or "searxng";
// SearxngURL is required for the searxng provider.
type Config struct {
	Provider   string
	SearxngURL string
}

// DefaultConfig uses DuckDuckGo.
func DefaultConfig() Config { return Config{Provider: "duckduckgo"} }

// New builds a web client for cfg. duckduckgo needs no setup; searxng requires
// SearxngURL.
func New(cfg Config) (*Client, error) {
	var provider Provider
	switch strings.ToLower(cfg.Provider) {
	case "", "duckduckgo":
		provider = &DuckDuckGo{http: defaultHTTP()}
	case "mojeek":
		provider = &Mojeek{http: defaultHTTP()}
	case "searxng":
		if cfg.SearxngURL == "" {
			return nil, fmt.Errorf("web_search.provider searxng requires web_search.searxng_url")
		}
		provider = &SearXNG{baseURL: strings.TrimRight(cfg.SearxngURL, "/"), http: defaultHTTP()}
	default:
		return nil, fmt.Errorf("unknown web_search.provider %q (duckduckgo | mojeek | searxng)", cfg.Provider)
	}
	return &Client{
		provider: provider, http: defaultHTTP(), fetchTimeout: 15 * time.Second,
	}, nil
}

// ProviderName reports the active backend.
func (c *Client) ProviderName() string { return c.provider.Name() }

// Search runs the configured backend.
func (c *Client) Search(ctx context.Context, query string, k int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is required")
	}
	return c.provider.Search(ctx, query, k)
}

// defaultHTTP is a shared client with a sane timeout and a redirect limit.
func defaultHTTP() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			return nil
		},
	}
}
