# Handoff

> Read this first (`CLAUDE.md`). Then check it against `git log` and open PRs — it can go stale.
> **Last updated:** 2026-11-23, after VTpass + households (FR-008) + token send (FR-007) on `arena/7cc28241-meterly`.

## 0. Start here (new session / new developer)

1. Read **Invariants** (§4) before touching orchestrator, store, or providers.
2. Branch: `arena/7cc28241-meterly` at `318b81c` (plus this handoff commit). Base is `origin/main` @ `90e0078` (PR #13 merged).
3. Work left is **not** “build the purchase engine”. M1–M11, web, support chat, VTpass, households, and token send are in code. Left is **hosting, live sandboxes, AMI/HES docs, legal flags, frontend for new APIs**.
4. Do **not** invent AMI/HES, Benin Electric `serviceID`, or native apps.
5. Do **not** commit secrets. Render keys stay in the dashboard.

### Immediate next work (priority order)

| # | What | Who | Blocker |
|---|---|---|---|
| 1 | Merge the PR from this branch | GitHub | review |
| 2 | **Render dashboard paste** so the hosted API exists | Human | Render is not an Arena connector. Paste session-pooler `DATABASE_URL` **port 5432**, `TOKEN_ENC_KEY`, `METER_HMAC_KEY`. See `docs/DEPLOY.md`. |
| 3 | Vercel `METERLY_API_ORIGIN` = Render URL | Human | step 2 |
| 4 | After keys: `MODE=staging`, `VENDING_PROVIDER=vtpass`, `PAYMENT_PROVIDER=paystack`, `REMOTE_PROVIDER=none` | Human | VTpass + Paystack sandbox keys |
| 5 | Live sandbox verification (Paystack, VTpass, AT, Resend, WhatsApp, OIDC) | Eng + keys | outbound to those hosts; this Arena sandbox cannot call them |
| 6 | Web UI for households + token send | Eng | APIs exist; `web/` has no household screens yet |
| 7 | AMI/HES remote credit | Eng | **real partner docs only** — do not invent |
| 8 | Distributed rate limits (Redis) | Eng | only if >1 API replica |
| 9 | Legal sign-off before prod flags | Founder/legal | NDPA / NCC / CBN |

---

## 1. What Meterly is

A Nigerian electricity platform. People buy prepaid electricity from anywhere, for their own meter or someone else's. Delivery is **STS token** today (VTpass). **Remote credit** (AMI/HES, no keypad) is an interface plus mock only — no partner docs.

Backend: Go modular monolith. Frontend: Next.js 15 in `web/`. Spec mapped in `docs/ROADMAP.md`. Decisions in `docs/DECISIONS.md`. Deploy in `docs/DEPLOY.md`.

---

## 2. Current state

| Milestone | Status | Where |
|---|---|---|
| Foundation → M11 (engine, PG, Paystack, OIDC/admin, notifications, OpenAPI, Pay-for-Me, Split the Meter, AutoPower, Power Desk, USSD/WhatsApp) | ✅ merged | PRs #1–#11 |
| Web frontend | ✅ merged | [PR #12](https://github.com/Teamthy/Meterly/pull/12) |
| Support chat (agent + E2E human) | ✅ merged | [PR #13](https://github.com/Teamthy/Meterly/pull/13) |
| **VTpass electricity vending** | ✅ this branch | `internal/providers/vtpass` — **not live-sandbox tested** |
| **FR-008 households / verified ownership** | ✅ this branch | `FEATURE_HOUSEHOLDS`, never mints `MANAGE` |
| **FR-007 token send** SMS/email/WhatsApp | ✅ this branch | share endpoint + notification worker |
| Hosting: Vercel `web/` | ✅ live | `meterly-iota.vercel.app` |
| Hosting: Render API + Supabase | ⏳ **blocked on dashboard paste** | `docs/DEPLOY.md` |

CI (`.github/workflows/ci.yml`): gofmt, vet, `go test -race` against PostgreSQL 16; fails if PG subtests were skipped.

---

## 3. Architecture in one minute

```
cmd/api        HTTP API (+ worker loops in-process when RUN_WORKER=true, default in dev)
cmd/worker     resume sweep + outbox publisher (refuses to run with mock providers)
cmd/migrate    applies embedded migrations

internal/orchestrator   purchase workflow; Resume = single recovery path   ← core
internal/store          Store/Repos contracts; InTx = unit of work
  memstore / pgstore
internal/registry       meter validation, capabilities, access grants
                        (+ household perm fallback)
internal/providers      Payment / Vending / RemoteCredit contracts
  mock                  deterministic mocks (meter-number behaviour; README)
  paystack (+paystacktest fake)
  vtpass  (+vtpasstest fake)   first real vending adapter
internal/households     FR-008 domain (roles, NamesMatch, PermissionForRole)
internal/tokenshares    COPY metadata + send channels (no raw tokens)
internal/notificationservice   outbox → deliveries; TOKEN_SHARE send path
internal/httpapi        /v1 REST + admin + public pay/pots + channels + households
api/openapi.yaml        OpenAPI 3.1 vs mux + writeErr AST
web/                    Next.js 15 (no household UI yet)
migrations/             0001 … 0016_token_share_send.sql
```

Purchase flow: quote → `CreateTransaction` (idempotent, one commit) → `EnsurePaymentInitialised` → webhook/sweeper → `checkPayment` (PSP verify) → `vend` (VTpass `request_id` stored at **claim**, never regenerated) → `deliver` → refund only on definitive vend failure.

### New HTTP (this branch)

Households (`FEATURE_HOUSEHOLDS`, default **on in `MODE=dev`**, else off):

- `POST/GET /v1/households`, `GET /v1/households/{id}`
- `POST /v1/households/{id}/meters`
- `POST /v1/households/{id}/members` (`user_id` **or** `external_id` + role)
- `POST …/members/me/accept|decline`, `DELETE …/members/{uid}`
- `POST/GET /v1/meters/{meter_id}/ownership[/verify]`
- Admin `POST /v1/admin/meters/{meter_id}/ownership` (reason 5–500)

Token shares (purchaser only, vend confirmed, token path):

- `POST /v1/electricity/transactions/{id}/token/shares`
  - `{"channel":"COPY"}` — metadata only
  - `{"channel":"SMS"|"EMAIL"|"WHATSAPP"}` — send to verified contact
  - `destination` set → third-party; requires `FEATURE_GIFT_SMS`
- `GET` same path — COPY + sends (masked dest only)
- Cap: 5 sends / purchaser / Africa/Lagos day. Token decrypted only in worker memory. No auto-resend of token-bearing `UNKNOWN`.

---

## 4. Invariants (must hold after every change)

1. No vending until payment is verified server-side and the amount equals the quote total exactly. A mismatch goes to MANUAL_REVIEW.
2. A timeout or ambiguous error means UNKNOWN, never FAILED. Resolve it by status query; re-send only if the provider confirms it has no record.
3. Remote ACCEPTED ≠ delivered. Only meter confirmation completes.
4. Refund only after a definitive vend failure.
5. Every money movement is a balanced, idempotent ledger posting. PostgreSQL triggers enforce balance and append-only.
6. No raw tokens or meter numbers in logs, URLs or plaintext columns. Tokens are readable only by the purchaser.
7. Duplicates and concurrency never double-charge, double-vend or double-credit.
8. **VTpass `request_id` is stored at vend-claim (`Transaction.VendIdempotencyKey`) and reused on retry.** Do not call `NewVendKey` again on resume.
9. **Household roles never mint `meters.MANAGE`.** Grants are not proof of ownership. Add-meter requires `ActiveOwnership`.
10. **Self-registration still grants at most `PURCHASE`.** Legacy `MANAGE` rows are unverified — do not silently downgrade, do not treat as ownership.

**How the code enforces them:**

- Every state change goes through `orchestrator.mutate` (lock row → guarded change → save state, history, ledger and outbox in one tx).
- Guards return `errSkip` when another worker got there first.
- **Only the worker whose commit claimed a step may call the provider** (`claimed` flag in `vend`, `deliver`, `refund`). Do not infer the claim from resulting state.

---

## 5. Running things

```bash
MODE=dev go run ./cmd/api                      # in-memory, mocked payment/vending/AMI
docker compose up -d && export DATABASE_URL='postgres://meterly:meterly@localhost:5432/meterly?sslmode=disable'
go run ./cmd/migrate && MODE=dev go run ./cmd/api
go test -race ./...                            # set DATABASE_URL for the PG suite
```

**Arena sandbox:** default PATH has no `go`; `GOPROXY=off` without the overlay fails uuid.

```bash
export PATH="/tmp/meterly-tools/go-extract/go/bin:$PATH"
export GOROOT="/tmp/meterly-tools/go-extract/go"
export GOPROXY=off GOSUMDB=off
go test -modfile=/tmp/meterly-tools/overlay.mod ./...
```

No local Postgres in this sandbox → pgstore tests skip. CI runs them.

### PostgreSQL locally (if you have binaries)

See older notes: cluster as `postgres`, `sslmode=disable`. Any new `pgtest` package needs `TestMain` → `pgtest.DropTemplate()`.

---

## 6. Mocks vs real adapters

| Provider | Config | Implementation | Tested against | Live sandbox |
|---|---|---|---|---|
| Payments | `PAYMENT_PROVIDER=mock\|paystack` | mock / `internal/providers/paystack` | `paystacktest` fake | **No** |
| Vending | `VENDING_PROVIDER=mock\|vtpass` | mock / `internal/providers/vtpass` | `vtpasstest` fake | **No** |
| Remote credit (AMI) | `REMOTE_PROVIDER=mock\|none` | mock or disabled | mock only | **No adapter** — no docs |
| Meter readings | `METER_READING_PROVIDER=mock\|none` | mock or disabled | mock | **No** |
| SMS | `SMS_PROVIDER=mock\|africastalking\|none` | AT adapter | `africastalkingtest` | **No** |
| Email | `EMAIL_PROVIDER=mock\|resend\|none` | Resend | `resendtest` | **No** |
| WhatsApp | `WHATSAPP_PROVIDER=mock\|whatsapp\|none` | Cloud API | `whatsapptest` | **No** |
| Auth | `AUTH_MODE=dev\|oidc` | DevAuth / OIDC JWKS | in-process JWKS | **No real IdP** |

`config.UsesMocks()` is true if **any** of payment/vending/remote/readings/sms/email/whatsapp is `mock`. Outside `MODE=dev`, mocks are refused. `cmd/worker` also refuses mocks (state is in-process).

**VTpass facts (do not “fix” without docs):**

- POST `/api/merchant-verify`, `/api/pay`, `/api/requery`
- Live `https://vtpass.com/api`, sandbox `https://sandbox.vtpass.com/api`
- Auth: POST headers `api-key` + `secret-key` (not Basic)
- Amount on pay is **naira**, not kobo
- `request_id` ≥ 12 chars; first 12 = Africa/Lagos `YYYYMMDDHHII` of **pay day**; **store at claim, never regenerate**
- `000` is not success — inspect `content.transactions.status` (`delivered` = success); `099` = requery
- Documented DisCos only: IKEDC, EKEDC, AEDC, IBEDC, EEDC, PHED, KEDCO, KAEDCO, JED, YEDC. **No Benin Electric `serviceID` — do not invent.**
- Sandbox prepaid `1111111111111`, postpaid `1010101010101`

**Mock vending** still keys behaviour off meter numbers (see README). memstore still will not find txns by **vend** ref (`txnByPayRef` is payment-only).

---

## 7. Feature flags

| Flag | Default | Notes |
|---|---|---|
| `FEATURE_HOUSEHOLDS` | on in `MODE=dev`, else off | this branch |
| `FEATURE_GIFT_SMS` | **false everywhere** until legal | also gates **third-party** token send |
| `FEATURE_PAY_FOR_ME` | on in dev | |
| `FEATURE_SPLIT_METER` | on in dev | legal |
| `FEATURE_AUTOPOWER` | false | legal |
| `FEATURE_POWER_DESK` | false | legal |
| `FEATURE_USSD` / `FEATURE_WHATSAPP` | false | NCC / Meta WABA |
| `FEATURE_SUPPORT` | on in dev | |

Purchaser **self-send** of a token to their own verified SMS/email/WhatsApp does **not** need `FEATURE_GIFT_SMS`. Sending to another destination does.

---

## 8. Gotchas

- **Migrations are immutable** once applied; runner checksums. Add `0017_*.sql`, never edit 0015/0016 after they hit a real DB.
- **Supabase session pooler must be port 5432**, not transaction-mode 6543 (pgx prepared statements). Project for Render: `wiqfbscyppgsolvmereh` (eu-west-1). Do **not** migrate inactive project `okatwpkovcmipjdnqkys`.
- **Git:** this session’s branch name is fixed (`arena/7cc28241-meterly`). After a PR merges, `git merge origin/main` — do not rebase/force-push.
- **Orchestrator time** is `o.Now` (injectable).
- **Auth:** `AUTH_MODE=dev` only in `MODE=dev`. Bearer token *is* the subject; `operator:` / `admin:` prefixes. `X-Dev-Display-Name`, `X-Dev-Verified-Email`, `X-Dev-Verified-Phone` populate contacts.
- **Rate limits are in-memory per process.** Redis only when one replica’s view is not enough.
- **Email is required** on `POST /v1/electricity/transactions` (Paystack).
- **Do not restore `internal/households/service.go`** — mutations live on `Orchestrator`.
- **Render connector is unsupported** in Arena. Dashboard + `render.yaml` only.
- Missing helpers from older notes: use `internal/secure` (not `box.go`), `observability` `InjectHTTP`.

---

## 9. What's left (honest)

### Engineering, unblocked once hosting/keys exist

- Render deploy + Vercel origin (human paste).
- Live sandbox runs: Paystack `sk_test_`, VTpass sandbox keys, AT, Resend, WhatsApp, a real OIDC issuer.
- `web/` screens for households, ownership verify, token send (API is ready; UI is not).
- Optional: Redis rate limits if Render scales to >1 instance.

### Engineering, blocked on documents / partners

- AMI/HES remote-credit adapter — **interface + mock only**. No partner API. Do not invent.
- Benin Electric and any undocumented DisCo.
- Native mobile apps.

### Not engineering (production blockers)

- Aggregator/DisCo commercial agreement (VTpass code exists; contract does not).
- Real-meter AMI confirmation before marketing “no tokens” (spec §19).
- Legal: NERC/state regulators, CBN/PSP money flow, NDPA 2023 before enabling gift SMS / split / AutoPower / Power Desk / USSD / WhatsApp in production.
- Identity provider choice (Auth0/Clerk/…) with `email_verified` / `phone_number_verified`, roles claim, MFA for staff.
- NCC/telco USSD shortcode + CIDRs; Meta WABA + approved templates (`meterly_offline_token_v1`, `meterly_token_available_v1`, …).

### Explicitly out of scope unless asked

- Inventing AMI/HES.
- Legal sign-off.
- Switching this Arena session to another git branch.

---

## 10. Hosting checklist (founder)

1. [ ] Push/merge this branch so `render.yaml` + migrations 0015/0016 are on GitHub.
2. [ ] https://render.com/deploy?repo=https://github.com/Teamthy/Meterly
3. [ ] Paste `DATABASE_URL` (Supabase **session** pooler `:5432`, `sslmode=require`).
4. [ ] Paste `TOKEN_ENC_KEY` and `METER_HMAC_KEY` (64 hex chars each; already generated offline — **not in git**).
5. [ ] Confirm `GET /healthz` and `GET /readyz`.
6. [ ] Vercel project `meterly`: `METERLY_API_ORIGIN=https://<render-host>` and redeploy.
7. [ ] Later, with sandbox keys: `MODE=staging`, `PAYMENT_PROVIDER=paystack`, `VENDING_PROVIDER=vtpass`, `REMOTE_PROVIDER=none`, `METER_READING_PROVIDER=none`. Keep SMS/email/WhatsApp `none` or `mock` until those keys exist.

---

## 11. Session log (newest first)

- **2026-11-23: VTpass + FR-008 households + FR-007 token send** (`318b81c` on `arena/7cc28241-meterly`).
  - VTpass: verify/pay/requery, documented DisCo map, 000/099 mapping, Lagos `request_id` persisted at claim (`VendIdempotencyKey`), `vtpasstest` fake. Wired in `app.go` / config. **Not called against live VTpass.**
  - Households: `migrations/0015_households.sql`, domain `internal/households`, memstore+pgstore, orchestrator, HTTP, registry grant **or** household perm, admin attest, `FEATURE_HOUSEHOLDS`. Tests: name match, never-MANAGE, HTTP ownership → invite → accept → purchase → revoke. OpenAPI paths added.
  - Token send: `migrations/0016_token_share_send.sql`, share dest ciphertext bound to `token-share-dest:<id>`, `RecipientKind=TOKEN_SHARE`, worker send (self vs gift template), opt-out, 5/day cap, `FEATURE_GIFT_SMS` for third-party. E2E against AT/Resend/WhatsApp fakes; logs must not contain token or E.164.
  - Verified: `go test -modfile=/tmp/meterly-tools/overlay.mod ./...` (memory). PG skipped (no `DATABASE_URL` in sandbox).
  - Hosting still waiting on Render paste. AMI still `none`.

- **2026-11-26 (prior): Support chat + Render/Supabase docs** — merged PR #13 @ `90e0078`.

- **Older milestones (M1–M12 web):** see git history PRs #1–#12. Do not treat “Copy metadata / gift contact / held candidates” session notes below as current — M5 notifications and FR-007 send superseded them.

<details>
<summary>Archived session notes (pre-M11)</summary>

Older entries (M5–M10, receipts, copy metadata, gift contact, held candidates) remain in git history of this file before this rewrite. They describe work that is already merged.

</details>
