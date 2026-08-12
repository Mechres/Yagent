package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config contains runtime configuration for the agent.
type Config struct {
	ServerURL string `yaml:"server_url"`
	Model     string `yaml:"model"`
	// APIKey, when set, is sent as `Authorization: Bearer <api_key>` on every
	// LLM/embedding request. This is the deliberately opt-in cloud path: point
	// server_url at any OpenAI-compatible endpoint (OpenRouter, Groq, Together,
	// Gemini) to run the whole loop in the cloud. Local-first remains the
	// default (empty = no auth header).
	APIKey         string `yaml:"api_key"`
	EmbeddingModel string `yaml:"embedding_model"`
	// EmbeddingServerURL is where /v1/embeddings is served; defaults to
	// ServerURL. Set it to a dedicated embedding server (e.g. a second
	// llama-server running bge-m3) for better recall.
	EmbeddingServerURL string `yaml:"embedding_server_url"`
	DataDir            string `yaml:"data_dir"`
	ContextWindow      int    `yaml:"context_window"`
	// Theme is the TUI color palette name (tokyo, catppuccin, nord).
	Theme  string       `yaml:"theme"`
	Skills SkillsConfig `yaml:"skills"`
	Web    WebConfig    `yaml:"web_search"`
	Shell  ShellConfig  `yaml:"shell"`
	// UI holds display preferences.
	UI UIConfig `yaml:"ui"`
	// Sampling is forwarded on every chat request (zero values are omitted, so
	// servers/cloud endpoints that don't understand a field only get what was
	// set). Defaults follow the Qwythos recipe for temperature/top_p; top_k and
	// repetition_penalty are opt-in (some OpenAI-compatible endpoints reject
	// them).
	Sampling SamplingConfig `yaml:"sampling"`
	// Models is an ordered list of per-model sampling profiles: the first whose
	// Match is a substring of the resolved model name wins, overriding the
	// base sampling defaults (P1 — turns docs/models.md data into code).
	Models []ModelProfile `yaml:"models"`
	// Consult points the `consult` tool at a second local model ("advisor")
	// the agent can ask for guidance. Empty = disabled.
	Consult ConsultConfig `yaml:"consult"`
	// Path is the config file this was loaded from ("" when none existed);
	// used to persist runtime toggles like skills.write_approval.
	Path string `yaml:"-"`
	// ProjectPath is a per-repo config (<workspace>/.yagent/config.yaml) that
	// overlays the global config, when one exists.
	ProjectPath string `yaml:"-"`
}

// SkillsConfig configures procedural memory (M3.5).
type SkillsConfig struct {
	// WriteApproval gates skill writes: when true every skill_manage write is
	// staged for review; when false (default) skill writes apply immediately.
	// The safety scanner still blocks dangerous content either way.
	WriteApproval bool `yaml:"write_approval"`
	// DataDir overrides where the global skills store lives (default: data_dir).
	DataDir string `yaml:"data_dir"`
	// ProjectDir overrides the project store (default: <workspace>/.yagent/skills).
	ProjectDir string `yaml:"project_dir"`
}

// WebConfig configures the M5 web tools.
type WebConfig struct {
	// Provider is the web_search backend: "duckduckgo" (default, no key or
	// server needed) or "searxng" (self-hosted JSON, requires SearxngURL).
	Provider string `yaml:"provider"`
	// SearxngURL is the base URL of a SearXNG instance with format=json enabled.
	SearxngURL string `yaml:"searxng_url"`
}

// ShellConfig configures shell_exec.
type ShellConfig struct {
	// Sandbox wraps shell commands in bubblewrap when set to "bwrap"
	// (workspace writable, system read-only, no network, private /tmp). It
	// fails loudly when bubblewrap is not installed — it never silently runs
	// unsandboxed. Empty disables the sandbox.
	Sandbox string `yaml:"sandbox"`
}

