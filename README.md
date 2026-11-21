# Meterly

Electricity, recharged from anywhere. Meterly lets people in Nigeria buy electricity for their own prepaid meters or someone else's, then either deliver it **remotely** to compatible smart meters (no keypad, no ladder) or hand back a conventional **STS token** as a fallback.

This repository holds the backend platform: a Go modular monolith built around a transaction engine that tracks **payment, vending and delivery as three separate, auditable steps**.

> **Status:** persistence (PostgreSQL) and crash-safe background processing are done. Providers are still **mocks**, and auth is a dev stub. Production also needs real provider adapters, OIDC, and the legal and DisCo agreements listed in [`docs/ROADMAP.md`](docs/ROADMAP.md). Design choices are recorded in [`docs/DECISIONS.md`](docs/DECISIONS.md). **Current state, next steps and session context: [`docs/HANDOFF.md`](docs/HANDOFF.md).**

## Quick start

In-memory, no database:

```bash
MODE=dev go run ./cmd/api        # listens on :8080, worker runs in-process
```

With PostgreSQL:

```bash
docker compose up -d
export DATABASE_URL='postgres://meterly:meterly@localhost:5432/meterly?sslmode=disable'
go run ./cmd/migrate
MODE=dev go run ./cmd/api
```

Tests (the PostgreSQL integration tests run only when `DATABASE_URL` is set; each test gets its own database cloned from a migrated template):

```bash
go test -race ./...
```

### Processes

| Command | Role |
|---|---|
| `cmd/migrate` | Applies embedded migrations (transactional, checksummed, advisory-locked). |
| `cmd/api` | HTTP API. Refuses to start while migrations are pending. With `RUN_WORKER=true` (default in dev) it also runs the background loops. |
| `cmd/worker` | Resume/reconciliation sweep and outbox publisher. Safe to run as several replicas. It refuses to run with mock providers, so until real adapters land the worker runs inside `cmd/api`. |

Configuration is read from the environment and validated at startup (`internal/platform/config`).

**Auth:** `AUTH_MODE=dev` (dev only: the bearer token is the user; prefix it with `operator:` or `admin:` for staff roles) or `oidc` (required outside dev) with `OIDC_ISSUER`, `OIDC_AUDIENCE`, and optionally `OIDC_JWKS_URL`, `OIDC_ROLES_CLAIM` (default `roles`) and `OIDC_MFA_AMR`. Staff roles need MFA.

**Admin API** (`/v1/admin/*`, operator or admin role): search and investigate transactions, reconcile, release a token after a remote-delivery failure, reveal a token (audited), requeue or refund from manual review (admin), and switch providers on or off (admin). Every action needs a `reason` and is written to the audit log.

**Payments:** `PAYMENT_PROVIDER=mock` (default) or `paystack`. For Paystack, set `PAYSTACK_SECRET_KEY` (only `sk_test_…` is accepted outside production), optionally `PAYSTACK_CALLBACK_URL`, and point Paystack's webhook at `POST /v1/payments/webhooks/paystack`. Vending and remote credit are still mocks. Outside `MODE=dev` it requires `DATABASE_URL`, `TOKEN_ENC_KEY` and `METER_HMAC_KEY` (64 hex characters each), and non-mock providers.

Run a full purchase in dev (the bearer token is the user ID in dev mode):

```bash
B=http://localhost:8080; AUTH='Authorization: Bearer ada'

# 1. Register a meter. Mock meters starting with 7 support remote credit.
curl -s -X POST $B/v1/meters -H "$AUTH" \
  -d '{"meter_number":"71012345678","disco_code":"IKEDC","label":"Mum - Ibadan"}'

# 2. Quote ₦5,000 (amounts are in kobo)
curl -s -X POST $B/v1/electricity/quotes -H "$AUTH" \
  -d '{"meter_id":"<meter_id>","amount_minor":500000,"currency":"NGN","delivery_preference":"AUTO"}'

# 3. Create the transaction (idempotent; email is required)
curl -s -X POST $B/v1/electricity/transactions -H "$AUTH" \
  -H 'Idempotency-Key: 0123456789abcdef' -d '{"quote_id":"<quote_id>","email":"you@example.com"}'

# 4. Simulate checkout + signed webhook (ref = last part of checkout_url)
curl -s -X POST $B/v1/dev/payments/<ref>/complete -d '{}'
curl -s -X POST $B/v1/payments/webhooks/mockpay -H 'X-Mock-Signature: dev-signature' -d '{"reference":"<ref>"}'

# 5. Simulate the meter acknowledging; the worker picks it up (or force it)
#    (operators: curl -H 'Authorization: Bearer operator:you' $B/v1/admin/transactions?review=true)
curl -s -X POST $B/v1/dev/remote/settle
curl -s -X POST $B/v1/dev/transactions/<transaction_id>/resume
curl -s $B/v1/electricity/transactions/<transaction_id> -H "$AUTH"
```

If you skip the webhook in step 4, the worker still finds the payment by asking the provider once the transaction has been idle for `RESUME_STALE_AFTER`.

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
cmd/api, cmd/worker, cmd/migrate
internal/
  app/                    wiring shared by the binaries; worker loops
  orchestrator/           purchase workflow: quote → pay → vend → deliver → refund; Resume (recovery)
  registry/               meter registry service: validation, capabilities, access grants
  store/                  persistence contracts; InTx unit of work
  store/memstore/         in-memory store (tests, dev) with rollback
  store/pgstore/          PostgreSQL store (pgx v5); pgtest = per-test databases
  transactions/ meters/ ledger/ events/ idempotency/   domain types and rules
  outbox/                 at-least-once outbox publisher
  providers/              contracts for payment, vending, remote-credit providers
  providers/mock/         deterministic mocks for every success and failure path
  secure/                 AES-GCM token encryption, HMAC meter lookup, masking
  httpapi/                /v1 REST API (spec §7)
  platform/config/        environment configuration, validated at startup
  platform/migrate/       migration runner
migrations/               SQL migrations (embedded into the binaries)
docs/                     roadmap and decisions
```

## Rules the code enforces

- **No vending before the payment is checked with the provider server-side**, and the paid amount must match the quote exactly.
- **A timeout is `UNKNOWN`, never `FAILED`.** Unknown operations are settled by querying the provider. They are only re-sent after the provider confirms it has no record of them.
- **Remote `ACCEPTED` is not delivery.** A transaction completes only when the meter confirms it.
- **Refunds happen only when vending has definitely failed.** Once electricity has been vended, it is never refunded automatically.
- **Every money movement is a balanced ledger posting**, made at most once per event. PostgreSQL also rejects unbalanced postings and any edit to a posted entry.
- **Each state change commits atomically** with its history, ledger posting and outbox events. Provider calls happen outside database transactions, and only the worker that claimed a step calls the provider for it.
- **Crash-safe:** if the process dies between any provider call and the next commit, `Resume` continues without double-charging, double-vending or double-crediting. Fault-injection tests cover this.
- **Stuck transactions escalate** to `MANUAL_REVIEW` after a bounded number of checks with backoff. Payment amount mismatches and late payments go to review too.
- **Tokens are encrypted at rest**, never logged, and only the purchaser can read them. Meter numbers are encrypted, looked up by HMAC and shown masked.
- **Access is granted per meter.** Validating a meter number grants no access. Revoking access blocks purchases even with a quote already issued.
