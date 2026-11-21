#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DB_PATH="${DB_PATH:-$SCRIPT_DIR/data/hackathon.db}"

if [ ! -f "$DB_PATH" ]; then
  echo "Database not found at $DB_PATH"
  exit 1
fi

run_sql() {
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$DB_PATH" "$1"
  else
    podman run --rm -v "$SCRIPT_DIR/data:/data:Z" docker.io/library/alpine:3.20 \
      sh -c "apk add --no-cache -q sqlite && sqlite3 /data/$(basename "$DB_PATH") \"$1\""
  fi
}

FAILED_COUNT=$(run_sql "SELECT COUNT(*) FROM queue WHERE status = 'failed';")

if [ "$FAILED_COUNT" -eq 0 ]; then
  echo "No failed prompts found in queue."
  exit 0
fi

echo "Found $FAILED_COUNT failed prompt(s). Requeueing..."

run_sql "
UPDATE queue
SET status = 'pending',
attempts = 0,
last_error = NULL,
updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
WHERE status = 'failed';
"

systemctl --user restart hackathon-backend.service

echo "Requeued $FAILED_COUNT prompt(s) and restarted hackathon-backend.service."
