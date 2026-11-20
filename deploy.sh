#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QUADLET_DIR="$HOME/.config/containers/systemd"

mkdir -p "$QUADLET_DIR" "$SCRIPT_DIR/data"

cp "$SCRIPT_DIR"/*.container "$QUADLET_DIR/hackathon-"
cp "$SCRIPT_DIR"/*.network "$QUADLET_DIR/hackathon-"

/usr/lib/systemd/user-generators/podman-user-generator --dryrun

systemctl --user daemon-reload
systemctl --user restart hackathon-network.service hackathon-backend.service hackathon-frontend.service

systemctl --user status hackathon-backend.service hackathon-frontend.service --no-pager
podman ps
