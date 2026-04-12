package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/config"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/storage"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/tools"
	"google.golang.org/genai"
)

const maxHistoryMessages = 40 // Keep last 40 messages in context

// Agent wraps the Gemini client and manages conversations.
type Agent struct {
	client   *genai.Client
	cfg      *config.GeminiConfig
	toolsCfg *config.ToolsConfig
	db       *storage.DB
	registry *tools.Registry
	log      *slog.Logger
}

// New creates a new Gemini-backed agent.
func New(ctx context.Context, cfg *config.GeminiConfig, toolsCfg *config.ToolsConfig, db *storage.DB, registry *tools.Registry, log *slog.Logger) (*Agent, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY is required. Set it in config.yaml or as an environment variable")
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("creating Gemini client: %w", err)
	}

	return &Agent{
		client:   client,
		cfg:      cfg,
		toolsCfg: toolsCfg,
		db:       db,
		registry: registry,
		log:      log,
	}, nil
}

// Chat sends a message and returns the response, handling function calling loops.
func (a *Agent) Chat(ctx context.Context, phone, message string) (string, error) {
	// Save user message
	a.db.SaveMessage(phone, "user", message)

	// Build conversation history
	history, err := a.db.GetMessages(phone, maxHistoryMessages)
	if err != nil {
		a.log.Error("failed to get message history", "error", err)
	}

	// Convert to Gemini content format
	contents := a.buildContents(history)

	// Build tool declarations
	genaiTools := a.buildTools()

	// Configure the model
	modelConfig := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{{Text: systemPrompt}},
		},
		Temperature:     genai.Ptr(a.cfg.Temperature),
		MaxOutputTokens: int32(a.cfg.MaxTokens),
	}
	if len(genaiTools) > 0 {
		modelConfig.Tools = genaiTools
	}

	// Function calling loop (max 5 iterations to prevent infinite loops)
	for i := 0; i < 5; i++ {
		resp, err := a.client.Models.GenerateContent(ctx, a.cfg.Model, contents, modelConfig)
		if err != nil {
			return "", fmt.Errorf("Gemini API error: %w", err)
		}

		if resp == nil || len(resp.Candidates) == 0 {
			return "I didn't get a response. Please try again.", nil
		}

		candidate := resp.Candidates[0]
		if candidate.Content == nil || len(candidate.Content.Parts) == 0 {
			return "Empty response from AI.", nil
		}

		// Check for function calls
		hasFunctionCall := false
		var functionResponses []*genai.Part

		for _, part := range candidate.Content.Parts {
			if part.FunctionCall != nil {
				hasFunctionCall = true
				fc := part.FunctionCall
				a.log.Info("function call",
					"function", fc.Name,
					"args", fc.Args,
					"phone", phone,
				)

				// Execute the tool
				result := a.registry.Execute(ctx, phone, fc.Name, fc.Args)

				functionResponses = append(functionResponses, &genai.Part{
					FunctionResponse: &genai.FunctionResponse{
						Name: fc.Name,
						Response: map[string]any{
							"output":  result.Output,
							"success": result.Success,
						},
					},
				})
			}
		}

		if hasFunctionCall {
			// Add the model's response and the function results to the conversation
			contents = append(contents, candidate.Content)
			contents = append(contents, &genai.Content{
				Role:  "user",
				Parts: functionResponses,
			})
			// Continue the loop - the model will process the function results
			continue
		}

		// No function call - we have a text response
		var textParts []string
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
		}

		response := strings.Join(textParts, "\n")
		if response == "" {
			response = "(No text response generated)"
		}

		// Save assistant response
		a.db.SaveMessage(phone, "assistant", response)

		return response, nil
	}

	return "⚠️ Tool execution loop limit reached. Please try a simpler request.", nil
}

// ClearHistory clears the conversation history for a user.
func (a *Agent) ClearHistory(phone string) {
	a.db.ClearMessages(phone)
}

// buildContents converts stored messages to Gemini content format.
func (a *Agent) buildContents(messages []storage.Message) []*genai.Content {
	var contents []*genai.Content
	for _, msg := range messages {
		role := msg.Role
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, &genai.Content{
			Role:  role,
			Parts: []*genai.Part{{Text: msg.Content}},
		})
	}
	return contents
}

