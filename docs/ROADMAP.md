# Roadmap and status against the master specification

This maps the *Electricity Technology Platform: Product Requirements, Architecture & Engineering Specification v1.0* to what exists in this repository today.

## Done in this scaffold

| Spec area | Status |
|---|---|
| §9.1 Three independent state dimensions | `internal/transactions`: validated transitions; overall status is derived |
| §9.3 Timeout ⇒ UNKNOWN, reconcile and never blindly resend | `orchestrator.Resume`; tested for vending and remote credit, on memory and PostgreSQL |
| §9.4 Idempotency | `internal/idempotency` and the `Idempotency-Key` header on transaction create |
| §9.5 Double-entry ledger | `internal/ledger`; Postgres triggers enforce balance and append-only |
| §9.6 Transactional outbox | `internal/outbox`: written in the same DB transaction as the state change; published at least once |
| §8 Provider adapter contracts | `internal/providers`: payment, vending and remote credit kept separate |
| FR-002/003 Meter registration and capability discovery | `internal/meters`: supported / authorised / reachable / confirmed |
| FR-004/005/006 Purchase, buy for someone else, beneficiaries | API: `/v1/beneficiaries`, `is_gift` on transactions |
| FR-009/010 Remote delivery with token fallback | Remote path plus operator `FallbackToToken` |
| FR-013 Refunds (definitive vend failure only) | Automatic refund with ledger postings |
| §5 Schema | `migrations/0001_init.sql`, `0002_persistence.sql`; used by `store/pgstore` |
| §13.2 Critical test scenarios | `internal/orchestrator/orchestrator_test.go` |

## Done: M1 persistence, M2 processes and recovery

- PostgreSQL store (pgx v5) behind `store.Store`, with an `InTx` unit of work: state, history, ledger and outbox commit together. Row locks plus version checks. Worker claims use `SKIP LOCKED`.
- Atomic idempotent creation: the key reservation, the quote consumption, the transaction and the completion commit as one.
- Migration runner plus `cmd/migrate`; `0002_persistence.sql`; `docker-compose.yml`.
- `cmd/api` / `cmd/worker` split; validated config; outbox publisher.
- `Resume`: crash recovery from any committed state. A payment sweeper covers missed webhooks, with expiry and late-payment review. Exponential backoff, and escalation to MANUAL_REVIEW.
- The whole scenario suite runs against memory **and** PostgreSQL. Fault-injection tests prove crash safety. An HTTP end-to-end test checks the logs for leaked secrets.

## Next engineering steps

1. ~~PostgreSQL repositories~~ (M1 done).
2. ~~Real payment adapter~~ (M3 done: Paystack with HMAC webhooks, server-side verify, idempotent refunds; tested against a stateful fake, **not yet against the live Paystack sandbox**).
3. **First real vending adapter**, through an aggregator API or a DisCo's third-party vending interface.
4. ~~OIDC authentication and admin API~~ (M4 done: OIDC/JWKS with MFA-gated staff roles; `/v1/admin/*` with search, detail, reconcile, fallback, audited token reveal, requeue, operator refund and provider switches; audit log; rate limits).
5. **Notifications** (SMS/WhatsApp/email) consuming outbox events: token delivery to gift recipients. Safe templates and a test-only mock sender exist; durable delivery, contacts and outbox wiring remain outstanding.
6. **Token sharing records** (`token_shares`) and households (FR-007/008). Purchaser-only JSON and printable HTML transaction records are implemented; receipt delivery remains outstanding.
7. OpenAPI 3.1 document (`api/openapi.yaml`) generated from or checked against the handlers.
8. Business dashboard, admin console and developer platform (Phases 5–7).

## Not engineering problems (blocking for production)

- An authorised vending pathway (aggregator or DisCo agreement).
- A **remote-credit pathway**: agreements with DisCos, Meter Asset Providers or meter manufacturers whose AMI/HES can push credit, plus an authorised end-to-end test on a real meter.
- Legal review: NERC and state electricity regulators, the CBN-licensed payment partner's money flow, and the NDPA 2023 data protection obligations.
- Do not market "no tokens" for a meter cohort until remote delivery has been demonstrated and is reliably confirmed for it (spec §19).
