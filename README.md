# tg-cli

Telegram bot for remote notification and interaction with Claude Code sessions. Get notified on Telegram when Claude Code completes tasks, needs input, or requests permissions — and respond directly from Telegram.

## Features

- Real-time notifications for Claude Code events (task completion, questions, permission requests)
- Interactive inline buttons for answering questions and approving permissions
- Voice message support (whisper.cpp transcription → inject to Claude Code)
- Multi-session management with tmux target tracking
- Group chat routing (bind sessions to specific Telegram groups)
- Project-based routing (bind by working directory)
- Permission mode switching (plan/auto/bypass/default)
- Session resume from Telegram
- Context window usage monitoring
- Long message pagination with inline keyboard
- systemd service management
- Single binary, no runtime dependencies

## Quick Start

### Build

```bash
go build -o tg-cli
```

### 1. Start the bot

```bash
./tg-cli bot --debug
```

On first run, enter your Telegram bot token (from [@BotFather](https://t.me/BotFather)).

### 2. Pair with Telegram

1. Send `/bot_pair` to your bot in Telegram
2. Enter the 6-character code in the bot terminal
3. Done — notifications will be sent to your chat

### 3. Install hooks

```bash
./tg-cli setup
```

This registers all Claude Code hooks (Stop, SessionStart, SessionEnd, PermissionRequest, PreToolUse, UserPromptSubmit) in `~/.claude/settings.json`.

### 4. (Optional) Setup voice

```bash
./tg-cli voice
```

Interactive setup for whisper.cpp model download and language configuration.

### 5. (Optional) Install as service

```bash
./tg-cli service install
```

## Subcommands

| Command | Description |
|---------|-------------|
| `tg-cli bot` | Start the Telegram bot + HTTP hook server |
| `tg-cli hook` | Hook handler called by Claude Code (reads stdin payload) |
| `tg-cli setup` | Install/uninstall hooks into Claude Code settings |
| `tg-cli voice` | Interactive whisper.cpp setup (model download, language config) |
| `tg-cli service` | systemd user service management (install/uninstall/start/stop/restart/status/upgrade) |
| `tg-cli statusline` | Claude Code statusline script for context window tracking |

### Flags

| Flag | Command | Description |
|------|---------|-------------|
| `--debug` | bot | Enable debug logging (console + `~/.tg-cli/bot.log`) |
| `--port` | bot, hook, setup | HTTP server port (default: 12500) |
| `--config-dir` | (global) | Custom config directory (default: `~/.tg-cli`) |
| `--settings` | setup | Additional Claude Code settings file path |
| `--uninstall` | setup | Remove hooks from Claude Code settings |

## Telegram Commands

| Command | Description |
|---------|-------------|
| `/start` | Welcome message |
| `/bot_pair` | Start pairing flow |
| `/bot_status` | Show bot and session status |
| `/bot_routes` | List all active routes (tmux + project) |
| `/bot_bind` | Bind a session to current group (tmux or project) |
| `/bot_unbind` | Unbind a session from current group |
| `/bot_capture` | Capture current tmux pane content |
| `/bot_perm_plan` | Switch to plan permission mode |
| `/bot_perm_auto` | Switch to auto-approve permission mode |
| `/bot_perm_bypass` | Switch to bypass permission mode |
| `/bot_perm_default` | Switch to default permission mode |
| `/resume` | List and resume previous Claude Code sessions |

### Replying to Notifications

- **Text reply** to any notification → injects text into the Claude Code session
- **Voice reply** → transcribes via whisper.cpp, then injects text
- **Button click** → answers questions or approves/denies permissions

## Notification Types

| Emoji | Event | Description |
|-------|-------|-------------|
| ✅ | Task Completed (Stop) | Claude finished a task |
| 🟢 | Session Started | New Claude Code session detected |
| 🔴 | Session Ended | Claude Code session closed |
| ❓ | Question (AskUserQuestion) | Claude is asking a question — inline buttons to answer |
| 🔐 | Permission Request | Claude needs permission — Allow/Deny/Always Allow buttons |
| 💬 | Update (PreToolUse) | Intermediate Claude output before tool calls |
| 📊 | Context | Context window usage (N% Xk/Yk) shown in notifications |

## Configuration

### Credentials (`~/.tg-cli/credentials.json`)

```json
{
  "botToken": "...",
  "pairingAllow": {
    "ids": ["123456"],
    "defaultChatId": "123456"
  },
  "port": 12500,
  "routeMap": {
    "tmux-session:0.0": "group-chat-id"
  },
  "projectRouteMap": {
    "/path/to/project": "group-chat-id"
  }
}
```

### App Config (`~/.tg-cli/config.json`)

```json
{
  "whisperPath": "/path/to/whisper-cli",
  "modelPath": "/path/to/model.bin",
  "language": "auto",
  "ffmpegPath": "ffmpeg",
  "voicePrefix": "🗣️"
}
```

## Advanced Features

### Group Routing

Bind Claude Code sessions to specific Telegram groups:

- **Tmux routing**: `/bot_bind` → select tmux target → messages route to that group
- **Project routing**: `/bot_bind` → select project → messages from that working directory route to the group

### Multi-Session Management

Multiple Claude Code sessions can run simultaneously. Each session is tracked by its tmux target. Reply to a specific notification to interact with that session.

### Permission Mode Switching

Switch Claude Code's permission mode from Telegram:
- `/bot_perm_plan` — Requires approval for each action
- `/bot_perm_auto` — Auto-approve safe actions
- `/bot_perm_bypass` — Skip all permission checks
- `/bot_perm_default` — Reset to default mode

### Session Resume

Use `/resume` to list previous Claude Code sessions and resume any of them directly from Telegram.

### Voice Messages

Reply to any notification with a voice message. It will be transcribed using whisper.cpp and injected into the Claude Code session.

### Context Window Monitoring

Notifications include context window usage (📊 line) showing current token consumption percentage.

## Multi-Instance Support

Run multiple bot instances with separate configurations:

```bash
./tg-cli --config-dir ~/.tg-cli-test bot --port 12501
./tg-cli --config-dir ~/.tg-cli-test setup --port 12501
```

## Service Management

```bash
./tg-cli service install   # Install systemd user service
./tg-cli service start     # Start the service
./tg-cli service stop      # Stop the service
./tg-cli service restart   # Restart the service
./tg-cli service status    # Check service status
./tg-cli service upgrade   # Rebuild binary and restart
./tg-cli service uninstall # Remove the service
```

## Development

### Build

```bash
go build -o tg-cli
```

### Run Tests

```bash
go test ./...              # Unit tests
bash tests/e2e.sh          # End-to-end tests
bash tests/voice_test.sh   # Voice setup tests
```

### Version

Version is defined in `main.go`. Current: `1.5.1`.
