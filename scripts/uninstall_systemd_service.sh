#!/usr/bin/env bash
set -euo pipefail

systemctl --user disable --now agent-tracker-server.service 2>/dev/null || true
rm -f "$HOME/.config/systemd/user/agent-tracker-server.service"
systemctl --user daemon-reload 2>/dev/null || true

echo "agent-tracker-server systemd user service removed."
