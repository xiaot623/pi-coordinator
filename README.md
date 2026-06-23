# pico (pi-coordinator)

> Remotely control [pi](https://github.com/xiaot623/pi) via Telegram. Start AI coding tasks in private chat, and watch the full execution progress unfold in a group chat Forum Topic.

## How It Works

```
Private Chat                pico (Coordinator)           pi (AI Agent)
────────────                ────────────────           ─────────────
  /new "fix bug"  ──────▶  Select workspace     ──────▶  Spawn pi process
                            Create Forum Topic           Execute task
                            Send prompt via stdin        Stream progress
                                                               │
  Group Topic ◀──────────────────────────────────────────────┘
  (real-time progress via pi-trace extension)
```

You send commands and task descriptions to the bot in a private chat.  
The bot spawns a pi agent process inside a Forum Topic in a configured Telegram group, where you can follow every step — thinking, tool calls, assistant messages, and summary — in real-time.

## Features

- **Remote task execution** — Start, steer, and resume AI coding sessions from Telegram
- **Real-time progress tracking** — Thinking, tool calls, and outputs streamed to a group topic
- **Workspace & session management** — Browse, sync, and organize historical sessions
- **Multi-mode runners** — Local (direct), Worktree (git-isolated), Docker (fully sandboxed)
- **Per-session model selection** — Choose AI models at Global / Workspace / Session scope
- **Git integration** — Rebase, commit, and view HTML diffs directly from Telegram
- **Todo management** — Save and organize task ideas across workspaces
- **Cron scheduling** — Create recurring tasks with auto execution or manual confirmation

## Commands

| Command | Description |
|---------|-------------|
| `/help` | Show available commands |
| `/workspace` | Browse workspaces and select a session |
| `/add` | Add a workspace from your filesystem |
| `/new [description]` | Start a new task in a workspace |
| `/todo` | Manage saved todos across workspaces |
| `/cron` | Manage scheduled tasks across workspaces |
| `/status` | List all active sessions with details |
| `/detail` | Show workspace path, run mode, model, and git summary |
| `/stop` | Force-stop the current session |
| `/open` | Open the workspace in your terminal (iTerm2 / xdg-open) |
| `/rebase` | Rebase a worktree/docker session from the main branch |
| `/commit <message>` | Commit worktree/docker session changes back to the main branch |
| `/diff [args]` | Generate an HTML diff of current changes |
| `/sync` | Import historical sessions from disk into the database |
| `/pin [query]` | Pin a workspace; subsequent messages auto-start new tasks |
| `/unpin` | Clear the pinned workspace |
| `/model` | Configure model at Global / Workspace / Session level |
| `/bots` | Show managed Telegram bot accounts |

### Session Topic Commands

Inside a session Forum Topic, you can send follow-up messages at any time — the bot forwards them to the running (or restarted) pi process to continue the task. The following commands also work inside a topic:

| Command | Context |
|---------|---------|
| `/stop` | Force-stop the current topic's session |
| `/open` | Open the session's workspace locally |
| `/rebase` | Rebase the session's worktree (worktree/docker modes) |
| `/commit <msg>` | Commit and push changes (worktree/docker modes) |
| `/diff [args]` | Generate a syntax-highlighted HTML diff |

## Run Modes

When starting a new task, you can choose one of three execution modes:

| Mode | Working Directory | Isolation |
|------|-------------------|-----------|
| **Local** | Original workspace | None — edits files directly |
| **Worktree** | Per-session Git worktree | File changes isolated on a separate branch |
| **Docker** | Per-session Git worktree in a container | Full filesystem + environment sandbox |

Worktree and Docker modes require the workspace to be a Git repository.

## Quick Start

### Prerequisites

- [Go](https://go.dev/dl/) 1.25+
- [Node.js](https://nodejs.org/) 18+ (for npm scripts)
- A Telegram bot token (from [@BotFather](https://t.me/BotFather))
- A Telegram group (forum-enabled) where the bot is a member with admin permissions

### 1. Clone & Install

```bash
git clone https://github.com/xiaot623/pi-coordinator.git
cd pi-coordinator
npm install
```

### 2. Configuration

On first run, a config template is created. Fill in the required values:

```bash
npm run dev
# → creates dev_assets/pico.config.yaml
```

Edit `dev_assets/pico.config.yaml` (or `~/.mypi/pico/config.yaml` for production):

```yaml
telegram:
  bot_token: "123456:ABC-DEF1234"
  group_chat_id: -1001234567890
  allowed_users:
    - 123456789
```

### 3. Run

```bash
# Development (uses dev_assets/)
npm run dev

# Production build
go build -o pico ./cmd/pico
./pico
```

### 4. Docker Images (for Docker runner)

```bash
docker build -f docker/Dockerfile --target pi-agent -t pi-agent:latest .
```

## Configuration Reference

```yaml
telegram:
  bot_token: ""                # Telegram bot token
  group_chat_id: 0             # Forum group chat ID
  allowed_users: []            # Telegram user IDs allowed to use the bot

plugins:
  - "@hahahhh/pi-trace@next"   # pi extensions (pi-trace is required for progress)

plugin_update_interval_minutes: 1440

runner:
  local:
    idle_timeout: 5m
    session_dir: "~/.pi/agent/sessions"
  worktree:
    idle_timeout: 5m
    session_dir: ""
  docker:
    idle_timeout: 5m
    image: "pi-agent:latest"
    network: "bridge"
    agent_dir: "~/.pi/agent"
    skills_dir: "~/.agents/skills"
    agent_mount_mode: "rw"
    extra_mounts:
      - "~/.config"            # host ~/.config -> container ~/.config (ro)
      - host: "~/scratch"
        mode: "rw"             # host ~/scratch -> container ~/scratch
                                  # non-~/ paths keep same-path mounts

open_tool: "iterm2"            # macOS: iterm2 / terminal; Linux: xdg-open
global_model: ""               # Global default model (empty = pi default)

diff:
  delivery: send               # send (to Telegram) | open (in browser) | all
```

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `PICO_ENV` | Set to `dev` to use `./dev_assets/` for config and database | (production) |

## npm Scripts

```bash
npm run dev           # Run in development mode
npm test              # Run tests
npm run build         # Build native binary via scripts/build-native.js
npm run check         # Lint (go vet ./...)
npm run release:patch # Bump patch version, tag, and push
npm run release:minor # Bump minor version, tag, and push
npm run release:major # Bump major version, tag, and push
```

## Project Structure

```
pico/
├── cmd/pico/main.go          # Entry point
├── internal/
│   ├── app/                  # Application orchestration
│   ├── config/               # YAML config loading and hot-reloading
│   ├── diff/                 # HTML diff rendering
│   ├── gitops/               # Shell script wrappers for git operations
│   ├── runner/               # Local / Worktree / Docker pi process management
│   ├── session/              # Session scanner (sync from disk)
│   ├── source/telegram/      # Telegram bot: handlers, keyboard UI, routing
│   ├── store/                # SQLite persistence
│   ├── todos/                # Todo list storage
│   └── crons/                # Cron task storage and schedule parsing
├── docker/                   # Docker image definitions
├── scripts/                  # Build and release scripts
├── docs/                     # Additional documentation
├── DESIGN.md                 # Architecture and design document
└── package.json              # npm package metadata
```

## Architecture

For a detailed architecture overview, design decisions, and internal data models, see [DESIGN.md](./DESIGN.md).

## License

MIT
