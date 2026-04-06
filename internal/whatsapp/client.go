package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/agent"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/auth"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/config"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/gateway"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/storage"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/tools"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Client wraps the whatsmeow client with application logic.
type Client struct {
	wm          *whatsmeow.Client
	cfg         *config.Config
	authEngine  *auth.Engine
	agentClient *agent.Agent
	router      *gateway.Router
	rateLimiter *gateway.RateLimiter
	registry    *tools.Registry
	db          *storage.DB
	log         *slog.Logger
}

// New creates and connects a new WhatsApp client.
func New(ctx context.Context, cfg *config.Config, db *storage.DB, agentClient *agent.Agent, registry *tools.Registry) (*Client, error) {
	log := slog.Default().With("component", "whatsapp")

	// Initialize WhatsApp session store
	dbLog := waLog.Stdout("WA-DB", "WARN", true)
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+cfg.WhatsApp.SessionDB+"?_foreign_keys=on", dbLog)
	if err != nil {
		return nil, fmt.Errorf("creating WhatsApp session store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting device store: %w", err)
	}

	clientLog := waLog.Stdout("WA-Client", "WARN", true)
	wmClient := whatsmeow.NewClient(deviceStore, clientLog)

	c := &Client{
		wm:          wmClient,
		cfg:         cfg,
		authEngine:  auth.NewEngine(&cfg.Auth, db, log),
		agentClient: agentClient,
		router:      gateway.NewRouter(log),
		rateLimiter: gateway.NewRateLimiter(cfg.RateLimit.PerUser, cfg.RateLimit.Global, log),
		registry:    registry,
		db:          db,
		log:         log,
	}

	// Register event handler
	wmClient.AddEventHandler(c.handleEvent)

	// Connect
	if wmClient.Store.ID == nil {
		// First time - need QR code pairing
		log.Info("no existing session found, starting QR pairing...")
		qrChan, _ := wmClient.GetQRChannel(ctx)

		if err := wmClient.Connect(); err != nil {
			return nil, fmt.Errorf("connecting to WhatsApp: %w", err)
		}

		for evt := range qrChan {
			switch evt.Event {
			case "code":
				fmt.Println("\n╔══════════════════════════════════════════╗")
				fmt.Println("║   Scan this QR code with WhatsApp       ║")
				fmt.Println("║   Settings > Linked Devices > Link      ║")
				fmt.Println("╚══════════════════════════════════════════╝")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				fmt.Println()
			case "success":
				log.Info("WhatsApp paired successfully!")
			case "timeout":
				return nil, fmt.Errorf("QR code timed out. Please restart")
			}
		}
	} else {
		// Existing session - reconnect
		log.Info("reconnecting with existing session...")
		if err := wmClient.Connect(); err != nil {
			return nil, fmt.Errorf("connecting to WhatsApp: %w", err)
		}
	}

	log.Info("WhatsApp client connected",
		"jid", wmClient.Store.ID,
	)
	return c, nil
}

// Disconnect cleanly disconnects the WhatsApp client.
func (c *Client) Disconnect() {
	c.wm.Disconnect()
}

// handleEvent processes incoming WhatsApp events.
func (c *Client) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		c.handleMessage(v)
	case *events.Connected:
		c.log.Info("WhatsApp connected")
	case *events.Disconnected:
		c.log.Warn("WhatsApp disconnected")
	case *events.StreamReplaced:
		c.log.Warn("WhatsApp stream replaced (logged in elsewhere)")
	}
}

// handleMessage processes an incoming WhatsApp message.
func (c *Client) handleMessage(evt *events.Message) {
	// Ignore our own messages
	if evt.Info.IsFromMe {
		return
	}

	// Only handle individual chats (not groups)
	if evt.Info.IsGroup {
		return
	}

	// Extract text
	msg := evt.Message
	text := extractText(msg)
	if text == "" {
		return
	}

	phone := evt.Info.Sender.User
	// Normalize phone format
	if !strings.HasPrefix(phone, "+") {
		phone = "+" + phone
	}

	c.log.Info("incoming message",
		"phone", phone,
		"length", len(text),
	)

	// Rate limit check
	if !c.rateLimiter.Allow(phone) {
		c.sendMessage(evt.Info.Sender, "⏳ Rate limit reached. Please wait a moment.")
		return
	}

	// Auth check
	if !c.authEngine.IsAuthenticated(phone) {
		result := c.authEngine.CheckAccess(phone)
		if !result.Allowed {
			if result.Message != "" {
				// Need PIN
				pinResult := c.authEngine.TryPIN(phone, text)
				if pinResult.Message != "" {
					c.sendMessage(evt.Info.Sender, pinResult.Message)
				}
			}
			return
		}
	}

	// Route the message
	route := c.router.Route(text)

	if route.IsCommand {
		c.handleCommand(evt.Info.Sender, phone, route.CommandName, route.CommandArgs)
		return
	}

	// Regular chat message - send to AI
	c.handleChat(evt.Info.Sender, phone, text)
}

