# tg-cli

Telegram bot for remote notification of Claude Code events. Get notified on Telegram when Claude Code completes a task or is waiting for input.

## Features

- Telegram bot with long-polling (`/start`, `/pair`, `/status` commands)
- HTTP hook server receiving Claude Code Stop/SubagentStop events
- JSONL transcript parsing to extract Claude's response
- Interactive pairing flow with 6-hex code (10min TTL)
- Debug logging mode (console + file)
- Single binary, no runtime dependencies

## Usage

### Start the bot

```bash
./tg-cli bot --debug --port 12500
```

On first run, you'll be prompted to enter your Telegram bot token (from @BotFather).

### Pair with Telegram

1. Send `/pair` to your bot in Telegram
2. Enter the 6-character code in the bot terminal
3. Done — notifications will be sent to your chat

### Install hooks

```bash
./tg-cli setup --port 12500
```

This registers Stop and SubagentStop hooks in `~/.claude/settings.json` so Claude Code automatically notifies the bot when tasks complete.

### Subcommands

| Command | Description |
|---------|-------------|
| `tg-cli bot` | Start the Telegram bot + HTTP hook server |
| `tg-cli hook` | Hook handler called by Claude Code (reads stdin) |
| `tg-cli setup` | Install hooks into Claude Code settings |

### Flags

| Flag | Command | Description |
|------|---------|-------------|
| `--debug` | bot | Enable debug logging (console + `~/.tg-cli/bot.log`) |
| `--port` | bot, setup | HTTP server port (default: 12500) |

## Build

```bash
go build -o tg-cli
```

## Version Management

The version number is defined in `main.go` (set via `rootCmd.Version`). Update it before committing new releases.

## Configuration

Credentials are stored in `~/.tg-cli/credentials.json`:

```json
{
  "botToken": "...",
  "pairingAllow": {
    "ids": ["123456"],
    "defaultChatId": "123456"
  },
  "port": 12500
}
```

## Notification Format

```
✅ Task Completed
Project: my-project

💬 Claude:
<Claude's response text>
```

For SubagentStop events, the emoji changes to ⏳ Task Waiting.