// ConsultConfig configures the `consult` tool (an "advisor" model). Two
// backends: a remote OpenAI-compatible server (ServerURL/Model/APIKey) or an
// installed terminal AI app run as a subprocess (Cmd, e.g. ["claude", "-p"],
// with the prompt appended as the final argument).
type ConsultConfig struct {
	// ServerURL of the advisor model (defaults to server_url when Model is set).
	ServerURL string `yaml:"server_url"`
	// Model is the advisor model name (HTTP backend).
	Model string `yaml:"model"`
	// APIKey enables cloud OpenAI-compatible endpoints (Authorization: Bearer).
	APIKey string `yaml:"api_key"`
	// Cmd is a terminal AI app used as the advisor, e.g. ["claude", "-p"].
	Cmd []string `yaml:"cmd"`
}

// SamplingConfig is the subset of generation parameters forwarded on chat
// requests. Zero values are omitted (top_k/repetition_penalty/min_p default
// off so OpenAI-compatible cloud endpoints that reject them aren't broken by
// default).
type SamplingConfig struct {
	Temperature       float64 `yaml:"temperature"`
	TopP              float64 `yaml:"top_p"`
	TopK              int     `yaml:"top_k"`
	RepetitionPenalty float64 `yaml:"repetition_penalty"`
	// MinP is the nucleus lower-bound filter (0 = off; llama.cpp/Ollama only —
	// often tightens up small local models).
	MinP float64 `yaml:"min_p"`
}

// ModelProfile overrides the base sampling for a model whose name contains
// Match (substring). Pointer fields mean "only set when present" — unset
// fields inherit the base SamplingConfig.
type ModelProfile struct {
	Match             string   `yaml:"match"`
	Temperature       *float64 `yaml:"temperature"`
	TopP              *float64 `yaml:"top_p"`
	TopK              *int     `yaml:"top_k"`
	RepetitionPenalty *float64 `yaml:"repetition_penalty"`
	MinP              *float64 `yaml:"min_p"`
}

// applyModels applies the first matching per-model profile to the base sampling
// (P1). Match is a substring of the model name; the first hit wins.
func (c *Config) applyModels() {
	for _, p := range c.Models {
		if p.Match == "" || !strings.Contains(c.Model, p.Match) {
			continue
		}
		if p.Temperature != nil {
			c.Sampling.Temperature = *p.Temperature
		}
		if p.TopP != nil {
			c.Sampling.TopP = *p.TopP
		}
		if p.TopK != nil {
			c.Sampling.TopK = *p.TopK
		}
		if p.RepetitionPenalty != nil {
			c.Sampling.RepetitionPenalty = *p.RepetitionPenalty
		}
		if p.MinP != nil {
			c.Sampling.MinP = *p.MinP
		}
		return
	}
}

// UIConfig holds display preferences.
type UIConfig struct {
	// ShowReasoning toggles the dimmed "thinking" block in the TUI/REPL
	// (reasoning_content). Reasoning never enters history either way.
	ShowReasoning bool `yaml:"show_reasoning"`
	// LoopGuard auto-cancels a running turn when the model visibly repeats
	// itself (a stuck generation loop). Default on.
	LoopGuard bool `yaml:"loop_guard"`
}

// Defaults applied when no config file and no env override is present.
const (
	DefaultServerURL      = "http://localhost:11434"
	DefaultModel          = "qwen2.5-coder:14b"
	DefaultEmbeddingModel = "nomic-embed-text"
	DefaultContextWindow  = 16384
	DefaultTheme          = "tokyo"
	// DefaultTemperature / DefaultTopP follow the Qwythos-9B recipe (0.6 /
	// 0.95). TopK (20) and RepetitionPenalty (1.05) are documented but not
	// defaulted, since some OpenAI-compatible endpoints reject them.
	DefaultTemperature = 0.6
	DefaultTopP        = 0.95
)

// ThemeOptions are the selectable TUI palette names.
var ThemeOptions = []string{"tokyo", "catppuccin", "nord"}

