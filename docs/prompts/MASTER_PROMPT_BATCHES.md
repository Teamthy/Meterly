# Meterly master prompt: remaining build, in batches (B0 → B9)

> **Who this is for:** a new engineer (human or AI coding agent) continuing the Meterly backend from the current `main` to the end of the engineering roadmap. Read all of it before writing code.
>
> **Baseline:** `main` at `1208ca6` (PR #25, which landed PR #24). Done and merged: M1–M11, the security engine, and P0–P4 (review fixes, certify harness, settlement reconciliation, risk and chargebacks, pilot operations). **Don't rebuild any of it.**
>
> **Read first, every session:** `CLAUDE.md`, then `docs/HANDOFF.md` (live state; verify against `git log` and open PRs), then `docs/prompts/MASTER_PROMPT_PILOT_READINESS.md` Parts 1 and 2 (product context, the 12 invariants, patterns). This file is the *plan*; the handoff is the *state*. If they disagree, the handoff and the code win.

---

## 1. How to work in batches

- **One batch = one PR = one session** (a batch may take several sessions; never mix batches in a PR).
- **Order matters.** A batch's "Needs" lists what must be merged first. Don't start a batch whose prerequisite is unmerged.
- **Every batch ends the same way:**
  1. `gofmt -l .` empty, `go vet ./...` clean.
  2. `go test -race ./...` **with `DATABASE_URL` set** (memory and PostgreSQL; PostgreSQL tests skip silently without it, and CI fails if they skip). New `pgtest` packages need a `TestMain` calling `pgtest.DropTemplate()`. Confirm no leftover `meterly_*` databases.
  3. Re-run the concurrency-heavy suites you touched at least twice.
  4. Each bug fix has a regression test, and you show that the test **fails without the fix**.
  5. Update `docs/HANDOFF.md` (state, gotchas, one session-log entry at the top: what was done, how verified, what is unverified, what is blocked) and `docs/DECISIONS.md`. Commit and push.
  6. Open the PR using the repo's PR template headings. Wait for CI green. Fix root causes, never skip or disable a test.
- **Git:** new migrations only (`0027_…` onward — B1 took `0025_privacy.sql`, B2 took `0026_data_export.sql`; never edit an applied one); never force-push, merge `origin/main` instead of rebasing; no model identifiers in commits, PRs or code.
- **Flags:** every customer-facing capability gets `FEATURE_<NAME>`, default **off** outside `MODE=dev`, validated in config.
- **Never** touch the hosted Supabase database or Render service from a dev session. Never put secrets, real tokens or real meter numbers in the repo, tests or logs.
- **Don't invent partner APIs** (vending, AMI/HES). Without official docs, write an interface plus a TODO and ask.
- **Money flows:** write the ledger postings table in `docs/DECISIONS.md` *before* coding, and test that the trial balance is 0 and every holding account nets to 0.

---

## 2. The batches at a glance

| Batch | What | Type | Needs |
|---|---|---|---|
| **B0** | Repo hygiene: handoff sync, migration chain check | Eng | none |
| **B1** | NDPA part 1: data map, consent ledger, retention registry and jobs | Eng | B0 |
| **B2** | NDPA part 2: data export | Eng | B1 |
| **B3** | NDPA part 3: account erasure (anonymise, cooling-off) | Eng | B1 |
| **B4** | Partner platform part 1: partners, API keys, auth, scopes, limits | Eng | B0 |
| **B5** | Partner platform part 2: float funding and the purchase/quote/validate API | Eng | B4 |
| **B6** | Partner platform part 3: outbound webhooks (SSRF-safe), sandbox, demo | Eng | B5 |
| **B7** | Developer portal (web) and partner docs | Eng | B6 |
| **B8** | Hardening and go-live readiness (load, chaos, security review, runbooks) | Eng | B1–B7 |
| **B9** | Founder-gated: hosting, migrations, sandbox certification, pilot | Human + Eng | B8 |
| **B10+** | Post-pilot, when documents arrive (HES adapter, more DisCos, apps) | Eng | partner docs |

---

## B0: Repo hygiene (small, do first)

**Goal:** make the repo's own documentation true again and prove the migration chain.

- `docs/HANDOFF.md`: rows 8–9 in "Right now" and several gotchas still say "on this branch". Rewrite them as merged. Keep the "hosted `0022`–`0024` unapplied" facts.
- Add a test (both stores where it applies) that applies **all** migrations in order on a fresh PostgreSQL database and then runs a smoke query per new table (`0022`–`0024`). It exists partially through `pgtest`; confirm and fill gaps.
- Confirm `api/openapi.yaml` parity test covers the P2–P4 admin routes; add any missing ones.
- Note the settlement fee and commission treatment as a *pending founder/finance decision* in HANDOFF section "Founder decisions".

**Done when:** handoff matches reality; the migration chain test is green in CI.

---

## B1: NDPA part 1, data map, consent ledger, retention

**Goal:** the foundations for data-subject rights. Nothing user-visible yet except consent queries.

1. **Data map** `docs/PRIVACY_ENGINEERING.md`: every table and column holding personal data: what it is, purpose, legal basis, retention, and whether it is encrypted or HMAC-only. Generate a first draft by scanning `migrations/` and then check by hand. A test fails if a new table with a column named like `email`, `phone`, `name`, `address`, `ip` or `msisdn` is added without an entry in a machine-readable registry (`internal/privacy/registry.go`).
2. **Retention registry** `internal/privacy`: per-table policy in code (duration, action DELETE or ANONYMISE, batch size, legal-hold exemption). Defaults, all configurable by env: financial records **7 years** (founder/legal to confirm), OTPs and challenges days, sessions 90 days after expiry, idempotency keys 30 days after completion, risk observations 90 days, notification HMACs per consent, support threads per policy.
3. **Retention job**: a batched, idempotent purge run by the existing scheduler pattern. Emits metrics (rows purged per table) and writes one audit row per run (counts only, no PII). Never purges a row under a legal hold or a financial record inside its period. Provide a dry-run mode.
4. **Consent ledger**: make existing consent records queryable per user (marketing vs transactional, per channel), including withdrawal history. Add `GET /v1/me/consents` and `POST /v1/me/consents/withdraw`. Withdrawal takes effect immediately for sending.

**Tests:** registry completeness test; the purge deletes **exactly** the expired rows and never financial rows in period; dry-run changes nothing; running twice is a no-op; withdrawal blocks the next send; all on both stores.

**Done when:** the data map is complete, the purge is proven, consent is queryable. Legal sign-off on the retention periods is a founder action (record it in HANDOFF as pending).

---

## B2: NDPA part 2, data export

**Goal:** `GET`-style subject access that is safe.

- `POST /v1/me/data-export` (step-up required, rate-limited, audited) creates an async job. A worker builds a JSON archive: profile, meters (masked), transactions, receipts (**no tokens**), consents, notification preferences, households, the user's own support threads (decrypted for the requester only), security events.
- Download link is **single-use and expires** (default 24h); the archive is encrypted at rest and deleted after download or expiry (use the retention registry).
- Requests are idempotent per user per day; a second request while one is pending returns the pending one.

**Tests (both stores):** the export contains everything listed; it contains **no** raw token, no raw meter number, nothing belonging to other users (build a two-user fixture and prove isolation, including shared household data and pay-for-me payers); step-up is required; the link is single-use and expires; audit row exists with no PII.

**Done when:** the isolation matrix passes and the export is reproducible.

---

## B3: NDPA part 3, account erasure

**Goal:** erase personal data **without** breaking the books.

- `POST /v1/me/delete-account` (step-up, reason optional) starts a **7-day cooling-off** (configurable); `POST /v1/me/delete-account/cancel` cancels. Sign-in during cooling-off is allowed and shows the pending state.
- After cooling-off, an idempotent, **resumable** job runs:
  - **Anonymise, don't delete,** anything financial (transactions, ledger, payments, refunds, disputes): keep amounts and references, replace the user link with a tombstone user ID.
  - **Delete:** contacts, passkeys, sessions, saved cards (also revoke at Paystack **only if documented**; otherwise TODO and ask), AutoPower rules, household memberships, support content, notification destinations, consent contacts.
  - Block erasure (return a clear error, audited) while the user has an open dispute, an unresolved transaction (UNKNOWN or MANUAL_REVIEW), a pot with funds, or an unpaid obligation.
  - Audit row with **no PII** in metadata.
- Define what happens to a meter linked to other households and to pay-for-me history for others.

**Tests (both stores):** ledger trial balance stays 0 and totals are unchanged; transactions remain, identity does not; cancel works; re-running is a no-op; interrupted job resumes; erasure blocked in each blocking condition; after erasure the user's email and phone can register again as new accounts; the export (B2) of an erased user is refused.

**Done when:** erasure passes on both stores and the data map shows every PII column handled.

---

## B4: Partner platform part 1, partners, keys, auth

**Goal:** the identity and access layer for approved partners. No purchases yet.

- Migration `0027_partners.sql` (B1 used `0025_privacy.sql`, B2 used `0026_data_export.sql`): `partners` (organisation, status PENDING/ACTIVE/SUSPENDED, environment SANDBOX/PRODUCTION, contact reference, created_by), `partner_api_keys` (prefix, **hash only**, scopes, created_by, last_used_at, expires_at, revoked_at), `partner_usage` (daily counters).
- Key format `mtr_test_…` / `mtr_live_…`, 32+ random bytes, shown **once**. Store a prefix for lookup and an HMAC/Argon2id hash for verification; constant-time compare.
- `Authorization: Bearer mtr_…` → partner principal with scopes: `meters:validate`, `quotes:create`, `purchases:create`, `purchases:read`, `webhooks:manage`, `float:read`.
- Rotation with overlap window; immediate revoke; a suspended partner's keys stop working at once.
- Per-partner rate limit (reuse `internal/ratelimit`, Redis-shared) and a per-partner daily value cap field (enforced in B5).
- Admin endpoints (operator-only, step-up, two-person for production activation): create partner, issue/rotate/revoke key, suspend, view usage. All audited with a reason.
- `FEATURE_PARTNER_API` flag, default off outside dev.

**Tests:** plaintext key never stored, logged or returned after creation (grep-style test over logs and audit); scope enforcement matrix (every scope × every route); revoked/expired/suspended keys fail; sandbox key cannot use production routes and vice versa; rate limit per partner; OpenAPI parity for the new routes.

**Done when:** a partner key authenticates, is scoped and rate-limited, and every admin action is audited.

---

## B5: Partner platform part 2, float and the purchase API

**Needs B4.** **Founder decision first (default prefunded):** partner funding model, and whether holding partner float is permitted (regulatory review flag). Record in `docs/DECISIONS.md`; keep behind the flag.

- **Ledger postings table first** (DECISIONS): `PARTNER_FLOAT` liability account. Top-up (recorded by operators against a bank receipt, **two-person rule**, idempotent by receipt reference): debit bank/settlement, credit `PARTNER_FLOAT`. Purchase: debit `PARTNER_FLOAT`, credit revenue and `PROVIDER_PAYABLE` as the consumer path does. Refund after a definitive vend failure: reverse to `PARTNER_FLOAT`.
- New `FundingSource=PARTNER_FLOAT`, branched everywhere refunds already branch on `FundingPot`. Invariants 1–12 still apply: debit the float only after the quote is accepted; vend only after the float debit commits; timeout is UNKNOWN; refunds only after a definitive failure.
- API under `/v1/partner/…`, versioned and in `api/openapi.yaml`:
  - `POST meters/validate`, `POST quotes`, `POST purchases` (**Idempotency-Key required**, `partner_ref` unique per partner), `GET purchases/{id}`, `GET purchases?partner_ref=`, `GET float`.
  - The token is returned only to scopes that allow it, over this authenticated channel only. Remote delivery keeps ACCEPTED ≠ CONFIRMED.
- Enforce per-partner daily value cap and a low-float refusal (clear error, no vend).

**Tests (both stores):** float debit/refund and the trial balance; concurrent identical creates produce one purchase and one debit; partner isolation (IDOR matrix: A cannot read, list or replay B's purchases or partner_refs); insufficient float; cap enforcement; UNKNOWN stays UNKNOWN and resolves; no token in logs or outbox for scopes without it.

**Done when:** a sandbox partner can validate, quote, purchase and track, and the books balance.

---

## B6: Partner platform part 3, webhooks, sandbox, demo

**Needs B5.**

- **Outbound webhooks** (reuse the `webhook_endpoints` / `webhook_deliveries` tables): HMAC-SHA256 over timestamp + body, replay window, event ID, **at-least-once** with exponential retry for 24h, then dead-letter with manual redelivery; per-endpoint secret rotation; test ping. Events: `purchase.payment_confirmed`, `purchase.vended`, `purchase.delivered`, `purchase.failed`, `purchase.refunded`, `purchase.unknown`. Payloads never include tokens unless the endpoint has the scope.
- **SSRF protection:** https only; resolve the host once, **pin the IP**, deny private, loopback, link-local and metadata ranges (IPv4 and IPv6, including IPv4-mapped); no redirects; short timeout; response body size cap. Test DNS rebinding explicitly.
- **Sandbox:** sandbox keys run against the mock vending and mock payments path with documented deterministic test meters (reuse the mock behaviour table), including failure and UNKNOWN cases.
- `scripts/partner-demo/`: a script that creates a sandbox partner, tops up float, validates, quotes, purchases, receives and verifies signed webhooks, and reconciles.

**Tests:** signature and replay-window verification; retry schedule and dead-lettering; SSRF matrix; webhook secret rotation overlap; ordering and duplicate delivery safety; sandbox determinism.

**Done when:** the demo script runs end to end against a local server and reconciles its purchases.

---

## B7: Developer portal and partner docs (web)

**Needs B6.** In `web/` (Next.js 15, Vercel):

- Partner sign-in, key management (shown-once secret, rotate, revoke), webhook management with delivery logs and redelivery, usage and float views.
- Docs rendered from the partner section of `api/openapi.yaml`, quick-start, error catalogue, webhook verification snippets, sandbox meter table, changelog.
- Accessibility and mobile checks; no secrets in client bundles; CSP kept strict.

**Tests:** component and route tests as the web app already does; a build must pass; a manual screenshot pass at phone and desktop width.

**Done when:** a partner can self-serve a sandbox integration from the portal alone.

---

## B8: Hardening and go-live readiness

**Needs B1–B7.** No new features.

- **Load:** a repeatable load test for purchase, webhook intake and the partner API; record baselines and limits in `docs/`.
- **Chaos/failure drills** in tests: provider 5xx and lost-response faults, DB failover simulation, Redis down (rate limiting degrades safely), outbox backlog; prove Resume/ResumeDue recovery and zero double charge, vend or refund.
- **Security review** of everything since M11 (use the code-review and security-review skills): authz on every new route, IDOR matrix, secret handling, logging, SSRF, rate-limit bypass, step-up coverage. Fix findings with regression tests.
- **Dependency and vulnerability scan**, `govulncheck`, container scan; pin versions.
- **Runbooks:** partner incident, webhook backlog, erasure failure, retention job failure, float exhaustion. Add alerts to `deployments/monitoring/alerts.yml`.
- Re-verify the 12 invariants with a checklist test list in HANDOFF.

**Done when:** load baselines are recorded, drills are green, no open high-severity findings.

---

## B9: Founder-gated go-live (human actions; engineer assists)

Do **not** attempt these from a dev session. Prepare the exact commands and checklists, then the founder runs them (see `docs/DEPLOY.md`, `docs/LAUNCH.md`).

1. Render API + Supabase live; run `/migrate` to apply `0018`–`0025+`; set `METERLY_API_ORIGIN` on Vercel.
2. Provide sandbox keys (Paystack, VTpass, Africa's Talking, Resend, WhatsApp, OIDC); run `go run ./cmd/certify`; fix every discrepancy between the fakes and reality (update the fakes to enforce the real rule, add a regression test). Mark each provider certified only on a passing report.
3. Staging dress rehearsal: canary, `scripts/restore-drill.sh`, forced alerts.
4. Legal sign-off (`docs/LEGAL.md`: NDPA, NCC, CBN, NERC, LASERC), then flip production flags one at a time.
5. Pilot with `PILOT_MODE` allow-list: 50–100 households, one DisCo, daily caps, first-200 runbook. Daily reconciliation must be clean before widening.

**Done when:** a week of pilot reconciliation shows zero unexplained differences.

---

## B10+: After the pilot, when documents arrive

Each needs partner documents; **don't build blind**.

- **HES/MAP translator** for the first DisCo, behind the existing `heshttp` contract, after that DisCo's official API docs and a witnessed real-meter test. Until then do not market "no token".
- More DisCos and tariff catalogue coverage; USSD shortcode and Meta WhatsApp templates once approved.
- Native mobile apps; IdP-hosted staff MFA; multi-region and read replicas if load demands.

---

## 3. Founder decisions to ask for (don't assume)

| Decision | Batch | Default until decided |
|---|---|---|
| Retention periods (financial, operational) | B1, B3 | 7 years financial; documented operational defaults |
| Fee, commission and VAT ledger treatment | B0/B5 | the documented default table, flagged |
| Partner funding model; is holding float allowed | B5 | prefunded, `FEATURE_PARTNER_API` off |
| Partner onboarding and KYB policy | B4 | operator-created partners only |
| Sandbox keys, IdP choice, hosting go-live | B9 | providers "not certified"; staging not live |
| Pilot scope (DisCo, estate, households, caps) | B9 | empty allow-list; nothing purchasable |
| HES partner and API docs | B10 | `REMOTE_PROVIDER=none`; keep STS tokens |

---

## 4. Definition of done for the whole roadmap

All of: B1–B8 merged with CI green on both stores; the 12 invariants proved by tests; data map and runbooks complete; hosted environment live with migrations applied; every provider certified against a live sandbox; one clean week of pilot reconciliation. Only then widen beyond the pilot.
