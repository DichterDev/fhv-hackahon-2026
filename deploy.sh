#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QUADLET_DIR="$HOME/.config/containers/systemd"

podman build -t localhost/hack-backend:latest "$SCRIPT_DIR/backend"
podman build -t localhost/hack-frontend:latest "$SCRIPT_DIR/frontend"

mkdir -p "$QUADLET_DIR" "$SCRIPT_DIR/data"

cp "$SCRIPT_DIR"/*.container "$QUADLET_DIR/"
cp "$SCRIPT_DIR"/*.network "$QUADLET_DIR/"

/usr/lib/systemd/user-generators/podman-user-generator --dryrun

systemctl --user daemon-reload
systemctl --user restart hackathon-network.service backend.service frontend.service

systemctl --user status backend.service frontend.service --no-pager
podman ps
