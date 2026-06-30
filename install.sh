#!/usr/bin/env bash
set -euo pipefail

if ! command -v go >/dev/null 2>&1 && [[ -x /opt/homebrew/bin/go ]]; then
	export PATH="/opt/homebrew/bin:$PATH"
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

mkdir -p "$ROOT_DIR/bin"
for bin in tracker-server tracker-mcp agent; do
	echo "Building $bin..."
	go build -o "bin/$bin" "./cmd/$bin"
done

echo "Built tracker-server, tracker-mcp, and agent into bin/"
