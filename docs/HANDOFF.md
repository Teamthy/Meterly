# Handoff

> Read this first (`CLAUDE.md`). Then check it against `git log` and open PRs — it can go stale.
> **Last updated:** 2026-11-27 (B0). North star locked: no-keypad + auto-recharge, **no backing out**. Operating pack: `docs/LAUNCH.md`, `RUNBOOK.md`, `LEGAL.md`, `OUTREACH.md`, `BUDGET.md`. **Merged to `main`:** M1–M11, security engine, P0–P4 (PR #25 @ `1208ca6`). Providers **not certified** (no sandbox keys). Remaining plan: `docs/prompts/MASTER_PROMPT_BATCHES.md` (B0→B9).

## 0. Start here

1. Read **Invariants** (§4) before touching orchestrator, store, or providers.
2. Branch: `arena/7cc28241-meterly`. **Do not switch branches.**
3. Work left is **not** “build the purchase engine”. M1–M11, web, restyle, Auto-renew coming-soon, the customer security engine, and P0–P4 are **merged**. Engineering left is **B1+** (NDPA, partner API) plus **hosting, live sandboxes, real AMI/HES partner docs, legal flags**.
4. Product truth: credit lands **on the meter**. `ACCEPTED ≠ CONFIRMED`. Keypad meters get STS fallback. **Do not invent AMI/HES APIs.**
5. Do not commit secrets.

### Right now (priority)

| # | What | Who | Status |
|---|---|---|---|
| 1 | Land security engine | Eng | **Done** — [PR #22](https://github.com/Teamthy/Meterly/pull/22) merged |
| 2 | Render API + apply migrations `0018`–`0024` | Human | Blocked on dashboard paste (`docs/DEPLOY.md`). Hosted `0022`–`0024` **unapplied**. |
| 3 | Vercel `METERLY_API_ORIGIN` = Render URL | Human | After 2 |
| 4 | Live sandbox keys (Paystack, VTpass, …) then `go run ./cmd/certify` | Human | Harness ready; **not certified** |
| 5 | Partner HES / DisCo / MAP agreements + **their API docs** | Founder | `docs/OUTREACH.md` — IE Energy Lagos **and** Ikeja Electric |
| 6 | Legal sign-off before prod flags | Legal | `docs/LEGAL.md` — NDPA / NCC / CBN / NERC / LASERC |
| 7 | First 200 + 2am runbook | Founder | `docs/LAUNCH.md` + `docs/RUNBOOK.md`. No kill date. |
| 8 | P3 risk engine + chargebacks | Eng | **Merged** — [PR #24](https://github.com/Teamthy/Meterly/pull/24) via [PR #25](https://github.com/Teamthy/Meterly/pull/25). Migration `0023` **unapplied hosted**. |
| 9 | P4 pilot ops (allow-list, float cover, alerts, restore, rekey) | Eng | **Merged** — same PRs. Migration `0024` **unapplied hosted**. Dress rehearsal blocked on hosting. |
| 10 | B1 NDPA (data map, consent, retention) | Eng | Next engineering batch after B0. `docs/prompts/MASTER_PROMPT_BATCHES.md` |

---

## 1. What Meterly is

**North star (founder, 2026-11-26): OPay for electricity.** Pay → units appear **on the meter** (no keypad). Then **auto-recharge** like airtime/data: amount and/or “when units hit X kWh.” Token typing is fallback, not the product.

OPay airtime works because every phone has a **network account** the telco can credit by API. Most Nigerian prepaid meters do **not**: the balance lives on the box, and the only input is a 20-digit STS token. The electricity equivalent of “airtime API” is the **AMI/HES** that already talks to **smart** meters. Meterly does not install hardware.

- **Code today:** Paystack (money) + VTpass (licensed vend / meter verify) + remote-credit plane (`heshttp`, cohorts, confirmation). AutoPower **engine** exists; UI is **coming soon** (prefs in localStorage, `FEATURE_AUTOPOWER=false`, no live charge).
- **Blocked for the north star:** one Disco + that Disco’s MAP/AMI **API docs** + a witnessed test on a real smart meter. Until then do not market “no token.”
- Frontend: Next.js `web/` on Vercel (`https://meterly-iota.vercel.app`).
- Hosted API is **not live** until Render + Supabase keys are pasted.

---

## 2. Current state

| Piece | Status | Where |
|---|---|---|
| Purchase engine (pay → vend → deliver, three dimensions) | ✅ `main` | `internal/orchestrator` |
| Web UI + ink/paper/volt restyle | ✅ `main` (PR #21) | `web/` |
| Auto-renew | ✅ UI **coming soon** (prefs only, no live charge) | `/autopower` |
| Security engine | ✅ `main` (PR #22) | `internal/security`, `/security` |
| VTpass / Paystack adapters | ✅ code, **not certified** (no keys) | `internal/providers/*`, `internal/certify` |
| Households, token send, support E2E, Power Desk, USSD, pots | ✅ code, flags mostly off outside dev | feature flags §7 |
| P2 daily settlement recon | ✅ `main` (PR #24/#25) | `internal/reconcile`, `0022` **unapplied hosted** |
| P3 risk engine + chargebacks | ✅ `main` (PR #24/#25) | `internal/risk`, `internal/disputes`, `0023` **unapplied hosted** |
| P4 pilot ops | ✅ `main` (PR #24/#25) | `PILOT_MODE`, alerts, `cmd/integrity`, `cmd/rekey`; `0024` **unapplied hosted** |
| Hosted API (Render) + migrations `0018`–`0024` | ⏳ | `docs/DEPLOY.md` |

**Git:** `origin/main` @ `1208ca6` (merge [PR #25](https://github.com/Teamthy/Meterly/pull/25), which landed [PR #24](https://github.com/Teamthy/Meterly/pull/24) plus CI/review fixes). P0–P4 are **merged**. P1 is **not live-certified**. Next engineering: B1 (NDPA) in `docs/prompts/MASTER_PROMPT_BATCHES.md`.

**Live web:** `https://meterly-iota.vercel.app` (rebuilds from `main`).

---

## 3. Architecture (one minute)

```
cmd/api        HTTP API (+ in-process worker in MODE=dev)
cmd/worker     resume + outbox (refuses mock providers)
cmd/migrate    embedded SQL, checksummed, advisory-locked
cmd/certify    live sandbox certification (skips without keys)

internal/orchestrator   quote → pay → vend → deliver → refund; Resume
internal/security       passkey step-up, sessions, event log
internal/identity       WebAuthn + mls_ sessions
internal/store          InTx unit of work (memstore / pgstore)
internal/providers      payment, vending, remote-credit — kept separate
internal/httpapi        /v1 + admin + public pay/pots + /v1/security
web/                    Next.js 15 (root directory on Vercel)
migrations/             0001 … 0024_pilot.sql (next file `0025_*.sql`)
```

Purchase: quote → `CreateTransaction` (one commit) → PSP verify **server-side** → vend (`request_id` stored at **claim**) → deliver → refund only on definitive vend failure.

---

## 4. Invariants (must hold)

1. No vending until payment is verified server-side and the amount equals the quote total. Mismatch → `MANUAL_REVIEW`.
2. Timeout / ambiguous error → `UNKNOWN`, never `FAILED`. Re-send only if the provider has **no record**.
3. Remote `ACCEPTED` ≠ delivered. Only meter confirmation completes.
4. Refund only after a definitive vend failure. Never auto-refund if electricity may have vended.
5. Every money movement is a balanced, idempotent ledger posting.
6. No raw tokens or meter numbers in logs, URLs, or plaintext columns.
7. Concurrency never double-charges, double-vends, or double-credits.
8. VTpass `request_id` is stored at vend-claim (`VendIdempotencyKey`) and reused. Never regenerate.
9. Household roles never mint `meters.MANAGE`. Grants ≠ ownership.
10. Self-registration grants at most `PURCHASE`.
12. A prepaid vend is never confirmed without an STS token.
13. Saved cards belong only to the person who paid with them.

Enforced by `orchestrator.mutate` (lock → guard → state + history + ledger + outbox in one tx). Only the worker whose commit **claimed** a step may call the provider.

---

## 5. Running

```bash
MODE=dev go run ./cmd/api
docker compose up -d
export DATABASE_URL='postgres://meterly:meterly@localhost:5432/meterly?sslmode=disable'
go run ./cmd/migrate && MODE=dev go run ./cmd/api
go test -race ./...     # PG suite needs DATABASE_URL
```

Arena sandbox:

```bash
export PATH="/tmp/meterly-tools/go-extract/go/bin:$PATH"
export GOROOT="/tmp/meterly-tools/go-extract/go"
export GOPROXY=direct
go test -modfile=/tmp/meterly.mod ./internal/security/ ./internal/identity/ ./internal/httpapi/
```

---

## 6. Mocks vs real adapters

| Provider | Config | Live sandbox |
|---|---|---|
| Payments | `mock` / `paystack` | **not certified** (no keys) |
| Vending | `mock` / `vtpass` | **not certified** (no keys) |
| Remote credit | `mock` / `none` / `http` (`heshttp`) | **No partner** |
| SMS / email / WhatsApp | mock or vendor adapters | **not certified** (no keys) |
| Auth | `dev` / `oidc` | **not certified** (no IdP; in-process JWKS only) |

Run `go run ./cmd/certify -o certify-report.json` (or `make certify`) when keys are in the environment. Live tests: `go test -tags certify ./internal/certify/...` (skipped without keys; not in CI). Certify refuses `sk_live_` and non-sandbox VTpass/Africa's Talking unless `CERTIFY_ALLOW_LIVE=true`.

Outside `MODE=dev`, mocks are refused. `cmd/worker` refuses mocks.

VTpass: amount is **naira**; `000` is not success; `request_id` first 12 chars = Africa/Lagos `YYYYMMDDHHII` of pay day; documented DisCos only (no invented Benin Electric `serviceID`).

---

## 7. Feature flags

| Flag | Prod default | Notes |
|---|---|---|
| `FEATURE_GIFT_SMS` | false | NDPA; also third-party token send |
| `FEATURE_SPLIT_METER` | false | fund-holding legal |
| `FEATURE_AUTOPOWER` | false | live auto-charge off; UI is coming-soon prefs |
| `FEATURE_POWER_DESK` | false | legal |
| `FEATURE_USSD` / `FEATURE_WHATSAPP` | false | NCC / Meta WABA |
| `FEATURE_HOUSEHOLDS` / `FEATURE_SUPPORT` / `FEATURE_PAY_FOR_ME` | on in dev | |
| `FEATURE_RISK_ENGINE` | on in dev + staging | DENY/REVIEW; disputes always ingest |
| `PILOT_MODE` | false | env, not FEATURE_*; empty allow-list means nothing is purchasable when true |
| `FEATURE_OWNERSHIP_NAME_MATCH` | false | self-service ownership by display-name match is spoofable; operators attest via `/v1/admin/meters/{id}/ownership` |

---

## 8. Gotchas

- **PR #24 review fixes (2026-11-27), see DECISIONS:**
  - dispute outcomes are never guessed and are final once set;
  - wallet top-ups need an `Idempotency-Key`;
  - risk rule changes are audited in the same transaction;
  - tests must use real user UUIDs for operators (PostgreSQL rejects `"admin:test"`).

- **Review fixes (2026-11-27), see DECISIONS "Review fixes":**
  - sessions stay bound to their first network until a passkey step-up;
  - passkey assertions need UV;
  - adding a passkey needs a step-up;
  - Pay-for-Me payer cards can't be saved;
  - a prepaid vend without a token stays pending;
  - VTpass `083` is pending;
  - Paystack references must be `[A-Za-z0-9-.=]` (`paystacktest` enforces it);
  - ownership name match is off outside dev.

- Migrations are **immutable** once applied. Add `0025_*.sql`, never edit applied files. Hosted `0022`/`0023`/`0024` are still unapplied; do not run them from this sandbox.
- Supabase **session** pooler **:5432**, not transaction 6543. Project `wiqfbscyppgsolvmereh` (eu-west-1). Do not migrate `okatwpkovcmipjdnqkys`.
- Hosted migrations **0018 identity, 0019 readings, 0020 catalog, 0021 security** are still unapplied until Render `/migrate` runs.
- `AUTH_MODE=dev` only in `MODE=dev`. Passkey sessions are `mls_…`.
- Security engine: **dev/OIDC bearers are not stepped-up**. Passkey sessions must confirm the device (15 min) plus IP HMAC bind for token read / save card / last-passkey delete / revoke-others.
- Auto-renew UI does **not** call `/v1/autopower/rules` (those routes can still create live rules if something hits the API; flag defaults false).
- Render is not an Arena connector.

---

## 9. What's left

**Engineering batches** (`docs/prompts/MASTER_PROMPT_BATCHES.md`): B0 hygiene (this session) → B1–B3 NDPA → B4–B7 partner API → B8 hardening → B9 founder go-live. Do not mix batches in one PR.

**Unblocked after hosting/keys:** Render + Vercel origin; live Paystack/VTpass/AT/Resend/WhatsApp/OIDC sandbox runs; `REDIS_URL` if >1 API replica; P4 dress rehearsal (canary, restore-drill, forced alerts).

**Blocked on documents / partners:** HES translator; undocumented DisCos; native apps.

**Not engineering:** commercial agreements; real-meter AMI before marketing “no tokens”; legal flags; IdP choice; USSD shortcode; Meta templates.

### Founder decisions (pending — do not assume)

| Decision | Needed for | Default until decided |
|---|---|---|
| **Settlement fee, VTpass commission, VAT on the convenience fee** (ledger treatment) | P2 / B5 | Documented default table in `docs/DECISIONS.md` “Chart of accounts (P2)”. **Founder/finance must confirm against the live merchant contract.** `VENDOR_COMMISSION_INCOME` is unused until a rate is set. Engine never invents a split. |
| Retention periods (financial, operational) | B1, B3 | 7 years financial; documented operational defaults |
| Partner funding model; is holding float allowed | B5 | prefunded, `FEATURE_PARTNER_API` off |
| Partner onboarding / KYB | B4 | operator-created partners only |
| Sandbox keys, IdP, hosting go-live | B9 | providers “not certified”; staging not live |
| Pilot scope (DisCo, estate, households, caps) | B9 | empty `PILOT_MODE` allow-list; nothing purchasable |
| HES partner + API docs | B10 | `REMOTE_PROVIDER=none`; keep STS |

---

## 10. Hosting checklist (founder)

1. [ ] `main` has PR #21 + #22 (yes after 2026-11-26).
2. [ ] https://render.com/deploy?repo=https://github.com/Teamthy/Meterly
3. [ ] `DATABASE_URL` = Supabase session pooler `:5432`, `sslmode=require`.
4. [ ] `TOKEN_ENC_KEY` + `METER_HMAC_KEY` (64 hex each, not in git).
5. [ ] Confirm migrate applied **through 0024** (`0018`–`0021` still unapplied on hosted; `0022`–`0024` also unapplied).
6. [ ] `GET /healthz` and `/readyz`.
7. [ ] Vercel `METERLY_API_ORIGIN=https://<render-host>` and redeploy.
8. [ ] Later: `MODE=staging`, `PAYMENT_PROVIDER=paystack`, `VENDING_PROVIDER=vtpass`, `REMOTE_PROVIDER=none`.

---

## 11. Documents you still need (from other people)

These are **not** in the repo. Do not invent replacements.

### Commercial / access
1. **VTpass (or other aggregator) contract** — sandbox + live keys, electricity product enabled, SLA, who owns failed vends.
2. **Paystack (CBN-licensed PSP) merchant agreement** — `sk_test_` then `sk_live_`, webhook secret, settlement account.
3. **DisCo letters / aggregator coverage** — confirmation that IKEDC, EKEDC, AEDC, … electricity `serviceID`s you vend are authorised for your merchant.
4. **Remote-credit path (pick one, then get their API PDF):**
   - Meter Asset Provider (MAP) / AMI operator HES API, **or**
   - DisCo AMI/HES API, **or**
   - Meter OEM HES API (Hexing / Conlog / Mojec / similar) **if they actually operate the head-end**.
5. **Identity provider** (Auth0, Clerk, Keycloak, …) — OIDC discovery, JWKS, `email_verified` / `phone_number_verified`, roles claim, MFA AMR for staff.

### Legal / regulator
6. **NDPA 2023** — DPIA, lawful basis, retention, gift-SMS to third parties, encryption at rest (already in code; counsel must sign).
7. **NERC / state electricity regulator** — whether Meterly is a vendor, a super-agent, or needs a licence to sell units.
8. **CBN / PSP money flow opinion** — Paystack settlement vs any fund-holding (pots, AutoPower mandates).
9. **NCC** — USSD short code (`*347*88#` is a product sketch until you own a code) + telco IP CIDRs.
10. **Meta WhatsApp Business** — WABA, `hub.verify_token`, approved templates (`meterly_offline_token_v1`, etc.).

### Technical (from the HES/MAP/DisCo partner)
11. Auth scheme (mTLS? HMAC? OAuth?).
12. How to **push credit / inject STS** for a meter (request + idempotency key).
13. How to **query status** (accepted vs meter-confirmed).
14. How the meter **confirms** (webhook vs poll). We will not treat network “accepted” as delivered.
15. Error catalogue (timeout, duplicate, meter unreachable, already credited).
16. Sandbox meters and a **witnessed test** on a real meter before any “no token” marketing (spec §19).

If a document is missing, leave `REMOTE_PROVIDER=none` and keep STS.

---

## 12. Who to partner with (OPay-for-power)

You do **not** partner with every meter factory. You partner with whoever can **credit a meter the way MTN credits a SIM**.

| Layer (like OPay) | Electricity equivalent | Who to call |
|---|---|---|
| Wallet / card | Pay | **Paystack** |
| “Is this number real?” | Meter name/address | **VTpass** `merchant-verify` (already coded). Not a Disco API. |
| Licensed units | STS vend | **VTpass** `pay` (still required even for no-keypad: we mint, then inject) |
| **Airtime lands on the phone** | **Units land on the meter, no keypad** | **One Disco + that Disco’s MAP / AMI / HES operator** |
| “Buy ₦2,000 data when I hit 100MB” | Auto-recharge ₦X or when kWh ≤ Y | Same HES **plus** Paystack saved card. Engine is AutoPower; live charge stays off until legal + AMI. |

**Reach out, in this order**

1. **Ikeja Electric (IKEDC) commercial / vendor / AMI** — *one* Disco. Ask: *who operates your prepaid **smart-meter HES**? We need remote credit push + confirmation + (for auto-recharge) a balance/reading API. We will vend through a licensed aggregator.* Historically IKEDC MAPs have included **Mojec**, **CIG/Conlog**, **New Hampshire**. Do not guess which one runs AMI today — ask Ikeja.
2. **That MAP’s integrations / HES team** (whoever Ikeja names). Ask for the API PDF: auth, push credit, status, meter-confirmed webhook, errors, sandbox meters. This is the OPay pipe.
3. **VTpass** — merchant + electricity keys. Meter verify + licensed token. Still needed.
4. **Paystack** — merchant + `sk_test_` then live. Money + later saved-card mandates for auto-recharge.

**Do not call yet:** Hexing/Conlog/Mojec sales (unless the Disco/MAP says they run the HES), Benin Electric, every other Disco, telcos, Meta.

**No hardware.** Customers already have meters. MAPs already installed AMI where it exists. Meterly is software on top.

**Honest coverage:** no-keypad + auto-recharge like OPay only works on **AMI-connected** meters. Keypad-only STS meters cannot receive a silent credit — there is no radio. Product copy: “works on compatible smart meters,” never “all meters, no token.”

Sequence: get Ikeja (or one Disco) HES docs → translator behind `heshttp` → attest cohort in `/admin` → witnessed real-meter test → then auto-recharge on that cohort. Repeat per Disco.

---

## 13. Security engine (as shipped)

- Passkey session (`mls_`) + at least one credential: `TOKEN_READ`, `SAVE_CARD`, `DELETE_PASSKEY`, `REVOKE_OTHERS` need a step-up younger than 15 minutes.
- Last remaining passkey cannot be deleted.
- Dev/OIDC bearers: **not** stepped-up (sandbox/staff).
- IP HMAC bind: if the session already has an IP hash and the request IP differs, step-up again.
- Events: action, outcome, reason, session id, IP HMAC. Never tokens, meters, contacts, raw IPs.
- API headers: `nosniff`, `DENY` framing, `no-store`, `no-referrer`.
- Step-up with zero passkeys does not issue a challenge.
- Failed step-up counts toward the auth-failure rate limit.

UI: **More → Security** (`/security`).

---

## 14. Session log (newest first)

- **2026-11-27: B0 repo hygiene.** `main` is `1208ca6` (PR #25 landed PR #24). Rewrote HANDOFF rows 8–9 and git/state as **merged**; kept hosted `0022`–`0024` unapplied. Added `TestEmbeddedMigrationsIncludeP2P4` (FS order, no PG) and `TestP2P4MigrationChain` (smoke-query every 0022–0024 table/account). OpenAPI/mux now lists P2–P4 admin routes explicitly. Recorded settlement fee/commission as a pending founder/finance decision. Plan file: `docs/prompts/MASTER_PROMPT_BATCHES.md`. Verified: `gofmt -l .` empty, `go vet ./...`, `go test -race ./...` (memory; PostgreSQL skipped, no `DATABASE_URL`). Unverified: CI with `DATABASE_URL` (the chain smoke needs PG). Blocked: hosted migrate, sandbox keys. Next: B1 NDPA (do not start until this PR merges).

- **2026-11-27: review of PR #24 (P0–P4) and merge.**
  - PR #24's CI was red: the pilot tests used a non-UUID operator ID.
  - Review found:
    - dispute outcomes guessed as LOST on unknown resolutions;
    - a dispute outcome flip that could release the reserve twice;
    - non-idempotent wallet top-ups;
    - risk rule changes audited outside their transaction with errors ignored;
    - a template-database leak in the reconcile tests.
  - Fixed on `claude/loving-wozniak-yr6igw` (PR #24 merged in plus the fixes), with tests on both stores.
  - Verified: full suite with the race detector on memory and PostgreSQL 16, 544 passed / 0 skipped / 0 failed (95 PostgreSQL subtests); concurrent suites repeated; no test DBs left.
  - Merged to `main` via the follow-up PR, which also marks #24 merged.

- **2026-11-27: P3 risk controls and chargebacks.** `internal/risk` + `internal/disputes` + migration `0023`. DENY never charges; REVIEW never vends; dispute open/win/loss postings balance. Paystack `GET /dispute` and `charge.dispute.*` payload fields. `FEATURE_RISK_ENGINE` on in dev/staging. Unverified: live Paystack dispute webhooks; hosted `0023`. Do not apply hosted migrations from this sandbox. Next is P4 (pilot ops).
- **2026-11-27: P2 daily settlement recon landed** (`3797530`). Engine `RunDay`/`CatchUp`, admin exceptions, `0022` unapplied hosted.
- **2026-11-27: CI Postgres failure from quote re-verify.** `TestObservabilityMetricsAndOTLPTracing` expected one `mockvend.validate_meter`; register + quote is two. Actions logs are not readable from this sandbox (`results-receiver.githubusercontent.com`). Unverified: CI green after the assertion change; P2 settlement not started.
- **2026-11-27: P1 certify harness.** `internal/certify` + `cmd/certify`. Untagged tests run the scenarios against in-repo fakes (pass). `go test -tags certify` and `cmd/certify` skip every provider here (no keys) and write a 30-scenario skipped report. Paystack fake gained documented `/charge`; VTpass fake rejects non-dates as `085`. Unverified: every live sandbox. Next is P2 settlement/reconciliation (`0022`).
- **2026-11-27: review fixes of PRs #5–#23 landed here.** Guest-payer cards cannot be saved; session IP binds once; passkey UV + step-up to add; attested key preferred; prepaid vend needs an STS token; VTpass `083` is pending; AutoPower Paystack refs use `apchg-`; ownership name-match is flagged off outside dev. `origin/main` (`9cac48b`) merged.
- **2026-11-26: 100 edge cases.** `docs/EDGE_CASES.md` — IN / OPS / GAP. Quote gates, in-flight, digits, wallet, dispute pause are in code.
- **2026-11-26: Operating pack.** Catch-22 (supply must be on), first 200 = Ikeja/IE Energy Lagos Mojec, 2am runbook, fraud, fee leak, legal drafts `/legal/*`, Lagos entity rename, feature freeze (later), no kill date.
- **2026-11-26: North star = no-keypad + OPay-style auto-recharge.** Token is fallback. First partner call is one Disco AMI/MAP, then VTpass + Paystack.
- **2026-11-26: PR #22 merged** (`b623755`). Customer security engine on `main`.
- **2026-11-26: PR #21 merged.** Restyle + coming-soon Auto-renew + consoles.
- **2026-11-26: PR #20** and earlier — identity, packs, Redis, credit-on-meter plane. See git history.
