# Meterly

Electricity, recharged from anywhere. Meterly lets people in Nigeria buy electricity for their own prepaid meters or someone else's, then either deliver it **remotely** to compatible smart meters (no keypad, no ladder) or hand back a conventional **STS token** as a fallback.

This repository holds the backend platform: a Go modular monolith built around a transaction engine that tracks **payment, vending and delivery as three separate, auditable steps**.

> **Status: foundation / Phase 1–3 scaffold.** Everything runs against **mock providers and in-memory storage**. Production needs real provider adapters, PostgreSQL repositories, OIDC auth and the legal and DisCo agreements listed in [`docs/ROADMAP.md`](docs/ROADMAP.md).

## Quick start

```bash
go test -race ./...
MODE=dev go run ./cmd/api        # listens on :8080
```

Run a full purchase in dev (the bearer token is the user ID in dev mode):

```bash
B=http://localhost:8080; AUTH='Authorization: Bearer ada'

# 1. Register a meter. Mock meters starting with 7 support remote credit.
curl -s -X POST $B/v1/meters -H "$AUTH" \
  -d '{"meter_number":"71012345678","disco_code":"IKEDC","label":"Mum - Ibadan"}'

# 2. Quote ₦5,000 (amounts are in kobo)
curl -s -X POST $B/v1/electricity/quotes -H "$AUTH" \
  -d '{"meter_id":"<meter_id>","amount_minor":500000,"currency":"NGN","delivery_preference":"AUTO"}'

# 3. Create the transaction (idempotent)
curl -s -X POST $B/v1/electricity/transactions -H "$AUTH" \
  -H 'Idempotency-Key: 0123456789abcdef' -d '{"quote_id":"<quote_id>"}'

# 4. Simulate checkout + signed webhook (ref = last part of checkout_url)
curl -s -X POST $B/v1/dev/payments/<ref>/complete -d '{}'
curl -s -X POST $B/v1/payments/webhooks/mockpay -H 'X-Mock-Signature: dev-signature' -d '{"reference":"<ref>"}'

# 5. Simulate the meter acknowledging, then reconcile
curl -s -X POST $B/v1/dev/remote/settle
curl -s -X POST $B/v1/dev/reconcile
curl -s $B/v1/electricity/transactions/<transaction_id> -H "$AUTH"
```

### Mock meter behaviour

| Meter number | Behaviour |
|---|---|
| starts with `0` | fails validation |
| starts with `7` | supports remote credit (accepted → confirmed after `/dev/remote/settle`) |
| starts with `77` | remote command times out *after* crediting → `UNKNOWN` → reconciled to `CONFIRMED` without resending |
| starts with `76` | remote credit accepted, then rejected by the meter → `MANUAL_REVIEW` → operator fallback to token |
| ends with `0000` | vend fails definitively → automatic refund |
| ends with `9999` | vend times out but did vend → `UNKNOWN` → reconciled, **not** refunded |
| ends with `8888` | vend times out and did not vend → provider confirms no record → safe retry |
| anything else (11–13 digits) | token vending |

## Layout

```
cmd/api/                  HTTP server + reconciliation worker
internal/
  orchestrator/           purchase workflow: quote → pay → vend → deliver → reconcile/refund
  transactions/           3-dimension state machine (payment / vending / delivery) + store
  ledger/                 double-entry ledger (balanced, idempotent, append-only)
  meters/                 meter registry, capability engine, per-user access grants
  providers/              internal contracts for payment, vending, remote-credit providers
  providers/mock/         deterministic mocks for every success and failure path
  idempotency/            Idempotency-Key handling
  secure/                 AES-GCM token encryption, HMAC meter lookup, masking
  events/                 transactional outbox
  httpapi/                /v1 REST API (spec §7)
migrations/0001_init.sql  PostgreSQL schema, ledger balance + immutability triggers
docs/                     architecture notes and roadmap
```

## Rules the code enforces

- **No vending before the payment is checked with the provider server-side**, and the paid amount must match the quote exactly.
- **A timeout is `UNKNOWN`, never `FAILED`.** Unknown operations are settled by querying the provider. They are only re-sent after the provider confirms it has no record of them.
- **Remote `ACCEPTED` is not delivery.** A transaction completes only when the meter confirms it.
- **Refunds happen only when vending has definitely failed.** Once electricity has been vended, it is never refunded automatically.
- **Every money movement is a balanced ledger posting**, made at most once per event.
- **Tokens are encrypted at rest**, never logged, and only the purchaser can read them. Meter numbers are encrypted, looked up by HMAC and shown masked.
- **Access is granted per meter.** Validating a meter number grants no access. Revoking access blocks purchases even with a quote already issued.