// EnvVarServerURL / EnvVarModel / EnvVarEmbeddingModel / EnvVarDataDir are the
// environment variable overrides, applied on top of whatever the config file
// (or defaults) resolved to.
const (
	EnvVarServerURL       = "YAGENT_SERVER_URL"
	EnvVarModel           = "YAGENT_MODEL"
	EnvVarAPIKey          = "YAGENT_API_KEY"
	EnvVarEmbeddingModel  = "YAGENT_EMBEDDING_MODEL"
	EnvVarEmbeddingServer = "YAGENT_EMBEDDING_SERVER_URL"
	EnvVarDataDir         = "YAGENT_DATA_DIR"
	EnvVarContextWindow   = "YAGENT_CONTEXT_WINDOW"
	EnvVarTheme           = "YAGENT_THEME"
	EnvVarWebProvider     = "YAGENT_WEB_SEARCH_PROVIDER"
	EnvVarSearxngURL      = "YAGENT_SEARXNG_URL"
	EnvVarConsultServer   = "YAGENT_CONSULT_SERVER_URL"
	EnvVarConsultModel    = "YAGENT_CONSULT_MODEL"
	EnvVarConsultAPIKey   = "YAGENT_CONSULT_API_KEY"
	EnvVarShellSandbox    = "YAGENT_SHELL_SANDBOX"
)

// DefaultPath is the config file used when no explicit path is given.
// Config file names and precedence: explicit path (must exist) >
// env overrides > default path (if it exists) > built-in defaults.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	return filepath.Join(dir, "yagent", "config.yaml"), nil
}

// DefaultDataDir is where sessions, vector memory and skills live:
// $XDG_DATA_HOME/yagent, falling back to ~/.local/share/yagent.
func DefaultDataDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "yagent"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", "yagent"), nil
}

// LoadConfig loads configuration from path, or the default path when path
// is empty. An explicit path that does not exist is an error; a missing
// default path silently falls back to built-in defaults.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
		ServerURL:      DefaultServerURL,
		Model:          DefaultModel,
		EmbeddingModel: DefaultEmbeddingModel,
		ContextWindow:  DefaultContextWindow,
		Theme:          DefaultTheme,
		UI:             UIConfig{ShowReasoning: true, LoopGuard: true},
		Sampling:       SamplingConfig{Temperature: DefaultTemperature, TopP: DefaultTopP},
		Skills:         SkillsConfig{WriteApproval: false},
	}
	dataDir, err := DefaultDataDir()
	if err != nil {
		return nil, err
	}
	cfg.DataDir = dataDir

	usePath := path
	if usePath == "" {
		def, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		usePath = def
	}
	cfg.Path = usePath

	data, err := os.ReadFile(usePath)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", usePath, err)
		}
	case os.IsNotExist(err):
		if path != "" {
			return nil, fmt.Errorf("config file %s: %w", path, err)
		}
		// no default config file: keep defaults
	default:
		return nil, fmt.Errorf("read config %s: %w", usePath, err)
	}

	// Per-repo config: <workspace>/.yagent/config.yaml overlays the global one
	// (only keys present in it are overridden), so a team can pin the model,
	// server, sandbox, etc. for a repo and commit it.
	if proj := ProjectConfigPath(); proj != "" {
		if data, err := os.ReadFile(proj); err == nil {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse project config %s: %w", proj, err)
			}
			cfg.ProjectPath = proj
		}
	}

	// Env overrides beat the file (and defaults).
	if v := os.Getenv(EnvVarServerURL); v != "" {
		cfg.ServerURL = v
	}
	if v := os.Getenv(EnvVarModel); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv(EnvVarAPIKey); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv(EnvVarEmbeddingModel); v != "" {
		cfg.EmbeddingModel = v
	}
	if v := os.Getenv(EnvVarEmbeddingServer); v != "" {
		cfg.EmbeddingServerURL = v
	}
	if v := os.Getenv(EnvVarDataDir); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv(EnvVarTheme); v != "" {
		cfg.Theme = v
	}
	if v := os.Getenv(EnvVarContextWindow); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 100 {
			return nil, fmt.Errorf("%s must be an integer >= 100, got %q", EnvVarContextWindow, v)
		}
		cfg.ContextWindow = n
	}
	if v := os.Getenv(EnvVarWebProvider); v != "" {
		cfg.Web.Provider = v
	}
	if v := os.Getenv(EnvVarSearxngURL); v != "" {
		cfg.Web.SearxngURL = v
	}
	if v := os.Getenv(EnvVarConsultServer); v != "" {
		cfg.Consult.ServerURL = v
	}
	if v := os.Getenv(EnvVarConsultModel); v != "" {
		cfg.Consult.Model = v
	}
	if v := os.Getenv(EnvVarConsultAPIKey); v != "" {
		cfg.Consult.APIKey = v
	}
	if v := os.Getenv(EnvVarShellSandbox); v != "" {
		cfg.Shell.Sandbox = v
	}

	if cfg.ServerURL == "" {
		cfg.ServerURL = DefaultServerURL
	}
	if cfg.EmbeddingServerURL == "" {
		cfg.EmbeddingServerURL = cfg.ServerURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.EmbeddingModel == "" {
		cfg.EmbeddingModel = DefaultEmbeddingModel
	}
	if cfg.ContextWindow <= 0 {
		cfg.ContextWindow = DefaultContextWindow
	}
	if cfg.Web.Provider == "" {
		cfg.Web.Provider = "duckduckgo"
	}
	if cfg.DataDir == "" {
		dataDir, err := DefaultDataDir()
		if err != nil {
			return nil, err
		}
		cfg.DataDir = dataDir
	}
	// Per-model sampling profiles override the base recipe for the resolved
	// model (P1) — last, so they apply on top of file + env sampling.
	cfg.applyModels()
	return cfg, nil
}

