package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config contains runtime configuration for the agent.
type Config struct {
	ServerURL string `yaml:"server_url"`
	Model     string `yaml:"model"`
}

// Defaults applied when no config file and no env override is present.
const (
	DefaultServerURL = "http://localhost:11434"
	DefaultModel     = "qwen2.5-coder:14b"
)

// EnvVarServerURL / EnvVarModel are the environment variable overrides,
// applied on top of whatever the config file (or defaults) resolved to.
const (
	EnvVarServerURL = "YAGENT_SERVER_URL"
	EnvVarModel     = "YAGENT_MODEL"
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

// LoadConfig loads configuration from path, or the default path when path
// is empty. An explicit path that does not exist is an error; a missing
// default path silently falls back to built-in defaults.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{ServerURL: DefaultServerURL, Model: DefaultModel}

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

	if cfg.ServerURL == "" {
		cfg.ServerURL = DefaultServerURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	return cfg, nil
}
