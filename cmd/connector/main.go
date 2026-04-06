package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/agent"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/config"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/storage"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/tools"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/whatsapp"
)

const banner = `
 █████╗ ███╗   ██╗████████╗██╗ ██████╗ ██████╗  █████╗ ██╗   ██╗██╗████████╗██╗   ██╗
██╔══██╗████╗  ██║╚══██╔══╝██║██╔════╝ ██╔══██╗██╔══██╗██║   ██║██║╚══██╔══╝╚██╗ ██╔╝
███████║██╔██╗ ██║   ██║   ██║██║  ███╗██████╔╝███████║██║   ██║██║   ██║    ╚████╔╝ 
██╔══██║██║╚██╗██║   ██║   ██║██║   ██║██╔══██╗██╔══██║╚██╗ ██╔╝██║   ██║     ╚██╔╝  
██║  ██║██║ ╚████║   ██║   ██║╚██████╔╝██║  ██║██║  ██║ ╚████╔╝ ██║   ██║      ██║   
╚═╝  ╚═╝╚═╝  ╚═══╝   ╚═╝   ╚═╝ ╚═════╝ ╚═╝  ╚═╝╚═╝  ╚═╝  ╚═══╝  ╚═╝   ╚═╝      ╚═╝   
                    WhatsApp Connector  v0.1.0
`

func main() {
	fmt.Print(banner)

	// Determine config path
	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	// Setup logging
	logLevel := slog.LevelInfo
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	// Load configuration
	logger.Info("loading configuration", "path", configPath)
	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		fmt.Println("\n⚠️  Configuration not found or invalid.")
		fmt.Println("   Copy config.example.yaml to config.yaml and edit it:")
		fmt.Println("   cp config.example.yaml config.yaml")
		os.Exit(1)
	}

	// Validate critical settings
	if cfg.Gemini.APIKey == "" {
		logger.Error("GEMINI_API_KEY is required")
		fmt.Println("\n⚠️  Gemini API key not configured.")
		fmt.Println("   Set GEMINI_API_KEY environment variable or add it to config.yaml")
		fmt.Println("   Get a key at: https://aistudio.google.com/apikey")
		os.Exit(1)
	}

	if len(cfg.Auth.AllowedPhones) == 0 {
		logger.Error("no phones configured")
		fmt.Println("\n⚠️  No authorized phone numbers configured.")
		fmt.Println("   Add your phone number to config.yaml under auth.allowed_phones")
		fmt.Println("   Format: +1234567890 (with country code)")
		os.Exit(1)
	}

	// Initialize database
	dbPath := cfg.WhatsApp.SessionDB
	// Use separate DB for app data
	appDBPath := "./data/app.db"
	logger.Info("initializing database", "path", appDBPath)
	db, err := storage.New(appDBPath)
	if err != nil {
		logger.Error("failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Initialize tool registry
	logger.Info("initializing tools", "workspace", cfg.Workspace.Root)
	registry := tools.NewRegistry(&cfg.Tools, cfg.Workspace.Root, db, logger)

	// Initialize Gemini agent
	ctx := context.Background()
	logger.Info("connecting to Gemini", "model", cfg.Gemini.Model)
	agentClient, err := agent.New(ctx, &cfg.Gemini, &cfg.Tools, db, registry, logger)
	if err != nil {
		logger.Error("failed to initialize Gemini agent", "error", err)
		os.Exit(1)
	}

	// Initialize WhatsApp client
	logger.Info("connecting to WhatsApp...", "session_db", dbPath)
	_ = dbPath // WhatsApp session DB is separate from app DB
	waClient, err := whatsapp.New(ctx, cfg, db, agentClient, registry)
	if err != nil {
		logger.Error("failed to connect to WhatsApp", "error", err)
		os.Exit(1)
	}

	// Print status
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════╗")
	fmt.Println("║  ✅ Antigravity WhatsApp Connector       ║")
	fmt.Println("║     Status: RUNNING                      ║")
	fmt.Printf("║     Model:  %-28s ║\n", cfg.Gemini.Model)
	fmt.Printf("║     Workspace: %-24s ║\n", truncate(cfg.Workspace.Root, 24))
	fmt.Printf("║     Auth phones: %-22d ║\n", len(cfg.Auth.AllowedPhones))
	fmt.Println("║                                          ║")
	fmt.Println("║  Send a WhatsApp message to get started! ║")
	fmt.Println("║  Press Ctrl+C to stop.                   ║")
	fmt.Println("╚══════════════════════════════════════════╝")
	fmt.Println()

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	logger.Info("shutting down...")
	waClient.Disconnect()
	logger.Info("goodbye!")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "..." + s[len(s)-max+3:]
}
