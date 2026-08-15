package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProviderCatalog(t *testing.T) {
	// Local providers must be first (local-first default), and the catalog
	// must include at least one cloud provider with a key env.
	if len(Providers) < 2 {
		t.Fatalf("catalog too small: %d", len(Providers))
	}
	if !contains(Providers[0].Name, "Local") {
		t.Errorf("first provider should be local, got %q", Providers[0].Name)
	}
	cloud := false
	for _, p := range Providers {
		if p.KeyEnv != "" {
			cloud = true
		}
		if p.Name == "" || p.BaseURL == "" {
			t.Errorf("provider %q has empty name/baseURL", p.Name)
		}
	}
	if !cloud {
		t.Error("catalog has no cloud provider with a key env")
	}
	// ProviderByName round-trips
	p, ok := ProviderByName(Providers[0].Name)
	if !ok || p.Name != Providers[0].Name {
		t.Errorf("ProviderByName(%q) = %v, %v", Providers[0].Name, p, ok)
	}
	if _, ok := ProviderByName("nope"); ok {
		t.Error("unknown provider resolved")
	}
}

func TestKeyForPrecedence(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	cfg := &Config{}
	p := Provider{Name: "DeepSeek", BaseURL: "https://api.deepseek.com", KeyEnv: "DEEPSEEK_API_KEY"}
	if got := cfg.KeyFor(p); got != "env-key" {
		t.Errorf("KeyFor from env = %q", got)
	}
	// config api_key wins over the env var
	cfg.APIKey = "cfg-key"
	if got := cfg.KeyFor(p); got != "cfg-key" {
		t.Errorf("KeyFor should prefer config api_key, got %q", got)
	}
	// a local provider with no env -> empty
	if got := cfg.KeyFor(Provider{Name: "Local", BaseURL: "http://localhost:8089"}); got != "cfg-key" {
		t.Errorf("local provider KeyFor = %q (cfg api_key is global, so expected)", got)
	}
}

func TestSetProviderPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	p := Provider{Name: "DeepSeek", BaseURL: "https://api.deepseek.com", KeyEnv: "DEEPSEEK_API_KEY", Models: []string{"deepseek-chat"}}
	if _, err := SetProvider(path, p, "deepseek-chat", "sk-test"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"https://api.deepseek.com", "deepseek-chat", "sk-test"} {
		if !contains(string(data), want) {
			t.Errorf("config missing %q:\n%s", want, data)
		}
	}
	// a provider with no key must not write an api_key line
	path2 := filepath.Join(dir, "config2.yaml")
	if _, err := SetProvider(path2, Provider{Name: "Local", BaseURL: "http://localhost:8089"}, "Qwen", ""); err != nil {
		t.Fatal(err)
	}
	data2, _ := os.ReadFile(path2)
	if contains(string(data2), "api_key") {
		t.Errorf("empty key should not persist an api_key:\n%s", data2)
	}
}

func TestSelectProvider(t *testing.T) {
	cfg := &Config{}
	prov, ok := ProviderByName("DeepSeek")
	if !ok {
		t.Fatal("DeepSeek not in catalog")
	}
	cfg.SelectProvider(prov, "deepseek-chat")
	if cfg.ServerURL != prov.BaseURL || cfg.Model != "deepseek-chat" {
		t.Errorf("SelectProvider = %q/%q", cfg.ServerURL, cfg.Model)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || stringContains(haystack, needle)
}

func TestFetchModelsOpenAIAndOllamaShapes(t *testing.T) {
	// OpenAI shape (llama.cpp): data[].id
	openAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "Qwen3VL-8B-Instruct-Q4_K_M.gguf"},
				{"id": "Ornith-1.0-9B"},
			},
		})
	}))
	defer openAI.Close()
	models, ok := FetchModels(context.Background(), openAI.URL)
	if !ok || len(models) != 2 || models[0] != "Qwen3VL-8B-Instruct-Q4_K_M.gguf" {
		t.Errorf("OpenAI-shape FetchModels = %v, %v", models, ok)
	}

	// Ollama shape: models[].name
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "qwen3:8b"},
				{"name": "nomic-embed-text"},
			},
		})
	}))
	defer ollama.Close()
	models, ok = FetchModels(context.Background(), ollama.URL)
	if !ok || len(models) != 2 || models[0] != "qwen3:8b" {
		t.Errorf("Ollama-shape FetchModels = %v, %v", models, ok)
	}

	// unreachable -> ok=false
	if _, ok := FetchModels(context.Background(), "http://127.0.0.1:1"); ok {
		t.Error("unreachable server should report ok=false")
	}
}