// ProjectConfigPath returns <workspace>/.yagent/config.yaml when it exists.
func ProjectConfigPath() string {
	ws, err := os.Getwd()
	if err != nil {
		return ""
	}
	proj := filepath.Join(ws, ".yagent", "config.yaml")
	if _, err := os.Stat(proj); err == nil {
		return proj
	}
	return ""
}

// SetWriteApproval persists skills.write_approval to the config file at path.
func SetWriteApproval(path string, on bool) error {
	return Set(path, "skills.write_approval", strconv.FormatBool(on))
}

// SettingKey is a dotted config key; Settings lists them for the /settings UI.
// Options, when non-empty, makes the field a chooser (select from the list)
// instead of free text.
type SettingKey struct {
	Key     string
	Label   string
	Options []string
}

// Settings is the ordered catalog of editable settings.
func Settings() []SettingKey {
	return []SettingKey{
		{Key: "server_url", Label: "Server URL"},
		{Key: "model", Label: "Model"},
		{Key: "api_key", Label: "API key (Bearer auth, cloud)"},
		{Key: "embedding_model", Label: "Embedding model"},
		{Key: "embedding_server_url", Label: "Embedding server URL"},
		{Key: "context_window", Label: "Context window (tokens)"},
		{Key: "data_dir", Label: "Data dir"},
		{Key: "theme", Label: "TUI theme", Options: ThemeOptions},
		{Key: "sampling.temperature", Label: "Sampling temperature"},
		{Key: "sampling.top_p", Label: "Sampling top_p"},
		{Key: "sampling.top_k", Label: "Sampling top_k (0 = off)"},
		{Key: "sampling.repetition_penalty", Label: "Sampling repetition penalty (0 = off)"},
		{Key: "sampling.min_p", Label: "Sampling min_p (0 = off; llama.cpp/Ollama)"},
		{Key: "ui.show_reasoning", Label: "Show thinking block", Options: []string{"true", "false"}},
		{Key: "ui.loop_guard", Label: "Stop repeating-generation loops", Options: []string{"true", "false"}},
		{Key: "web_search.provider", Label: "Web search provider", Options: []string{"duckduckgo", "mojeek", "searxng"}},
		{Key: "web_search.searxng_url", Label: "SearXNG URL"},
		{Key: "skills.write_approval", Label: "Skills write approval", Options: []string{"false", "true"}},
		{Key: "skills.data_dir", Label: "Skills data dir"},
		{Key: "skills.project_dir", Label: "Skills project dir"},
		{Key: "shell.sandbox", Label: "Shell sandbox", Options: []string{"", "bwrap"}},
		{Key: "consult.server_url", Label: "Consult server URL"},
		{Key: "consult.model", Label: "Consult model"},
		{Key: "consult.api_key", Label: "Consult API key"},
		{Key: "consult.cmd", Label: "Consult CLI app (space-separated argv)"},
	}
}

