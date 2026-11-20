#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QUADLET_DIR="$HOME/.config/containers/systemd"

mkdir -p "$QUADLET_DIR" "$SCRIPT_DIR/data"

rm -f "$QUADLET_DIR/backend.container" "$QUADLET_DIR/frontend.container"

for f in "$SCRIPT_DIR"/*.container; do
  cp "$f" "$QUADLET_DIR/hackathon-$(basename "$f")"
done

cp "$SCRIPT_DIR/hackathon.network" "$QUADLET_DIR/hackathon.network"

/usr/lib/systemd/user-generators/podman-user-generator --dryrun > /dev/null

systemctl --user daemon-reload
systemctl --user restart hackathon-network.service hackathon-backend.service hackathon-frontend.service

systemctl --user status hackathon-backend.service hackathon-frontend.service --no-pager
podman ps
