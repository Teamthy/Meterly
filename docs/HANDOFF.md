# Handoff

> Read this first. Update it at the end of every session (see `CLAUDE.md`).
> **Last updated:** 2026-11-23, after M11 (F5 Meterly Offline — USSD & WhatsApp purchase channels).

## 1. What Meterly is

A Nigerian electricity platform. People buy prepaid electricity from anywhere, for their own meter or someone else's. It is delivered **remotely** to compatible smart meters (no keypad, no ladder) or as an **STS token** fallback. The full product spec (PRD, architecture, schema, API, roadmap) was supplied by the founder in the first session's chat; `docs/ROADMAP.md` maps it to the code. The backend is a Go modular monolith.

## 2. Current state

| Milestone | Status | Where |
|---|---|---|
| Foundation: transaction engine, mocks, API | ✅ merged | [PR #1](https://github.com/Teamthy/Meterly/pull/1) |
| M1: PostgreSQL persistence, atomic units of work | ✅ merged | [PR #2](https://github.com/Teamthy/Meterly/pull/2) |
| M2: api/worker split, Resume/reconciliation, config | ✅ merged | [PR #2](https://github.com/Teamthy/Meterly/pull/2) |
| M3: Paystack payment adapter | ✅ merged | [PR #3](https://github.com/Teamthy/Meterly/pull/3) |
| M4: OIDC auth, admin API, audit, rate limits | ✅ merged | [PR #3](https://github.com/Teamthy/Meterly/pull/3), [PR #4](https://github.com/Teamthy/Meterly/pull/4) |
| M5: notification delivery, preferences, inbound STOP, gift SMS | ✅ merged | [PR #5](https://github.com/Teamthy/Meterly/pull/5) |
| M6: OpenAPI 3.1, Prometheus `/metrics`, OpenTelemetry, Dockerfiles, graceful shutdown | ✅ merged | [PR #6](https://github.com/Teamthy/Meterly/pull/6) |
| M7: F1 Pay-for-Me shareable payment request links | ✅ merged | [PR #7](https://github.com/Teamthy/Meterly/pull/7) |
| M8: F2 Split the Meter (reusable Funding Pot) | ✅ merged | [PR #8](https://github.com/Teamthy/Meterly/pull/8) |
| M9: F3 AutoPower (scheduled auto-recharge with spend guard) | ✅ merged | [PR #9](https://github.com/Teamthy/Meterly/pull/9) |
| M10: F4 Landlord Power Desk (orgs, unit meters, `LANDLORD_BULK` pots, statements) | ✅ merged | [PR #10](https://github.com/Teamthy/Meterly/pull/10) |
| M11: F5 Meterly Offline (USSD & WhatsApp purchase channels) | ✅ complete on `arena/7cc28241-meterly` | PR #11 open to `main` |

PR #3 is merged. The current session branch was fast-forwarded to `origin/main` before this work; the earlier unrelated foundation was preserved in a Git stash and not reused.

CI (`.github/workflows/ci.yml`) runs gofmt, vet, `go test -race` against a PostgreSQL 16 service, and fails if the PostgreSQL subtests were skipped. It passed on GitHub for PR #2.

## 3. Architecture in one minute

```
cmd/api        HTTP API (+ worker loops in-process when RUN_WORKER=true, default in dev)
cmd/worker     resume sweep + outbox publisher (refuses to run with mock providers)
cmd/migrate    applies embedded migrations

internal/orchestrator   purchase workflow; Resume = single recovery path   ← core
internal/store          Store/Repos contracts; InTx = unit of work
  memstore              in-memory, undo-log rollback (tests, dev)
  pgstore               PostgreSQL (pgx v5); pgtest = per-test DBs
internal/registry       meter validation, capabilities, access grants
internal/providers      Payment / Vending / RemoteCredit contracts
  mock                  deterministic mocks (behaviour keyed by meter number; see README)
  paystack (+paystacktest fake)
internal/transactions meters ledger events idempotency money secure   domain
internal/notifications  templates (i18n catalog + gift SMS), status transitions, transport contracts
  africastalking (+africastalkingtest)   SMS adapter + HMAC inbound STOP
  resend (+resendtest)                   email adapter + status API + Svix webhook verification
  whatsapp (+whatsapptest)               WhatsApp Cloud API (template outside 24h window) + X-Hub-Signature-256
internal/notificationservice   outbox sink -> notification_deliveries, claim/send/UNKNOWN engine, inbound STOP, retention purge
internal/payrequests    Pay-for-Me domain, 128-bit base32 HMAC codes, anti-phishing validation, pure-stdlib QR PNG encoder
internal/pots           reusable Funding Pot domain (`SPLIT_METER`, `LANDLORD_BULK`), contributions, allocations, initials & AEAD helpers
internal/autopower      AutoPower domain, bound AEAD card mandate encryption, card expiry & Africa/Lagos schedule engine
internal/authz          centralised organisation RBAC & zero-IDOR guard (`authz.Require`)
internal/powerdesk      Landlord Power Desk domain, Africa/Lagos monthly statement bounds & CSV formula-safe exporter
internal/conversation   channel-agnostic USSD & WhatsApp purchase state machine, OTP & Africa/Lagos daily caps
internal/channels/ussd  Africa's Talking USSD HTTP adapter (+ussdtest simulator; IP allowlist + HMAC, <= 182 chars)
internal/offline        USSD & WhatsApp store-to-orchestrator wiring, phone OTP linking & audited token resends
internal/observability  stdlib Prometheus 0.0.4 registry + W3C TraceContext / OTLP/HTTP JSON spans + provider wrappers
internal/httpapi        /v1 REST + /v1/admin + /v1/payment-requests + /v1/public/pay/* + /v1/pots + /v1/public/pots/* + /v1/me/payment-methods + /v1/autopower/* + /v1/orgs/* + /v1/properties/* + /v1/units/* + /v1/channels/* + /v1/notifications/inbound/{provider}
api/openapi.yaml        OpenAPI 3.1 spec verified against Server.RegisteredPatterns() + writeErr AST codes
Dockerfile              multi-stage non-root distroless targets: api, worker, migrate
internal/auth           DevAuth + OIDC (stdlib JWT/JWKS, verified contact capture), roles, MFA gate
internal/audit          audit events + actor; correlation ID in context
internal/ratelimit      in-memory token buckets
internal/orchestrator/admin.go   operator actions (all audited, reason required)
internal/platform/{config,migrate}
migrations/0001_init.sql .. 0013_offline_channels.sql
```

How a purchase flows: quote → `CreateTransaction` (idempotent, one commit) → `EnsurePaymentInitialised` (checkout) → webhook or sweeper → `checkPayment` (always verifies with the PSP) → `vend` → `deliver` (TOKEN: confirmed in the same commit; REMOTE: send, then ACCEPTED, then the meter confirms) → refund only on a definitive vend failure.

## 4. Invariants (must hold after every change)

1. No vending until payment is verified server-side and the amount equals the quote total exactly. A mismatch goes to MANUAL_REVIEW.
2. A timeout or ambiguous error means UNKNOWN, never FAILED. Resolve it by status query; re-send only if the provider confirms it has no record.
3. Remote ACCEPTED ≠ delivered. Only meter confirmation completes.
4. Refund only after a definitive vend failure.
5. Every money movement is a balanced, idempotent ledger posting. PostgreSQL triggers enforce balance and append-only.
6. No raw tokens or meter numbers in logs, URLs or plaintext columns. Tokens are readable only by the purchaser.
7. Duplicates and concurrency never double-charge, double-vend or double-credit.

**How the code enforces them:**
- Every state change goes through `orchestrator.mutate` (lock row → guarded change → save state, history, ledger and outbox in one tx).
- Guards return `errSkip` when another worker got there first.
- **Only the worker whose commit claimed a step may call the provider**: look for the `claimed` flag in `vend`, `deliver`, `refund` and the reconcile re-sends. Do not infer the claim from the resulting state. That exact bug (10 webhooks → 10 vend calls) was only caught on PostgreSQL.

## 5. Running things

```bash
MODE=dev go run ./cmd/api                      # in-memory, everything mocked
docker compose up -d && export DATABASE_URL='postgres://meterly:meterly@localhost:5432/meterly?sslmode=disable'
go run ./cmd/migrate && MODE=dev go run ./cmd/api
go test -race ./...                            # with DATABASE_URL set for the PG suite
```

### Running PostgreSQL locally (cloud sandbox, no Docker daemon)

PostgreSQL 16 binaries are at `/usr/lib/postgresql/16/bin`. Run the cluster as the `postgres` user in a directory it owns (it can't access the scratchpad):

```bash
D=/var/tmp/meterly-pg; PG=/usr/lib/postgresql/16/bin
mkdir -p $D && chown postgres $D
su postgres -c "$PG/initdb -D $D/data -A trust -U postgres"
su postgres -c "$PG/pg_ctl -D $D/data -o '-p 5499 -k $D -c listen_addresses=127.0.0.1 -c max_connections=300' -l $D/log start"
export DATABASE_URL='postgres://postgres@127.0.0.1:5499/postgres?sslmode=disable'
```

The server can stop between turns, which shows up as "connection refused"; rerun `pg_ctl … start`. Tests create `meterly_tmpl_*` / `meterly_test_*` databases and drop them; check for none left with `psql -h $D -p 5499 -U postgres -Atc "select datname from pg_database where datname like 'meterly_%'"`. Any new test package that uses `pgtest` needs a `TestMain` that calls `pgtest.DropTemplate()`.

## 6. Gotchas and context

- **Mock providers keep state in-process.** That's why `cmd/worker` refuses to run with mocks; in dev, the worker runs inside `cmd/api`. With any mock in use (vending and AMI are still mocks), only `MODE=dev` can start.
- **Paystack** has been tested only against `paystacktest` (a stateful fake), **never against the live sandbox**. Each initialisation attempt gets a new reference (`<txn-id>.<random>`). Refunds are made idempotent by listing existing ones first. Only `sk_test_` keys are accepted outside production.
- **Email is required** on `POST /v1/electricity/transactions` (Paystack needs it).
- **Migrations are immutable once applied**; the runner checks checksums. `0001` was edited once, before it was ever applied (its BEGIN/COMMIT were removed). Add new files only.
- **Git:** force-pushes are blocked by the session's permission classifier. When a PR has merged and the branch needs `main`, use `git merge origin/main`, not a rebase. Develop on the branch named in the session's system instructions.
- **Orchestrator time** is `o.Now` (injectable); tests use it to make transactions "stale" for `ResumeDue`. Transaction timestamps themselves use wall time.
- **Auth:** `AUTH_MODE=dev` (dev only): the bearer token *is* the subject, and the `operator:`/`admin:` prefixes grant staff roles. `AUTH_MODE=oidc` is required outside dev, and staff roles need MFA (`amr`). Users are created on first sight from `sub`.
- **Operator actions** live under `/v1/admin/*`. Each needs a `reason` and writes an audit row in the same tx. `/v1/dev/*` now only has simulation helpers (complete payment, settle meter, resume).
- **Operator refunds** only from MANUAL_REVIEW, only if vending is NOT_STARTED/FAILED, and for the amount the PSP says was paid. The new payment transitions PENDING/FAILED→REFUND_PENDING are for that path only; automation never takes them.
- **Provider disable** pauses new outbound work without failing or escalating anything (`ErrProviderDisabled`; `schedule(..., paused)`).
- **Rate limits are per process** (in-memory). The dev-auth "admin:" prefix must never be reachable in prod; config refuses `AUTH_MODE=dev` outside `MODE=dev`.
- **Tests with a frozen clock** (`e.later`) give identical `CreatedAt`s; advance the clock between creations if order matters.

## 7. What's next

**All M1–M11 core platform & flagship product milestones (F1–F5) are implemented and verified.** Next engineering focus: first real Nigerian DisCo/aggregator vending adapter, AMI/HES remote-credit adapter, and live provider sandbox verification.

**Blocked on the founder or partners (not code):**
- **M11 USSD founder blocker — NCC/aggregator USSD shortcode:** provisioning a dedicated or shared Nigerian USSD shortcode (e.g. `*xxx#` via Africa's Talking + NCC/telco approval), configuring aggregator source CIDR ranges (`USSD_ALLOWED_IPS`) and `USSD_SHARED_SECRET`, and setting `FEATURE_USSD=true`
- **M11 WhatsApp founder blocker — WhatsApp Business Account (WABA) with approved templates:** registering a verified Meta WhatsApp Business Account + phone number ID, obtaining Meta approval for out-of-window utility templates (including `meterly_offline_token_v1` alongside M5 templates), configuring `WHATSAPP_APP_SECRET` + `WHATSAPP_VERIFY_TOKEN` + `WHATSAPP_ACCESS_TOKEN`, and setting `FEATURE_WHATSAPP=true`
- a real **vending** partner/aggregator (e.g. Buypower / VTpass / Irecharge) and its API docs (needed before any non-dev deployment)
- an **AMI/HES remote-credit** partner and an authorised test on a real meter
- live sandbox credentials + webhook configuration to verify Paystack, Africa's Talking, Resend and WhatsApp Cloud API against live provider sandboxes
- legal sign-off (NDPA/NCC and CBN/PSP fund-holding terms) before enabling `FEATURE_GIFT_SMS=true`, `FEATURE_SPLIT_METER=true`, `FEATURE_AUTOPOWER=true`, `FEATURE_POWER_DESK=true`, `FEATURE_USSD=true`, or `FEATURE_WHATSAPP=true` in production
- choice of identity provider (Auth0, Clerk, Keycloak, Cognito…) with `email_verified` / `phone_number_verified` claims, a roles claim and MFA for staff

## 8. Session log (newest first)

- **2026-11-23 (latest): M11 Meterly Offline (USSD & WhatsApp purchase channels) completed (M10 merged in PR #10).**
  - Engine, USSD & WhatsApp adapters (`internal/conversation`, `internal/channels/ussd`, `internal/offline`, `internal/notifications/whatsapp`, `docs/DECISIONS.md`): built channel-agnostic conversational state machine persisted in `conversation_sessions` (`0013_offline_channels.sql` across `memstore` and `pgstore`), Africa's Talking USSD HTTP adapter + `ussdtest.Simulator` enforcing CIDR allowlist + HMAC-SHA256 (`401` on failure), `<= 182`-character `CON`/`END` responses, and ~180s session TTL (creating zero transactions on mid-flow timeout), and Meta WhatsApp Cloud API inbound `hub.verify_token` + `X-Hub-Signature-256` verification (`401` on failure), duplicate `wamid` + out-of-order timestamp protection, free-form in-chat token delivery inside the 24h customer-service window with automatic fallback to `meterly_offline_token_v1` outside 24h, and `STOP` integration with M5 `opt_outs`.
  - Phone identity, SIM-swap defense, orchestrator wiring & HTTP/OpenAPI (`internal/httpapi/offline.go`, `api/openapi.yaml`): mapped authenticated inbound phones via HMAC (`phone_channel_links`), auto-linked OIDC-verified phones, added `POST /v1/channels/phone/link/{start,verify}` + conversational 6-digit SMS OTP linking (`channel_otps`), enforced SIM-swap inactivity reverification (`OFFLINE_REVERIFY_AFTER`, default `30d`) and unverified vs verified `Africa/Lagos` daily purchase caps, routed all purchases through `orchestrator.Quote` + `CreateTransaction` (`Idempotency-Key = ussd:<sessionId>:<step>` / `whatsapp:<sessionId>:<step>`), added caller-scoped audited `<= 3/day` `"Last token"` resends, hashed session turn inputs so zero raw phones/meters/OTPs/tokens persist in logs or PostgreSQL tables, and gated behind `FEATURE_USSD` and `FEATURE_WHATSAPP` (both default `false`).

- **2026-11-23 (earlier): M10 Landlord Power Desk completed (M9 merged in PR #9).**
  - RBAC, domain & decisions (`internal/authz`, `internal/powerdesk`, `docs/DECISIONS.md`): implemented `authz.Require(ctx, orgID, role)` (`OWNER > MANAGER > VIEWER`) returning `404` on non-member/cross-org access (zero IDOR), unit-meter verification states (`ACCOUNT_NAME_MATCH`, `EVIDENCE_APPROVED`), honest partial-success bulk summary formatting, and `Africa/Lagos` monthly statement bounds + CSV formula-injection sanitizer labelling vending `"electricity purchased"`.
  - Schema & dual stores (`migrations/0012_power_desk.sql`, `internal/store/{memstore,pgstore}/powerdesk.go`): reused `organisations`, `roles`, `organisation_members`, `properties`, `buildings`, `units`; added `unit_meters`, `bulk_topups`, `bulk_topup_items`, `organisations.manager_approval_threshold_minor`, and `PENDING_APPROVAL` on `funding_pots`; verified with `TestPowerDeskRepoParity` across both stores.
  - Orchestrator, `LANDLORD_BULK` pots & HTTP/OpenAPI (`internal/orchestrator/{powerdesk,pots}.go`, `internal/httpapi/powerdesk.go`, `api/openapi.yaml`): implemented 16 Power Desk + admin endpoints gated behind `FEATURE_POWER_DESK` (default `false`); `LANDLORD_BULK` Funding Pots create 1 landlord contribution and N POT-funded unit allocations (₦100 fee per unit), support `MANAGER` over-threshold `OWNER` approval before checkout, execute automatic partial pot refunds for failed units while never refunding `UNKNOWN` vends, and compute monthly statements strictly from double-entry ledger postings. Verified under `-race` on `memstore` and PostgreSQL 16.16.

- **2026-11-23 (earlier): M9 AutoPower scheduled & low-balance auto-recharge completed (M8 merged in PR #8).**
  - Saved mandates & Paystack (`internal/autopower`, `migrations/0011_autopower.sql`, `internal/providers/paystack`): explicit consent-gated `POST /v1/me/payment-methods/{transaction_id}/save` sealing card authorization codes with AEAD bound to `(user_id, pm_id)`; `ChargeSaved` via `POST /transaction/charge_authorization` always verified via `VerifyPayment`; deleting a saved card or revoking meter access atomically pauses active rules.
  - Scheduler, spend guards & low-balance poller (`internal/orchestrator/autopower.go`, `internal/httpapi/autopower.go`, `api/openapi.yaml`): `Africa/Lagos` weekly/monthly scheduler with 12h heads-up notifications, one-tap `POST /v1/autopower/runs/{id}/skip`, 6h catch-up window, row-locked monthly rule cap + `AUTOPOWER_DAILY_MAX_MINOR` per-user cap, 3-strike failure auto-pause, and capability-gated `MeterReadingProvider` low-balance poller with 24h cooldown; gated behind `FEATURE_AUTOPOWER` (default `false`).

- **2026-11-23 (latest): M8 Split the Meter (reusable Funding Pot) completed (M7 merged in PR #7).**
  - Legal gate & ledger (`docs/DECISIONS.md`, `internal/ledger`, `migrations/0009_funding_pots.sql`): gated behind `FEATURE_SPLIT_METER` (default `true` in `dev`, `false` outside `dev`) and `POT_MAX_HOLD` (default `72h`, cap `7d`), added liability account `POT_FUNDS_HELD`, and extended `electricity_transactions` with `funding_source` (`PSP` | `POT`).
  - Domain & stores (`internal/pots`, `internal/store/{memstore,pgstore}`, `internal/providers/paystack`): built `Pot` (`SPLIT_METER`, `LANDLORD_BULK`), `Contribution` (`AmountMinor`, `RetainedMinor`, `RefundAmountMinor`, `RefundedMinor`, deterministic `refund:contribution:<id>` keys, privacy-preserving `FormatInitials`), and `Allocation`; updated Paystack client + `paystacktest` to match refund `merchant_note` references and support cumulative partial refunds.
  - Orchestrator, notifications & HTTP API (`internal/orchestrator/pots.go`, `internal/httpapi/pots.go`, `internal/notificationservice`, `migrations/0010_pot_contributor_notifications.sql`, `api/openapi.yaml`): implemented `POST /v1/pots`, `GET /v1/pots/{id}`, `POST /v1/pots/{id}/cancel`, `GET /v1/public/pots/{code}`, and `POST /v1/public/pots/{code}/contribute`; branched all automated and operator refund paths on `FundingSource` (`POT` returns `CUSTOMER_FUNDS_CLEARING -> POT_FUNDS_HELD` and drives per-contributor PSP refunds while blocking refunds on `VendUnknown`); added M5 `POT_CONTRIBUTOR` consented settlement SMS; verified under `-race` on both `memstore` and PostgreSQL 16.16.

- **2026-11-23 (earlier): M7 Pay-for-Me links completed (M6 merged in PR #6).**
  - Domain & QR (`internal/payrequests`): 128-bit unpadded RFC 4648 base32 link and guest status codes indexed solely by domain-separated HMAC-SHA256 (`payrequest-code:<code>`, `payer-status-code:<code>`) with constant-time comparison, AES-256-GCM bound encryption for notes, guest emails, guest names and replay status codes, anti-phishing URL/domain rejection on notes and payer names, and a pure-stdlib ISO/IEC 18004 QR Code PNG encoder (`EncodeQRPNGBase64`).
  - Schema & stores (`migrations/0007_payment_requests.sql`, `migrations/0008_payer_notifications.sql`, `internal/store/{memstore,pgstore}`): added `payment_requests`, `transaction_payers` (`slot_released` guard), and widened `notification_deliveries.recipient_kind` to include `'PAYER'` for token-free payment/refund receipt emails (with automatic `DUPLICATE_DESTINATION` suppression when payer == requester).
  - Orchestrator & HTTP API (`internal/orchestrator/payrequests.go`, `internal/httpapi/payrequests.go`, `api/openapi.yaml`): implemented `POST/GET/DELETE /v1/payment-requests`, `GET /v1/public/pay/{code}`, `POST /v1/public/pay/{code}/checkout`, and `GET /v1/public/pay/status/{status_code}` behind `FEATURE_PAY_FOR_ME` (`no-store` + `Referrer-Policy: no-referrer`, per-IP + per-link rate limits, `SELECT ... FOR UPDATE` slot concurrency returning `409 LINK_ALREADY_PAID`, `409 LINK_UNUSABLE` on revoked meter grant, automatic slot reopening across all three `PayFailed` paths, and mid-checkout link expiry honouring). Verified with `-race` on both `memstore` and PostgreSQL 16.16 + `paystacktest`.

- **2026-11-23 (later): M6 production readiness completed (M5 merged in PR #5).**
  - OpenAPI 3.1 (`api/openapi.yaml` + `internal/httpapi/openapi_test.go`): authored full OpenAPI 3.1.0 contract covering all 34 production routes, schemas, `bearerAuth` security scheme, and standard error envelope + all 18 error codes; added `Server.RegisteredPatterns()` and CI tests verifying bidirectional mux parity, `$ref` integrity, path parameter coverage, and AST `writeErr` code coverage (`gopkg.in/yaml.v3` justified in `docs/DECISIONS.md`).
  - Observability (`internal/observability`): implemented stdlib Prometheus 0.0.4 metrics registry served strictly on an internal listener (`METRICS_ADDR`, default `:9090`, `404` on public API port) with zero PII/high-cardinality labels (`http_requests_total`, `http_request_duration_seconds`, `provider_calls_total`, `provider_call_duration_seconds`, 1-minute `transactions_by_status` / `manual_review_open` / `outbox_lag_seconds` gauges, `resume_escalations_total`, `notification_deliveries_total`), plus W3C `traceparent` + OTLP/HTTP JSON span export (`OTEL_EXPORTER_OTLP_ENDPOINT`) with `meterly.correlation_id` on every HTTP and provider span.
  - Graceful shutdown (`internal/app/app.go`, `internal/app/shutdown_test.go`): unified ordered shutdown draining public HTTP, stopping worker loops from claiming new rows while finishing in-flight claims within `SHUTDOWN_TIMEOUT`, shutting down the internal metrics listener, flushing OTel spans, and only then closing PostgreSQL; verified under `-race`.
  - Containers & CI (`Dockerfile`, `.dockerignore`, `docker-compose.yml`, `scripts/smoke-compose.sh`, `.github/workflows/ci.yml`): added multi-stage non-root `gcr.io/distroless/static-debian12:nonroot` targets (`api`, `worker`, `migrate`), extended `docker-compose.yml` for the full stack, and added CI image builds + compose end-to-end purchase and metrics smoke test.

- **2026-11-23: M5 notification delivery completed.**
  - Schema (`migrations/0006_notifications.sql`): dropped unused plaintext `users.email`/`users.phone` columns and indexes; added bound-encrypted contact columns (`email_ciphertext`, `email_hmac`, `email_masked`, `email_key_version`, `phone_ciphertext`, `phone_hmac`, `phone_masked`, `phone_key_version`, `display_name`) on `users`; created `notification_preferences`, `opt_outs`, `whatsapp_service_windows`, and `notification_deliveries` (`UNIQUE (event_id, recipient_kind, channel)`). Kept `notification_candidates` (`0005`) intact.
  - Contact capture & preferences: `auth.OIDC` captures `email` / `phone_number` strictly when `email_verified === true` / `phone_number_verified === true`; `httpapi` seals contacts via `EncryptBound` (`"user-email:"+uid`, `"user-phone:"+uid`). Added `GET/PUT /v1/me/notification-preferences` (`sms=true, email=true, whatsapp=false` defaults).
  - Delivery engine (`internal/notificationservice`): atomic outbox dispatch via `outbox.Publisher` (`store.WithRepos`), explicit `PENDING -> SENDING` claim before external calls, send-time opt-out/preference re-check (`-> SUPPRESSED`), `UNKNOWN` handling on timeout/5xx/lost response (status API reconciliation on Resend; zero auto-resend of token-bearing gift SMS; single retry on non-token `UNKNOWN`; `30s, 2m, 10m, 1h` backoff up to 5 attempts on retryable errors), gift-to-self phone dedupe (`DUPLICATE_DESTINATION`), and configurable 12-month `destination_hmac` retention purge.
  - Provider adapters & fakes: `africastalking` + `africastalkingtest`, `resend` + `resendtest`, and `whatsapp` + `whatsapptest` (enforcing pre-approved templates outside the 24h customer-service window and Meta `131047`), plus `GET/POST /v1/notifications/inbound/{provider}` signature-verified `STOP`/complaint handling and audited redacted `notifications` array on `GET /v1/admin/transactions/{id}` (`ResourceType: "notification_deliveries"`).
  - Verified: `gofmt`, `go vet ./...`, `go test -race -count=1 ./...` on memory + PostgreSQL 16.16, 3× concurrency repeats, twice-idempotent `cmd/migrate` run, and clean `meterly_*` DB check. Adapters verified against stateful fakes only (no live provider calls; Supabase untouched).

- **Opt-in held-candidate worker:** `NOTIFICATION_CANDIDATES_SINCE` (nonzero RFC3339 timestamp) enables bounded collection; absent means off. Inclusive event-created-at cutoff applies independently of published status; a late commit whose creation time predates the cutoff is excluded. No sends or recipient consent. Keep cutoff stable across replicas and define rollout/backfill/retention before production; database does not enforce consistency. Worker now waits for background loops during shutdown. Boundary, delayed arrival, opt-in/default, log privacy and worker tests pass on memory/PostgreSQL. Supabase remains untouched.


- **Purchase/removal serialization:** transaction creation now locks the meter before reauthorizing the purchase grant, serializing with self-removal and registration. Deterministic gate tests force both lock orderings against memory/PostgreSQL: removal first rejects creation without consuming the quote; creation first leaves the committed purchase in flight. Future grant mutation paths must also take the meter lock. Race suite and vet pass with disposable local PostgreSQL.


- **Meter access hardening:** registration now grants PURCHASE only, never MANAGE based on a meter number. `DELETE /v1/meters/{id}` removes caller's own access atomically with audit; row locking serializes registration/removal. Invalid permission comparisons fail closed. Concurrent removal and injected audit failure tested on memory/PostgreSQL; HTTP tests cover privacy, restored registration, quote reauthorization and isolation. Legacy MANAGE grants persist unverified; do not use them for household delegation. Existing in-flight purchases are not cancelled. Verified ownership and household delegation remain unimplemented.


- **Held notification candidates:** migration 0005 and the then-unwired transactional consumer record metadata-only candidates for allowlisted events, independently of outbox publication. Unique event keys, row locks and rollback tests cover concurrent/replayed consumption on memory and PostgreSQL. No sender or consent/destination logic is wired; candidates must never be treated as delivery jobs. Define retention and a historical-event cutoff/backfill policy before enabling. Supabase was not migrated.


- **Encrypted gift-contact storage:** optional E.164 recipient phone and purchaser notification request are stored as AES-GCM ciphertext bound to the transaction, with a key version. New migration 0004 adds metadata; creation is atomic with the transaction and idempotency record. Contact-bearing request hashes use HMAC. This does not establish recipient consent, send messages, or implement key rotation and retention. HTTP tests cover encryption, associated data, idempotency and log/outbox/response redaction against both memory and PostgreSQL 16.16; race tests and vet use the temporary offline module overlay. Migration 0004 has not been applied to Supabase.


- **Copy metadata:** added purchaser-only, idempotent COPY record and history endpoints. Records, audit and outbox metadata commit atomically; no token is copied into the record, and the endpoint does not send messages or prove delivery. Concurrent duplicate requests and HTTP authorization tests pass on memory and PostgreSQL 16.16. The local PG server was downloaded temporarily from an npm-packaged distribution because Docker and system PostgreSQL were unavailable; it is not part of the repo. `go test -race -count=1` and `go vet` passed with the temporary offline Go module overlay. SMS/email/WhatsApp, durable notification delivery remained unimplemented at that point; gift contact storage was added subsequently.


- **Notification foundation:** added privacy-preserving event templates and process-local mock SMS/email/WhatsApp sender contracts with concurrent dedup tests. They are not connected to the outbox or real providers; durable contact storage and ambiguous-send handling are still required. `go test -race` and `go vet` passed with the temporary offline module overlay; PostgreSQL tests remained skipped (no local PostgreSQL or Docker). M5 is not complete.


- **Receipt slice:** added purchaser-only JSON and escaped printable HTML transaction records with no-store headers and independently displayed statuses. Unit and HTTP tests cover pending, completed, ownership, no token leakage and HTML escaping. Ran `go test -race` and `go vet` with a temporary offline module overlay and Go 1.27.4; PostgreSQL tests were skipped because this sandbox had neither Docker nor PostgreSQL binaries. M5 remains incomplete. No live provider or Supabase changes were made.


- **2026-11-21 (later): M4.** Opened PR #3 (M3 + handoff). Built:
  - `internal/auth`: OIDC verifier using only the stdlib (RS256/ES256, iss/aud/exp/nbf/iat, ≤60s skew, cached JWKS with rate-limited refresh on rotation, discovery), roles claim, MFA gate for staff.
  - `/v1/admin/*`: search with keyset paging, detail (ledger, events, audit), reconcile, fallback-to-token, audited token reveal, requeue, operator refund of the amount actually paid, provider enable/disable.
  - Audit log in the same tx as each action. Customer token reads audited.
  - Rate limits: per-user writes, per-IP auth failures and webhooks.
  - Migration `0003_operations.sql`.
  - Found and fixed a JWKS bug: the rate limit blocked a legitimate key rotation within 30s of startup.
  - Verified: full suite with the race detector on memory and PostgreSQL (green, repeated 3× for the concurrent suites). OIDC tested against an in-process JWKS only, **not a real identity provider**.

- **2026-11-21: M1, M2, M3.**
  - M1/M2 (PR #2, merged, CI green on GitHub): PostgreSQL store with `InTx`; orchestrator rewrite with atomic transitions and explicit claims; atomic idempotent create; migration runner; Resume worker with payment sweeper, expiry, late-payment and mismatch review, backoff and escalation; api/worker split; validated config; outbox publisher.
  - M3 (pushed, no PR yet): Paystack adapter, `ParseWebhook` interface, paystacktest fake, e2e tests on both stores, email required.
  - Verified: full suite with the race detector on memory and PostgreSQL 16 (all green); a manual binary run against PostgreSQL.
  - Not verified: the live Paystack sandbox.
  - Added this handoff and `CLAUDE.md`.
- **2026-11-20: foundation (PR #1, merged).** Transaction engine, mock providers, `/v1` API, schema `0001`, tests for the spec's critical scenarios.