// handleCommand processes slash commands.
func (c *Client) handleCommand(jid types.JID, phone, cmd, args string) {
	switch cmd {
	case "/help":
		c.sendMessage(jid, `🤖 *Antigravity WhatsApp Connector*

*Commands:*
/help — Show this help
/status — Show connection status
/reset — Clear conversation history
/logout — End session

*Tools Available:*
📖 File reading
📝 File writing (with confirmation)
⚡ Command execution (allowlist)
📁 Directory listing

Just type naturally to interact with the AI.`)

	case "/status":
		c.sendMessage(jid, fmt.Sprintf(`📊 *Status*

✅ WhatsApp: Connected
✅ Auth: Authenticated
🤖 Model: %s
📁 Workspace: %s`, c.cfg.Gemini.Model, c.cfg.Workspace.Root))

	case "/reset":
		c.agentClient.ClearHistory(phone)
		c.sendMessage(jid, "🔄 Conversation history cleared.")

	case "/logout":
		c.authEngine.Logout(phone)
		c.sendMessage(jid, "👋 Session ended. Send any message to re-authenticate.")

	case "/confirm":
		c.handleConfirmation(jid, phone, args)

	default:
		c.sendMessage(jid, "❓ Unknown command. Type /help for available commands.")
	}
}

// handleConfirmation handles YES/NO responses for pending operations.
func (c *Client) handleConfirmation(jid types.JID, phone, response string) {
	toolName, argsJSON, err := c.db.GetPendingConfirmation(phone)
	if err != nil || toolName == "" {
		// No pending confirmation - treat as regular chat
		c.handleChat(jid, phone, response)
		return
	}

	response = strings.ToUpper(strings.TrimSpace(response))
	if response == "YES" || response == "Y" {
		// Execute the pending write
		var args map[string]any
		json.Unmarshal([]byte(argsJSON), &args)

		path, _ := args["path"].(string)
		content, _ := args["content"].(string)

		absPath, err := c.registry.ResolveAndValidatePath(path)
		if err != nil {
			c.sendMessage(jid, fmt.Sprintf("❌ Error: %v", err))
			return
		}

		result := c.registry.ExecuteWrite(absPath, content)
		c.sendMessage(jid, result.Output)
	} else {
		c.db.DeletePendingConfirmation(phone)
		c.sendMessage(jid, "❌ Write cancelled.")
	}
}

// handleChat sends a message to the AI agent and returns the response.
func (c *Client) handleChat(jid types.JID, phone, text string) {
	// Send typing indicator
	c.wm.SendChatPresence(context.Background(), jid, types.ChatPresenceComposing, types.ChatPresenceMediaText)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	response, err := c.agentClient.Chat(ctx, phone, text)
	if err != nil {
		c.log.Error("agent error", "error", err, "phone", phone)
		c.sendMessage(jid, fmt.Sprintf("⚠️ Error: %v", err))
		return
	}

	// Clear typing indicator
	c.wm.SendChatPresence(context.Background(), jid, types.ChatPresencePaused, types.ChatPresenceMediaText)

	// Chunk long messages for WhatsApp
	chunks := chunkMessage(response, 4000)
	for _, chunk := range chunks {
		c.sendMessage(jid, chunk)
		if len(chunks) > 1 {
			time.Sleep(500 * time.Millisecond) // Small delay between chunks
		}
	}
}

// sendMessage sends a text message to a WhatsApp user.
func (c *Client) sendMessage(jid types.JID, text string) {
	_, err := c.wm.SendMessage(context.Background(), jid, &waE2E.Message{
		Conversation: &text,
	})
	if err != nil {
		c.log.Error("failed to send message", "error", err, "jid", jid)
	}
}

// extractText gets the text content from a WhatsApp message.
func extractText(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	if msg.Conversation != nil {
		return *msg.Conversation
	}
	if msg.ExtendedTextMessage != nil && msg.ExtendedTextMessage.Text != nil {
		return *msg.ExtendedTextMessage.Text
	}
	return ""
}

// chunkMessage splits a long message into WhatsApp-friendly chunks.
func chunkMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	remaining := text

	for len(remaining) > 0 {
		if len(remaining) <= maxLen {
			chunks = append(chunks, remaining)
			break
		}

		// Try to break at a newline
		cutoff := maxLen
		lastNewline := strings.LastIndex(remaining[:cutoff], "\n")
		if lastNewline > cutoff/2 {
			cutoff = lastNewline + 1
		}

		chunks = append(chunks, remaining[:cutoff])
		remaining = remaining[cutoff:]
	}

	return chunks
}
