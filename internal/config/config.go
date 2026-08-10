package config

import (
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
	DataDir        string `yaml:"data_dir"`
	ContextWindow  int    `yaml:"context_window"`
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
	EnvVarServerURL      = "YAGENT_SERVER_URL"
	EnvVarModel          = "YAGENT_MODEL"
	EnvVarEmbeddingModel = "YAGENT_EMBEDDING_MODEL"
	EnvVarDataDir        = "YAGENT_DATA_DIR"
	EnvVarContextWindow  = "YAGENT_CONTEXT_WINDOW"
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

	if cfg.ServerURL == "" {
		cfg.ServerURL = DefaultServerURL
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
	if cfg.DataDir == "" {
		dataDir, err := DefaultDataDir()
		if err != nil {
			return nil, err
		}
		cfg.DataDir = dataDir
	}
	return cfg, nil
}
