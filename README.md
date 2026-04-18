# Antigravity WhatsApp Bridge

Remote-control the [Antigravity](https://antigravity.dev) desktop AI agent from your phone via WhatsApp.

Send a task in plain English → AI executes it on your Mac → response back to your phone.

## What it does

- 📱 **WhatsApp → AI Agent** — send any coding task from your phone
- 🤖 **Gemini AI** — same model as Antigravity, with full tool access
- 📁 **File read/write** — read, edit, create files in your workspace
- 💻 **Shell commands** — run git, npm, build scripts, etc.
- 📝 **Blog publishing** — research, write, and publish to Sanity CMS
- 🖼️ **AI cover images** — auto-generated and uploaded to Sanity
- 🔌 **Skills system** — drop `.md` files in `skills/` to extend capabilities

## Quick Start

### Prerequisites

- [Go 1.22+](https://golang.org/dl/)
- A Google [Gemini API key](https://aistudio.google.com/apikey)
- WhatsApp on your phone

### Build

```bash
git clone https://github.com/YOUR_USERNAME/Antigravity-WhatsApp-Connector
cd Antigravity-WhatsApp-Connector
go build -o bridge .
```

### Run

```bash
GEMINI_API_KEY=your_key_here ./bridge
```

Scan the QR code with WhatsApp → Settings → Linked Devices → Link a Device.

### Authenticate

Send your PIN (default: `314159`) to the bridge in WhatsApp to unlock it.

## Configuration

Edit the constants at the top of `main.go`:

| Constant | Description |
|---|---|
| `allowedPhone` | Your WhatsApp number (only this number can control the bridge) |
| `authPIN` | PIN to authenticate each session |
| `workspace` | Default working directory for file/shell operations |
| `geminiModel` | Gemini model to use (default: `gemini-2.5-flash`) |

## Commands

| Command | Description |
|---|---|
| `/help` | Show available commands |
| `/status` | Bridge and model status |
| `/reset` | Clear conversation history |
| `/logout` | End the session |

## Skills

Drop any `.md` skill file into the `skills/` directory and restart — the bridge loads them automatically at startup.

Included skills:
- 🐙 **github** — interact with GitHub via `gh` CLI
- 🧾 **summarize** — summarize URLs, YouTube videos, PDFs
- 🌤️ **weather** — weather forecast via wttr.in
- 🧵 **tmux** — manage persistent terminal sessions

## Blog Publishing

Send from WhatsApp:
> "Write a 1500-word SEO article about graphene in batteries and publish it"

The agent will:
1. Research and write the article (1500–2000 words, SEO optimized)
2. Generate a cinematic cover image via Pollinations.ai
3. Upload image to Sanity CMS
4. Publish the post with author, category, SEO title/description

## Security

- Only the `allowedPhone` number can interact with the bridge
- PIN authentication required each session
- Shell commands are blocklisted for dangerous patterns
- Session data stored locally in `data/whatsapp.db` (gitignored)

## Architecture

```
WhatsApp (phone)
    ↓
bridge (Go)
    ↓
Gemini API (AI reasoning + tool calling)
    ↓
Tools: read_file, write_file, run_shell, list_dir, find_files, publish_blog_post
    ↓
Your Mac (files, git, Sanity CMS, etc.)
    ↓
Response → WhatsApp
```

## License

MIT
