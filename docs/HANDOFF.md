# Handoff

> Read this first (`CLAUDE.md`). Then check it against `git log` and open PRs — it can go stale.
> **Last updated:** 2026-11-26 after PR **#22** (security engine) merged to `main` (`b623755`). Hardening (IP bind, headers, step-up with no passkeys) follows on this branch.

## 0. Start here

1. Read **Invariants** (§4) before touching orchestrator, store, or providers.
2. Branch: `arena/7cc28241-meterly`. **Do not switch branches.**
3. Work left is **not** “build the purchase engine”. M1–M11, web, restyle, Auto-renew coming-soon, and the customer security engine are in code. Left is **hosting, live sandboxes, real AMI/HES partner docs, legal flags**.
4. Product truth: credit lands **on the meter**. `ACCEPTED ≠ CONFIRMED`. Keypad meters get STS fallback. **Do not invent AMI/HES APIs.**
5. Do not commit secrets.

### Right now (priority)

| # | What | Who | Status |
|---|---|---|---|
| 1 | Land security engine | Eng | **Done** — [PR #22](https://github.com/Teamthy/Meterly/pull/22) merged |
| 2 | Render API + apply migrations `0018`–`0021` | Human | Blocked on dashboard paste (`docs/DEPLOY.md`) |
| 3 | Vercel `METERLY_API_ORIGIN` = Render URL | Human | After 2 |
| 4 | Live sandbox keys (Paystack, VTpass, …) | Human | After 2 |
| 5 | Partner HES / DisCo / MAP agreements + **their API docs** | Founder | See §12 Partners |
| 6 | Legal sign-off before prod flags | Legal | NDPA / NCC / CBN / NERC |

---

## 1. What Meterly is

A Nigerian electricity platform. People buy prepaid units from anywhere, for their own meter or someone else's.

- **Today in production-shaped code:** pay via CBN-licensed PSP (Paystack adapter), vend via aggregator (VTpass adapter, sandbox-fake tested), keypad **STS token** fallback.
- **Credit-on-meter:** licensed vend still happens first; a Head-End System (HES) injects that token/credit into a compatible smart meter. We only call it delivered when the **meter confirms**.
- Frontend: Next.js `web/` on Vercel (`https://meterly-iota.vercel.app`).
- API: Go modular monolith. Hosted API is **not live** until Render + Supabase keys are pasted.

---

## 2. Current state

| Piece | Status | Where |
|---|---|---|
| Purchase engine (pay → vend → deliver, three dimensions) | ✅ `main` | `internal/orchestrator` |
| Web UI + ink/paper/volt restyle | ✅ `main` (PR #21) | `web/` |
| Auto-renew | ✅ UI **coming soon** (prefs only, no live charge) | `/autopower` |
| Security engine | ✅ `main` (PR #22) | `internal/security`, `/security` |
| VTpass / Paystack adapters | ✅ code, **not live-sandbox tested** | `internal/providers/*` |
| Households, token send, support E2E, Power Desk, USSD, pots | ✅ code, flags mostly off outside dev | feature flags §7 |
| Hosted API (Render) + migrations `0018`–`0021` | ⏳ | `docs/DEPLOY.md` |

**Git:** `origin/main` @ `b623755` (merge #22). This branch may be 1+ hardening commits ahead.

**Live web:** `https://meterly-iota.vercel.app` (rebuilds from `main`).

---

## 3. Architecture (one minute)

```
cmd/api        HTTP API (+ in-process worker in MODE=dev)
cmd/worker     resume + outbox (refuses mock providers)
cmd/migrate    embedded SQL, checksummed, advisory-locked

internal/orchestrator   quote → pay → vend → deliver → refund; Resume
internal/security       passkey step-up, sessions, event log
internal/identity       WebAuthn + mls_ sessions
internal/store          InTx unit of work (memstore / pgstore)
internal/providers      payment, vending, remote-credit — kept separate
internal/httpapi        /v1 + admin + public pay/pots + /v1/security
web/                    Next.js 15 (root directory on Vercel)
migrations/             0001 … 0021_security.sql
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
| Payments | `mock` / `paystack` | **No** |
| Vending | `mock` / `vtpass` | **No** |
| Remote credit | `mock` / `none` / `http` (`heshttp`) | **No partner** |
| SMS / email / WhatsApp | mock or vendor adapters | **No** |
| Auth | `dev` / `oidc` | **No real IdP** |

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

---

## 8. Gotchas

- Migrations are **immutable** once applied. Add `0022_*.sql`, never edit applied files.
- Supabase **session** pooler **:5432**, not transaction 6543. Project `wiqfbscyppgsolvmereh` (eu-west-1). Do not migrate `okatwpkovcmipjdnqkys`.
- Hosted migrations **0018 identity, 0019 readings, 0020 catalog, 0021 security** are still unapplied until Render `/migrate` runs.
- `AUTH_MODE=dev` only in `MODE=dev`. Passkey sessions are `mls_…`.
- Security engine: **dev/OIDC bearers are not stepped-up**. Passkey sessions must confirm the device (15 min) plus IP HMAC bind for token read / save card / last-passkey delete / revoke-others.
- Auto-renew UI does **not** call `/v1/autopower/rules` (those routes can still create live rules if something hits the API; flag defaults false).
- Render is not an Arena connector.

---

## 9. What's left

**Unblocked after hosting/keys:** Render + Vercel origin; live Paystack/VTpass/AT/Resend/WhatsApp/OIDC sandbox runs; `REDIS_URL` if >1 API replica.

**Blocked on documents / partners:** HES translator; undocumented DisCos; native apps.

**Not engineering:** commercial agreements; real-meter AMI before marketing “no tokens”; legal flags; IdP choice; USSD shortcode; Meta templates.

---

## 10. Hosting checklist (founder)

1. [ ] `main` has PR #21 + #22 (yes after 2026-11-26).
2. [ ] https://render.com/deploy?repo=https://github.com/Teamthy/Meterly
3. [ ] `DATABASE_URL` = Supabase session pooler `:5432`, `sslmode=require`.
4. [ ] `TOKEN_ENC_KEY` + `METER_HMAC_KEY` (64 hex each, not in git).
5. [ ] Confirm migrate applied **through 0021**.
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

## 12. Who to partner with (not “every meter factory”)

You do **not** need to partner with every company that *builds* meters.

| Goal | Who | Why |
|---|---|---|
| Take payment | **CBN-licensed PSP** (Paystack today) | Card/transfer; Meterly never holds PAN. |
| Mint an STS token | **Vending aggregator** (VTpass) **or** a **DisCo vending API** | Token is what keypad meters need. One aggregator can cover many DisCos. |
| Units appear **without typing a token** | Whoever runs the **HES / AMI** for that meter cohort | Usually a **MAP** (meter asset provider), the **DisCo’s AMI team**, or an OEM **only if they operate the head-end** (Hexing, Conlog, Mojec, etc.). |
| Confirm “on the meter” | Same HES, plus a confirmation webhook or status API | This is the difference between ACCEPTED and CONFIRMED. |
| USSD | **Telco + NCC shortcode**, Africa’s Talking or similar | Not a meter company. |
| WhatsApp | **Meta WABA** | Not a meter company. |

**Practical sequence**

1. Ship **token vending** with VTpass + Paystack (no meter OEM required).
2. Pick **one** remote-credit partner for **one** DisCo + one meter brand/cohort (e.g. IKEDC + Hexing via that DisCo’s MAP).
3. Get their HES docs → implement a translator behind `heshttp` → attest the cohort in `/admin` → test on a real meter.
4. Repeat per cohort. Never advertise “no tokens” globally.

Meter manufacturers matter **only** as the operator of (or gateway into) the HES. DisCos matter for **authorising** you to vend and, if they own AMI, for remote credit. MAPs often sit in between.

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

- **2026-11-26: PR #22 merged** (`b623755`). Customer security engine on `main`.
- **2026-11-26: PR #21 merged.** Restyle + coming-soon Auto-renew + consoles.
- **2026-11-26: PR #20** and earlier — identity, packs, Redis, credit-on-meter plane. See git history.
