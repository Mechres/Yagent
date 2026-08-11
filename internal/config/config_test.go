package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	// No env overrides, no file at the default path. Use a default path in a
	// temp HOME so the real user config is never read.
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarServerURL, "")
	t.Setenv(EnvVarModel, "")
	t.Setenv(EnvVarEmbeddingModel, "")
	t.Setenv(EnvVarDataDir, "")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ServerURL != DefaultServerURL {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, DefaultServerURL)
	}
	if cfg.Model != DefaultModel {
		t.Errorf("Model = %q, want %q", cfg.Model, DefaultModel)
	}
	if cfg.EmbeddingModel != DefaultEmbeddingModel {
		t.Errorf("EmbeddingModel = %q, want %q", cfg.EmbeddingModel, DefaultEmbeddingModel)
	}
	if cfg.DataDir == "" {
		t.Error("DataDir should default to a non-empty path")
	}
}

func TestLoadConfigContextWindowEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarContextWindow, "4096")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ContextWindow != 4096 {
		t.Errorf("ContextWindow = %d, want 4096", cfg.ContextWindow)
	}
	// invalid value → error
	t.Setenv(EnvVarContextWindow, "abc")
	if _, err := LoadConfig(""); err == nil {
		t.Error("expected error for non-integer YAGENT_CONTEXT_WINDOW")
	}
	t.Setenv(EnvVarContextWindow, "50")
	if _, err := LoadConfig(""); err == nil {
		t.Error("expected error for YAGENT_CONTEXT_WINDOW < 100")
	}
}

func TestLoadConfigEnvOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarServerURL, "http://env.test")
	t.Setenv(EnvVarModel, "env-model")
	t.Setenv(EnvVarEmbeddingModel, "env-embed")
	t.Setenv(EnvVarDataDir, "/tmp/env-data")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ServerURL != "http://env.test" || cfg.Model != "env-model" ||
		cfg.EmbeddingModel != "env-embed" || cfg.DataDir != "/tmp/env-data" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfigFromFile(t *testing.T) {
	path := writeConfig(t, "server_url: http://example.test:9999\nmodel: some-model\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ServerURL != "http://example.test:9999" {
		t.Errorf("ServerURL = %q", cfg.ServerURL)
	}
	if cfg.Model != "some-model" {
		t.Errorf("Model = %q", cfg.Model)
	}
}

func TestLoadConfigEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, "server_url: http://file.test\nmodel: file-model\n")
	t.Setenv(EnvVarServerURL, "http://env.test:1234")
	t.Setenv(EnvVarModel, "env-model")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ServerURL != "http://env.test:1234" {
		t.Errorf("ServerURL = %q, want env override", cfg.ServerURL)
	}
	if cfg.Model != "env-model" {
		t.Errorf("Model = %q, want env override", cfg.Model)
	}
}

func TestLoadConfigPartialFileKeepsDefaults(t *testing.T) {
	path := writeConfig(t, "model: only-model\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Model != "only-model" {
		t.Errorf("Model = %q", cfg.Model)
	}
	if cfg.ServerURL != DefaultServerURL {
		t.Errorf("ServerURL = %q, want default %q", cfg.ServerURL, DefaultServerURL)
	}
}

func TestLoadConfigExplicitMissingFileErrors(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected error for explicit missing config file")
	}
}

func TestLoadConfigBadYAML(t *testing.T) {
	path := writeConfig(t, "server_url: [unclosed")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for malformed yaml")
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("HOME", "/tmp/fakehome")
	p, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	want := filepath.Join("/tmp/fakehome", ".config", "yagent", "config.yaml")
	if p != want {
		t.Errorf("DefaultPath = %q, want %q", p, want)
	}
}

func TestSkillsWriteApprovalDefaultsFalse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarServerURL, "")
	t.Setenv(EnvVarModel, "")
	t.Setenv(EnvVarEmbeddingModel, "")
	t.Setenv(EnvVarDataDir, "")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Skills.WriteApproval {
		t.Error("skills.write_approval should default to false (automatic skill creation)")
	}
}

func TestSkillsWriteApprovalFromFile(t *testing.T) {
	path := writeConfig(t, "skills:\n  write_approval: false\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Skills.WriteApproval {
		t.Error("skills.write_approval should be false from the file")
	}
}

func TestLoadConfigEmbeddingServerURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarServerURL, "http://chat.test")
	t.Setenv(EnvVarEmbeddingServer, "")
	t.Setenv(EnvVarEmbeddingModel, "")
	t.Setenv(EnvVarDataDir, "")
	t.Setenv(EnvVarModel, "")

	// defaults to server_url
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.EmbeddingServerURL != "http://chat.test" {
		t.Errorf("EmbeddingServerURL = %q, want server_url fallback", cfg.EmbeddingServerURL)
	}

	// explicit env override wins
	t.Setenv(EnvVarEmbeddingServer, "http://embed.test")
	cfg, err = LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.EmbeddingServerURL != "http://embed.test" {
		t.Errorf("EmbeddingServerURL = %q, want env override", cfg.EmbeddingServerURL)
	}
}

func TestLoadConfigWebSearch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarServerURL, "")
	t.Setenv(EnvVarModel, "")
	t.Setenv(EnvVarEmbeddingModel, "")
	t.Setenv(EnvVarDataDir, "")
	t.Setenv(EnvVarWebProvider, "")
	t.Setenv(EnvVarSearxngURL, "")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Web.Provider != "duckduckgo" {
		t.Errorf("Web.Provider = %q, want default duckduckgo", cfg.Web.Provider)
	}

	path := writeConfig(t, "web_search:\n  provider: searxng\n  searxng_url: http://searx:8080\n")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.Provider != "searxng" || cfg.Web.SearxngURL != "http://searx:8080" {
		t.Errorf("Web = %+v", cfg.Web)
	}

	t.Setenv(EnvVarWebProvider, "duckduckgo")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.Provider != "duckduckgo" {
		t.Errorf("env override failed: %+v", cfg.Web)
	}
}

func TestSetWriteApprovalPersists(t *testing.T) {
	path := writeConfig(t, "server_url: http://example.test\nmodel: some-model\n")

	if err := SetWriteApproval(path, false); err != nil {
		t.Fatalf("SetWriteApproval: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Skills.WriteApproval {
		t.Error("write_approval not set to false")
	}
	if cfg.ServerURL != "http://example.test" || cfg.Model != "some-model" {
		t.Errorf("unrelated config keys were clobbered: %+v", cfg)
	}

	if err := SetWriteApproval(path, true); err != nil {
		t.Fatalf("SetWriteApproval: %v", err)
	}
	cfg2, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg2.Skills.WriteApproval {
		t.Error("write_approval not restored to true")
	}
}

func TestSetWriteApprovalCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if err := SetWriteApproval(path, false); err != nil {
		t.Fatalf("SetWriteApproval: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Skills.WriteApproval {
		t.Error("write_approval should be false in the created file")
	}
}

func TestLoadConfigConsult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarServerURL, "http://chat.test")
	t.Setenv(EnvVarConsultServer, "")
	t.Setenv(EnvVarConsultModel, "")
	t.Setenv(EnvVarDataDir, "")
	t.Setenv(EnvVarModel, "")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Consult.Model != "" {
		t.Errorf("consult should default disabled, got %+v", cfg.Consult)
	}
	// env override
	t.Setenv(EnvVarConsultServer, "http://advisor.test")
	t.Setenv(EnvVarConsultModel, "advisor-3b")
	cfg, err = LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Consult.ServerURL != "http://advisor.test" || cfg.Consult.Model != "advisor-3b" {
		t.Errorf("consult = %+v", cfg.Consult)
	}
}

func TestLoadConfigConsultAPIKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarConsultServer, "")
	t.Setenv(EnvVarConsultModel, "")
	t.Setenv(EnvVarConsultAPIKey, "")
	t.Setenv(EnvVarDataDir, "")
	t.Setenv(EnvVarModel, "")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVarConsultModel, "gemini-2.0-flash")
	t.Setenv(EnvVarConsultAPIKey, "sk-secret")
	cfg, err = LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Consult.Model != "gemini-2.0-flash" || cfg.Consult.APIKey != "sk-secret" {
		t.Errorf("consult = %+v", cfg.Consult)
	}
}
