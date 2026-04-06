package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/config"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/storage"
)

// Registry manages available tools and their permissions.
type Registry struct {
	cfg           *config.ToolsConfig
	workspaceRoot string
	db            *storage.DB
	log           *slog.Logger
}

// NewRegistry creates a new tool registry.
func NewRegistry(cfg *config.ToolsConfig, workspaceRoot string, db *storage.DB, log *slog.Logger) *Registry {
	return &Registry{
		cfg:           cfg,
		workspaceRoot: workspaceRoot,
		db:            db,
		log:           log,
	}
}

// ToolResult is the result of a tool execution.
type ToolResult struct {
	Output  string
	Success bool
}

// Execute runs a tool with the given arguments.
func (r *Registry) Execute(ctx context.Context, phone, toolName string, args map[string]any) ToolResult {
	start := time.Now()
	r.log.Info("tool execution requested", "tool", toolName, "phone", phone, "args", args)

	result := r.executeInternal(ctx, phone, toolName, args)

	duration := time.Since(start)
	summary := result.Output
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}
	r.db.LogAudit(phone, toolName, fmt.Sprintf("%v", args), summary, result.Success, duration.Milliseconds())

	return result
}

func (r *Registry) executeInternal(ctx context.Context, phone, toolName string, args map[string]any) ToolResult {
	switch toolName {
	case "read_file":
		return r.readFile(args)
	case "write_file":
		return r.writeFile(phone, args)
	case "list_directory":
		return r.listDirectory(args)
	case "run_command":
		return r.runCommand(ctx, args)
	default:
		return ToolResult{Output: fmt.Sprintf("Unknown tool: %s", toolName), Success: false}
	}
}

// --- read_file ---

func (r *Registry) readFile(args map[string]any) ToolResult {
	if !r.cfg.ReadFile.Enabled {
		return ToolResult{Output: "read_file tool is disabled", Success: false}
	}

	path, ok := args["path"].(string)
	if !ok || path == "" {
		return ToolResult{Output: "Missing required argument: path", Success: false}
	}

	absPath, err := r.resolveAndValidatePath(path)
	if err != nil {
		return ToolResult{Output: err.Error(), Success: false}
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return ToolResult{Output: fmt.Sprintf("File not found: %s", path), Success: false}
	}

	// Check file size
	maxSize := int64(1024 * 1024) // 1MB default
	if info.Size() > maxSize {
		return ToolResult{
			Output:  fmt.Sprintf("File too large: %d bytes (max: %d bytes). Try reading a specific section.", info.Size(), maxSize),
			Success: false,
		}
	}

	if info.IsDir() {
		return ToolResult{Output: "Path is a directory. Use list_directory instead.", Success: false}
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return ToolResult{Output: fmt.Sprintf("Error reading file: %v", err), Success: false}
	}

	// Basic binary detection
	for _, b := range data[:min(512, len(data))] {
		if b == 0 {
			return ToolResult{Output: "File appears to be binary. Cannot display.", Success: false}
		}
	}

	return ToolResult{Output: string(data), Success: true}
}

// --- write_file ---

func (r *Registry) writeFile(phone string, args map[string]any) ToolResult {
	if !r.cfg.WriteFile.Enabled {
		return ToolResult{Output: "write_file tool is disabled", Success: false}
	}

	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if path == "" || content == "" {
		return ToolResult{Output: "Missing required arguments: path, content", Success: false}
	}

	absPath, err := r.resolveAndValidatePath(path)
	if err != nil {
		return ToolResult{Output: err.Error(), Success: false}
	}

	// If confirmation is required, store as pending and return
	if r.cfg.WriteFile.RequireConfirmation {
		argsJSON, _ := json.Marshal(args)
		r.db.SetPendingConfirmation(phone, "write_file", string(argsJSON))
		return ToolResult{
			Output:  fmt.Sprintf("⚠️ *Write Confirmation Required*\n\nFile: `%s`\nSize: %d bytes\n\nReply *YES* to confirm or *NO* to cancel.", path, len(content)),
			Success: true,
		}
	}

	return r.executeWrite(absPath, content)
}

// ExecuteWrite performs the actual file write (called after confirmation).
func (r *Registry) ExecuteWrite(absPath, content string) ToolResult {
	return r.executeWrite(absPath, content)
}

func (r *Registry) executeWrite(absPath, content string) ToolResult {
	// Backup original if it exists
	if r.cfg.WriteFile.BackupOriginal {
		if _, err := os.Stat(absPath); err == nil {
			backupDir := filepath.Join(filepath.Dir(absPath), ".backup")
			os.MkdirAll(backupDir, 0755)
			backupPath := filepath.Join(backupDir, filepath.Base(absPath)+"."+time.Now().Format("20060102-150405"))
			orig, err := os.ReadFile(absPath)
			if err == nil {
				os.WriteFile(backupPath, orig, 0644)
			}
		}
	}

	// Ensure directory exists
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return ToolResult{Output: fmt.Sprintf("Error creating directory: %v", err), Success: false}
	}

	// Atomic write: write to temp file, then rename
	tmpFile := absPath + ".tmp"
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		return ToolResult{Output: fmt.Sprintf("Error writing file: %v", err), Success: false}
	}
	if err := os.Rename(tmpFile, absPath); err != nil {
		os.Remove(tmpFile)
		return ToolResult{Output: fmt.Sprintf("Error finalizing write: %v", err), Success: false}
	}

	return ToolResult{Output: fmt.Sprintf("✅ File written: %s (%d bytes)", absPath, len(content)), Success: true}
}

