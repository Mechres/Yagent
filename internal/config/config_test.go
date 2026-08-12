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

func TestLoadConfigAPIKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from the real user config
	t.Setenv("XDG_CONFIG_HOME", "")
	// from file
	path := writeConfig(t, "server_url: https://openrouter.ai/api/v1\nmodel: x\napi_key: file-key\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.APIKey != "file-key" {
		t.Errorf("APIKey from file = %q", cfg.APIKey)
	}
	// env beats file
	t.Setenv(EnvVarAPIKey, "env-key")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.APIKey != "env-key" {
		t.Errorf("APIKey after env = %q", cfg.APIKey)
	}
	// defaults to empty
	t.Setenv(EnvVarAPIKey, "")
	cfg, err = LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.APIKey != "" {
		t.Errorf("APIKey default = %q, want empty", cfg.APIKey)
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
	// UserConfigDir prefers $XDG_CONFIG_HOME over $HOME, so clear it: some CI
	// runners inherit an XDG path that would override the fake HOME below.
	t.Setenv("HOME", "/tmp/fakehome")
	t.Setenv("XDG_CONFIG_HOME", "")
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

func TestSetGeneralAndValidation(t *testing.T) {
	path := writeConfig(t, "server_url: http://example.test\nmodel: some-model\n")
	if err := Set(path, "consult.model", "gemini-2.0-flash"); err != nil {
		t.Fatal(err)
	}
	if err := Set(path, "context_window", "8192"); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Consult.Model != "gemini-2.0-flash" || cfg.ContextWindow != 8192 || cfg.ServerURL != "http://example.test" {
		t.Errorf("cfg = %+v", cfg)
	}
	// validation
	if err := Set(path, "nope", "x"); err == nil {
		t.Error("unknown key should error")
	}
	if err := Set(path, "context_window", "50"); err == nil {
		t.Error("small context_window should error")
	}
	if err := Set(path, "web_search.provider", "bogus"); err == nil {
		t.Error("bad provider should error")
	}
	if err := Set(path, "skills.write_approval", "true"); err != nil {
		t.Errorf("write_approval set: %v", err)
	}
}

func TestSetConsultCmdRoundTrip(t *testing.T) {
	path := writeConfig(t, "server_url: x\nmodel: y\n")
	// /set consult.cmd claude -p must persist as a YAML sequence and reload
	// into ConsultConfig.Cmd without "cannot unmarshal" errors.
	if err := Set(path, "consult.cmd", "claude -p"); err != nil {
		t.Fatalf("set consult.cmd: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(cfg.Consult.Cmd) != 2 || cfg.Consult.Cmd[0] != "claude" || cfg.Consult.Cmd[1] != "-p" {
		t.Errorf("consult.cmd = %v, want [claude -p]", cfg.Consult.Cmd)
	}
	if cfg.Get("consult.cmd") != "claude -p" {
		t.Errorf("Get(consult.cmd) = %q", cfg.Get("consult.cmd"))
	}
	// empty value is rejected (a command is required when the key is used)
	if err := Set(path, "consult.cmd", ""); err == nil {
		t.Error("empty consult.cmd should be rejected")
	}
}

func TestPerModelSamplingProfiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarModel, "")
	t.Setenv(EnvVarServerURL, "")
	t.Setenv(EnvVarEmbeddingModel, "")
	t.Setenv(EnvVarDataDir, "")
	path := writeConfig(t, `model: "Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf"
sampling:
  temperature: 0.6
  top_p: 0.95
models:
  - match: Qwythos
    top_k: 20
    repetition_penalty: 1.05
  - match: qwen2.5-coder
    temperature: 0.2
    top_k: 40
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// first matching profile applies; unset fields inherit the base recipe
	if cfg.Sampling.TopK != 20 || cfg.Sampling.RepetitionPenalty != 1.05 {
		t.Errorf("Qwythos profile not applied: %+v", cfg.Sampling)
	}
	if cfg.Sampling.Temperature != 0.6 || cfg.Sampling.TopP != 0.95 {
		t.Errorf("profile must not override unset fields: %+v", cfg.Sampling)
	}

	// a different model matches its own profile
	path2 := writeConfig(t, `model: "qwen2.5-coder:14b"
sampling:
  temperature: 0.6
models:
  - match: qwen2.5-coder
    temperature: 0.2
`)
	cfg2, err := LoadConfig(path2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Sampling.Temperature != 0.2 {
		t.Errorf("qwen profile not applied: %+v", cfg2.Sampling)
	}
}

func TestLoadConfigTheme(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarTheme, "")
	// default
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Theme != DefaultTheme {
		t.Errorf("default theme = %q, want %q", cfg.Theme, DefaultTheme)
	}
	// from file
	path := writeConfig(t, "theme: nord\n")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Theme != "nord" {
		t.Errorf("file theme = %q", cfg.Theme)
	}
	// env beats file
	t.Setenv(EnvVarTheme, "catppuccin")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Theme != "catppuccin" {
		t.Errorf("env theme = %q", cfg.Theme)
	}
	// validation
	err = Set(path, "theme", "beige")
	if err == nil {
		t.Error("invalid theme should be rejected")
	}
	if err := Set(path, "theme", "nord"); err != nil {
		t.Errorf("valid theme rejected: %v", err)
	}
}

func TestLoadConfigSampling(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// defaults
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Sampling.Temperature != DefaultTemperature || cfg.Sampling.TopP != DefaultTopP {
		t.Errorf("default sampling = %+v", cfg.Sampling)
	}
	if cfg.Sampling.TopK != 0 || cfg.Sampling.RepetitionPenalty != 0 {
		t.Errorf("top_k/rep_penalty should default off: %+v", cfg.Sampling)
	}
	// from file
	path := writeConfig(t, "sampling:\n  temperature: 0.8\n  top_p: 0.9\n  top_k: 40\n  repetition_penalty: 1.1\n")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Sampling.Temperature != 0.8 || cfg.Sampling.TopP != 0.9 ||
		cfg.Sampling.TopK != 40 || cfg.Sampling.RepetitionPenalty != 1.1 {
		t.Errorf("file sampling = %+v", cfg.Sampling)
	}
	// /set round-trip + validation
	if err := Set(path, "sampling.top_k", "20"); err != nil {
		t.Errorf("set top_k: %v", err)
	}
	if err := Set(path, "sampling.temperature", "0.6"); err != nil {
		t.Errorf("set temperature: %v", err)
	}
	if err := Set(path, "sampling.temperature", "hot"); err == nil {
		t.Error("non-numeric temperature should be rejected")
	}
	if err := Set(path, "sampling.top_k", "-3"); err == nil {
		t.Error("negative top_k should be rejected")
	}
	// Set must persist typed scalars (not quoted strings), so LoadConfig can
	// round-trip without "cannot unmarshal !!str" errors
	path = writeConfig(t, "server_url: x\nmodel: y\n")
	if err := Set(path, "sampling.top_k", "20"); err != nil {
		t.Fatalf("set top_k: %v", err)
	}
	if err := Set(path, "sampling.repetition_penalty", "1.05"); err != nil {
		t.Fatalf("set rep_penalty: %v", err)
	}
	if err := Set(path, "sampling.min_p", "0.05"); err != nil {
		t.Fatalf("set min_p: %v", err)
	}
	if err := Set(path, "sampling.temperature", "0.6"); err != nil {
		t.Fatalf("set temperature: %v", err)
	}
	if err := Set(path, "sampling.min_p", "1.5"); err == nil {
		t.Error("min_p > 1 should be rejected")
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload after Set: %v", err)
	}
	if reloaded.Sampling.TopK != 20 || reloaded.Sampling.RepetitionPenalty != 1.05 ||
		reloaded.Sampling.Temperature != 0.6 || reloaded.Sampling.MinP != 0.05 {
		t.Errorf("round-trip sampling = %+v", reloaded.Sampling)
	}
}

func TestSettingsCatalogAndGet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarServerURL, "")
	t.Setenv(EnvVarModel, "")
	t.Setenv(EnvVarEmbeddingModel, "")
	t.Setenv(EnvVarDataDir, "")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if len(Settings()) < 10 {
		t.Errorf("settings catalog too small: %d", len(Settings()))
	}
	if cfg.Get("server_url") != DefaultServerURL {
		t.Errorf("Get(server_url) = %q", cfg.Get("server_url"))
	}
	if cfg.Get("skills.write_approval") != "false" {
		t.Errorf("Get(write_approval) = %q", cfg.Get("skills.write_approval"))
	}
}

func TestLoadConfigShellSandbox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVarShellSandbox, "")
	t.Setenv(EnvVarDataDir, "")
	t.Setenv(EnvVarModel, "")
	t.Setenv(EnvVarServerURL, "")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Shell.Sandbox != "" {
		t.Errorf("sandbox should default empty, got %q", cfg.Shell.Sandbox)
	}
	t.Setenv(EnvVarShellSandbox, "bwrap")
	cfg, err = LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Shell.Sandbox != "bwrap" {
		t.Errorf("sandbox = %q", cfg.Shell.Sandbox)
	}
}

func TestLoadConfigProjectOverlay(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".yagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".yagent", "config.yaml"),
		[]byte("model: project-model\nshell:\n  sandbox: bwrap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(ws); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	path := writeConfig(t, "server_url: http://global.test\nmodel: global-model\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "project-model" {
		t.Errorf("model = %q, want project override", cfg.Model)
	}
	if cfg.ServerURL != "http://global.test" {
		t.Errorf("server_url = %q, want global preserved", cfg.ServerURL)
	}
	if cfg.Shell.Sandbox != "bwrap" {
		t.Errorf("sandbox = %q, want project bwrap", cfg.Shell.Sandbox)
	}
	if cfg.ProjectPath == "" {
		t.Error("ProjectPath should be set")
	}
	// env still beats project
	t.Setenv(EnvVarModel, "env-model")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "env-model" {
		t.Errorf("model = %q, want env override", cfg.Model)
	}
}
