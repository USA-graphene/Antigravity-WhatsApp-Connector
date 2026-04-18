package main

import (
	_ "modernc.org/sqlite" // pure-Go sqlite, no CGO

	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	genai "github.com/google/generative-ai-go/genai"
	qrterminal "github.com/mdp/qrterminal/v3"
	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/api/option"
)

// ── Config ───────────────────────────────────────────────────────────────────

const (
	allowedPhone = "+18035046509"
	authPIN      = "314159"
	workspace    = "/Users/raimis/aa"
	sessionDB    = "./data/whatsapp.db"
	geminiModel  = "gemini-2.5-flash"
)

const systemPrompt = `You are an AI coding assistant with full remote access to the user's Mac.

Workspace: /Users/raimis/aa
Project: Next.js 16 website for usa-graphene.com (TypeScript, Sanity CMS, Tailwind CSS)

You have tools to read/write files, run shell commands, and list directories.
The user is on their phone, controlling this Mac remotely via WhatsApp.

Rules:
- Always COMPLETE the task, never just give advice
- Run commands and make changes directly
- After finishing, send a short summary: what you did + outcome
- If something fails, explain why and what you tried
- Keep responses concise (phone screen)
- Use plain text, avoid heavy markdown

BLOG POST RULES — apply every time you use publish_blog_post:
- Word count: STRICTLY 1500 to 2000 words in the body. Count carefully.
- Structure: Introduction → 4-6 detailed sections with informative headings → Conclusion
- Each paragraph must be fully developed: 4-8 sentences, no truncating
- Tone: authoritative, educational, engaging — for engineers and curious readers
- Separate every paragraph with a blank line (double newline) in the body field
- seoTitle: max 60 chars, keyword-rich
- seoDescription: max 160 chars, compelling and click-worthy
- imagePrompt: vivid, hyper-realistic, cinematic, specific to the article topic

SEO WRITING RULES — mandatory for every blog post:
- Identify 1 primary keyword and 3-5 LSI (related) keywords from the topic
- Place primary keyword in: first sentence of intro, at least 2 section headings, conclusion
- Natural keyword density: primary keyword appears every 200-300 words (no stuffing)
- Use LSI keywords naturally throughout to signal topic depth to search engines
- First paragraph must answer "what is this about" in 2 sentences — hooks readers and crawlers
- Every section heading must be descriptive and keyword-rich (not generic like "Section 1")
- Include statistical facts, research citations, or real-world examples in every section
- Write for featured snippets: include at least one concise 40-60 word direct answer block
- Use active voice, short sentences mixed with longer ones for readability (Flesch score 60+)
- Conclusion must include a call-to-action relevant to graphene/usa-graphene.com
- Never repeat the same sentence structure twice in a row — vary rhythm
- The website is usa-graphene.com — articles must establish it as an authority on graphene`

// Shell commands that are never allowed
var blockedPrefixes = []string{
	"rm -rf /", "sudo rm -rf", "mkfs", "dd if=/dev", ":(){", "shutdown", "reboot",
}

// loadSkills reads all .md files from the skills/ directory and returns them
// as formatted context to append to the system prompt.
func loadSkills(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	var loaded []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		// Strip YAML frontmatter (--- ... ---)
		content := string(data)
		if strings.HasPrefix(content, "---") {
			if end := strings.Index(content[3:], "---"); end != -1 {
				content = strings.TrimSpace(content[3+end+3:])
			}
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
		loaded = append(loaded, strings.TrimSuffix(e.Name(), ".md"))
	}
	if len(loaded) == 0 {
		return ""
	}
	slog.Default().Info("loaded skills", "skills", strings.Join(loaded, ", "))
	return "\n\n── LOADED SKILLS ──\nYou have the following skills available. Use them when relevant.\n\n" + sb.String()
}

// ── Types ────────────────────────────────────────────────────────────────────