// --- list_directory ---

func (r *Registry) listDirectory(args map[string]any) ToolResult {
	if !r.cfg.ListDirectory.Enabled {
		return ToolResult{Output: "list_directory tool is disabled", Success: false}
	}

	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}

	absPath, err := r.resolveAndValidatePath(path)
	if err != nil {
		return ToolResult{Output: err.Error(), Success: false}
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return ToolResult{Output: fmt.Sprintf("Error listing directory: %v", err), Success: false}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📁 %s\n\n", absPath))

	for _, entry := range entries {
		info, _ := entry.Info()
		if entry.IsDir() {
			sb.WriteString(fmt.Sprintf("📂 %s/\n", entry.Name()))
		} else if info != nil {
			size := formatSize(info.Size())
			sb.WriteString(fmt.Sprintf("📄 %s (%s)\n", entry.Name(), size))
		} else {
			sb.WriteString(fmt.Sprintf("📄 %s\n", entry.Name()))
		}
	}

	if len(entries) == 0 {
		sb.WriteString("(empty directory)")
	}

	return ToolResult{Output: sb.String(), Success: true}
}

// --- run_command ---

func (r *Registry) runCommand(ctx context.Context, args map[string]any) ToolResult {
	if !r.cfg.RunCommand.Enabled {
		return ToolResult{Output: "run_command tool is disabled", Success: false}
	}

	command, _ := args["command"].(string)
	if command == "" {
		return ToolResult{Output: "Missing required argument: command", Success: false}
	}

	// Security check based on mode
	if err := r.validateCommand(command); err != nil {
		return ToolResult{Output: fmt.Sprintf("🚫 Command blocked: %v", err), Success: false}
	}

	timeout := r.cfg.RunCommand.GetTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = r.workspaceRoot

	// Capture output
	output, err := cmd.CombinedOutput()

	// Truncate output if too large
	maxOutput := 32 * 1024 // 32KB
	outputStr := string(output)
	if len(outputStr) > maxOutput {
		outputStr = outputStr[:maxOutput] + "\n\n[... output truncated at 32KB ...]"
	}

	if ctx.Err() != nil {
		return ToolResult{
			Output:  fmt.Sprintf("⏱️ Command timed out after %v\n\nPartial output:\n%s", timeout, outputStr),
			Success: false,
		}
	}

	if err != nil {
		return ToolResult{
			Output:  fmt.Sprintf("Command exited with error: %v\n\nOutput:\n%s", err, outputStr),
			Success: false,
		}
	}

	if outputStr == "" {
		outputStr = "(no output)"
	}

	return ToolResult{Output: outputStr, Success: true}
}

func (r *Registry) validateCommand(command string) error {
	cmdLower := strings.ToLower(strings.TrimSpace(command))

	switch r.cfg.RunCommand.Mode {
	case "allowlist":
		for _, allowed := range r.cfg.RunCommand.Allowlist {
			if strings.HasPrefix(cmdLower, strings.ToLower(allowed)) {
				return nil
			}
		}
		return fmt.Errorf("command not in allowlist. Allowed prefixes: %s", strings.Join(r.cfg.RunCommand.Allowlist, ", "))

	case "blocklist":
		for _, blocked := range r.cfg.RunCommand.Blocklist {
			if strings.Contains(cmdLower, strings.ToLower(blocked)) {
				return fmt.Errorf("command matches blocklist pattern: %s", blocked)
			}
		}
		return nil

	case "unrestricted":
		return nil

	default:
		return fmt.Errorf("unknown command mode: %s", r.cfg.RunCommand.Mode)
	}
}

// --- Helpers ---

// resolveAndValidatePath resolves a path relative to workspace root
// and ensures it doesn't escape the workspace via path traversal.
func (r *Registry) resolveAndValidatePath(path string) (string, error) {
	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		absPath = filepath.Clean(filepath.Join(r.workspaceRoot, path))
	}

	// Ensure the resolved path is within workspace
	if !strings.HasPrefix(absPath, r.workspaceRoot) {
		return "", fmt.Errorf("🚫 path escapes workspace: %s (workspace: %s)", path, r.workspaceRoot)
	}

	return absPath, nil
}

// ResolveAndValidatePath is the public version for use by the handler.
func (r *Registry) ResolveAndValidatePath(path string) (string, error) {
	return r.resolveAndValidatePath(path)
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
