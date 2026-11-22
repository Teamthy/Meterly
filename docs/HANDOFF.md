# Handoff

> Read this first. Update it at the end of every session (see `CLAUDE.md`).
> **Last updated:** 2026-11-21, after M4 (auth, admin API, audit, rate limits).

## 1. What Meterly is

A Nigerian electricity platform. People buy prepaid electricity from anywhere, for their own meter or someone else's. It is delivered **remotely** to compatible smart meters (no keypad, no ladder) or as an **STS token** fallback. The full product spec (PRD, architecture, schema, API, roadmap) was supplied by the founder in the first session's chat; `docs/ROADMAP.md` maps it to the code. The backend is a Go modular monolith.

## 2. Current state

| Milestone | Status | Where |
|---|---|---|
| Foundation: transaction engine, mocks, API | ✅ merged | [PR #1](https://github.com/Teamthy/Meterly/pull/1) |
| M1: PostgreSQL persistence, atomic units of work | ✅ merged | [PR #2](https://github.com/Teamthy/Meterly/pull/2) |
| M2: api/worker split, Resume/reconciliation, config | ✅ merged | [PR #2](https://github.com/Teamthy/Meterly/pull/2) |
| M3: Paystack payment adapter | ✅ merged | [PR #3](https://github.com/Teamthy/Meterly/pull/3) |
| M4: OIDC auth, admin API, audit, rate limits | ✅ merged | [PR #3](https://github.com/Teamthy/Meterly/pull/3) (same branch) |
| M5: notifications, gifting delivery, token sharing, receipts | ⏳ next | — |
| M6: OpenAPI, metrics/tracing, Dockerfile | ⏳ | — |

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
internal/httpapi        /v1 REST + /v1/admin (admin.go); internal/app wiring; internal/outbox publisher
internal/auth           DevAuth + OIDC (stdlib JWT/JWKS), roles, MFA gate
internal/audit          audit events + actor; correlation ID in context
internal/ratelimit      in-memory token buckets
internal/orchestrator/admin.go   operator actions (all audited, reason required)
internal/platform/{config,migrate}
migrations/0001_init.sql, 0002_persistence.sql
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

**M5 (next session):** notification senders (SMS, email, WhatsApp; mocks first) consuming outbox events, deduped by event ID. Token delivery to gift recipients (`gift_transactions` contact encrypted). `token_shares` destination handling. Purchaser-only JSON/HTML receipts and COPY metadata are implemented; external sending remains.

**M6:** `api/openapi.yaml` plus a route-coverage test, OpenTelemetry, Prometheus `/metrics`, distroless Dockerfiles.

**Blocked on the founder or partners (not code):**
- a real **vending** partner/aggregator and its API docs (needed before any non-dev deployment)
- an **AMI/HES remote-credit** partner and an authorised test on a real meter
- a Paystack sandbox key plus webhook configuration for a live sandbox test
- choice of identity provider (Auth0, Clerk, Keycloak, Cognito…) with a roles claim and MFA for staff, so OIDC can be tested end to end
- legal review: NERC and state regulators, the CBN-licensed payment money flow, NDPA 2023

## 8. Session log (newest first)

- **Held notification candidates:** migration 0005 and an unwired transactional consumer record metadata-only candidates for allowlisted events, independently of outbox publication. Unique event keys, row locks and rollback tests cover concurrent/replayed consumption on memory and PostgreSQL. No sender or consent/destination logic is wired; candidates must never be treated as delivery jobs. Define retention and a historical-event cutoff/backfill policy before enabling. Supabase was not migrated.


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