type Bridge struct {
	wm            *whatsmeow.Client
	geminiClient  *genai.Client
	model         *genai.GenerativeModel
	sessions      map[string]*genai.ChatSession
	sessionsMu    sync.Mutex
	authenticated map[string]bool
	authMu        sync.RWMutex
	ownJID        string
	log           *slog.Logger
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	log := slog.Default()

	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "ERROR: GEMINI_API_KEY environment variable is required")
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║   🚀 Antigravity WhatsApp Bridge                 ║")
	fmt.Println("║   Full AI agent — responds on your phone         ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	os.MkdirAll(filepath.Dir(sessionDB), 0700)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ── Init Gemini ──────────────────────────────────────────────────────────
	geminiClient, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		log.Error("Gemini init failed", "err", err)
		os.Exit(1)
	}
	defer geminiClient.Close()

	// Load skills from ./skills/ and append to system prompt
	fullPrompt := systemPrompt + loadSkills("./skills")

	model := geminiClient.GenerativeModel(geminiModel)
	model.SystemInstruction = &genai.Content{
		Parts: []genai.Part{genai.Text(fullPrompt)},
	}
	model.Tools = agentTools()
	model.ToolConfig = &genai.ToolConfig{
		FunctionCallingConfig: &genai.FunctionCallingConfig{
			Mode: genai.FunctionCallingAuto,
		},
	}

	b := &Bridge{
		geminiClient:  geminiClient,
		model:         model,
		sessions:      make(map[string]*genai.ChatSession),
		authenticated: make(map[string]bool),
		log:           log,
	}

	// ── Init WhatsApp ────────────────────────────────────────────────────────
	name := "Antigravity Bridge"
	store.DeviceProps.Os = &name

	dbLog := waLog.Stdout("DB", "ERROR", true)
	container, err := sqlstore.New(ctx, "sqlite", "file:"+sessionDB+"?_pragma=foreign_keys(1)", dbLog)
	if err != nil {
		log.Error("DB init failed", "err", err)
		os.Exit(1)
	}
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Error("device store failed", "err", err)
		os.Exit(1)
	}

	wmLog := waLog.Stdout("WA", "ERROR", true)
	b.wm = whatsmeow.NewClient(deviceStore, wmLog)
	b.wm.AddEventHandler(b.handleEvent)

	if b.wm.Store.ID == nil {
		if err := b.onboard(ctx); err != nil {
			log.Error("onboarding failed", "err", err)
			os.Exit(1)
		}
	} else {
		b.ownJID = b.wm.Store.ID.User
		log.Info("reconnecting...", "jid", b.ownJID)
		if err := b.wm.Connect(); err != nil {
			log.Error("connect failed", "err", err)
			os.Exit(1)
		}
	}

	time.Sleep(2 * time.Second)

	if b.ownJID != "" {
		selfJID := types.NewJID(b.ownJID, types.DefaultUserServer)
		b.send(selfJID, fmt.Sprintf(
			"🟢 Antigravity Bridge online\nModel: %s\nWorkspace: %s\n\nSend PIN to start: %s",
			geminiModel, workspace, authPIN))
	}

	log.Info("bridge running", "model", geminiModel, "workspace", workspace)
	<-ctx.Done()
	log.Info("shutting down...")
	b.wm.Disconnect()
}

// ── WhatsApp pairing ──────────────────────────────────────────────────────────

func (b *Bridge) onboard(ctx context.Context) error {
	fmt.Println("📱 Scan QR code: WhatsApp → Settings → Linked Devices → Link a Device")
	qrChan, _ := b.wm.GetQRChannel(ctx)
	if err := b.wm.Connect(); err != nil {
		return err
	}
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			qrPath := "data/whatsapp_qr.png"
			if err := qrcode.WriteFile(evt.Code, qrcode.Medium, 512, qrPath); err == nil {
				abs, _ := filepath.Abs(qrPath)
				exec.Command("open", abs).Start()
				fmt.Printf("\n📷 QR saved: %s\n⏳ Waiting for scan...\n", abs)
			}
		case "success":
			fmt.Println("\n✅ Paired!")
			if b.wm.Store.ID != nil {
				b.ownJID = b.wm.Store.ID.User
			}
		}
	}
	return nil
}

// ── Event handler ─────────────────────────────────────────────────────────────

func (b *Bridge) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		go b.handleMessage(v) // non-blocking
	case *events.Connected:
		b.log.Info("WhatsApp connected")
	case *events.Disconnected:
		b.log.Warn("WhatsApp disconnected")
	}
}