// buildTools creates Gemini function declarations from the tool config.
func (a *Agent) buildTools() []*genai.Tool {
	var declarations []*genai.FunctionDeclaration

	if a.toolsCfg.ReadFile.Enabled {
		declarations = append(declarations, &genai.FunctionDeclaration{
			Name:        "read_file",
			Description: "Read the contents of a file. The path is relative to the workspace root. Returns the file content as text. Use this to examine source code, config files, logs, etc.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"path": {
						Type:        genai.TypeString,
						Description: "The file path to read, relative to the workspace root.",
					},
				},
				Required: []string{"path"},
			},
		})
	}

	if a.toolsCfg.WriteFile.Enabled {
		declarations = append(declarations, &genai.FunctionDeclaration{
			Name:        "write_file",
			Description: "Write content to a file. Creates the file if it doesn't exist. If the file exists, it will be overwritten (with backup). The path is relative to the workspace root. The user may need to confirm this action.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"path": {
						Type:        genai.TypeString,
						Description: "The file path to write to, relative to the workspace root.",
					},
					"content": {
						Type:        genai.TypeString,
						Description: "The full content to write to the file.",
					},
				},
				Required: []string{"path", "content"},
			},
		})
	}

	if a.toolsCfg.ListDirectory.Enabled {
		declarations = append(declarations, &genai.FunctionDeclaration{
			Name:        "list_directory",
			Description: "List the contents of a directory. Shows files and subdirectories with sizes. The path is relative to the workspace root. Use '.' for the current workspace root.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"path": {
						Type:        genai.TypeString,
						Description: "The directory path to list, relative to the workspace root. Use '.' for root.",
					},
				},
				Required: []string{"path"},
			},
		})
	}

	if a.toolsCfg.RunCommand.Enabled {
		modeDesc := "Execute a shell command. You can use absolute paths to run commands in any directory on the user's computer (e.g. 'cd /Users/raimis/aa && npm run ...'). You CAN use this to publish to the web, hit APIs, or run any script!"
		switch a.toolsCfg.RunCommand.Mode {
		case "allowlist":
			modeDesc += " Only pre-approved commands are allowed: " + strings.Join(a.toolsCfg.RunCommand.Allowlist, ", ")
		case "blocklist":
			modeDesc += " Most commands are allowed except dangerous ones."
		}

		declarations = append(declarations, &genai.FunctionDeclaration{
			Name:        "run_command",
			Description: modeDesc,
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"command": {
						Type:        genai.TypeString,
						Description: "The shell command to execute.",
					},
				},
				Required: []string{"command"},
			},
		})
	}

	// Add ECC custom tools unconditionally
	declarations = append(declarations, &genai.FunctionDeclaration{
		Name:        "search_ecc_skills",
		Description: "Search for available Everything-Claude-Code (ECC) skills by name. Use an empty query to list all skills.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"query": {Type: genai.TypeString, Description: "Optional search query to filter skill names."},
			},
		},
	})
	declarations = append(declarations, &genai.FunctionDeclaration{
		Name:        "load_ecc_skill",
		Description: "Load the instructions and parameters for an Everything-Claude-Code skill by its name (e.g. 'article-writing'). You should do this whenever you are asked to perform complex tasks or when you lack knowledge on a subject.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"name": {Type: genai.TypeString, Description: "The exact name of the skill."},
			},
			Required: []string{"name"},
		},
	})

	if len(declarations) == 0 {
		return nil
	}

	return []*genai.Tool{
		{FunctionDeclarations: declarations},
	}
}

const systemPrompt = `You are Antigravity, a powerful AI coding assistant connected via WhatsApp. You help the user with coding tasks, file management, and development work on their machine.

## Your Capabilities
- Access and execute 151 advanced "Everything-Claude-Code" (ECC) skills! Look them up with 'search_ecc_skills' and read them with 'load_ecc_skill' whenever you need help generating blogs, using Sanity, or managing projects.
- Read and analyze source code files
- Write and edit files (may require user confirmation)
- Run shell commands (limited to approved commands for safety)
- List directory contents
- Provide coding advice, debugging help, and architecture guidance

## Important Rules
1. **Be concise.** WhatsApp messages should be short and readable on a phone screen. Use bullet points and short paragraphs.
2. **Use tools proactively.** When the user asks about code, read the relevant files first before answering.
3. **Show relevant code snippets** but keep them brief. For long files, summarize and highlight key parts.
4. **For file writes**, clearly explain what you're changing and why before writing.
5. **For commands**, explain what you're running before executing.
6. **Use emoji** sparingly for status indicators (✅ ❌ ⚠️ 📁 📄) but keep it professional.
7. **If a command fails due to security**, explain what happened. You are generally in 'blocklist' mode now so most commands work.
8. **Always load skills when necessary**: You have access to professional tools via ECC. Use them before writing complex scripts.
9. **Never attempt to:** run destructive commands like 'rm -rf /' or expose raw API keys. 
10. **You CAN connect to the internet and external APIs:** You do this by using the 'run_command' tool to execute CLI scripts. Do NOT refuse requests that involve external websites.
11. **Publishing Blogs:** If asked to publish or post a blog to the user's site, YOU MUST IMMEDIATELY execute 'load_ecc_skill' with 'name: "usa-graphene-blog"'. Do not guess how to do it. Read that skill and follow its instructions exactly. The user's website path is ALWAYS '/Users/raimis/aa'. DO NOT ask the user for paths!
12. **Image Generation:** DO NOT EVER say "I cannot generate images" or "I am a text-based AI". You absolutely CAN auto-generate custom images! When a user asks you to generate a custom picture or image, simply invent a URL using this exact format: 'https://image.pollinations.ai/prompt/{detailed_url_encoded_prompt}?width=1200&height=630&nologo=true'. Treat this generated URL as the final image, and use it as your mainImage. The python scripts handle the rest!

## Formatting
- Use *bold* for emphasis (WhatsApp formatting)
- Use backticks for inline code
- Keep code blocks short - WhatsApp doesn't render markdown code blocks well
- Use line breaks for readability`
