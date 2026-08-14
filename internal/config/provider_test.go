package config

import (
	"os"
	"path/filepath"
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
	if err := SetProvider(path, p, "deepseek-chat", "sk-test"); err != nil {
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
	if err := SetProvider(path2, Provider{Name: "Local", BaseURL: "http://localhost:8089"}, "Qwen", ""); err != nil {
		t.Fatal(err)
	}
	data2, _ := os.ReadFile(path2)
	if contains(string(data2), "api_key") {
		t.Errorf("empty key should not persist an api_key:\n%s", data2)
	}
}

func TestSelectProvider(t *testing.T) {
	cfg := &Config{}
	cfg.SelectProvider(Providers[2], "deepseek-chat")
	if cfg.ServerURL != Providers[2].BaseURL || cfg.Model != "deepseek-chat" {
		t.Errorf("SelectProvider = %q/%q", cfg.ServerURL, cfg.Model)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || stringContains(haystack, needle)
}

func stringContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
