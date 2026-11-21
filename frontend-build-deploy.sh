#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QUADLET_DIR="$HOME/.config/containers/systemd"

mkdir -p "$QUADLET_DIR"

echo "Building localhost/hack-frontend:latest..."
podman build -t localhost/hack-frontend:latest "$SCRIPT_DIR/frontend"

echo "Updating frontend Quadlet unit..."
rm -f "$QUADLET_DIR/frontend.container"
if [ -f "$SCRIPT_DIR/frontend.container" ]; then
  cp "$SCRIPT_DIR/frontend.container" "$QUADLET_DIR/hackathon-frontend.container"
fi

/usr/lib/systemd/user-generators/podman-user-generator --dryrun > /dev/null

systemctl --user daemon-reload
systemctl --user restart hackathon-frontend.service

systemctl --user status hackathon-frontend.service --no-pager
podman ps --filter "name=hackathon-frontend"