func (b *Bridge) handleMessage(evt *events.Message) {
	// Skip group messages
	if evt.Info.IsGroup {
		return
	}

	// Determine phone + reply target
	var phone string
	var replyTo types.JID

	if evt.Info.IsFromMe {
		if b.ownJID == "" && b.wm.Store.ID != nil {
			b.ownJID = b.wm.Store.ID.User
		}
		if b.ownJID == "" {
			return
		}
		phone = "+" + b.ownJID
		replyTo = types.NewJID(b.ownJID, types.DefaultUserServer)
	} else {
		phone = "+" + evt.Info.Sender.User
		replyTo = types.NewJID(evt.Info.Sender.User, types.DefaultUserServer)
	}

	// Only allow the configured phone
	if phone != allowedPhone {
		b.log.Info("rejected unauthorized phone", "phone", phone)
		return
	}

	text := extractText(evt.Message)
	if text == "" {
		return
	}

	// Skip our own outgoing messages
	if evt.Info.IsFromMe && isOutgoing(text) {
		return
	}

	b.log.Info("message", "phone", phone, "text", text)

	// ── Auth gate ──────────────────────────────────────────────────────────
	b.authMu.RLock()
	authed := b.authenticated[phone]
	b.authMu.RUnlock()

	if !authed {
		if strings.TrimSpace(text) == authPIN {
			b.authMu.Lock()
			b.authenticated[phone] = true
			b.authMu.Unlock()
			b.send(replyTo, "✅ Authenticated!\n\nReady. Send any task and I'll execute it.\n\n/help for commands")
		} else {
			b.send(replyTo, "🔐 Send your PIN to unlock.")
		}
		return
	}

	// ── Built-in commands ─────────────────────────────────────────────────
	switch strings.TrimSpace(text) {
	case "/help":
		b.send(replyTo, helpText())
		return
	case "/status":
		b.send(replyTo, fmt.Sprintf("🟢 Online\nModel: %s\nWorkspace: %s", geminiModel, workspace))
		return
	case "/reset":
		b.sessionsMu.Lock()
		delete(b.sessions, phone)
		b.sessionsMu.Unlock()
		b.send(replyTo, "🔄 Conversation reset.")
		return
	case "/logout":
		b.authMu.Lock()
		delete(b.authenticated, phone)
		b.authMu.Unlock()
		b.send(replyTo, "👋 Logged out.")
		return
	}

	// ── Run AI agent ───────────────────────────────────────────────────────
	b.send(replyTo, "⏳ Working...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Send periodic heartbeat so user knows we're still working
	heartbeat := time.AfterFunc(2*time.Minute, func() {
		b.send(replyTo, "⏳ Still working on it...")
	})
	heartbeat2 := time.AfterFunc(5*time.Minute, func() {
		b.send(replyTo, "⏳ This is a big task, almost there...")
	})
	defer heartbeat.Stop()
	defer heartbeat2.Stop()

	response, err := b.runAgent(ctx, phone, text)
	heartbeat.Stop()
	heartbeat2.Stop()
	if err != nil {
		b.send(replyTo, fmt.Sprintf("❌ Error: %v", err))
		return
	}

	// Send response (chunked if long)
	for _, chunk := range chunkMessage(response, 3800) {
		b.send(replyTo, chunk)
	}
}

// ── AI Agent with tool calling ────────────────────────────────────────────────

func (b *Bridge) runAgent(ctx context.Context, phone, message string) (string, error) {
	session := b.getSession(phone)

	resp, err := session.SendMessage(ctx, genai.Text(message))
	if err != nil {
		return "", fmt.Errorf("Gemini: %w", err)
	}

	// Tool calling loop — Gemini may call tools multiple times
	for {
		if len(resp.Candidates) == 0 {
			return "", fmt.Errorf("no response from Gemini")
		}

		content := resp.Candidates[0].Content
		if content == nil {
			return "", fmt.Errorf("empty content from Gemini")
		}

		// Collect function calls from this response
		var calls []genai.FunctionCall
		var textParts []string

		for _, part := range content.Parts {
			switch v := part.(type) {
			case genai.FunctionCall:
				calls = append(calls, v)
			case genai.Text:
				if s := strings.TrimSpace(string(v)); s != "" {
					textParts = append(textParts, s)
				}
			}
		}

		// No more tool calls — return final text
		if len(calls) == 0 {
			return strings.Join(textParts, "\n"), nil
		}

		// Execute tools and collect responses
		b.log.Info("tool calls", "count", len(calls))
		var responses []genai.Part
		for _, call := range calls {
			b.log.Info("executing tool", "name", call.Name, "args", call.Args)
			result := b.executeTool(call.Name, call.Args)
			responses = append(responses, genai.FunctionResponse{
				Name:     call.Name,
				Response: result,
			})
		}

		// Send tool results back to Gemini
		resp, err = session.SendMessage(ctx, responses...)
		if err != nil {
			return "", fmt.Errorf("tool response: %w", err)
		}
	}
}

func (b *Bridge) getSession(phone string) *genai.ChatSession {
	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()
	if s, ok := b.sessions[phone]; ok {
		return s
	}
	s := b.model.StartChat()
	b.sessions[phone] = s
	return s
}

// ── Tool definitions ──────────────────────────────────────────────────────────

func agentTools() []*genai.Tool {
	str := func(desc string) *genai.Schema { return &genai.Schema{Type: genai.TypeString, Description: desc} }
	obj := func(props map[string]*genai.Schema, required ...string) *genai.Schema {
		return &genai.Schema{Type: genai.TypeObject, Properties: props, Required: required}
	}

	return []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        "read_file",
				Description: "Read the contents of a file. Use absolute path or relative to workspace.",
				Parameters:  obj(map[string]*genai.Schema{"path": str("File path to read")}, "path"),
			},
			{
				Name:        "write_file",
				Description: "Write or overwrite a file with new content.",
				Parameters: obj(map[string]*genai.Schema{
					"path":    str("File path to write"),
					"content": str("Full file content"),
				}, "path", "content"),
			},
			{
				Name:        "run_shell",
				Description: "Run a shell command. Working directory defaults to workspace. Returns stdout+stderr.",
				Parameters: obj(map[string]*genai.Schema{
					"command": str("Shell command to execute"),
					"cwd":     str("Working directory (optional, defaults to workspace)"),
				}, "command"),
			},
			{
				Name:        "list_dir",
				Description: "List files and directories at a path.",
				Parameters:  obj(map[string]*genai.Schema{"path": str("Directory path to list")}, "path"),
			},
			{
				Name:        "find_files",
				Description: "Find files matching a pattern using find/grep.",
				Parameters: obj(map[string]*genai.Schema{
					"pattern": str("Search pattern or filename"),
					"dir":     str("Directory to search in (optional, defaults to workspace)"),
				}, "pattern"),
			},
			{
				Name:        "publish_blog_post",
				Description: "Publish a full blog post to the website's Sanity CMS. The post will be strictly formatted and immediately live.",
				Parameters: obj(map[string]*genai.Schema{
					"title":       str("The title of the blog post"),
					"slug":        str("A URL friendly slug (e.g., how-to-use-graphene)"),
					"excerpt":     str("A brief, two-sentence summary of the post"),
					"body":        str("The full contents of the post in plaintext or markdown-style text"),
					"imagePrompt":    str("A detailed prompt to generate the article cover image (e.g., 'A hyperrealistic futuristic city with graphene roofs')"),
					"categoryId":     str("The Sanity category ID. IDs: Innovation (7BrkOiqqnrTDYuPuTq0PcD), Science (7QyVE6fI6HWfwHJOF8VGju), Education (7QyVE6fI6HWfwHJOF8VJiA), Graphene Applications (3c7c1ec6-d835-49e0-b0f2-1c2873c6d86c), Material Science (h24s9UzTAZYMYBX6aDftBN)"),
					"seoTitle":       str("SEO optimized title, max 60 chars"),
					"seoDescription": str("SEO optimized description, max 160 chars"),
				}, "title", "slug", "excerpt", "body", "imagePrompt", "categoryId", "seoTitle", "seoDescription"),
			},
		},
	}}
}

