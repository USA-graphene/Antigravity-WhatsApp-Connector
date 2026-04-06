package config

// Defaults returns a Config with sane default values.
func Defaults() *Config {
	return &Config{
		WhatsApp: WhatsAppConfig{
			SessionDB: "./data/whatsapp.db",
		},
		Auth: AuthConfig{
			AllowedPhones:     []string{},
			PIN:               "",
			SessionTTL:        "8h",
			MaxFailedAttempts: 5,
		},
		Gemini: GeminiConfig{
			Model:       "gemini-2.5-flash",
			MaxTokens:   8192,
			Temperature: 0.7,
		},
		Workspace: WorkspaceConfig{
			Root: ".",
		},
		Tools: ToolsConfig{
			ReadFile: ToolConfig{
				Enabled:     true,
				MaxFileSize: "1MB",
			},
			WriteFile: ToolConfig{
				Enabled:             true,
				RequireConfirmation: true,
				BackupOriginal:      true,
			},
			RunCommand: ToolConfig{
				Enabled: true,
				Mode:    "allowlist",
				Allowlist: []string{
					"go build", "go test", "go run", "go mod", "go fmt", "go vet",
					"git status", "git diff", "git log", "git add", "git commit",
					"git push", "git pull", "git branch", "git checkout",
					"ls", "cat", "head", "tail", "grep", "find", "wc",
					"make", "which", "pwd", "echo", "env", "tree",
				},
				Blocklist: []string{
					"rm -rf", "sudo", "chmod", "chown", "mkfs", "dd",
					"curl | sh", "wget | sh",
				},
				Timeout:   "30s",
				MaxOutput: "32KB",
			},
			ListDirectory: ToolConfig{
				Enabled: true,
			},
			SearchWeb: ToolConfig{
				Enabled: false,
			},
		},
		RateLimit: RateLimitConfig{
			PerUser: 10,
			Global:  60,
		},
		Logging: LoggingConfig{
			Level:     "info",
			AuditFile: "./data/audit.log",
		},
	}
}
