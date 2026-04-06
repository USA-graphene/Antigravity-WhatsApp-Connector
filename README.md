# Antigravity WhatsApp Connector

> Connect your Gemini-powered AI coding assistant to WhatsApp. Work on your code from your phone.

## What It Does

Antigravity WhatsApp Connector bridges WhatsApp messaging to a Gemini AI agent that can:

- 📖 **Read files** in your workspace
- 📝 **Write files** with confirmation prompts
- ⚡ **Run commands** (safely allowlisted)
- 📁 **Browse directories**
- 💬 **Have conversations** with full context memory

All from your phone via WhatsApp.

## Quick Start

### Prerequisites

- Go 1.21+
- A [Gemini API key](https://aistudio.google.com/apikey) (free tier available)
- WhatsApp on your phone

### Setup

```bash
# Clone the repo
git clone https://github.com/USA-graphene/Antigravity-WhatsApp-Connector.git
cd Antigravity-WhatsApp-Connector

# Create your config
cp config.example.yaml config.yaml

# Edit config.yaml:
# 1. Add your Gemini API key (or set GEMINI_API_KEY env var)
# 2. Add your phone number to allowed_phones (e.g., +1234567890)
# 3. Set your PIN
# 4. Set your workspace root directory

# Install dependencies & build
make deps
make build

# Run
make run
```

On first run, you'll see a QR code in the terminal. **Scan it with WhatsApp** (Settings → Linked Devices → Link a Device).

### First Message

Once connected, send a WhatsApp message to the linked number. You'll be asked for your PIN, then you're in!

## Commands

| Command | Description |
|---------|-------------|
| `/help` | Show available commands |
| `/status` | Connection and config status |
| `/reset` | Clear conversation history |
| `/logout` | End your session |

## Security

This connector implements defense-in-depth security:

- **Phone allowlist** — Only authorized numbers can interact
- **PIN authentication** — Required on each session
- **Session TTL** — Sessions auto-expire (default: 8 hours)
- **Command allowlisting** — Only approved commands can execute
- **Workspace scoping** — File operations restricted to workspace directory
- **Write confirmation** — File writes require explicit YES/NO confirmation
- **Rate limiting** — Prevents API abuse
- **Audit logging** — Every tool invocation is logged
- **Account lockout** — Locks after 5 failed PIN attempts

## Configuration

See `config.example.yaml` for all available options. Key settings:

```yaml
auth:
  allowed_phones: ["+1234567890"]  # Your phone number
  pin: "123456"                     # Your auth PIN
  session_ttl: "8h"                 # Session duration

gemini:
  model: "gemini-2.5-flash"        # or gemini-2.5-pro
  
workspace:
  root: "/path/to/your/code"       # Where file operations are scoped

tools:
  run_command:
    mode: "allowlist"               # safest mode
    allowlist:                      # commands you trust
      - "go build"
      - "git status"
      - "ls"
```

## Architecture

```
WhatsApp Phone ←→ whatsmeow Client
                        ↓
                  Rate Limiter → Auth Engine
                        ↓
                  Message Router
                   ↙        ↘
            Commands      AI Chat
                            ↓
                    Gemini 2.5 (Function Calling)
                            ↓
                     Tool Registry
                    ↙    ↓    ↘
              Read   Write   Command
              File   File    Runner
```

## License

MIT
