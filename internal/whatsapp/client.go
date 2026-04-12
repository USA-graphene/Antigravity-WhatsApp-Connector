package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/agent"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/auth"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/config"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/gateway"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/storage"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/tools"
	"github.com/mdp/qrterminal/v3"
	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func init() {
	// Set device name and OS info that appears in "Linked Devices"
	store.DeviceProps.Os = strPtr("Antigravity")
	store.DeviceProps.PlatformType = store.DeviceProps.PlatformType.Enum()
}

func strPtr(s string) *string { return &s }

// Client wraps the whatsmeow client with application logic.
type Client struct {
	wm           *whatsmeow.Client
	cfg          *config.Config
	authEngine   *auth.Engine
	agentClient  *agent.Agent
	router       *gateway.Router
	rateLimiter  *gateway.RateLimiter
	registry     *tools.Registry
	db           *storage.DB
	log          *slog.Logger
	ownJID       string   // Our own phone number from the linked account
	sentMessages sync.Map // Tracks message IDs we sent to prevent self-reply loops
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
		// ==========================================
		//  ONBOARDING - First Time Setup
		// ==========================================
		c.runOnboarding(ctx, wmClient)
	} else {
		// Existing session - reconnect
		log.Info("reconnecting with existing session...")
		if err := wmClient.Connect(); err != nil {
			return nil, fmt.Errorf("connecting to WhatsApp: %w", err)
		}
		c.ownJID = wmClient.Store.ID.User
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	log.Info("WhatsApp client connected",
		"jid", wmClient.Store.ID,
		"own_phone", c.ownJID,
	)

	// Send onboarding message to self
	if c.ownJID != "" {
		selfJID := types.NewJID(c.ownJID, types.DefaultUserServer)
		c.sendMessage(selfJID, "🚀 *Antigravity WhatsApp Connector is online!*\n\n"+
			"✅ Connected and ready\n"+
			"🤖 Model: "+cfg.Gemini.Model+"\n"+
			"📁 Workspace: "+cfg.Workspace.Root+"\n\n"+
			"Type /help for commands, or just start chatting!")
	}

	return c, nil
}

