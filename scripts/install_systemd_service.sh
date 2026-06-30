#!/usr/bin/env bash
set -euo pipefail

if ! command -v systemctl >/dev/null 2>&1; then
  echo "Error: systemctl not found" >&2
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user"

go build -o bin/tracker-server ./cmd/tracker-server
go build -o bin/tracker-mcp ./cmd/tracker-mcp
go build -o bin/agent ./cmd/agent

cp bin/tracker-server "$HOME/.local/bin/tracker-server"
cp bin/tracker-mcp "$HOME/.local/bin/tracker-mcp"
cp bin/agent "$HOME/.local/bin/agent-tracker"

cat >"$HOME/.config/systemd/user/agent-tracker-server.service" <<'EOF'
[Unit]
Description=Agent Tracker Server
After=default.target

[Service]
Type=simple
ExecStart=%h/.local/bin/tracker-server
Restart=on-failure
RestartSec=2
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
PassEnvironment=DISPLAY WAYLAND_DISPLAY XAUTHORITY DBUS_SESSION_BUS_ADDRESS XDG_CURRENT_DESKTOP

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now agent-tracker-server.service

echo "agent-tracker-server installed as a systemd user service."
echo "Check status: systemctl --user status agent-tracker-server.service"
echo "Logs: journalctl --user -u agent-tracker-server.service -f"
echo "For boot without login: sudo loginctl enable-linger \"$USER\""
