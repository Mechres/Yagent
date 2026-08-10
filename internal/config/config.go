package config

// Config contains minimal runtime configuration for the scaffold.
type Config struct {
    ServerURL string
    Model     string
}

// LoadConfig loads configuration from path or returns sensible defaults.
func LoadConfig(path string) (*Config, error) {
    return &Config{
        ServerURL: "http://localhost:11434",
        Model:     "qwen2.5-coder:14b",
    }, nil
}