// ── Tool execution ────────────────────────────────────────────────────────────

func (b *Bridge) executeTool(name string, args map[string]any) map[string]any {
	str := func(key string) string {
		if v, ok := args[key].(string); ok {
			return v
		}
		return ""
	}

	switch name {
	case "read_file":
		return b.toolReadFile(str("path"))
	case "write_file":
		return b.toolWriteFile(str("path"), str("content"))
	case "run_shell":
		cwd := str("cwd")
		if cwd == "" {
			cwd = workspace
		}
		return b.toolRunShell(str("command"), cwd)
	case "list_dir":
		return b.toolListDir(str("path"))
	case "find_files":
		dir := str("dir")
		if dir == "" {
			dir = workspace
		}
		return b.toolFindFiles(str("pattern"), dir)
	case "publish_blog_post":
		return b.toolPublishBlogPost(str("title"), str("slug"), str("excerpt"), str("body"), str("imagePrompt"), str("categoryId"), str("seoTitle"), str("seoDescription"))
	default:
		return result("", fmt.Sprintf("unknown tool: %s", name))
	}
}

func (b *Bridge) toolReadFile(path string) map[string]any {
	path = resolvePath(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return result("", err.Error())
	}
	content := string(data)
	// Truncate very large files
	if len(content) > 50000 {
		content = content[:50000] + "\n... (truncated)"
	}
	return result(content, "")
}

