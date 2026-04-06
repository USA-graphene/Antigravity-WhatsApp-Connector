package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure.
type Config struct {
	WhatsApp  WhatsAppConfig  `yaml:"whatsapp"`
	Auth      AuthConfig      `yaml:"auth"`
	Gemini    GeminiConfig    `yaml:"gemini"`
	Workspace WorkspaceConfig `yaml:"workspace"`
	Tools     ToolsConfig     `yaml:"tools"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	Logging   LoggingConfig   `yaml:"logging"`
}

type WhatsAppConfig struct {
	SessionDB string `yaml:"session_db"`
}

type AuthConfig struct {
	AllowedPhones     []string `yaml:"allowed_phones"`
	PIN               string   `yaml:"pin"`
	SessionTTL        string   `yaml:"session_ttl"`
	MaxFailedAttempts int      `yaml:"max_failed_attempts"`
}

func (a AuthConfig) GetSessionTTL() time.Duration {
	d, err := time.ParseDuration(a.SessionTTL)
	if err != nil {
		return 8 * time.Hour
	}
	return d
}

type GeminiConfig struct {
	APIKey      string  `yaml:"api_key"`
	Model       string  `yaml:"model"`
	MaxTokens   int     `yaml:"max_tokens"`
	Temperature float32 `yaml:"temperature"`
}

type WorkspaceConfig struct {
	Root string `yaml:"root"`
}

type ToolConfig struct {
	Enabled             bool     `yaml:"enabled"`
	MaxFileSize         string   `yaml:"max_file_size,omitempty"`
	RequireConfirmation bool     `yaml:"require_confirmation,omitempty"`
	BackupOriginal      bool     `yaml:"backup_original,omitempty"`
	Mode                string   `yaml:"mode,omitempty"`
	Allowlist           []string `yaml:"allowlist,omitempty"`
	Blocklist           []string `yaml:"blocklist,omitempty"`
	Timeout             string   `yaml:"timeout,omitempty"`
	MaxOutput           string   `yaml:"max_output,omitempty"`
}

func (t ToolConfig) GetTimeout() time.Duration {
	d, err := time.ParseDuration(t.Timeout)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

type ToolsConfig struct {
	ReadFile      ToolConfig `yaml:"read_file"`
	WriteFile     ToolConfig `yaml:"write_file"`
	RunCommand    ToolConfig `yaml:"run_command"`
	ListDirectory ToolConfig `yaml:"list_directory"`
	SearchWeb     ToolConfig `yaml:"search_web"`
}

type RateLimitConfig struct {
	PerUser int `yaml:"per_user"`
	Global  int `yaml:"global"`
}

type LoggingConfig struct {
	Level     string `yaml:"level"`
	AuditFile string `yaml:"audit_file"`
}

// Load reads the config file from disk and returns a Config.
func Load(path string) (*Config, error) {
	cfg := Defaults()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	// Override API key from environment if not set in config
	if cfg.Gemini.APIKey == "" {
		cfg.Gemini.APIKey = os.Getenv("GEMINI_API_KEY")
	}

	// Resolve workspace root to absolute path
	if cfg.Workspace.Root == "" || cfg.Workspace.Root == "." {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("getting working directory: %w", err)
		}
		cfg.Workspace.Root = cwd
	} else {
		abs, err := filepath.Abs(cfg.Workspace.Root)
		if err != nil {
			return nil, fmt.Errorf("resolving workspace root: %w", err)
		}
		cfg.Workspace.Root = abs
	}

	// Ensure data directory exists
	dataDir := filepath.Dir(cfg.WhatsApp.SessionDB)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("creating data directory: %w", err)
	}

	return cfg, nil
}