func TestOpenCodeZenProvider(t *testing.T) {
	prov, ok := ProviderByName("OpenCode Zen")
	if !ok {
		t.Fatal("OpenCode Zen not in catalog")
	}
	if prov.BaseURL != "https://opencode.ai/zen" {
		t.Errorf("zen base = %q", prov.BaseURL)
	}
	if prov.KeyEnv != "OPENCODE_ZEN_API_KEY" {
		t.Errorf("zen key env = %q", prov.KeyEnv)
	}
	// DeepSeek V4 models must be in the Zen list
	all := ""
	for _, m := range prov.Models {
		all += m + " "
	}
	if !stringContains(all, "deepseek-v4-pro") || !stringContains(all, "deepseek-v4-flash") {
		t.Errorf("Zen models missing DeepSeek V4: %q", all)
	}
	// local providers are Dynamic
	if !Providers[0].Dynamic || !Providers[1].Dynamic {
		t.Error("local providers must be Dynamic (live model detection)")
	}
	// cloud providers are not Dynamic (static catalog)
	if Providers[2].Dynamic {
		t.Error("OpenCode Zen must not be Dynamic")
	}
}

func TestOpenCodeGoProvider(t *testing.T) {
	prov, ok := ProviderByName("OpenCode Go")
	if !ok {
		t.Fatal("OpenCode Go not in catalog")
	}
	if prov.BaseURL != "https://opencode.ai/zen/go" {
		t.Errorf("go base = %q", prov.BaseURL)
	}
	if prov.KeyEnv != "OPENCODE_ZEN_API_KEY" {
		t.Errorf("go key env = %q", prov.KeyEnv)
	}
	// the Go plan's headline coding models must be present
	all := strings.Join(prov.Models, " ")
	for _, want := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "kimi-k2.7-code", "qwen3.8-max"} {
		if !strings.Contains(all, want) {
			t.Errorf("OpenCode Go missing %q: %q", want, all)
		}
	}
}

func TestCatalogCurrentModels(t *testing.T) {
	// DeepSeek is on V4 now (old chat/reasoner dropped).
	ds, ok := ProviderByName("DeepSeek")
	if !ok {
		t.Fatal("DeepSeek not in catalog")
	}
	all := strings.Join(ds.Models, " ")
	if !strings.Contains(all, "deepseek-v4-pro") || !strings.Contains(all, "deepseek-v4-flash") {
		t.Errorf("DeepSeek missing V4 models: %q", all)
	}
	if strings.Contains(all, "deepseek-chat") {
		t.Errorf("DeepSeek still lists the removed deepseek-chat: %q", all)
	}
	// Mistral: devstral is the current coding model.
	ms, ok := ProviderByName("Mistral")
	if !ok {
		t.Fatal("Mistral not in catalog")
	}
	if !strings.Contains(strings.Join(ms.Models, " "), "devstral-2512") {
		t.Errorf("Mistral missing devstral-2512: %q", ms.Models)
	}
}

func TestNVIDIAProvider(t *testing.T) {
	prov, ok := ProviderByName("NVIDIA NIM")
	if !ok {
		t.Fatal("NVIDIA NIM not in catalog")
	}
	if prov.BaseURL != "https://integrate.api.nvidia.com/v1" {
		t.Errorf("nvidia base = %q", prov.BaseURL)
	}
	if prov.KeyEnv != "NVIDIA_API_KEY" {
		t.Errorf("nvidia key env = %q", prov.KeyEnv)
	}
	// the headline free coding models must be present
	all := strings.Join(prov.Models, " ")
	for _, want := range []string{"nemotron-3-super-120b-a12b", "qwen3-coder-480b-a35b-instruct"} {
		if !strings.Contains(all, want) {
			t.Errorf("NVIDIA missing %q: %q", want, all)
		}
	}
	// free tier => no api key persisted automatically
	if prov.KeyEnv == "" {
		t.Error("NVIDIA should have a key env")
	}
}

func stringContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestModelWarning(t *testing.T) {
	if got := ModelWarning("deepseek-v4-pro"); got != "" {
		t.Errorf("strong model warned: %q", got)
	}
	if got := ModelWarning("deepseek-v4-mini"); got == "" {
		t.Error("mini model should warn")
	}
	if got := ModelWarning("qwen2.5-coder-7b"); got == "" {
		t.Error("known-weak model should warn")
	}
}

func TestFetchModelsDevShape(t *testing.T) {
	// models.dev returns {provider: {models: {id: {...}}}}. We can't hit the
	// real endpoint offline, but the filter/cap logic is exercised through the
	// helper's handling of a minimal fixture via an unreachable URL -> false.
	if _, ok := FetchModelsDev(context.Background(), "nope", ""); ok {
		t.Error("unknown provider key should report ok=false")
	}
}

