#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QUADLET_DIR="$HOME/.config/containers/systemd"

podman build -t localhost/hack-backend:latest "$SCRIPT_DIR/backend"
podman build -t localhost/hack-frontend:latest "$SCRIPT_DIR/frontend"


