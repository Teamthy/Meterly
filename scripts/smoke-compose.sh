#!/usr/bin/env bash
set -euo pipefail

API_URL="${API_URL:-http://127.0.0.1:${API_PORT:-8080}}"
METRICS_URL="${METRICS_URL:-http://127.0.0.1:${METRICS_PORT:-9090}}"
AUTH_HEADER="Authorization: Bearer smoke-customer"

cleanup() {
  if [[ "${KEEP_COMPOSE:-0}" != "1" ]]; then
    docker compose logs api migrate || true
    docker compose down -v --remove-orphans
  fi
}
trap cleanup EXIT

echo "==> Starting compose stack..."
docker compose up -d --build

echo "==> Waiting for ${API_URL}/readyz..."
ready=0
for _ in $(seq 1 45); do
  if curl -fsS "${API_URL}/readyz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
if [[ "${ready}" -ne 1 ]]; then
  echo "ERROR: API did not become ready in time" >&2
  exit 1
fi

echo "==> Verifying /metrics is NOT exposed on the public API port..."
pub_metrics_code=$(curl -s -o /dev/null -w "%{http_code}" "${API_URL}/metrics")
if [[ "${pub_metrics_code}" != "404" ]]; then
  echo "ERROR: expected 404 from public /metrics, got ${pub_metrics_code}" >&2
  exit 1
fi

echo "==> Registering prepaid meter..."
meter_json=$(curl -fsS -X POST "${API_URL}/v1/meters" \
  -H "${AUTH_HEADER}" \
  -H "Content-Type: application/json" \
  -d '{"meter_number":"44123456789","disco_code":"IKEDC","label":"Smoke Home"}')
meter_id=$(python3 -c 'import json,sys; print(json.loads(sys.stdin.read())["meter_id"])' <<<"${meter_json}")

echo "==> Creating quote..."
quote_json=$(curl -fsS -X POST "${API_URL}/v1/electricity/quotes" \
  -H "${AUTH_HEADER}" \
  -H "Content-Type: application/json" \
  -d "{\"meter_id\":\"${meter_id}\",\"amount_minor\":250000,\"currency\":\"NGN\",\"delivery_preference\":\"TOKEN\"}")
quote_id=$(python3 -c 'import json,sys; print(json.loads(sys.stdin.read())["quote_id"])' <<<"${quote_json}")

echo "==> Creating electricity transaction..."
txn_json=$(curl -fsS -X POST "${API_URL}/v1/electricity/transactions" \
  -H "${AUTH_HEADER}" \
  -H "Idempotency-Key: smoke-compose-idem-0001" \
  -H "Content-Type: application/json" \
  -d "{\"quote_id\":\"${quote_id}\",\"email\":\"smoke@example.com\"}")
txn_id=$(python3 -c 'import json,sys; print(json.loads(sys.stdin.read())["transaction_id"])' <<<"${txn_json}")
checkout_url=$(python3 -c 'import json,sys; print(json.loads(sys.stdin.read())["checkout_url"])' <<<"${txn_json}")
pay_ref="${checkout_url##*/}"

echo "==> Completing simulated checkout (${pay_ref}) and verifying server-side..."
curl -fsS -X POST "${API_URL}/v1/dev/payments/${pay_ref}/complete" \
  -H "Content-Type: application/json" \
  -d '{}' >/dev/null

settled_json=$(curl -fsS -X POST "${API_URL}/v1/electricity/transactions/${txn_id}/payment-check" \
  -H "${AUTH_HEADER}")
overall_status=$(python3 -c 'import json,sys; print(json.loads(sys.stdin.read())["status"])' <<<"${settled_json}")
if [[ "${overall_status}" != "COMPLETED" ]]; then
  echo "ERROR: expected COMPLETED transaction status, got ${overall_status}: ${settled_json}" >&2
  exit 1
fi

echo "==> Retrieving STS token and JSON receipt..."
token_json=$(curl -fsS "${API_URL}/v1/electricity/transactions/${txn_id}/token" -H "${AUTH_HEADER}")
token_len=$(python3 -c 'import json,sys; print(len(json.loads(sys.stdin.read())["token"]))' <<<"${token_json}")
if [[ "${token_len}" -lt 10 ]]; then
  echo "ERROR: expected non-empty STS token" >&2
  exit 1
fi
curl -fsS "${API_URL}/v1/electricity/transactions/${txn_id}/receipt" -H "${AUTH_HEADER}" >/dev/null

echo "==> Verifying internal Prometheus metrics on ${METRICS_URL}/metrics..."
found_completed=0
for _ in $(seq 1 15); do
  metrics_out=$(curl -fsS "${METRICS_URL}/metrics")
  if grep -Fq 'transactions_by_status{status="COMPLETED"} 1' <<<"${metrics_out}"; then
    found_completed=1
    break
  fi
  sleep 1
done
if [[ "${found_completed}" -ne 1 ]]; then
  echo "ERROR: expected transactions_by_status{status=\"COMPLETED\"} 1 in /metrics" >&2
  exit 1
fi
grep -Fq 'http_requests_total{route="POST /v1/electricity/transactions",code="202"} 1' <<<"${metrics_out}"
grep -Fq 'provider_calls_total{provider="mockvend",op="vend",outcome="ok"} 1' <<<"${metrics_out}"
if grep -Fq "44123456789" <<<"${metrics_out}" || grep -Fq "${txn_id}" <<<"${metrics_out}"; then
  echo "ERROR: high-cardinality or sensitive identifier leaked into /metrics" >&2
  exit 1
fi

echo "==> Compose smoke test passed!"
