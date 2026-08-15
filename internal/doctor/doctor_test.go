package doctor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/config"
)

// fakeServer answers /v1/models, /v1/embeddings and /v1/chat/completions.
// When requireKey is set, a missing Authorization header is a 401 (like a
// cloud OpenAI-compatible endpoint).
func fakeServer(t *testing.T, embedOK, requireKey bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireKey && r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, `{"error":"authorization header missing"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]any{{"id": "test-model"}},
			})
		case "/v1/embeddings":
			if !embedOK {
				http.Error(w, `{"error":{"code":501,"message":"not support embeddings"}}`, http.StatusNotImplemented)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"embedding": make([]float32, 384)}},
			})
		case "/v1/chat/completions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": "pong"}}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestDoctorHealthy(t *testing.T) {
	ts := fakeServer(t, true, false)
	defer ts.Close()
	cfg := &config.Config{ServerURL: ts.URL, Model: "test-model", EmbeddingModel: "test-embed", DataDir: t.TempDir()}
	rep := Run(cfg)
	if rep.Failures != 0 {
		for _, c := range rep.Checks {
			t.Logf("%s %s: %s", c.Status, c.Name, c.Detail)
		}
		t.Fatalf("failures = %d, want 0", rep.Failures)
	}
}

// TestDoctorAPIKeyAuth: a cloud endpoint (NVIDIA NIM style) that requires
// Authorization: Bearer must pass doctor when cfg.APIKey is set — the probes
// must send the key just like the agent loop does.
func TestDoctorAPIKeyAuth(t *testing.T) {
	ts := fakeServer(t, true, true)
	defer ts.Close()
	cfg := &config.Config{ServerURL: ts.URL + "/v1", Model: "test-model", EmbeddingModel: "test-embed", DataDir: t.TempDir(), APIKey: "test-key"}
	rep := Run(cfg)
	if rep.Failures != 0 {
		for _, c := range rep.Checks {
			if c.Status == StatusFail {
				t.Errorf("FAIL %s: %s", c.Name, c.Detail)
			}
		}
		t.Fatalf("failures = %d, want 0 (doctor must send the API key)", rep.Failures)
	}
}

// TestDoctorV1SuffixedBase covers the NVIDIA-NIM-style config where the
// documented base URL already ends in /v1 (https://…/v1): the doctor must
// strip it before appending /v1/models, or it hits /v1/v1/models -> 404.
func TestDoctorV1SuffixedBase(t *testing.T) {
	ts := fakeServer(t, true, false)
	defer ts.Close()
	cfg := &config.Config{ServerURL: ts.URL + "/v1", Model: "test-model", EmbeddingModel: "test-embed", DataDir: t.TempDir()}
	rep := Run(cfg)
	if rep.Failures != 0 {
		for _, c := range rep.Checks {
			if c.Status == StatusFail {
				t.Errorf("FAIL %s: %s", c.Name, c.Detail)
			}
		}
		t.Fatalf("failures = %d, want 0 (server_url with /v1 suffix)", rep.Failures)
	}
	// unit-level: baseURL strips the suffix
	if got := baseURL("https://integrate.api.nvidia.com/v1"); got != "https://integrate.api.nvidia.com" {
		t.Errorf("baseURL(/v1) = %q", got)
	}
	if got := baseURL("http://localhost:8089/"); got != "http://localhost:8089" {
		t.Errorf("baseURL(trailing slash) = %q", got)
	}
}

func TestDoctorServerDown(t *testing.T) {
	cfg := &config.Config{ServerURL: "http://127.0.0.1:1", Model: "m", DataDir: t.TempDir()}
	rep := Run(cfg)
	failed := false
	for _, c := range rep.Checks {
		if c.Status == StatusFail && c.Name == "server" {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("server-down not diagnosed: %+v", rep.Checks)
	}
}

func TestDoctorModelMissing(t *testing.T) {
	ts := fakeServer(t, true, false)
	defer ts.Close()
	cfg := &config.Config{ServerURL: ts.URL, Model: "nope-model", EmbeddingModel: "e", DataDir: t.TempDir()}
	rep := Run(cfg)
	warned := false
	for _, c := range rep.Checks {
		if c.Name == "model" && c.Status == StatusWarn {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("missing model not warned: %+v", rep.Checks)
	}
}

func TestDoctorEmbeddingsWarn(t *testing.T) {
	ts := fakeServer(t, false, false)
	defer ts.Close()
	cfg := &config.Config{ServerURL: ts.URL, Model: "test-model", EmbeddingModel: "e", DataDir: t.TempDir()}
	rep := Run(cfg)
	for _, c := range rep.Checks {
		if c.Name == "embeddings" && c.Status == StatusWarn {
			return // expected
		}
	}
	t.Fatalf("embeddings 501 not warned: %+v", rep.Checks)
}

// TestDoctorEmbeddingServerURL: the embeddings probe must hit the dedicated
// embedding server when one is configured, not the chat server.
func TestDoctorEmbeddingServerURL(t *testing.T) {
	chat := fakeServer(t, false, false) // chat server: no embeddings
	embed := fakeServer(t, true, false) // embedding server: works
	defer chat.Close()
	defer embed.Close()
	cfg := &config.Config{
		ServerURL:          chat.URL,
		EmbeddingServerURL: embed.URL,
		Model:              "test-model",
		EmbeddingModel:     "test-embed",
		DataDir:            t.TempDir(),
	}
	rep := Run(cfg)
	passed := false
	for _, c := range rep.Checks {
		if c.Name == "embeddings" && c.Status == StatusPass {
			passed = true
		}
	}
	if !passed {
		t.Fatalf("embeddings should pass against the dedicated embedding server: %+v", rep.Checks)
	}
}

// TestDoctorWebSearchConfig: a misconfigured web_search provider (searxng
// without a URL, langsearch without a key, unknown provider) must be a doctor
// FAIL, because `yagent chat` would refuse to start.
func TestDoctorWebSearchConfig(t *testing.T) {
	ts := fakeServer(t, true, false)
	defer ts.Close()
	for _, tc := range []struct {
		provider string
		searxng  string
		lang     string
	}{
		{"searxng", "", ""},
		{"langsearch", "", ""},
		{"bogus", "", ""},
	} {
		cfg := &config.Config{ServerURL: ts.URL, Model: "test-model", EmbeddingModel: "test-embed", DataDir: t.TempDir()}
		cfg.Web.Provider = tc.provider
		cfg.Web.SearxngURL = tc.searxng
		cfg.Web.LangSearchKey = tc.lang
		rep := Run(cfg)
		failed := false
		for _, c := range rep.Checks {
			if c.Name == "web_search" && c.Status == StatusFail {
				failed = true
			}
		}
		if !failed {
			t.Errorf("provider %q should be a doctor FAIL: %+v", tc.provider, rep.Checks)
		}
	}
	// valid providers pass
	cfg := &config.Config{ServerURL: ts.URL, Model: "test-model", EmbeddingModel: "test-embed", DataDir: t.TempDir()}
	cfg.Web.Provider = "searxng"
	cfg.Web.SearxngURL = "http://searx:8080"
	rep := Run(cfg)
	for _, c := range rep.Checks {
		if c.Name == "web_search" && c.Status == StatusFail {
			t.Errorf("searxng with URL should pass, got: %+v", rep.Checks)
		}
	}
}

func TestDoctorBadConfig(t *testing.T) {
	cfg := &config.Config{ServerURL: "://bad", Model: "", DataDir: t.TempDir()}
	rep := Run(cfg)
	gotFail := false
	for _, c := range rep.Checks {
		if c.Status == StatusFail {
			gotFail = true
		}
	}
	if !gotFail {
		t.Fatalf("bad config not diagnosed: %+v", rep.Checks)
	}
}

func TestDoctorProjectToolchain(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	// no project markers -> no toolchain check (nothing to assert on)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	cfg := &config.Config{ServerURL: "http://127.0.0.1:1", Model: "m", DataDir: t.TempDir()}
	rep := Run(cfg)
	found := false
	for _, c := range rep.Checks {
		if c.Name == "toolchain" {
			found = true
			if c.Status != StatusPass || !strings.Contains(c.Detail, "go") {
				t.Errorf("toolchain = %s %s, want PASS with go", c.Status, c.Detail)
			}
		}
	}
	if !found {
		t.Errorf("no toolchain check for a go.mod project: %+v", rep.Checks)
	}
}

func TestAddServerPerfLargeContextWarns(t *testing.T) {
	// a server reporting f16 KV with a large configured context should warn
	// about KV spill risk (agy #5).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_slots": 1,
				"default_generation_settings": map[string]any{
					"n_ctx":        32768,
					"cache_type_k": "f16",
					"cache_type_v": "f16",
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	var rep Report
	rep.addServerPerf(&config.Config{ServerURL: ts.URL, ContextWindow: 32768})
	found := false
	for _, c := range rep.Checks {
		if c.Name == "server perf" {
			found = true
			if c.Status != StatusWarn || !strings.Contains(c.Detail, "q8_0") {
				t.Errorf("server perf = %s %s, want WARN with q8_0 hint", c.Status, c.Detail)
			}
		}
	}
	if !found {
		t.Errorf("no server perf check produced: %+v", rep.Checks)
	}
}
