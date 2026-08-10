package doctor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"yagent/internal/config"
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
