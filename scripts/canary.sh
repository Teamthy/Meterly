#!/usr/bin/env bash
# Hourly P1 staging canary: liveness, readiness, optional live-sandbox certify.
# Exits 0 when STAGING_API_URL is unset (no secrets in CI).
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ -z "${STAGING_API_URL:-}" ]]; then
  echo "STAGING_API_URL unset; skip canary"
  exit 0
fi

base="${STAGING_API_URL%/}"
echo "==> GET $base/healthz"
code=$(curl -fsS -o /tmp/meterly-canary-health.json -w "%{http_code}" "$base/healthz")
if [[ "$code" != "200" ]]; then
  echo "healthz HTTP $code" >&2
  exit 1
fi

echo "==> GET $base/readyz"
set +e
ready_code=$(curl -sS -o /tmp/meterly-canary-ready.json -w "%{http_code}" "$base/readyz")
set -e
if [[ "$ready_code" != "200" ]]; then
  echo "readyz HTTP $ready_code" >&2
  cat /tmp/meterly-canary-ready.json >&2 || true
  exit 1
fi

if [[ -n "${PAYSTACK_SECRET_KEY:-}" || -n "${VTPASS_API_KEY:-}" ]]; then
  echo "==> cmd/certify (sandbox keys present)"
  go run ./cmd/certify -o /tmp/meterly-canary-certify.json
fi

echo "==> canary OK"
