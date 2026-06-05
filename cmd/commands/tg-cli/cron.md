# /tg-cli:cron — Cron Job Management

tg-cli cron system for scheduling recurring or one-time tasks. Two execution modes, multiple scheduling formats.

## Modes

| Mode | Description | Use case |
|------|-------------|----------|
| `print` | Spawns `claude -p --resume` as independent process | Heartbeat checks, periodic reviews |
| `inject` | Injects text into active CC session by agent name or tmux target | In-session reminders, timed prompts |

## Schedule Formats

- **Duration**: `30m`, `2h`, `1h30m` — repeats at this interval
- **Cron expression**: `*/30 * * * *`, `0 4 * * *` — standard 5-field cron
- **One-time**: Add `--once` flag — auto-deletes after execution

## CLI Commands

### Create a heartbeat (print mode)
```bash
tg-cli cron add --mode print --schedule 30m \
  --prompt "Review pending tasks. If nothing needs attention, reply HEARTBEAT_OK." \
  --cwd /path/to/project
```

### Create a timed reminder (inject mode, by agent name)
```bash
tg-cli cron add --mode inject --schedule 2h \
  --agent mybot \
  --prompt "Reminder: check CI status"
```

### Create a reminder targeting current tmux session
```bash
tg-cli cron add --mode inject --schedule 1h --self \
  --name my-reminder \
  --prompt "Time to take a break"
```

### Create a named job for easy management
```bash
tg-cli cron add --mode inject --schedule 30m \
  --agent mybot --name daily-review \
  --prompt "Review pending tasks"
```

### One-time delayed task
```bash
tg-cli cron add --mode inject --schedule 30m --once \
  --agent mybot --prompt "30 minutes passed, time to review"
```

### Daily scheduled task (cron expression)
```bash
tg-cli cron add --mode print --schedule "0 9 * * *" \
  --prompt "Morning check: review overnight alerts" \
  --cwd /path/to/project
```

### List all jobs
```bash
tg-cli cron list
```

### Remove a job (by ID or name)
```bash
tg-cli cron remove --id <job-id>
tg-cli cron remove --id daily-review
tg-cli cron remove --name daily-review
```

### Update a job
```bash
tg-cli cron update --id daily-review --prompt "New prompt text"
tg-cli cron update --id daily-review --schedule 1h
tg-cli cron update --id <job-id> --agent new-agent --name new-name
```

### View print job transcript
```bash
tg-cli cron log --name my-heartbeat
tg-cli cron log --id abc12345
```

Shows the conversation history of a print mode job (user prompts and assistant responses from the --resume session).

## TG Management

Open `/bot_settings → ⏰ Cron` in Telegram to view all jobs with inline delete buttons. Named jobs show their name in the button label.

## Behavior Notes

- **Print mode**: If Claude responds with `HEARTBEAT_OK`, no notification is sent (silent). Any other response is pushed to Telegram.
- **Print mode**: Uses `--resume` to maintain conversation context across invocations.
- **Inject mode**: Looks up agent by name via `sessionState.findByName`. Falls back to `tmux_target` if agent name not set. If neither is online, sends a Telegram notification instead.
- **Inject mode**: On successful injection, sends a `🔔 Cron injected` notification to Telegram.
- **--self flag**: Auto-detects current tmux pane from `TMUX_PANE` env var. Useful when creating jobs from within a session.
- **Named jobs**: Use `--name` for human-friendly IDs. Names must be unique. Can be used in place of job IDs in remove/update commands.
- **Scheduler**: Checks every 30 seconds (~30s max delay).
- **Persistence**: Jobs saved to `~/.tg-cli/cron-jobs.json`, survive bot restart.
- **Hook filtering**: Print mode sets `TG_CLI_CRON=1` env var, which causes hooks to exit immediately — no SessionStart/Stop/SessionEnd notifications from heartbeat sessions.

## Flags Reference

### `cron add` Flags

| Flag | Required | Description |
|------|----------|-------------|
| `--mode` | Yes | `print` or `inject` |
| `--schedule` | Yes | Duration (`30m`) or cron expression (`*/30 * * * *`) |
| `--prompt` | Yes | Text to send/inject |
| `--agent` | inject only (or --self) | Agent name to inject into |
| `--self` | No | Auto-detect current session from TMUX_PANE |
| `--name` | No | Human-friendly unique name for this job |
| `--cwd` | No | Working directory for print mode (default: current dir) |
| `--once` | No | Single execution, auto-delete after run |
| `--port` | No | Bot HTTP port (default: from config or 12500) |
| `--max-turns` | No | Max agentic turns for print mode (0 = no limit) |

### `cron remove` Flags

| Flag | Required | Description |
|------|----------|-------------|
| `--id` | One of --id/--name | Job ID or name to remove |
| `--name` | One of --id/--name | Job name to remove |
| `--port` | No | Bot HTTP port |

### `cron update` Flags

| Flag | Required | Description |
|------|----------|-------------|
| `--id` | Yes | Job ID or name to update |
| `--prompt` | No | New prompt text |
| `--schedule` | No | New schedule |
| `--agent` | No | New agent name |
| `--name` | No | New name (must be unique) |
| `--port` | No | Bot HTTP port |

### `cron log` Flags

| Flag | Required | Description |
|------|----------|-------------|
| `--id` | One of --id/--name | Job ID or name to view transcript |
| `--name` | One of --id/--name | Job name to view transcript |
| `--port` | No | Bot HTTP port |