// runOnboarding handles the first-time setup flow.
func (c *Client) runOnboarding(ctx context.Context, wmClient *whatsmeow.Client) error {
	c.log.Info("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	c.log.Info("  FIRST TIME SETUP - WhatsApp Pairing Required")
	c.log.Info("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║           🚀 ANTIGRAVITY SETUP                  ║")
	fmt.Println("║                                                  ║")
	fmt.Println("║  Step 1: Open WhatsApp on your phone             ║")
	fmt.Println("║  Step 2: Go to Settings → Linked Devices         ║")
	fmt.Println("║  Step 3: Tap 'Link a Device'                     ║")
	fmt.Println("║  Step 4: Scan the QR code (opening in Preview)   ║")
	fmt.Println("║                                                  ║")
	fmt.Println("║  Your PIN for authentication: " + c.cfg.Auth.PIN + "              ║")
	fmt.Println("║  Authorized phone: " + strings.Join(c.cfg.Auth.AllowedPhones, ", ") + "    ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	qrChan, _ := wmClient.GetQRChannel(ctx)

	if err := wmClient.Connect(); err != nil {
		return fmt.Errorf("connecting to WhatsApp: %w", err)
	}

	for evt := range qrChan {
		switch evt.Event {
		case "code":
			fmt.Println("📱 QR Code generated! Scan it now...")
			fmt.Println()

			// Print in terminal
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			fmt.Println()

			// Save as PNG and auto-open
			qrPath := filepath.Join("data", "whatsapp_qr.png")
			if err := qrcode.WriteFile(evt.Code, qrcode.Medium, 512, qrPath); err == nil {
				absQR, _ := filepath.Abs(qrPath)
				fmt.Printf("📱 QR code image: %s\n", absQR)
				fmt.Println("   Opening in Preview...")
				exec.Command("open", absQR).Start()
			}

			fmt.Println("\n⏳ Waiting for scan... (expires in ~2 minutes)")

		case "success":
			fmt.Println()
			fmt.Println("╔══════════════════════════════════════════════════╗")
			fmt.Println("║  ✅ PAIRED SUCCESSFULLY!                         ║")
			fmt.Println("║                                                  ║")
			fmt.Println("║  WhatsApp is now linked to Antigravity.          ║")
			fmt.Println("║  The device will appear as 'Antigravity' in      ║")
			fmt.Println("║  your Linked Devices list.                       ║")
			fmt.Println("╚══════════════════════════════════════════════════╝")
			fmt.Println()
			c.ownJID = wmClient.Store.ID.User
			c.log.Info("WhatsApp paired successfully!", "jid", wmClient.Store.ID)

		case "timeout":
			return fmt.Errorf("QR code timed out. Please restart the connector")
		}
	}

	return nil
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
	// Skip messages we sent ourselves (prevents infinite reply loops)
	msgID := evt.Info.ID
	if _, wasSentByUs := c.sentMessages.LoadAndDelete(msgID); wasSentByUs {
		return
	}

	// Skip low-value service/status chatter from our own account.
	// This avoids reacting to onboarding banners, auth acknowledgements,
	// and other connector-generated noise in the self-chat thread.
	if evt.Info.IsFromMe {
		text := extractText(evt.Message)
		if strings.Contains(text, "Antigravity WhatsApp Connector is online") ||
			strings.Contains(text, "Authenticated!") ||
			strings.Contains(text, "Rate limit reached") ||
			strings.Contains(text, "Please enter your PIN") ||
			strings.Contains(text, "Account locked") ||
			strings.Contains(text, "Try again in") {
			return
		}
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

	// Determine the phone number of the user
	var phone string
	var replyJID types.JID

	if evt.Info.IsFromMe {
		// Message sent by the user from their phone.
		// Use our own account JID (the phone number we registered with)
		if c.ownJID != "" {
			phone = c.ownJID
		} else if c.wm.Store.ID != nil {
			phone = c.wm.Store.ID.User
		} else {
			return
		}
		// Reply to the chat where the user sent the message
		replyJID = evt.Info.Chat
	} else {
		// Message from another person
		phone = evt.Info.Sender.User
		replyJID = evt.Info.Sender
	}

	// Normalize phone format
	if !strings.HasPrefix(phone, "+") {
		phone = "+" + phone
	}

	c.log.Info("incoming message",
		"phone", phone,
		"from_me", evt.Info.IsFromMe,
		"chat", evt.Info.Chat.User,
		"length", len(text),
	)

	// Rate limit check
	if !c.rateLimiter.Allow(phone) {
		c.sendMessage(replyJID, "⏳ Rate limit reached. Please wait a moment.")
		return
	}

	// Auth check
	// Auth check - PIN is required for this phone
	if !c.authEngine.IsAuthenticated(phone) {
		result := c.authEngine.CheckAccess(phone)

		if !result.Allowed {
			// If the account is locked, show the lockout message
			if strings.Contains(result.Message, "locked") {
				c.sendMessage(replyJID, result.Message)
				return
			}

			// Only try the message as a PIN if it looks like a PIN (e.g., 6 digits)
			// This prevents random protocol messages or "Hello" from locking the account.
			isPinFormat := len(text) == 6 && isNumeric(text)

			if isPinFormat {
				pinResult := c.authEngine.TryPIN(phone, text)
				c.sendMessage(replyJID, pinResult.Message)
				if pinResult.Allowed {
					// Successfully authenticated!
					return
				}
			} else {
				// Not a PIN format - just send the prompt message
				if result.Message != "" {
					c.sendMessage(replyJID, result.Message)
				}
			}
			return
		}
	}

	// Route the message
	route := c.router.Route(text)

	if route.IsCommand {
		c.handleCommand(replyJID, phone, route.CommandName, route.CommandArgs)
		return
	}

	// Regular chat message - send to AI
	c.handleChat(replyJID, phone, text)
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
	resp, err := c.wm.SendMessage(context.Background(), jid, &waE2E.Message{
		Conversation: &text,
	})
	if err != nil {
		c.log.Error("failed to send message", "error", err, "jid", jid)
		return
	}
	// Track this message ID so we don't respond to our own reply
	c.sentMessages.Store(resp.ID, true)
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

func isNumeric(s string) bool {
	for _, char := range s {
		if !strings.ContainsRune("0123456789", char) {
			return false
		}
	}
	return true
}
