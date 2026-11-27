#!/usr/bin/env bash
# Restore into a scratch database, migrate, run cmd/integrity.
# Never points at production unless you set RESTORE_DATABASE_URL to it (don't).
#
#   RESTORE_DATABASE_URL=postgres://... ./scripts/restore-drill.sh
#   RESTORE_DATABASE_URL=... DUMP=/path/to.dump ./scripts/restore-drill.sh
set -euo pipefail
cd "$(dirname "$0")/.."

url="${RESTORE_DATABASE_URL:-${DATABASE_URL:-}}"
if [[ -z "$url" ]]; then
  echo "RESTORE_DATABASE_URL (or DATABASE_URL) is required — use an empty scratch database" >&2
  exit 1
fi

if [[ -n "${DUMP:-}" ]]; then
  if ! command -v pg_restore >/dev/null 2>&1; then
    echo "pg_restore is required when DUMP is set" >&2
    exit 1
  fi
  echo "==> pg_restore $DUMP"
  pg_restore --no-owner --no-acl --dbname="$url" "$DUMP"
fi

export DATABASE_URL="$url"
echo "==> cmd/migrate"
go run ./cmd/migrate
echo "==> cmd/integrity"
go run ./cmd/integrity
echo "==> restore-drill OK"