// Get returns the current value of a dotted setting key.
func (c *Config) Get(key string) string {
	switch key {
	case "server_url":
		return c.ServerURL
	case "model":
		return c.Model
	case "api_key":
		return c.APIKey
	case "embedding_model":
		return c.EmbeddingModel
	case "embedding_server_url":
		return c.EmbeddingServerURL
	case "context_window":
		return strconv.Itoa(c.ContextWindow)
	case "data_dir":
		return c.DataDir
	case "theme":
		return c.Theme
	case "sampling.temperature":
		return strconv.FormatFloat(c.Sampling.Temperature, 'f', -1, 64)
	case "sampling.top_p":
		return strconv.FormatFloat(c.Sampling.TopP, 'f', -1, 64)
	case "sampling.top_k":
		return strconv.Itoa(c.Sampling.TopK)
	case "sampling.repetition_penalty":
		return strconv.FormatFloat(c.Sampling.RepetitionPenalty, 'f', -1, 64)
	case "sampling.min_p":
		return strconv.FormatFloat(c.Sampling.MinP, 'f', -1, 64)
	case "ui.show_reasoning":
		return strconv.FormatBool(c.UI.ShowReasoning)
	case "ui.loop_guard":
		return strconv.FormatBool(c.UI.LoopGuard)
	case "web_search.provider":
		return c.Web.Provider
	case "web_search.searxng_url":
		return c.Web.SearxngURL
	case "skills.write_approval":
		return strconv.FormatBool(c.Skills.WriteApproval)
	case "skills.data_dir":
		return c.Skills.DataDir
	case "skills.project_dir":
		return c.Skills.ProjectDir
	case "shell.sandbox":
		return c.Shell.Sandbox
	case "consult.server_url":
		return c.Consult.ServerURL
	case "consult.model":
		return c.Consult.Model
	case "consult.api_key":
		return c.Consult.APIKey
	case "consult.cmd":
		return strings.Join(c.Consult.Cmd, " ")
	}
	return ""
}

// Set persists a dotted config key (e.g. "web_search.provider",
// "consult.model") to the config file at path, validating known keys and
// values. Returns a ValidationError for unknown keys or bad values.
func Set(path, key, value string) error {
	if path == "" {
		return fmt.Errorf("no config file path is known")
	}
	parts := strings.Split(key, ".")
	if err := validateKey(parts, value); err != nil {
		return err
	}
	doc, m, err := loadConfigNode(path)
	if err != nil {
		return err
	}
	node := m
	for _, part := range parts[:len(parts)-1] {
		child := mappingValue(node, part)
		if child == nil || child.Kind != yaml.MappingNode {
			child = &yaml.Node{Kind: yaml.MappingNode}
			setMappingKey(node, part, child)
		}
		node = child
	}
	setMappingKey(node, parts[len(parts)-1], typedScalar(parts[len(parts)-1], value))
	return saveConfigNode(doc, path)
}

