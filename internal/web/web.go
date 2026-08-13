// Package web implements M5: web search (DuckDuckGo HTML by default, SearXNG
// optional) and web fetch (GET → HTML→text). Local-first means the only
// external traffic is these explicit search/fetch calls.
package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
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

// Client is the configured search + fetch entry point. Search runs the
// providers in order, falling back to the next on error or empty results.
type Client struct {
	providers    []Provider
	http         *http.Client
	fetchTimeout time.Duration

	// cache memoizes search results and fetched pages within one client
	// lifetime (a session), keyed by query / URL, so a repeated identical web
	// query doesn't re-hit the (slow, rate-limited) network. Bounded + TTL'd.
	mu    sync.Mutex
	cache map[string]cacheEntry
	hits  int // total cache hits (for tool result markers / diagnostics)
}

type cacheEntry struct {
	value     string
	results   []Result
	expiresAt time.Time
}

// cacheTTL bounds how long a web result is reused before a re-fetch.
const cacheTTL = 10 * time.Minute

// maxCacheEntries bounds the web cache (LRU-ish: evict the oldest on overflow).
const maxCacheEntries = 64

// cached returns the entry for key if present and unexpired.
func (c *Client) cached(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok {
		return cacheEntry{}, false
	}
	if time.Now().After(e.expiresAt) {
		delete(c.cache, key)
		return cacheEntry{}, false
	}
	c.hits++
	return e, true
}

// CacheHits returns how many web results were served from cache this session.
func (c *Client) CacheHits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

// store caches value under key with a TTL, evicting an arbitrary entry when the
// cache is full (map iteration is unordered but bounded).
func (c *Client) store(key string, e cacheEntry) {
	e.expiresAt = time.Now().Add(cacheTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = map[string]cacheEntry{}
	}
	if len(c.cache) >= maxCacheEntries {
		for k := range c.cache {
			delete(c.cache, k)
			break
		}
	}
	c.cache[key] = e
}

// ClearCache drops every cached web result (session end / explicit reset).
func (c *Client) ClearCache() {
	c.mu.Lock()
	c.cache = map[string]cacheEntry{}
	c.mu.Unlock()
}

// Config selects the backend. Provider is "duckduckgo" (default), "mojeek" or
// "searxng"; SearxngURL is required for the searxng provider.
type Config struct {
	Provider   string
	SearxngURL string
}

// DefaultConfig uses DuckDuckGo.
func DefaultConfig() Config { return Config{Provider: "duckduckgo"} }

// New builds a web client for cfg. duckduckgo and mojeek need no setup; searxng
// requires SearxngURL. Fallback order: the configured provider first, then the
// no-key third-party alternatives. A searxng primary never falls back to third
// parties (privacy).
func New(cfg Config) (*Client, error) {
	http := defaultHTTP()
	var primary Provider
	switch strings.ToLower(cfg.Provider) {
	case "", "duckduckgo":
		primary = &DuckDuckGo{http: http}
	case "mojeek":
		primary = &Mojeek{http: http}
	case "searxng":
		if cfg.SearxngURL == "" {
			return nil, fmt.Errorf("web_search.provider searxng requires web_search.searxng_url")
		}
		return newClient([]Provider{&SearXNG{baseURL: strings.TrimRight(cfg.SearxngURL, "/"), http: http}}), nil
	default:
		return nil, fmt.Errorf("unknown web_search.provider %q (duckduckgo | mojeek | searxng)", cfg.Provider)
	}
	// DDG/Mojeek primaries fall back to the other + SearXNG when configured.
	fallbacks := []Provider{&DuckDuckGo{http: http}, &Mojeek{http: http}}
	if cfg.SearxngURL != "" {
		fallbacks = append(fallbacks, &SearXNG{baseURL: strings.TrimRight(cfg.SearxngURL, "/"), http: http})
	}
	ordered := []Provider{primary}
	for _, f := range fallbacks {
		if f.Name() != primary.Name() {
			ordered = append(ordered, f)
		}
	}
	return newClient(ordered), nil
}

// newClient builds a client around explicit providers (tests + New).
func newClient(providers []Provider) *Client {
	return &Client{
		providers:    providers,
		http:         defaultHTTP(),
		fetchTimeout: 15 * time.Second,
	}
}

// ProviderName reports the primary (first) backend.
func (c *Client) ProviderName() string {
	if len(c.providers) == 0 {
		return ""
	}
	return c.providers[0].Name()
}

// Search runs the providers in order, returning the first non-empty result
// set; a provider that errors or returns nothing is skipped. If every
// provider searches cleanly but finds nothing, it returns (nil, nil).
// Identical queries within the cache TTL are served from cache (no network).
func (c *Client) Search(ctx context.Context, query string, k int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is required")
	}
	key := "search:" + query + fmt.Sprintf(":%d", k)
	if e, ok := c.cached(key); ok {
		slog.Debug("web search cache hit", "query", query)
		return e.results, nil
	}
	var lastErr error
	for i, p := range c.providers {
		results, err := p.Search(ctx, query, k)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", p.Name(), err)
			slog.Debug("web search provider failed, trying next", "provider", p.Name(), "error", err)
			continue
		}
		if len(results) > 0 {
			if i > 0 {
				slog.Info("web search fell back", "provider", p.Name())
			}
			c.store(key, cacheEntry{results: results})
			return results, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
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
