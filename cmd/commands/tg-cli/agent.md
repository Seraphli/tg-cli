# /tg-cli:agent — Agent Session & Communication

CLI commands for managing CC sessions, inter-agent mailbox communication, and tmux pane management.

## Session Commands

### List active sessions
```bash
tg-cli session list
tg-cli session list --host https://tg-cli.example.com --token mytoken
```

### View agent's conversation log
```bash
tg-cli session log --name mybot --lines 20
tg-cli session log --self --no-tools
tg-cli session log --self --format json
```

Output format (text mode):
```
📟 %0 | 📊 Context: 14% (118.6k/800.0k)
────────────────────────
2026-03-19 10:40:32 [assistant]
message text here
────────────────────────
2026-03-19 10:40:35 [assistant] [Bash]
go build -o tg-cli . 2>&1
ℹ️ Verify build
────────────────────────
2026-03-19 10:41:00 [user]
message text
────────────────────────
```

- Header shows tmux target and context usage (percentage + tokens used/total)
- `--format json` returns raw JSON with `target`, `context_pct`, `context_used`, `context_total`, `messages`
- `--no-tools` filters out Bash/Read/Write/Edit/Glob/Grep/Agent calls, but keeps AskUserQuestion and other interactions

### Send message to agent
```bash
tg-cli session send --name mybot --text "Please review the latest changes"
tg-cli session send --self --text "Continue working on the task"
```

### Create new CC session
```bash
tg-cli session new --session mybot --workdir /path/to/project
tg-cli session new --session mybot --workdir /path/to/project --command "claude --model sonnet"
tg-cli session new --session mybot --workdir /path/to/project --name reviewer
```

### Exit CC session
```bash
tg-cli session exit --name mybot
```

### Watch session events (blocking)
```bash
tg-cli session watch --name mybot
```

Blocks until the watched session triggers Stop (task complete), AskUserQuestion, or PermissionRequest. Returns the event as JSON and exits.

Output format:
```json
{"event":"Stop","agent":"mybot","summary":"Task completed","detail":"..."}
```

## Mailbox Commands (Inter-Agent Communication)

### Send message to another agent
```bash
tg-cli mailbox send --self --to other-agent --subject "Task Done" --text "Task completed, please review"
tg-cli mailbox send --from mybot --to reviewer --subject "PR Ready" --text "PR ready for review"
# With file attachment
tg-cli mailbox send --from mybot --to reviewer --subject "Report" --text "See attached" --file /path/to/report.pdf
```

### Wait for incoming messages (blocking)
```bash
tg-cli mailbox receive --self
tg-cli mailbox receive --name mybot
# Remote access
tg-cli mailbox receive --name mybot --host https://tg-cli.example.com --token mytoken
```

> **Tip:** `mailbox receive` blocks indefinitely. Use `run_in_background: true`
> to avoid blocking your session. When a message arrives, CC will automatically
> send you a task-notification with the output. Use TaskOutput to read the result.
> Do NOT use sleep to poll — the notification is automatic.

### View inbox
```bash
tg-cli mailbox inbox --self
tg-cli mailbox inbox --name mybot
```

### Content Limits

- **Subject + Text combined**: max 3500 characters
- If content exceeds the limit, the send command will return an error
- For long content, write to a file and use `--file` to attach it
- Attachments: max 20MB per file (Telegram Bot API download limit)

## tmux Commands

### List all tmux panes
```bash
tg-cli tmux list
```

### Kill a tmux pane
```bash
tg-cli tmux kill --target %0
```

### tmux Session Lifecycle Notifications

After running `tg-cli install`, the bot registers tmux hooks for `session-created` and `session-closed` events.
You will receive TG notifications when tmux sessions are created or closed:
- `🟢 Tmux Session Created: %pane (session_name)` — a new tmux session was created
- `🔴 Tmux Session Closed: %pane (session_name) [agent_name]` — a tmux session was closed (bot also cleans up stale session state)

## Remote Access (cloudflared)

When using tg-cli across machines, expose the bot via cloudflared tunnel:

```bash
cloudflared tunnel --url http://127.0.0.1:12500
```

Then on the remote agent machine, use `--host` and `--token` flags:

```bash
tg-cli session list --host https://your-tunnel.trycloudflare.com --token mytoken
tg-cli mailbox send --from agentA --to agentB \
  --subject "Update" --text "Done" \
  --host https://your-tunnel.trycloudflare.com --token mytoken
```

## Mailbox Channel Binding

To receive mailbox notifications in a Telegram group, open `/bot_settings → 📬 Mailbox` in the group and tap "Bind as mailbox group". The bot will bind the mailbox to that group and deliver new messages there.

## Agent Collaboration Workflow

Example: Agent A finishes work and needs Agent B to review.

1. Agent A completes work:
```bash
tg-cli mailbox send --self --to agentB --subject "Feature X" --text "I've finished implementing feature X. Please review."
```

2. Agent B is waiting for messages:
```bash
tg-cli mailbox receive --self
# Output: [agentA] 2024-01-15T10:30:00Z I've finished implementing feature X. Please review.
```

3. Agent B checks Agent A's recent activity:
```bash
tg-cli session log --name agentA --lines 10 --no-tools
```

4. Agent B reviews and responds:
```bash
tg-cli mailbox send --self --to agentA --subject "Review Done" --text "Review complete. Found 2 issues, see my session log."
```

## Flags Reference

### Common Flags
| Flag | Description |
|------|-------------|
| `--name` | Agent name to target |
| `--self` | Auto-detect current agent from TMUX_PANE |
| `--port` | Bot HTTP port (default: from config or 12500) |
| `--host` | Bot API host URL for remote access (e.g., https://tg-cli.example.com) |
| `--token` | API authentication token for remote access |

### session log Flags
| Flag | Default | Description |
|------|---------|-------------|
| `--lines` | 20 | Number of messages to show |
| `--no-tools` | false | Exclude Bash/Read/Write/Edit/Glob/Grep/Agent calls (keeps AskUserQuestion) |
| `--format` | text | Output format: `text` (human-readable) or `json` (raw API response) |

### session new Flags
| Flag | Required | Description |
|------|----------|-------------|
| `--session` | Yes | tmux session name |
| `--workdir` | Yes | Working directory |
| `--command` | No | CC launch command |
| `--name` | No | Agent name to assign automatically after launch |

### session watch Flags
| Flag | Required | Description |
|------|----------|-------------|
| `--name` | Yes | Agent name to watch |

### mailbox send Flags
| Flag | Required | Description |
|------|----------|-------------|
| `--to` | Yes | Target agent name |
| `--subject` | Yes | Message subject |
| `--text` | Yes | Message body |
| `--from` | One of --from/--self | Sender name |
| `--self` | One of --from/--self | Auto-detect sender from TMUX_PANE |
| `--file` | No | Attachment file path |
| `--port` | No | Bot HTTP port |