// loadConfigNode reads the config file into yaml nodes (creating an empty one
// when the file does not exist) and returns the document and root mapping.
func loadConfigNode(path string) (*yaml.Node, *yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("read config %s: %w", path, err)
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, doc); err != nil {
			return nil, nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("config %s is not a mapping", path)
	}
	return doc, m, nil
}

func saveConfigNode(doc *yaml.Node, path string) error {
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("render config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// validateKey rejects unknown keys and invalid values.
func validateKey(parts []string, value string) error {
	known := map[string]bool{
		"server_url": true, "model": true, "api_key": true, "embedding_model": true,
		"embedding_server_url": true, "context_window": true, "data_dir": true,
		"theme":                true,
		"sampling.temperature": true, "sampling.top_p": true,
		"sampling.top_k": true, "sampling.repetition_penalty": true,
		"sampling.min_p":      true,
		"ui.show_reasoning":   true,
		"ui.loop_guard":       true,
		"web_search.provider": true, "web_search.searxng_url": true,
		"skills.write_approval": true, "skills.data_dir": true, "skills.project_dir": true,
		"shell.sandbox":      true,
		"consult.server_url": true, "consult.model": true, "consult.api_key": true,
		"consult.cmd": true,
	}
	key := strings.Join(parts, ".")
	if !known[key] {
		return &ValidationError{msg: fmt.Sprintf("unknown setting %q (see /settings)", key)}
	}
	switch key {
	case "context_window":
		n, err := strconv.Atoi(value)
		if err != nil || n < 100 {
			return &ValidationError{msg: "context_window must be an integer >= 100"}
		}
	case "skills.write_approval":
		if value != "true" && value != "false" {
			return &ValidationError{msg: "skills.write_approval must be true or false"}
		}
	case "web_search.provider":
		switch value {
		case "duckduckgo", "mojeek", "searxng":
		default:
			return &ValidationError{msg: "web_search.provider must be duckduckgo, mojeek or searxng"}
		}
	case "theme":
		if !slices.Contains(ThemeOptions, value) {
			return &ValidationError{msg: "theme must be one of: " + strings.Join(ThemeOptions, ", ")}
		}
	case "sampling.temperature", "sampling.top_p", "sampling.repetition_penalty":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f < 0 || f > 2 {
			return &ValidationError{msg: key + " must be a number between 0 and 2"}
		}
	case "sampling.top_k":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return &ValidationError{msg: "sampling.top_k must be a non-negative integer (0 = off)"}
		}
	case "sampling.min_p":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f < 0 || f > 1 {
			return &ValidationError{msg: "sampling.min_p must be a number between 0 and 1 (0 = off)"}
		}
	case "ui.show_reasoning", "ui.loop_guard":
		if value != "true" && value != "false" {
			return &ValidationError{msg: key + " must be true or false"}
		}
	case "shell.sandbox":
		if value != "" && value != "bwrap" {
			return &ValidationError{msg: "shell.sandbox must be empty or bwrap"}
		}
	default:
		if value == "" {
			return &ValidationError{msg: key + " cannot be empty"}
		}
	}
	return nil
}

// typedScalar renders a leaf value as the right scalar tag. Set() passes the
// last dotted segment (e.g. "top_k" for "sampling.top_k"), matching the
// existing write_approval/context_window convention.
func typedScalar(key, value string) *yaml.Node {
	switch key {
	case "write_approval", "show_reasoning", "loop_guard":
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: value}
	case "context_window", "top_k":
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: value}
	case "temperature", "top_p", "repetition_penalty", "min_p":
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: value}
	case "cmd":
		// consult.cmd is stored as a YAML sequence: "/set consult.cmd claude -p"
		// round-trips to `cmd: [claude, -p]` (ConsultConfig.Cmd is []string).
		return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: func() []*yaml.Node {
			var items []*yaml.Node
			for _, part := range strings.Fields(value) {
				items = append(items, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part})
			}
			return items
		}()}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

// ValidationError marks a bad setting key or value (feeds back to the UI).
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

// mappingValue returns the value node for key in a mapping, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setMappingKey(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value)
}
