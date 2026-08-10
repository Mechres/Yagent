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
