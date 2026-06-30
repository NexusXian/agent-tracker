# agent-traker

A tmux-aware **agent task tracker**. Coding agents (Claude Code, opencode, Codex, …) report what they're working on via an **MCP tool**; a background **server** keeps the task list; a **TUI** panel shows live task status, durations, and lets you jump to a task's tmux pane.

> Rebuilt in the style of [`theniceboy/.config/agent-tracker`](https://github.com/theniceboy/.config/tree/master/agent-tracker).

```
Tracker  ·  5-4  ·  opencode
Active 2 · Waiting 0 · 4:52PM

 ⠹ 重构 agent-tracker 为任务跟踪器
    5-4 / opencode  ·  03m12s
 ⠹ 修复登录bug
    2-1 / zsh  ·  01m05s

k/j move  Enter open  c toggle  D delete  r refresh  ? help  esc quit
```

## How it works

```
                 MCP (stdio)                unix socket
  coding agent  ───────────►  tracker-mcp  ───────────►  tracker-server
   (claude/                                                    │
    opencode/                                                  │ broadcast state
    codex)                                                     ▼
                                                          agent (TUI)
                                                       (subscribes / polls)
```

- Agents call MCP tools `start_working` / `finish_working` / `update_summary`; opencode exposes them as `tracker_start_working` / `tracker_finish_working` / `tracker_update_summary` because the server is named `tracker`.
- The server keeps tasks keyed by that tmux target, broadcasts state to UI subscribers, refreshes the tmux status line, and fires a **desktop notification** when a task completes.
- The TUI shows the queue; `Enter` runs `tmux switch-client / select-window / select-pane` to jump to the task.

## Three binaries

| Binary | Role |
| --- | --- |
| `tracker-server` | Long-running backend. Unix socket (`$XDG_RUNTIME_DIR/agent-tracker.sock` or `/tmp/...`), holds tasks, broadcasts state, sends notifications. |
| `tracker-mcp` | MCP server (stdio) exposing the tracker tools so agents can self-report task progress. |
| `agent-tracker` | Bubble Tea TUI panel. Polls/subscribes to the server, renders the task queue. |

## Build

```bash
./install.sh        # builds all three into ./bin
```

or individually:

```bash
go build -o bin/tracker-server ./cmd/tracker-server
go build -o bin/tracker-mcp    ./cmd/tracker-mcp
go build -o bin/agent          ./cmd/agent
```

## Run

Start the server (foreground for testing):

```bash
./bin/tracker-server
```

Open the panel inside tmux:

```bash
./bin/agent
```

One-shot, non-interactive print of current tasks:

```bash
./bin/agent -once
```

### Background service (macOS via Homebrew)

```bash
./scripts/install_brew_service.sh
```

This builds `tracker-server`, installs it as a Homebrew formula, and starts it under `brew services`. Check it with:

```bash
brew services list | grep agent-tracker-server
```

Logs land in `$(brew --prefix)/var/log/agent-tracker-server.log`.

### Background service (Linux via systemd user)

```bash
./scripts/install_systemd_service.sh
```

This builds all three binaries, installs them into `~/.local/bin`, and starts `tracker-server` as a user service.

```bash
systemctl --user status agent-tracker-server.service
journalctl --user -u agent-tracker-server.service -f
```

For boot without login:

```bash
sudo loginctl enable-linger "$USER"
```

### Wire the panel to a tmux key

In `~/.tmux.conf`:

```tmux
bind-key T display-popup -E -x R -y 1 -w 64 -h 45% "agent-tracker"
bind-key -n M-a display-popup -E -x R -y 1 -w 64 -h 45% "agent-tracker"
```

Then `Alt-a` opens the tracker directly, and `prefix + T` stays available as a fallback.

## Wire the MCP tool to your agent

Point your coding agent at the `tracker-mcp` binary. Example for opencode/Claude-style MCP config:

```json
{
  "mcpServers": {
    "tracker": {
      "command": "/path/to/bin/tracker-mcp"
    }
  }
}
```

The agent can then call:

- `tracker_start_working` — `{ "tmux_id": "$3::@12::%30", "summary": "add login flow" }`
- `tracker_finish_working` — `{ "tmux_id": "$3::@12::%30", "note": "done" }`
- `tracker_update_summary` — `{ "tmux_id": "$3::@12::%30", "summary": "…new summary…" }`

`tmux_id` is `session_id::window_id::pane_id`, obtainable inside tmux with:

```bash
tmux display-message -p '#{session_id}::#{window_id}::#{pane_id}'
```

## TUI keys

Defaults (overridable in `~/.config/agent-tracker/agent-config.json`):

| Key | Action |
| --- | --- |
| `j` / `k` | move down / up |
| `Enter` | jump to the task's tmux session/window/pane |
| `c` | finish an in-progress task / acknowledge a completed one |
| `D` | delete the selected task |
| `r` | refresh now |
| `?` | help |
| `Alt-a` / `Esc` / `q` | quit |

Status indicators: `⠹` spinner = in progress, `⚑` = completed & awaiting review, `✓` = acknowledged.

## Configuration

`~/.config/agent-tracker/agent-config.json`:

```json
{
  "keys": {
    "move_up": "k",
    "move_down": "j",
    "edit": "Enter",
    "cancel": "Escape",
    "toggle_todo": "c",
    "destroy": "D",
    "help": "?",
    "refresh": "r"
  },
  "notifications_enabled": true
}
```

Notifications use `terminal-notifier` (or `osascript`) on macOS and `notify-send` on Linux. Clicking a macOS notification runs `tmux switch-client/select-window/select-pane` to jump to the task.

## Wire protocol

Line-delimited JSON over the Unix socket. Messages use the `ipc.Envelope` shape:

```jsonc
// client → server: register for state
{ "kind": "ui-register" }
// server → client: state push
{ "kind": "state", "message": "Active 2 · Waiting 0 · 4:52PM", "tasks": [ … ] }
// client → server: command
{ "kind": "command", "command": "start_task", "session_id": "$3", "window_id": "@12", "pane": "%30", "summary": "…" }
// server → client: ack
{ "kind": "ack" }
```

Commands: `start_task`, `finish_task`, `update_task`, `acknowledge`, `delete_task`, `notify`.

## Files

| Path | Purpose |
| --- | --- |
| `$XDG_RUNTIME_DIR/agent-tracker.sock` (else `/tmp/`) | Unix socket, mode `0600` |
| `~/.config/agent-tracker/run/settings.json` | persisted settings (notifications toggle) |
| `~/.config/agent-tracker/agent-config.json` | TUI key config |

## Project layout

```
cmd/
  tracker-server/   backend (unix socket, tasks, notifications)
  tracker-mcp/      MCP stdio server (agent-facing tools)
  agent/            bubbletea TUI panel
internal/
  ipc/              Envelope + Task protocol types
  tracker/          Status / Entry domain types
scripts/
  install_brew_service.sh
  install_systemd_service.sh
  uninstall_systemd_service.sh
.brew/
  agent-tracker-server.rb   (regenerated by the install script)
```

## Uninstall

```bash
brew services stop agent-tracker-server && brew uninstall agent-tracker-server
./scripts/uninstall_systemd_service.sh
rm -f ~/.config/agent-tracker/run/settings.json
```
