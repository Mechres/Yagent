package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config contains runtime configuration for the agent.
type Config struct {
	ServerURL      string `yaml:"server_url"`
	Model          string `yaml:"model"`
	EmbeddingModel string `yaml:"embedding_model"`
	// EmbeddingServerURL is where /v1/embeddings is served; defaults to
	// ServerURL. Set it to a dedicated embedding server (e.g. a second
	// llama-server running bge-m3) for better recall.
	EmbeddingServerURL string       `yaml:"embedding_server_url"`
	DataDir            string       `yaml:"data_dir"`
	ContextWindow      int          `yaml:"context_window"`
	Skills             SkillsConfig `yaml:"skills"`
	Web                WebConfig    `yaml:"web_search"`
	// Path is the config file this was loaded from ("" when none existed);
	// used to persist runtime toggles like skills.write_approval.
	Path string `yaml:"-"`
}

// SkillsConfig configures procedural memory (M3.5).
type SkillsConfig struct {
	// WriteApproval gates skill writes: when true (default) every skill_manage
	// write is staged for review instead of applied.
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

// Defaults applied when no config file and no env override is present.
const (
	DefaultServerURL      = "http://localhost:11434"
	DefaultModel          = "qwen2.5-coder:14b"
	DefaultEmbeddingModel = "nomic-embed-text"
	DefaultContextWindow  = 16384
)

// EnvVarServerURL / EnvVarModel / EnvVarEmbeddingModel / EnvVarDataDir are the
// environment variable overrides, applied on top of whatever the config file
// (or defaults) resolved to.
const (
	EnvVarServerURL       = "YAGENT_SERVER_URL"
	EnvVarModel           = "YAGENT_MODEL"
	EnvVarEmbeddingModel  = "YAGENT_EMBEDDING_MODEL"
	EnvVarEmbeddingServer = "YAGENT_EMBEDDING_SERVER_URL"
	EnvVarDataDir         = "YAGENT_DATA_DIR"
	EnvVarContextWindow   = "YAGENT_CONTEXT_WINDOW"
	EnvVarWebProvider     = "YAGENT_WEB_SEARCH_PROVIDER"
	EnvVarSearxngURL      = "YAGENT_SEARXNG_URL"
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
		Skills:         SkillsConfig{WriteApproval: true},
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

	// Env overrides beat the file (and defaults).
	if v := os.Getenv(EnvVarServerURL); v != "" {
		cfg.ServerURL = v
	}
	if v := os.Getenv(EnvVarModel); v != "" {
		cfg.Model = v
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
	return cfg, nil
}

// SetWriteApproval persists skills.write_approval to the config file at path
// (creating it if needed), preserving every other key. Returns an error only
// on I/O or YAML problems; the caller may fall back to an in-memory toggle.
func SetWriteApproval(path string, on bool) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	var root yaml.Node
	if len(bytes.TrimSpace(data)) == 0 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	} else if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	doc := &root
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		doc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return fmt.Errorf("config %s is not a mapping", path)
	}
	skills := mappingValue(m, "skills")
	if skills == nil {
		skills = &yaml.Node{Kind: yaml.MappingNode}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "skills"},
			skills)
	}
	setMappingKey(skills, "write_approval",
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(on)})

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