func (b *Bridge) toolWriteFile(path, content string) map[string]any {
	path = resolvePath(path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return result("", err.Error())
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return result("", err.Error())
	}
	b.log.Info("wrote file", "path", path, "bytes", len(content))
	return result(fmt.Sprintf("wrote %d bytes to %s", len(content), path), "")
}

func (b *Bridge) toolRunShell(command, cwd string) map[string]any {
	// Safety check
	lower := strings.ToLower(command)
	for _, blocked := range blockedPrefixes {
		if strings.Contains(lower, strings.ToLower(blocked)) {
			return result("", fmt.Sprintf("blocked command: %s", blocked))
		}
	}

	b.log.Info("running command", "cmd", command, "cwd", cwd)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "PATH=/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin")

	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return result(output, "command timed out after 60s")
		}
		// Return output even on error (e.g. test failures with output)
		return result(output, err.Error())
	}
	return result(output, "")
}

func (b *Bridge) toolListDir(path string) map[string]any {
	if path == "" {
		path = workspace
	}
	path = resolvePath(path)

	entries, err := os.ReadDir(path)
	if err != nil {
		return result("", err.Error())
	}

	var lines []string
	for _, e := range entries {
		info, _ := e.Info()
		if e.IsDir() {
			lines = append(lines, fmt.Sprintf("📁 %s/", e.Name()))
		} else {
			size := ""
			if info != nil {
				size = fmt.Sprintf(" (%d bytes)", info.Size())
			}
			lines = append(lines, fmt.Sprintf("📄 %s%s", e.Name(), size))
		}
	}
	return result(strings.Join(lines, "\n"), "")
}

func (b *Bridge) toolFindFiles(pattern, dir string) map[string]any {
	dir = resolvePath(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c",
		fmt.Sprintf(`find %q -name %q -not -path "*/node_modules/*" -not -path "*/.next/*" -not -path "*/.git/*" | head -50`,
			dir, pattern))
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Try grep fallback
		cmd2 := exec.CommandContext(ctx, "bash", "-c",
			fmt.Sprintf(`grep -r %q %q --include="*.ts" --include="*.tsx" --include="*.js" -l 2>/dev/null | grep -v node_modules | head -20`,
				pattern, dir))
		out2, _ := cmd2.CombinedOutput()
		return result(strings.TrimSpace(string(out2)), "")
	}
	return result(strings.TrimSpace(string(out)), "")
}

