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
5. ~~Notifications (SMS/WhatsApp/email) consuming outbox events~~ (M5 done: encrypted verified OIDC contact capture on `users`, `notification_preferences`, HMAC-keyed global `opt_outs` via signature-verified `POST /v1/notifications/inbound/{provider}`, `notification_deliveries` with `UNKNOWN` semantics and zero auto-resend of token-bearing gift SMS, Africa's Talking + Resend + WhatsApp Cloud API adapters with stateful fakes, 12-month HMAC retention purge, and audited operator views; tested against stateful fakes, **not yet against live provider sandboxes**, and `FEATURE_GIFT_SMS` defaults to `false` until legal sign-off).
6. **Token sharing and households** (FR-007/008). Purchaser-only COPY metadata and JSON/HTML transaction records are implemented. Self-service saves now grant purchase-only access, and users can remove their own saved access with an audit trail. External channel sending, receipt delivery, verified ownership and household delegation remain outstanding; existing legacy MANAGE grants are unverified.
7. ~~OpenAPI 3.1 document (`api/openapi.yaml`) generated from or checked against the handlers~~ (M6 done: `api/openapi.yaml` checked bidirectionally against `httpapi.Server.RegisteredPatterns()` and `writeErr` AST codes, plus internal-port Prometheus `/metrics`, OTLP/HTTP JSON tracing with correlation IDs, graceful shutdown draining HTTP + in-flight worker claims, and non-root distroless Docker targets with CI compose smoke tests).
8. ~~Pay-for-Me shareable payment request links (F1)~~ (M7 done: `POST/GET/DELETE /v1/payment-requests` + public `GET /v1/public/pay/{code}`, `POST /v1/public/pay/{code}/checkout`, `GET /v1/public/pay/status/{status_code}` with 128-bit domain-separated HMAC codes, pure-stdlib QR PNG generation, row-locked slot concurrency + abandoned-checkout slot reopening, anti-phishing notes, token-free guest receipts + M5 payer receipt email, and `FEATURE_PAY_FOR_ME` gate).
9. ~~Split the Meter reusable Funding Pot (F2)~~ (M8 done: `internal/pots` engine supporting `SPLIT_METER` and future `LANDLORD_BULK`, `POT_FUNDS_HELD` double-entry ledger liability account, `FundingSource` (`PSP` | `POT`) branching across all orchestrator and operator refund paths, `POST /v1/pots`, `GET /v1/pots/{id}`, `POST /v1/pots/{id}/cancel`, `GET /v1/public/pots/{code}` with initials only, `POST /v1/public/pots/{code}/contribute`, overfund partial refunds, crash-safe contributor refund sweeps, M5 consented contributor settlement SMS, and `FEATURE_SPLIT_METER` / `POT_MAX_HOLD` legal gate).
10. ~~AutoPower scheduled & low-balance auto-recharge with spend guard (F3)~~ (M9 done: consent-gated AEAD-bound `saved_payment_methods`, Paystack `ChargeSaved` / `POST /transaction/charge_authorization` verified via `VerifyPayment`, `Africa/Lagos` weekly/monthly scheduler with 12h heads-up + one-tap skip + 6h catch-up/missed semantics, row-locked monthly rule cap + per-user daily spend cap, 3-strike failure auto-pause, capability-gated `MeterReadingProvider` low-balance poller with 24h cooldown, and `FEATURE_AUTOPOWER` defaulting to `false`).
11. ~~Landlord Power Desk / Compound Bulk Top-Up (F4)~~ (M10 done: `organisations` + `organisation_members` RBAC (`OWNER`, `MANAGER`, `VIEWER`) enforced via `authz.Require` with zero cross-org IDOR (`404`), `properties` → `buildings` → `units` → `unit_meters` with audited operator verification (`ACCOUNT_NAME_MATCH` / `EVIDENCE_APPROVED`) gating history & tenant delegation, `LANDLORD_BULK` Funding Pots with 1 payment + N POT-funded unit allocations, honest partial-success reporting & partial refunds without refunding `UNKNOWN` vends, optional `MANAGER` approval thresholds, ledger-derived `Africa/Lagos` monthly statements + CSV export labelling `"electricity purchased"`, and `FEATURE_POWER_DESK` defaulting to `false`).
12. ~~Meterly Offline USSD + WhatsApp purchase channels (F5)~~ (M11 done: channel-agnostic `internal/conversation` state machine with DB-persisted `conversation_sessions`, Africa's Talking USSD adapter enforcing IP allowlist + HMAC + `<= 182` char `CON`/`END` + ~180s session TTL with zero orphan transactions, Meta WhatsApp Cloud API adapter with `hub.verify_token` + `X-Hub-Signature-256` + 24h customer-service window free-form text vs approved template fallback + `STOP` opt-out, HMAC phone identity with OIDC auto-link + SMS OTP link + SIM-swap inactivity reverification + daily spend caps, deterministic `ussd:<sessionId>:<step>` / `whatsapp:<sessionId>:<step>` orchestrator idempotency, audited `<= 3/day` caller-scoped `"Last token"` resends, and `FEATURE_USSD` / `FEATURE_WHATSAPP` defaulting to `false`).
13. First real vending / AMI adapters and live provider sandbox verification.
14. ~~Support chat (Meterly Agent + E2E human operators)~~ (`FEATURE_SUPPORT`, `/support`, `/admin/support`, device-local P-256 ECDH). Hosted API on Render against Supabase still waits on a Render dashboard deploy (`docs/DEPLOY.md`).

## Not engineering problems (blocking for production)

- An authorised vending pathway (aggregator or DisCo agreement).
- A **remote-credit pathway**: agreements with DisCos, Meter Asset Providers or meter manufacturers whose AMI/HES can push credit, plus an authorised end-to-end test on a real meter.
- Legal review: NERC and state electricity regulators, the CBN-licensed payment partner's money flow, and the NDPA 2023 data protection obligations.
- Do not market "no tokens" for a meter cohort until remote delivery has been demonstrated and is reliably confirmed for it (spec §19).
