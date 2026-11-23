# Meterly

Electricity, recharged from anywhere. Meterly lets people in Nigeria buy electricity for their own prepaid meters or someone else's, then either deliver it **remotely** to compatible smart meters (no keypad, no ladder) or hand back a conventional **STS token** as a fallback.

This repository holds the backend platform: a Go modular monolith built around a transaction engine that tracks **payment, vending and delivery as three separate, auditable steps**.

> **Status:** persistence (PostgreSQL) and crash-safe background processing are done. Providers are still **mocks**, and auth is a dev stub. Production also needs real provider adapters, OIDC, and the legal and DisCo agreements listed in [`docs/ROADMAP.md`](docs/ROADMAP.md). Design choices are recorded in [`docs/DECISIONS.md`](docs/DECISIONS.md). **Current state, next steps and session context: [`docs/HANDOFF.md`](docs/HANDOFF.md).**

## Quick start

In-memory, no database:

```bash
MODE=dev go run ./cmd/api        # listens on :8080, worker runs in-process
```

With PostgreSQL (full local stack via Docker Compose — runs PostgreSQL 16, `migrate`, and `api` on `:8080` with internal Prometheus metrics on `127.0.0.1:9090/metrics`):

```bash
docker compose up -d --build
./scripts/smoke-compose.sh
```

Or run Go binaries directly on the host against Compose PostgreSQL:

```bash
docker compose up -d postgres
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

**Meter access:** self-service registration and beneficiary saving grant `PURCHASE`, not ownership or `MANAGE`; provider meter validation does not prove authority over a household. `DELETE /v1/meters/{meter_id}` removes only the caller's saved access and writes an audit record. It does not cancel already-created purchases, remove another person's access or prevent the caller from saving the meter again. New transaction creation is serialized with access removal and rechecks the grant. Household delegation and verified ownership are not implemented. Existing `MANAGE` grants created by older versions remain unchanged and must not be trusted as proof of ownership.

**Gift contact & token SMS:** a gift purchase can optionally include an E.164 `recipient_phone` and `notify_recipient: true`. The phone is encrypted and bound to the transaction, with its key version stored; it is never returned in transaction responses or added to outbox events. Sending a token-bearing SMS to the gift recipient requires all three gates to hold: `FEATURE_GIFT_SMS=true` (defaults to `false` until legal sign-off), `notify_recipient: true` on `gift_transactions`, and no matching global `STOP` opt-out for that destination HMAC. The token is decrypted only in worker memory at send time and is never automatically resent once an attempt ends in `UNKNOWN`.

**Notification candidates (disabled by default):** setting `NOTIFICATION_CANDIDATES_SINCE` to a fixed RFC3339 timestamp with timezone opts worker processes into collecting held, metadata-only candidates for selected events with `created_at` at or after the cutoff, including already-published events. Use the same stable cutoff on every replica; changing it can include historic events or skip uncollected ones. Event creation time is not commit time: transactions that create an event before the cutoff are excluded even if committed later. Migration `0005_notification_candidates.sql` must already be applied. Collection does not send, infer consent or select destinations; candidates are not delivery jobs. Review retention, capacity and backfill policy before enabling.

**Notifications (M5):** committed allowlisted outbox events create deduplicated `notification_deliveries` (`UNIQUE (event_id, recipient_kind, channel)`) without storing rendered bodies or raw destinations. Account-holder contacts are captured only from verified OIDC claims (`email` + `email_verified=true`, `phone_number` + `phone_number_verified=true`, or dev `X-Dev-Verified-Email` / `X-Dev-Verified-Phone` headers), encrypted bound to `users.id` with HMAC lookup and masked display (`migrations/0006_notifications.sql` drops the unused plaintext `users.email`/`users.phone` columns). Customers manage channel preferences at `GET/PUT /v1/me/notification-preferences` (SMS and email default `true`; WhatsApp defaults `false` per WhatsApp Business Policy and uses pre-approved templates outside the 24-hour customer-service window). Adapters and stateful fault-injecting fakes are implemented for Africa's Talking SMS (`SMS_PROVIDER=africastalking`), Resend (`EMAIL_PROVIDER=resend`), and WhatsApp Cloud API (`WHATSAPP_PROVIDER=whatsapp`), with signature-verified inbound `STOP`/complaint handling at `POST /v1/notifications/inbound/{provider}` (`opt_outs` keyed by destination HMAC so `STOP` works for non-users too) and a configurable 12-month `NOTIFICATION_HMAC_RETENTION` purge on settled delivery rows. Operators see redacted notification statuses on `GET /v1/admin/transactions/{id}` (every view is audited).

**Token copy history:** purchaser-only `POST /v1/electricity/transactions/{id}/token/shares` accepts `{"channel":"COPY"}` and requires an `Idempotency-Key` (16–128 characters); `GET` on the same path lists recorded copy actions. The entry is only a client-reported action—not proof of clipboard use, delivery or meter credit. No token or destination is stored in share metadata. SMS, email and WhatsApp sending are not implemented. Tokens work only on their own meter; sharing does not transfer credit.

**Receipt:** The purchaser can request `GET /v1/electricity/transactions/{id}/receipt` as JSON, or append `?format=html` for a printable HTML transaction record. Responses are `no-store`, display separate payment, vending and delivery states, and never include the token or checkout details. Pending records are not presented as completed deliveries.

**Observability, OpenAPI & Graceful Shutdown (M6):**
- **OpenAPI 3.1 (`api/openapi.yaml`):** complete specification covering all 34 production routes, request/response schemas, `bearerAuth` security scheme, and the standard error envelope + all 18 error codes, enforced in CI by a bidirectional mux-pattern and AST error-code parity test (`internal/httpapi/openapi_test.go`).
- **Prometheus metrics (`METRICS_ADDR`, default `:9090`, `off` to disable):** served strictly on a separate internal listener (never on the public API port), exposing `http_requests_total`, `http_request_duration_seconds`, `provider_calls_total`, `provider_call_duration_seconds`, `transactions_by_status` (refreshed every `METRICS_REFRESH_INTERVAL`, default `1m`), `manual_review_open`, `resume_escalations_total`, `outbox_lag_seconds`, and `notification_deliveries_total`. Meter numbers, tokens, phone numbers, emails, user IDs and transaction IDs are never used as metric labels.
- **OpenTelemetry tracing (`OTEL_EXPORTER_OTLP_ENDPOINT`):** W3C `traceparent` extraction/injection and OTLP/HTTP JSON span export for every HTTP request and outbound provider call with `meterly.correlation_id`; zero-overhead no-op when unset.
- **Containers & graceful shutdown:** multi-stage `Dockerfile` building static non-root `gcr.io/distroless/static-debian12:nonroot` images for `api`, `worker`, and `migrate`. On `SIGINT`/`SIGTERM`, `cmd/api` and `cmd/worker` drain in-flight HTTP requests, stop worker tickers from starting new claims, finish already-claimed operations within `SHUTDOWN_TIMEOUT`, flush the OTel tracer, and only then close PostgreSQL.

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
  httpapi/                /v1 REST API (spec §7) + OpenAPI parity tests
  observability/          Prometheus exposition & OTLP/HTTP JSON tracing (stdlib)
  platform/config/        environment configuration, validated at startup
  platform/migrate/       migration runner
api/openapi.yaml          OpenAPI 3.1 specification for all production routes
Dockerfile                multi-stage non-root distroless targets (api, worker, migrate)
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
