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
func fakeServer(t *testing.T, embedOK bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	ts := fakeServer(t, true)
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
	ts := fakeServer(t, true)
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
	ts := fakeServer(t, false)
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