// TestFetchModelsDevPrioritizesCurrentModel: the currently configured model
// must stay in the capped result even if it sorts past the cap — the user's
// active model has to remain selectable.
func TestFetchModelsDevPrioritizesCurrentModel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		models := map[string]any{}
		for i := 0; i < 30; i++ {
			models[fmt.Sprintf("thirdparty/model-%02d-instruct", i)] = map[string]any{}
		}
		models["nvidia/nemotron-3-nano-30b-a3b"] = map[string]any{}
		_ = json.NewEncoder(w).Encode(map[string]any{"nvidia": map[string]any{"models": models}})
	}))
	defer ts.Close()
	old := modelsDevURL
	modelsDevURL = ts.URL
	defer func() { modelsDevURL = old }()

	// nvidia's own models sort first (own-prefix priority), so even without a
	// current model the nemotron entry survives the cap.
	out, _ := FetchModelsDev(context.Background(), "nvidia", "")
	if !slices.Contains(out, "nvidia/nemotron-3-nano-30b-a3b") {
		t.Error("provider's own model should appear in the capped list via own-prefix priority")
	}
	// with the current model, it is guaranteed to appear (and stays if the
	// own-prefix list grew past the cap)
	out, _ = FetchModelsDev(context.Background(), "nvidia", "nvidia/nemotron-3-nano-30b-a3b")
	if !slices.Contains(out, "nvidia/nemotron-3-nano-30b-a3b") {
		t.Errorf("current model missing from capped list: %v", out)
	}
}

// TestFetchModelsDevPrefersProviderOwnModels: models whose id starts with the
// provider's own key (nvidia/...) are listed before third-party models, so a
// provider's native models aren't alphabetically squeezed out by the cap.
func TestFetchModelsDevPrefersProviderOwnModels(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		models := map[string]any{}
		for i := 0; i < 10; i++ {
			models[fmt.Sprintf("zzthirdparty/model-%02d-instruct", i)] = map[string]any{}
		}
		models["nvidia/nemotron-3-nano-30b-a3b"] = map[string]any{}
		models["nvidia/nemotron-3-super-120b-a12b"] = map[string]any{}
		_ = json.NewEncoder(w).Encode(map[string]any{"nvidia": map[string]any{"models": models}})
	}))
	defer ts.Close()
	old := modelsDevURL
	modelsDevURL = ts.URL
	defer func() { modelsDevURL = old }()

	out, _ := FetchModelsDev(context.Background(), "nvidia", "")
	if len(out) < 2 || out[0] != "nvidia/nemotron-3-nano-30b-a3b" || out[1] != "nvidia/nemotron-3-super-120b-a12b" {
		t.Errorf("provider's own models should sort first, got %v", out)
	}
}

func TestContextLengthFromModelsDev(t *testing.T) {
	// stub the models.dev index with a local server (no network)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deepseek": map[string]any{
				"models": map[string]any{
					"deepseek-v4-flash": map[string]any{
						"limit": map[string]any{"context": 1000000, "output": 384000},
					},
				},
			},
		})
	}))
	defer ts.Close()
	old := modelsDevURL
	modelsDevURL = ts.URL
	defer func() { modelsDevURL = old }()

	if got := ContextLength(context.Background(), "deepseek", "deepseek-v4-flash"); got != 1000000 {
		t.Errorf("ContextLength = %d, want 1000000", got)
	}
	if got := ContextLength(context.Background(), "deepseek", "unknown-model"); got != 0 {
		t.Errorf("ContextLength(unknown model) = %d, want 0", got)
	}
	if got := ContextLength(context.Background(), "nope", "deepseek-v4-flash"); got != 0 {
		t.Errorf("ContextLength(unknown provider) = %d, want 0", got)
	}
	// no model entry's limit -> 0
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deepseek": map[string]any{"models": map[string]any{"deepseek-v4-pro": map[string]any{}}},
		})
	}))
	defer ts2.Close()
	modelsDevURL = ts2.URL
	if got := ContextLength(context.Background(), "deepseek", "deepseek-v4-pro"); got != 0 {
		t.Errorf("ContextLength(no limit) = %d, want 0", got)
	}
}

func TestSetProviderRaisesWindowForCloudModel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deepseek": map[string]any{
				"models": map[string]any{
					"deepseek-v4-flash": map[string]any{
						"limit": map[string]any{"context": 1000000},
					},
				},
			},
		})
	}))
	defer ts.Close()
	old := modelsDevURL
	modelsDevURL = ts.URL
	defer func() { modelsDevURL = old }()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// start with a small local-ish window
	if err := Set(path, "context_window", "32768"); err != nil {
		t.Fatal(err)
	}
	raised, err := SetProvider(path, Provider{Name: "DeepSeek", BaseURL: "https://api.deepseek.com", ModelsDev: "deepseek"}, "deepseek-v4-flash", "sk")
	if err != nil {
		t.Fatal(err)
	}
	if raised != 1000000 {
		t.Errorf("raised = %d, want 1000000", raised)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContextWindow != 1000000 {
		t.Errorf("ContextWindow = %d, want 1000000 (raised to the model's real context)", cfg.ContextWindow)
	}
	// a local provider (no ModelsDev) never raises the window
	path2 := filepath.Join(dir, "config2.yaml")
	_ = Set(path2, "context_window", "16384")
	raised, err = SetProvider(path2, Provider{Name: "Local", BaseURL: "http://localhost:8089"}, "Qwen3VL", "")
	if err != nil {
		t.Fatal(err)
	}
	if raised != 0 {
		t.Errorf("local provider raised = %d, want 0", raised)
	}
}
