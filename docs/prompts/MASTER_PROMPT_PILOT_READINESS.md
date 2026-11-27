# Meterly master build prompt: pilot readiness and the partner platform

> **Who this is for:** a new engineer (human or AI coding agent) continuing the Meterly backend. Read it all before writing code. It explains where the product is, the rules you must not break, and exactly what to build next (milestones P0–P6), with acceptance criteria.
>
> **Baseline:** `main` at `9cac48b` (PR #23, 2026-11-26), plus branch `claude/loving-wozniak-yr6igw` (review fixes, commit `b573cff`). If more has merged since, read `docs/HANDOFF.md` first. The handoff is the live state; this prompt is the plan.
>
> **Previous plan:** `docs/prompts/MASTER_PROMPT_M5_TO_FEATURES.md` (M5–M11) is **done**. Don't rebuild any of it.

---

## Part 1: Where Meterly is

### 1.1 The product in one paragraph

Meterly lets people in Nigeria buy prepaid electricity for any meter, from anywhere. They pay through a CBN-licensed PSP (Paystack), a vending aggregator (VTpass) mints the STS token, and the credit is delivered either **remotely** onto compatible smart meters (through a meter-system/HES partner, only for operator-attested cohorts) or as a **token**. Payment, vending and delivery are tracked separately and honestly. On top of that core sit Pay-for-Me links, Split the Meter (funding pots), AutoPower (saved-card auto-recharge), Landlord Power Desk, Meterly Offline (USSD and WhatsApp), households, token sending, support chat, passkey login with step-up security, and a Next.js web app.

### 1.2 What exists (don't rebuild it)

| Area | Where |
|---|---|
| Purchase engine, `Resume` recovery, ledger, outbox, idempotency | `internal/orchestrator`, `internal/store`, `internal/ledger` |
| Paystack (checkout, verify, refund, charge saved card, HMAC webhooks) | `internal/providers/paystack` (+ `paystacktest` fake) |
| VTpass vending | `internal/providers/vtpass` (+ fake) |
| Remote credit gateway contract + attested cohorts | `internal/providers/heshttp`, `internal/meters/cohorts.go` |
| Notifications (Africa's Talking, Resend, WhatsApp), consent, STOP | `internal/notifications*`, `internal/notificationservice` |
| Pay-for-Me, pots, AutoPower, Power Desk, offline channels, households, support | their own packages, plus orchestrator files |
| OIDC + passkeys (WebAuthn) + security engine (step-up, session network binding) | `internal/auth`, `internal/identity`, `internal/security` |
| Admin/ops API, audit log, Redis-shared rate limits, Prometheus, OTLP tracing, OpenAPI parity test, Docker, Render blueprint | `internal/httpapi/admin*`, `internal/observability`, `api/openapi.yaml`, `Dockerfile`, `render.yaml` |
| Web app | `web/` (Next.js 15, on Vercel) |
| Migrations | `0001` … `0021` (immutable once applied) |

The test suite (`go test -race ./...` with `DATABASE_URL`) has about 460 tests, about 80 of which run on PostgreSQL, and all pass.

### 1.3 What's true but uncomfortable

- **Nothing has run against a live provider.** Paystack, VTpass, Africa's Talking, Resend, WhatsApp and the identity provider are tested only against in-repo fakes. A recent review found a bug the Paystack fake hid (an invalid reference format). **Fakes can be wrong about reality.**
- **The hosted API isn't live.** Render and Supabase are configured on paper; migrations `0018`–`0021` haven't been applied to the hosted database.
- **The ledger stops at "receivable".** It records `PSP_SETTLEMENT_RECEIVABLE` and `PROVIDER_PAYABLE` but never their settlement: nobody checks that Paystack actually paid us, or that VTpass actually debited the wallet.
- **There's no fraud control** beyond rate limits, and **no chargeback handling**.
- **No data-subject rights** (NDPA 2023) and **no tested backup or restore**.

**Your job:** make Meterly safe to run a **controlled pilot with real money**, then build the **partner platform** that is the long-term business.

---

## Part 2: Rules you must follow

### 2.1 Invariants (every change must keep them, and tests must prove it)

1. No vending until payment is verified server-side and the amount equals the quote total exactly. A mismatch goes to MANUAL_REVIEW.
2. A timeout or ambiguous error is UNKNOWN, never FAILED. Resolve it by status query; re-send only if the provider confirms it has no record.
3. Remote ACCEPTED ≠ delivered. Only meter confirmation completes a remote delivery.
4. Refund only after a definitive vend failure, or an operator refund where nothing was or may have been vended.
5. Every money movement is a balanced, idempotent ledger posting (PostgreSQL triggers enforce balance and append-only).
6. No raw tokens, meter numbers, phone numbers or emails in logs, URLs, analytics, outbox payloads, audit metadata or plaintext columns.
7. Duplicates and concurrency never double-charge, double-vend, double-credit, double-refund or double-send.
8. The VTpass `request_id` is stored when the vend is claimed and reused. Never regenerate it.
9. Household roles never mint `meters.MANAGE`; grants are not ownership.
10. Self-registration grants at most `PURCHASE`.
11. **(new)** A prepaid vend is never confirmed without an STS token.
12. **(new)** Saved cards belong only to the person who paid with them.

### 2.2 Patterns to reuse

- **`orchestrator.mutate`:** lock the row, apply a guarded change, then save state, history, ledger, outbox and audit in **one DB transaction**. Guards return `errSkip`.
- **Claim before an external call:** an explicit `claimed` flag set inside the mutate function. Never infer the claim from the resulting state.
- **Provider calls never run inside a DB transaction.**
- **Every operator action** takes a reason and writes an audit row in the same transaction.
- **New money flows:** write the ledger postings table in `docs/DECISIONS.md` **before** coding, and add a test that the trial balance is 0 and every holding account nets to 0.
- **New external APIs:**
  - build only from official docs, and cite the URL in the package comment;
  - ship a stateful fake that **enforces the provider's real validation rules** (formats, lengths, allowed characters) and can inject "applied, but the response was lost", 5xx and timeout faults;
  - **do not invent partner APIs.** If the docs aren't available, build an interface plus a TODO and ask.

### 2.3 Workflow

- **Start of session:** read `docs/HANDOFF.md` and `CLAUDE.md`, then check against `git log` and the open PRs.
- **End of session:** update HANDOFF (state, gotchas, one session-log entry including what's unverified) and DECISIONS, then commit and push.
- **Tests:** `go test -race ./...` **with `DATABASE_URL` set** (CI fails if the PostgreSQL tests were skipped). New `pgtest` packages need `TestMain` calling `pgtest.DropTemplate()`.
- **Regression tests:** every bug fix gets a test, **and you must show that test fails without the fix**.
- **Git:**
  - new migrations only (`0022_…`);
  - never force-push; merge `origin/main` instead of rebasing;
  - one milestone per PR;
  - no model identifiers in commits, PRs or code (`CLAUDE.md`).
- **Feature flags:** every new customer-facing capability gets `FEATURE_<NAME>`, default **off** outside `MODE=dev`, validated in config.
- **Never touch the hosted Supabase database or Render service from a dev session.**

---

## Part 3: The plan

| # | Milestone | Why now |
|---|---|---|
| P0 | Land the review fixes, sync branches | Clean base |
| P1 | **Live sandbox certification harness** | The biggest unverified area; proves the adapters against reality |
| P2 | **Settlement and daily reconciliation** | Spec P0: you can't take real money without checking you got it |
| P3 | **Risk controls and chargebacks** | Public pay links and pots invite card testing and disputes |
| P4 | **Pilot operations:** VTpass float, alerts, runbooks, backups, pilot gating | Run a 50–100 household pilot safely (spec Phase 6) |
| P5 | **NDPA data rights and retention** | Legal prerequisite for real users |
| P6 | **Partner API platform** (spec Product C) | The long-term business: fintechs and estates integrate |

---

### P0: Land the review fixes

1. Open a PR from `claude/loving-wozniak-yr6igw` to `main` containing the 2026-11-27 review fixes (see DECISIONS "Review fixes"). Its merge base is `9cac48b`.
2. After it merges, every working branch must `git merge origin/main`.
3. Confirm CI is green on `main` and record it in HANDOFF.

**Done when:** `main` contains `b573cff`, and CI is green.

---

### P1: Live sandbox certification harness

**Goal:** one command that proves each adapter works against the **real sandbox** of its provider. It runs only when that provider's keys are present.

**Design:**
- New package `internal/certify` and command `cmd/certify`.
- Each provider has a **certification scenario**, written as Go tests behind the build tag `certify` (`go test -tags certify ./internal/certify/...`). Each scenario is skipped unless its env vars are set, and **never runs in normal CI**.
- Reports are written to `certify-report.json` (provider, scenario, pass/fail, latency, the provider's request/reference IDs) with **no secrets, tokens or meter numbers**.

**Scenarios (use the providers' documented sandbox test data only):**

| Provider | Scenarios |
|---|---|
| Paystack (`sk_test_`) | initialize → pay with Paystack's documented test card → verify success; failed-card path; refund and refund listing; webhook signature verification using a captured real payload; `charge_authorization` with a test authorization; reference-format edge cases |
| VTpass sandbox | merchant-verify on each documented sandbox meter number; pay prepaid (token present); pay postpaid; requery delivered, pending and not-found; duplicate `request_id` (014); invalid `request_id` date (085); low-balance handling if the sandbox supports it |
| Africa's Talking sandbox | send SMS; delivery report callback signature; STOP inbound |
| Resend | send to the documented test address; webhook signature |
| WhatsApp Cloud API (test number) | template send; verify-token handshake; `X-Hub-Signature-256` |
| OIDC (the founder's chosen IdP) | discovery; JWKS; a staff token with MFA `amr`; roles claim; key rotation |

**Then fix the fakes.** Every difference found between a fake and the real sandbox becomes:
- a change to the fake, so it matches reality;
- a regression test;
- an entry in DECISIONS under "Sandbox findings".

**Done when:**
- `cmd/certify` runs green against every provider whose keys the founder has supplied;
- the report is attached to the PR;
- HANDOFF's "Mocks vs real adapters" table says "certified on <date>" per provider.

**Blocked on:** the founder supplying sandbox keys, entered through the environment's secret settings and **never committed**. Build the harness anyway, and mark providers without keys as "not certified".

---

### P2: Settlement and daily reconciliation

**Goal:** every day, prove that what our ledger says happened matches what Paystack and VTpass say happened. Book the money actually settling, and put every mismatch in an exception queue with an owner.

**Ledger (agree it with finance; write the table in DECISIONS first):**

| Event | Debit | Credit |
|---|---|---|
| Paystack settles a batch to our bank (net of fees) | `CASH_AT_BANK` (net) + `PSP_FEES_EXPENSE` (fees) | `PSP_SETTLEMENT_RECEIVABLE` (gross) |
| VTpass wallet top-up from the bank | `VENDOR_WALLET_PREFUND` | `CASH_AT_BANK` |
| A vend recognised against the wallet | `PROVIDER_PAYABLE` | `VENDOR_WALLET_PREFUND` |
| VTpass commission or discount (if the contract has one) | `VENDOR_WALLET_PREFUND` | `VENDOR_COMMISSION_INCOME` |
| A refund settled by Paystack | (already posted) | reconcile only |

Add the new accounts in migration `0022`. **The fee and commission treatment is a founder/finance decision**: default to the table above, and flag it.

**Design:**
- `internal/reconcile`, with a daily job in the worker that is idempotent per `(provider, business_date)` and runs in Africa/Lagos time.
- **Paystack:** pull the documented settlements and transaction-list APIs for the date range, and match by `reference` against `payments`. Categories:
  - matched;
  - **in PSP, not in ours** (a payment we never recorded; possibly a lost webhook plus a missed sweep);
  - **in ours, not in PSP**;
  - amount or currency mismatch;
  - status mismatch (we say confirmed, they say reversed);
  - refund mismatch.
- **VTpass:** use the documented transaction requery (and a statement or report endpoint **only if documented**) to match our `vending_records` by `request_id`. Same categories, plus "token missing".
- **Exceptions:** a `reconciliation_exceptions` table with category, provider references, our transaction ID, amounts, status (OPEN → INVESTIGATING → RESOLVED / WRITTEN_OFF), owner and notes. Resolving one needs a reason and an audit row. Resolution actions **never post to the ledger implicitly**: any correction is an explicit, reviewed adjusting entry with a two-person rule (one operator proposes, an admin approves).
- **Admin API:** list and filter exceptions; detail; assign; resolve; a daily summary (matched count and value, exceptions by category, PSP receivable ageing).
- **Metrics:** `reconciliation_exceptions_open{provider,category}`, plus an alert when there are any new HIGH exceptions.

**Tests (both stores):**
- each category produced from fixture provider responses;
- re-running the same day creates no duplicates;
- a settlement posting balances, and the receivable nets correctly;
- the two-person adjusting entry;
- a crash mid-run leaves no partial state;
- fakes gain settlement and list endpoints matching the official docs.

**Done when:** a dev run over fixture data produces the expected exceptions, the ledger balances, and an operator can work an exception end to end.

---

### P3: Risk controls and chargebacks

**Goal:** stop the obvious abuse before it costs money, and handle card disputes properly.

**Risk engine (`internal/risk`):**
- **Evaluated at:** transaction create, Pay-for-Me public checkout, pot contribution, saving a card, AutoPower rule creation, and token send to a third party.
- **Rules (configurable, with sensible defaults, stored in DB as `risk_rules` so operators can tune them, every change audited):**
  - velocity per user, card fingerprint (Paystack `authorization.signature`), email HMAC, IP, meter and device;
  - many distinct cards on one account or link in a short window (card testing);
  - first-purchase caps for new accounts;
  - amount anomaly against the user's history;
  - public links with many failed payments (auto-pause the link);
  - deny-lists (card signature, email HMAC, phone HMAC, IP range) managed by admins.
- **Decisions:** ALLOW, REVIEW (hold before vending: payment is captured, the transaction goes to MANUAL_REVIEW with reason `risk:<rule>`), or DENY (refuse before checkout).
  - **Never deny after payment.** Hold for review instead, so no money is stranded.
  - Every decision is recorded (`risk_decisions`, metadata only, HMACs, no PII) and exposed in admin transaction detail.

**Chargebacks:**
- Ingest Paystack's documented dispute webhooks and dispute APIs (create, remind, resolve) into `disputes`, linked to the transaction.
- Ledger: a dispute opening moves funds to a `DISPUTE_RESERVE` liability; a loss is written off; a win releases the reserve. Write the table first.
- Ops: evidence pack generation (receipt, vend and delivery evidence, timestamps; **no token**), deadlines, reminders, outcome.
- Repeated disputes on one card or user feed the deny-list.

**Tests:**
- every rule's threshold edge cases;
- concurrent checkouts can't bypass a velocity limit (count under a lock or with `SKIP LOCKED` semantics);
- REVIEW never vends;
- DENY never charges;
- dispute webhook signature checks, replay and idempotency;
- the dispute ledger balances.

**Done when:** a scripted card-testing attack on a public link is stopped and paused, and a disputed transaction flows from webhook to outcome with balanced books. Ships behind `FEATURE_RISK_ENGINE` (on in staging and pilot).

---

### P4: Pilot operations

**Goal:** run a controlled pilot (spec Phase 6: one DisCo, one estate or community, 50–100 households) without surprises.

1. **VTpass float monitor.** Use the wallet-balance API **if documented**, otherwise the sum of vends against recorded top-ups. Alert at configurable thresholds (for example 3 days of average spend), and **pause new vends** with `ErrProviderDisabled` semantics (already paused-not-failed) before the wallet runs dry. A dry wallet would otherwise turn every purchase into an automatic refund.
2. **Pilot gating.**
   - `PILOT_MODE=true` restricts purchases to an allow-list of users and meters, plus global and per-user daily value caps and a per-DisCo enable list.
   - Admin endpoints manage the allow-list, with every change audited.
   - A clear "service paused" response and UI state.
3. **Alerting.** Prometheus alert rules in `deployments/monitoring/alerts.yml`, with a unit test that parses them:
   - provider error rate;
   - UNKNOWN or MANUAL_REVIEW counts rising;
   - outbox lag;
   - notification failures;
   - reconciliation exceptions;
   - VTpass float;
   - 5xx rate;
   - p95 latency;
   - worker not running.

   Each alert links to a runbook.
4. **Runbooks** in `docs/runbooks/`, one per alert, plus: provider outage, mass refunds, a stuck UNKNOWN cohort, key rotation (`TOKEN_ENC_KEY`/`METER_HMAC_KEY` with `key_version`), a suspected breach, and rolling back a bad deploy.
5. **Backups and restore drill.**
   - `scripts/restore-drill.sh` restores the latest backup into a scratch database, runs `cmd/migrate` and a read-only integrity check (`cmd/integrity`: trial balance = 0, holding accounts net to 0, no orphan records, outbox not stuck).
   - Document Supabase PITR settings, RPO/RTO targets and the drill cadence in `docs/DEPLOY.md`.
6. **Synthetic canary.** In staging, a scheduled end-to-end sandbox purchase (P1 harness) every hour, alerting on failure.
7. **Key rotation tooling.** `cmd/rekey` re-encrypts tokens, contacts and auth codes to a new `key_version` in batches, resumably and idempotently, with a dry-run mode.

**Done when:** a dress rehearsal in staging works end to end:
- the canary passes;
- pausing via the float monitor works;
- a restore drill passes the integrity check;
- every alert fires in a forced test.

**Blocked on:** the hosted environment being live (HANDOFF §10).

---

### P5: NDPA data rights and retention

**Goal:** meet NDPA 2023 obligations for access, erasure and retention, without breaking financial record-keeping.

- **Export.** `GET /v1/me/data-export` (step-up gated). It runs as an async job producing a JSON archive of the user's profile, meters (masked), transactions, receipts (no tokens), consents, notification preferences, households, support threads (decrypted for the requester) and security events. The download link expires and is single-use. Audited.
- **Erasure.** `POST /v1/me/delete-account` (step-up, with a 7-day cooling-off and cancel).
  - **Anonymise**, don't delete, anything financial (transactions, ledger, payments, refunds, disputes): these must be kept for the statutory period. The retention period is a founder/legal decision; default 7 years and make it configurable.
  - Delete contacts, passkeys, sessions, saved cards (also revoke at Paystack if documented), AutoPower rules, household memberships, support content and notification destinations.
  - Must be idempotent and resumable, with an audit row and no PII left in the audit metadata.
- **Retention jobs.** A per-table policy registry in code, with documented durations: notification HMACs, OTPs, sessions, challenges, idempotency keys, risk decisions, support threads. Purge in batches, with metrics.
- **Consent ledger.** Make the existing consent records queryable per user (marketing vs transactional), including withdrawal.
- **Docs.** A `docs/PRIVACY_ENGINEERING.md` data map (each table's PII, purpose, legal basis, retention), to support counsel's DPIA.

**Tests:**
- the export contains everything listed and **no** tokens, raw meter numbers or other users' data;
- erasure leaves the ledger balanced and keeps transactions while removing PII;
- the cooling-off cancel works;
- a re-run of erasure is a no-op;
- the retention purge deletes exactly the expired rows.

**Done when:** the data map is complete and both flows pass on both stores. Legal sign-off on the retention periods is a founder action.

---

### P6: Partner API platform (spec Product C)

**Goal:** let approved partners (fintechs, estates, aggregators) validate meters, quote, purchase and track electricity through Meterly via a secure, documented API with webhooks. This is the long-term asset.

**Design:**
- **Partners and keys.**
  - Tables: `partners` (organisation, status, environment SANDBOX/PRODUCTION, contact), `partner_api_keys` (prefix + Argon2id/HMAC hash only, scopes, created_by, last_used, expires, revoked) and `partner_usage` (daily counters).
  - Keys have the form `mtr_test_…` / `mtr_live_…`; the full key is shown once.
  - Rotate with overlap; revoke immediately.
- **Auth.** `Authorization: Bearer mtr_…` → partner principal with scopes such as `meters:validate`, `quotes:create`, `purchases:create`, `purchases:read` and `webhooks:manage`. Per-partner rate limits in Redis (reuse the existing limiter), plus a per-partner daily value cap.
- **Funding model (founder decision; default prefunded).** The partner has a prefunded float with Meterly, recorded as a `PARTNER_FLOAT` liability. A purchase debits the float instead of using a PSP checkout: a new `FundingSource=PARTNER_FLOAT`, branching like `FundingPot` throughout refunds. Top-ups are recorded by operators against bank receipts, with the two-person rule. **Holding partner float may need regulatory review** (see CBN/PSP note): flag it, and keep the feature behind `FEATURE_PARTNER_API`.
- **API (`/v1/partner/...`, versioned, OpenAPI documented):**
  - `POST meters/validate`, `POST quotes`, `POST purchases` (Idempotency-Key required, partner reference unique), `GET purchases/{id}`, `GET purchases?partner_ref=`, `GET float`;
  - the token is returned only to scopes that allow it, and only over this authenticated channel;
  - remote-delivery semantics identical to consumers (ACCEPTED ≠ CONFIRMED).
- **Outbound webhooks.** Use the existing (unused) `webhook_endpoints` and `webhook_deliveries` tables:
  - signed with HMAC-SHA256 plus a timestamp (replay window), an event ID and at-least-once delivery;
  - exponential retry for 24h, then dead-lettered with manual redelivery;
  - per-endpoint secret rotation and a test ping;
  - events: `purchase.payment_confirmed`, `purchase.vended`, `purchase.delivered`, `purchase.failed`, `purchase.refunded`, `purchase.unknown`;
  - the payload never contains tokens unless the endpoint is configured for token delivery with that scope.
  - **SSRF protection:** https only, a resolved-IP deny-list (private and link-local ranges), no redirects, and a short timeout.
- **Sandbox.** Partner sandbox keys run against the mock vending and mock payments path, with deterministic test meter numbers (reuse the mock behaviour table), so partners can integrate before going live.
- **Developer portal (web).** Keys, webhooks, delivery logs, usage, docs rendered from `api/openapi.yaml` (partner section), and a changelog.

**Tests:**
- key hashing (the plaintext never stored or logged);
- scope enforcement matrix;
- partner isolation (IDOR matrix: partner A can't see B's purchases);
- idempotent create under concurrency;
- float debit, refund and ledger balance;
- webhook signing, retry, dead-lettering and SSRF blocks (including DNS rebinding: resolve once, then pin the IP);
- rate and value caps;
- an OpenAPI parity test covering `/v1/partner`.

**Done when:** a sample partner (a script in `scripts/partner-demo/`) integrates end to end against the sandbox, receives signed webhooks and reconciles its purchases.

---

## Part 4: Founder decisions (ask; don't assume)

| Decision | Needed for | Default until decided |
|---|---|---|
| Sandbox keys: Paystack, VTpass, Africa's Talking, Resend, WhatsApp test number | P1 | providers marked "not certified" |
| Identity provider choice + staff MFA | P1, pilot | OIDC tested with an in-process JWKS only |
| Ledger treatment of PSP fees, VTpass commission, VAT on the convenience fee | P2 | the table in P2, flagged |
| Risk thresholds and deny-list policy | P3 | conservative defaults, tunable |
| Pilot scope: DisCo, estate, household count, caps | P4 | `PILOT_MODE` with an empty allow-list (nothing purchasable) |
| Hosting go-live (Render + Supabase, migrations through `0021`) | P4 | staging not live |
| Retention periods (financial and operational) | P5 | 7 years financial; documented operational defaults |
| Partner funding model and whether holding partner float is allowed | P6 | `FEATURE_PARTNER_API` off |
| Remote-credit (HES) partner + API docs + witnessed real-meter test | "no tokens" marketing | `REMOTE_PROVIDER=none`; keep STS |

## Part 5: Definition of done (whole plan)

- P0–P6 merged, each behind its flag where marked, with CI green.
- P1 certification reports exist for every provider whose keys were supplied; every fake fixed to match reality.
- Daily reconciliation runs, with zero unexplained exceptions over a 7-day staging run.
- The risk engine and chargeback flow are tested, with ledger balance proven.
- Pilot dress rehearsal passed: canary, float pause, restore drill, forced alerts.
- NDPA export and erasure work, and the data map is complete.
- The partner demo integrates end to end on the sandbox.
- The invariants still hold; every new money flow has a ledger table in DECISIONS and a balance test; no PII or secrets in logs, URLs or audit metadata.
- HANDOFF and DECISIONS are current. Every founder-blocked item is listed with its default.
