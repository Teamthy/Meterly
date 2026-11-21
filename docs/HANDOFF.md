# Handoff

> Read this first. Update it at the end of every session (see `CLAUDE.md`).
> **Last updated:** 2026-11-21, after M3 (Paystack adapter).

## 1. What Meterly is

A Nigerian electricity platform. People buy prepaid electricity from anywhere, for their own meter or someone else's. It is delivered **remotely** to compatible smart meters (no keypad, no ladder) or as an **STS token** fallback. The full product spec (PRD, architecture, schema, API, roadmap) was supplied by the founder in the first session's chat; `docs/ROADMAP.md` maps it to the code. The backend is a Go modular monolith.

## 2. Current state

| Milestone | Status | Where |
|---|---|---|
| Foundation: transaction engine, mocks, API | ✅ merged | [PR #1](https://github.com/Teamthy/Meterly/pull/1) |
| M1: PostgreSQL persistence, atomic units of work | ✅ merged | [PR #2](https://github.com/Teamthy/Meterly/pull/2) |
| M2: api/worker split, Resume/reconciliation, config | ✅ merged | [PR #2](https://github.com/Teamthy/Meterly/pull/2) |
| M3: Paystack payment adapter | ✅ done, **pushed but not in any PR** | branch `claude/loving-wozniak-yr6igw` (commit `2419928`, plus a merge of `main`) |
| M4: OIDC auth, admin API, audit, rate limits | ⏳ next | — |
| M5: notifications, gifting delivery, token sharing, receipts | ⏳ | — |
| M6: OpenAPI, metrics/tracing, Dockerfile | ⏳ | — |

**Open action for the founder:** open a PR for M3 (the branch is `main` + M3 and was green when last run).

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
internal/httpapi        /v1 REST; internal/app wiring; internal/outbox publisher
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
- **Dev auth:** the bearer token *is* the user's external ID; users are created on first sight (`users.external_id`). M4 replaces this with OIDC.
- Operator actions (resume, fallback-to-token) live under `/v1/dev/*` until M4.

## 7. What's next

**M4 (next session):** OIDC JWT verification (JWKS cache, RS256/ES256, ≤60s skew) replacing `DevAuth` outside dev. Customer and operator/admin roles. `/v1/admin/*`:
- transaction search and detail with history
- reconcile one transaction
- fallback-to-token with a reason
- resolve MANUAL_REVIEW (approve or reject a refund, re-queue)
- provider enable/disable

Also: `audit_events` written in the same tx as each admin action and each token read; an audited, masked-by-default admin token reveal; and per-user/IP rate limits.

**M5:** notification senders (SMS, email, WhatsApp; mocks first) consuming outbox events, deduped by event ID. Token delivery to gift recipients (`gift_transactions` contact encrypted). `token_shares` with hashed destinations. Receipts.

**M6:** `api/openapi.yaml` plus a route-coverage test, OpenTelemetry, Prometheus `/metrics`, distroless Dockerfiles.

**Blocked on the founder or partners (not code):**
- a real **vending** partner/aggregator and its API docs (needed before any non-dev deployment)
- an **AMI/HES remote-credit** partner and an authorised test on a real meter
- a Paystack sandbox key plus webhook configuration for a live sandbox test
- legal review: NERC and state regulators, the CBN-licensed payment money flow, NDPA 2023

## 8. Session log (newest first)

- **2026-11-21: M1, M2, M3.**
  - M1/M2 (PR #2, merged, CI green on GitHub): PostgreSQL store with `InTx`; orchestrator rewrite with atomic transitions and explicit claims; atomic idempotent create; migration runner; Resume worker with payment sweeper, expiry, late-payment and mismatch review, backoff and escalation; api/worker split; validated config; outbox publisher.
  - M3 (pushed, no PR yet): Paystack adapter, `ParseWebhook` interface, paystacktest fake, e2e tests on both stores, email required.
  - Verified: full suite with the race detector on memory and PostgreSQL 16 (all green); a manual binary run against PostgreSQL.
  - Not verified: the live Paystack sandbox.
  - Added this handoff and `CLAUDE.md`.
- **2026-11-20: foundation (PR #1, merged).** Transaction engine, mock providers, `/v1` API, schema `0001`, tests for the spec's critical scenarios.