func (b *Bridge) toolPublishBlogPost(title, slug, excerpt, body, imagePrompt, categoryId, seoTitle, seoDescription string) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sanityToken := "REDACTED"
	projectId := "t9t7is4j"

	// 1. Fetch image from pollinations.ai
	imageReqUrl := fmt.Sprintf("https://image.pollinations.ai/prompt/%s?nologo=true&width=1024&height=1024", neturl.QueryEscape(imagePrompt))
	imgResp, err := http.Get(imageReqUrl)
	if err != nil {
		return result("", fmt.Sprintf("Failed to generate image: %v", err))
	}
	defer imgResp.Body.Close()

	// 2. Upload image to Sanity
	imgUploadUrl := fmt.Sprintf("https://%s.api.sanity.io/v2023-05-03/assets/images/production", projectId)
	imgReq, _ := http.NewRequestWithContext(ctx, "POST", imgUploadUrl, imgResp.Body)
	imgReq.Header.Set("Content-Type", "image/jpeg")
	imgReq.Header.Set("Authorization", "Bearer "+sanityToken)

	sanityImgResp, err := http.DefaultClient.Do(imgReq)
	if err != nil {
		return result("", fmt.Sprintf("Failed to upload image to Sanity: %v", err))
	}
	defer sanityImgResp.Body.Close()

	var imgData struct {
		Document struct {
			ID string `json:"_id"`
		} `json:"document"`
	}
	if err := json.NewDecoder(sanityImgResp.Body).Decode(&imgData); err != nil || imgData.Document.ID == "" {
		return result("", fmt.Sprintf("Failed to parse Sanity image response"))
	}
	imageId := imgData.Document.ID

	// 3. Format body paragraphs into Sanity blocks and inject _key
	paragraphs := strings.Split(body, "\n\n")
	var blocks []map[string]any
	for i, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		
		// Create unique keys for Sanity lists
		blockKey := fmt.Sprintf("block-%d-%d", time.Now().UnixNano(), i)
		spanKey := fmt.Sprintf("span-%d-%d", time.Now().UnixNano(), i)

		blocks = append(blocks, map[string]any{
			"_type": "block",
			"_key":  blockKey,
			"style": "normal",
			"children": []map[string]any{
				{"_type": "span", "_key": spanKey, "text": p, "marks": []string{}},
			},
		})
	}

	// 4. Construct final Sanity mutation
	authorId := "0c2b4f1a-4e5d-446f-87f4-55e49cf585d7" // Raimundas Juodvalkis

	postObj := map[string]any{
		"_type":          "post",
		"title":          title,
		"slug":           map[string]any{"_type": "slug", "current": slug},
		"excerpt":        excerpt,
		"body":           blocks,
		"seoTitle":       seoTitle,
		"seoDescription": seoDescription,
		"publishedAt":    time.Now().UTC().Format(time.RFC3339),
		"author":         map[string]any{"_type": "reference", "_ref": authorId},
		"mainImage": map[string]any{
			"_type": "image",
			"asset": map[string]any{"_type": "reference", "_ref": imageId},
		},
	}

	if categoryId != "" {
		postObj["categories"] = []map[string]any{
			{"_type": "reference", "_ref": categoryId, "_key": "cat1"},
		}
	}

	payload := map[string]any{
		"mutations": []map[string]any{
			{"create": postObj},
		},
	}

	payloadBytes, _ := json.Marshal(payload)

	req, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("https://%s.api.sanity.io/v2023-05-03/data/mutate/production", projectId), strings.NewReader(string(payloadBytes)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sanityToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return result("", fmt.Sprintf("Sanity final API Error: %v", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result("", fmt.Sprintf("Sanity final API returned status %d", resp.StatusCode))
	}
	return result(fmt.Sprintf("Blog post '%s' with generated header image successfully published to Sanity CMS!", title), "")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func resolvePath(path string) string {
	if path == "" || path == "." {
		return workspace
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workspace, path)
}

func result(output, errMsg string) map[string]any {
	r := map[string]any{"output": output}
	if errMsg != "" {
		r["error"] = errMsg
	}
	return r
}

func (b *Bridge) send(jid types.JID, text string) {
	_, err := b.wm.SendMessage(context.Background(), jid, &waE2E.Message{
		Conversation: &text,
	})
	if err != nil {
		b.log.Error("send failed", "jid", jid, "err", err)
	}
}

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

var outgoingPrefixes = []string{"🟢", "✅", "❌", "⏳", "🔐", "👋", "🔄", "/help", "Model:"}

func isOutgoing(text string) bool {
	for _, p := range outgoingPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

func chunkMessage(text string, max int) []string {
	if len(text) <= max {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= max {
			chunks = append(chunks, text)
			break
		}
		cut := max
		if nl := strings.LastIndex(text[:cut], "\n"); nl > cut/2 {
			cut = nl + 1
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return chunks
}

func helpText() string {
	return `🤖 Antigravity Remote Bridge

Send any task in plain English:
"fix the redirect bug in proxy.ts"
"push everything to github"
"what's the build error?"
"add a new blog post about X"

Commands:
/status  — bridge status
/reset   — clear conversation
/logout  — end session
/help    — this message`
}

// keep json imported for potential future use
var _ = json.Marshal
