# Meterly master build prompt: from M5 to five distinctive features

> **Who this is for:** a new engineer (human or AI coding agent) taking over the Meterly backend. Read all of it before writing code. It is self-contained: it explains the product, what exists, the rules you must not break, and exactly what to build next, milestone by milestone, with acceptance criteria.
>
> **Baseline:** `main` at commit `68acdab` (merge of PR #4), 2026-11-23. If `main` has moved, read `docs/HANDOFF.md` to see what changed and adjust. The handoff is the live state; this prompt is the plan.

---

## Part 1: The product you are building

### 1.1 The problem

In Nigeria most homes use **prepaid electricity meters**. To recharge, a customer buys a 20-digit **STS token** (from a bank app, a vendor, or the DisCo's app) and then types it into the meter's keypad. That keypad is often on a pole outside, or on a "CIU" unit indoors. The pain points:

- the meter is hard to reach (on a pole, up a ladder, in a locked compound);
- the person paying is often not the person at the meter: a child in Lagos paying for parents in Ibadan, someone abroad paying for family, a landlord paying for tenants;
- tokens get lost in SMS threads, are sometimes rejected, and nobody can tell whether a purchase actually reached the meter;
- several tenants often **share one meter** and argue over who pays.

### 1.2 What Meterly does

Meterly lets anyone buy electricity **for any meter, from anywhere**:

1. **Remote delivery.** For compatible smart meters (through a DisCo, Meter Asset Provider or manufacturer AMI/HES system), the credit goes straight into the meter. No token, no keypad.
2. **Token fallback.** For every other meter, Meterly vends a normal STS token and delivers it securely.
3. **Honest status tracking.** Payment, vending and delivery are tracked separately and truthfully. "Accepted by the meter system" is never shown as "delivered".

The consumer app is the distribution channel. The long-term asset is the **transaction and integration infrastructure**: payments, vending partners, meter systems, a ledger, reconciliation, and APIs for landlords, estates and fintechs.

### 1.3 The users

- **Consumers:** they buy for their own meter, for family, as gifts.
- **Tenants who share a meter.**
- **Landlords, estates and property managers:** many meters, many tenants.
- **Operators (Meterly staff):** investigate stuck transactions, refund, support customers.
- **Partners (later):** fintechs consuming the API.

### 1.4 Non-negotiable product truths

- Never claim credit reached a meter without evidence from the meter system.
- Never refund automatically if electricity *was or may have been* vended.
- A token only works on the meter it was bought for; "sharing" a token never moves credit between meters, and the UI and copy must never say it does.
- Meterly must **not** become an unlicensed wallet or stored-value product. Money flows through a CBN-licensed PSP (Paystack). Holding customer funds is a legal question; features that pool money are behind a flag until the founder confirms the legal position (see F2).

---

## Part 2: What exists today (do not rebuild it)

### 2.1 Milestones done

| Milestone | Status |
|---|---|
| Foundation: transaction engine, mock providers, `/v1` API, schema | ✅ merged (PR #1) |
| M1: PostgreSQL persistence (pgx v5), `InTx` unit of work, migration runner | ✅ merged (PR #2) |
| M2: `cmd/api` / `cmd/worker` / `cmd/migrate`, validated config, `Resume` recovery worker, outbox publisher | ✅ merged (PR #2) |
| M3: Paystack payment adapter (initialize, verify, refund, HMAC webhooks) | ✅ merged (PR #3) |
| M4: OIDC auth (stdlib JWT/JWKS, MFA-gated staff roles), `/v1/admin/*`, audit log, rate limits | ✅ merged (PR #3) |
| M5 (partial): receipts, copy records, encrypted gift contacts, templates, mock senders, held notification candidates, meter-access hardening | ✅ merged (PR #4); **sending is not built** |
| M6: OpenAPI, metrics/tracing, Docker images | ⏳ not started |

### 2.2 Code map

```
cmd/api        HTTP API (+ background loops in-process when RUN_WORKER=true; default in dev)
cmd/worker     resume sweep + outbox publisher (refuses to run with in-process mock providers)
cmd/migrate    applies embedded SQL migrations (transactional, checksummed, advisory-locked)

internal/orchestrator      THE CORE. Purchase workflow, Resume (single recovery path), operator actions (admin.go), copy records (shares.go)
internal/store             Store/Repos interfaces; InTx = one DB transaction
  memstore                 in-memory implementation with undo-log rollback (tests, dev)
  pgstore                  PostgreSQL implementation; pgtest gives each test its own DB
internal/registry          meter validation via the vending provider, capabilities, access grants (now PURCHASE-only on registration)
internal/providers         contracts: PaymentProvider, VendingProvider, RemoteCreditProvider
  mock                     deterministic mocks; behaviour keyed by meter number (README table)
  paystack (+paystacktest) real Paystack adapter + stateful fake server for tests
internal/transactions      domain: 3-dimension state machine, Quote
internal/meters ledger events idempotency money secure audit   domain packages
internal/auth              DevAuth (dev only) + OIDC verifier; roles customer/operator/admin
internal/ratelimit         in-memory token buckets (per process)
internal/httpapi           /v1 REST, /v1/admin (admin.go), receipts (receipt.go), copy records (shares.go)
internal/outbox            at-least-once publisher (sink currently logs metadata)
internal/notifications     templates (TemplateFor) + Sender interfaces + process-local mock senders. NOT wired.
internal/notificationintents / notificationconsumer   held "candidates" (metadata only; never a send queue)
internal/gifts             encrypted gift-recipient phone (E.164) + purchaser's notification request (NOT consent)
internal/receipts          token-free receipt model
internal/tokenshares       COPY records (metadata only)
internal/platform/{config,migrate}
migrations/0001…0005       schema (immutable once applied; add new files only)
docs/HANDOFF.md            live state + session log (READ FIRST)
docs/DECISIONS.md          why things are the way they are
docs/ROADMAP.md            spec ↔ code map
CLAUDE.md                  session protocol
```

### 2.3 How a purchase flows

```
POST /v1/electricity/quotes                    → priced, single-use, 15-min quote
POST /v1/electricity/transactions              → Idempotency-Key required; email required
     (one DB commit: idempotency reservation + quote consumed + meter locked + grant re-checked + transaction + outbox)
EnsurePaymentInitialised → Paystack checkout URL (new reference per attempt)
Paystack webhook (HMAC-verified) or the resume sweeper
     → checkPayment: ALWAYS re-verify with Paystack; amount must equal the quote total exactly
vend (claimed step) → token encrypted at rest; ledger posting
deliver: TOKEN → confirmed in the same commit | REMOTE → send → ACCEPTED → meter CONFIRMED
failure paths: timeout = UNKNOWN → Resume queries the provider by idempotency key;
               definitive vend failure → refund; mismatch/late payment/stuck → MANUAL_REVIEW
```

### 2.4 Existing API

Customer endpoints (OIDC bearer; dev: the bearer token is the subject):

```
POST   /v1/meters/validate                 POST /v1/meters (grants PURCHASE)
GET    /v1/meters                          GET  /v1/meters/{id}    DELETE /v1/meters/{id}
GET    /v1/meters/{id}/capabilities        POST /v1/beneficiaries
POST   /v1/electricity/quotes              POST /v1/electricity/transactions
GET    /v1/electricity/transactions[/{id}[/status]]
POST   /v1/electricity/transactions/{id}/payment-check
GET    /v1/electricity/transactions/{id}/token          (audited read)
POST|GET /v1/electricity/transactions/{id}/token/shares  (COPY records)
GET    /v1/electricity/transactions/{id}/receipt         (JSON or HTML)
POST   /v1/payments/webhooks/{provider}
```

Admin endpoints (operator/admin; every mutation needs a `reason`; all audited):

```
GET /v1/admin/transactions   GET /v1/admin/transactions/{id}
POST …/{id}/reconcile | fallback-to-token | token-reveal          (operator)
POST …/{id}/requeue | refund                                      (admin)
GET /v1/admin/providers   POST /v1/admin/providers/{name}/enable|disable (admin)
```

Dev-only helpers: `/v1/dev/payments/{ref}/complete`, `/v1/dev/remote/settle`, `/v1/dev/reconcile`, `/v1/dev/transactions/{id}/resume`.

### 2.5 Still simulated, or blocked on the founder

- **Vending and AMI/HES are mocks.** No real vending partner or meter-system partner is signed yet. **Do not invent their APIs.** Build interfaces and mocks, leave a TODO, and ask.
- **Paystack** has only been tested against `paystacktest`, never the live sandbox (no key yet).
- **OIDC** has only been tested against an in-process JWKS (no identity provider chosen yet).
- The handoff mentions **Supabase**: the founder may host PostgreSQL there. Migrations 0004/0005 have not been applied to it. Never touch a shared or production database from a dev session.

---

## Part 3: Rules you must follow

### 3.1 The seven invariants (every change must keep them, and tests must prove it)

1. **No vending until payment is verified server-side** and the amount equals the quote total exactly. A mismatch goes to MANUAL_REVIEW.
2. **A timeout or ambiguous error means UNKNOWN, never FAILED.** Resolve it by status query; re-send only when the provider confirms it has no record.
3. **Remote ACCEPTED is not delivered.** Only meter confirmation completes a remote delivery.
4. **Refund only after a definitive vend failure** (or an operator refund where nothing was or may have been vended).
5. **Every money movement is a balanced, idempotent ledger posting.** PostgreSQL triggers enforce balance and append-only.
6. **No raw tokens, meter numbers, phone numbers or emails** in logs, URLs, analytics, outbox payloads, audit metadata or plaintext columns. Encrypt with `secure.Box` (use `EncryptBound` with a purpose/resource context for new data); look up by HMAC; display masked.
7. **Duplicates and concurrency never double-charge, double-vend, double-credit, double-refund or double-send.**

### 3.2 Engineering patterns already in the code (reuse them, don't reinvent)

- **`orchestrator.mutate(ctx, id, fn)`**: lock the row (`GetForUpdate`), apply guarded changes, then save the state, history, ledger postings, outbox events and audit rows **in one DB transaction**. Guards return `errSkip` when someone else already moved the state.
- **Claim before calling a provider.** Only the worker whose commit claimed a step (moved it to `PENDING`) may make the external call. Use an explicit `claimed` flag set inside the mutate function. **Never infer the claim from the resulting state**: that exact bug (10 webhooks → 10 vend calls) was only caught on PostgreSQL.
- **Provider calls never run inside a DB transaction.**
- **Idempotency.** Externally triggered creates take an `Idempotency-Key`; reservation, work and completion commit together. Provider calls use deterministic keys (`vend:<id>`, `credit:<id>`, `refund:<id>`).
- **`Resume`** is the single recovery path: anything left half-done after a crash is continued from its last committed state by the worker (`ResumeDue`, with backoff and escalation to review).
- **Every operator or admin action** needs a reason and writes an `audit_events` row in the same transaction.
- **New money flows need ledger accounts and postings designed up front.** Write the postings table in `docs/DECISIONS.md` before coding.

### 3.3 Workflow rules

- **Start of each session:** read `docs/HANDOFF.md`, then check it against `git log` and open PRs.
- **End of each session:** update `docs/HANDOFF.md` (state, next steps, gotchas, one log entry at the top), record design choices in `docs/DECISIONS.md`, commit and push.
- **One milestone per PR**, with a description following the existing PRs (Summary / Not in this PR / Test plan). Don't open PRs unless asked if you are an agent.
- **Git:** never force-push. When `main` moves, `git merge origin/main` into your branch.
- **Migrations:** add new numbered files only (`0006_…`). Never edit an applied file; the runner rejects checksum changes.
- **Tests:** `go test -race ./...` **with `DATABASE_URL` set**, so every scenario runs on both the in-memory store and PostgreSQL. The PostgreSQL tests skip silently without it, and CI fails if they were skipped. Any new test package using `pgtest` needs a `TestMain` calling `pgtest.DropTemplate()`. Use the injectable clock (`o.Now`, `e.later`) for time-based logic.
- **External APIs:** implement adapters only from official documentation you have read (and cite the doc URL in the package comment). Every adapter ships with a stateful fake (like `paystacktest`) with fault injection: "applied but response lost", 5xx, timeouts. No live keys in tests or in the repo.
- **Feature flags:** every new customer-facing feature has a config flag (`FEATURE_<NAME>`), default **off** outside `MODE=dev`, validated in `internal/platform/config`.
- **No model identifiers** in commits, PRs or code.

---

## Part 4: The build plan

The plan has eight milestones: finish M5, then M6, then five features. Do them in this order, because later ones depend on earlier ones.

| # | Milestone | Depends on |
|---|---|---|
| M5 | Notification delivery (finish M5) | — |
| M6 | Production readiness: OpenAPI, observability, containers | — |
| M7 | **F1: Pay-for-Me links** | M5 |
| M8 | **F2: Split the Meter** (built on a reusable *Funding Pot*) | M5, M7 patterns |
| M9 | **F3: AutoPower** (scheduled auto-recharge with spend guard) | M5 |
| M10 | **F4: Landlord Power Desk** | M8 (pots), M5 |
| M11 | **F5: Meterly Offline** (USSD + WhatsApp purchase channels) | M5, M7 |

Each milestone below has: **Goal → User stories → Design → Data model → API → Edge cases → Security and privacy → Tests → Definition of done**.

---

### M5: Notification delivery (finish M5)

**Goal.** Customers get timely, truthful, privacy-safe messages (SMS, email, WhatsApp), delivered exactly once in effect, with consent and opt-out respected. Gift recipients can receive their token by SMS, **only if** the founder enables it after legal review.

**What already exists:**
- `notifications.TemplateFor` (safe templates);
- `Sender` interfaces and process-local mocks;
- held `notification_candidates` (metadata only, opt-in via `NOTIFICATION_CANDIDATES_SINCE`);
- encrypted gift contact plus the purchaser's `notification_requested` flag.

Respect the existing design notes: **candidates are not a send queue; a purchaser's request is not recipient consent.**

**User stories:**
- As a buyer, I get "payment confirmed", "token ready" (with a prompt to open the app, never the token itself over email or push), "remote delivery confirmed / failed / unknown" and "refund completed".
- As a buyer of a gift, if gift SMS is enabled, the recipient gets an SMS with the token, the masked meter number and my display name, and nothing else about me.
- As anyone, I can reply STOP (SMS/WhatsApp) or use a settings endpoint to opt out of non-essential messages.
- As an operator, I can see delivery attempts per transaction (channel, status, timestamps; never content or destination).

**Design:**

1. **Contact capture.** When OIDC claims include `email` + `email_verified=true` or `phone_number` + `phone_number_verified=true`, store them on `users`: encrypted (bound to the user ID), with an HMAC for lookup, and masked for display. Unverified contacts are never used.
2. **Notification preferences and consent.** Table `notification_preferences` (user, channel, enabled, updated_at). A global `opt_outs` table keyed by HMAC of the destination (so STOP works for non-users such as gift recipients). Transactional messages to the account holder are allowed by default. Gift-recipient SMS needs all three of:
   - `FEATURE_GIFT_SMS=true`;
   - a purchaser request;
   - the destination not being opted out.
3. **Delivery jobs.** A new consumer turns allowlisted outbox events into `notification_deliveries` rows: `id, event_id, transaction_id, recipient_kind (PURCHASER|GIFT_RECIPIENT), channel, destination_hmac, template_id, template_version, status (PENDING|SENDING|SENT|FAILED|UNKNOWN|SUPPRESSED), attempts, next_attempt_at, provider_ref, last_error_code, created_at, sent_at`. Add a unique constraint on `(event_id, recipient_kind, channel)`; that is the dedupe. **Never store the rendered body or the plaintext destination.** Resolve the destination at send time from the encrypted source.
4. **Sending.** Use the same claim pattern as vending: claim (`PENDING→SENDING` in a commit), send outside the transaction, record the result.
   - A timeout after sending means `UNKNOWN`. Query the provider's message-status API if it has one; otherwise mark it `UNKNOWN` and **do not auto-resend** token-bearing messages (a duplicate token SMS is harmless for STS but confusing). Non-token messages may be retried once.
   - Backoff: 30s, 2m, 10m, 1h; at most 5 attempts.
5. **Token-bearing SMS.** The token is decrypted only in the sender process memory, at send time. The template's version is recorded. The message is never logged. Format: `Meterly: ₦5,000 electricity for meter ****5678. Token: 1234-5678-9012-3456-7890. Sent by Ada. Reply STOP to opt out.`
6. **Providers.** Build adapters from official docs only, behind the interfaces:
   - SMS: Termii or Africa's Talking (pick one; ask the founder);
   - email: Postmark, SES or Resend;
   - WhatsApp Cloud API: needs pre-approved templates, and outside the 24-hour customer-service window only templates may be sent.

   Each adapter gets a stateful fake. Inbound STOP webhooks must have their signatures verified.
7. **The outbox sink** becomes a dispatcher that calls the registered consumers: the notification consumer plus the logger. Delivery stays at-least-once; consumers dedupe on event ID.

**API:**
```
GET  /v1/me/notification-preferences
PUT  /v1/me/notification-preferences          {"sms":true,"email":true,"whatsapp":false}
POST /v1/notifications/inbound/{provider}     (STOP handling; signature-verified; public)
GET  /v1/admin/transactions/{id}              (add a "notifications" array: channel, status, attempts, times)
```

**Edge cases:**
- an event replayed after a crash;
- two workers racing to send;
- the provider accepted the message but our response was lost;
- the destination opted out after the job was created (check at send time, then `SUPPRESSED`);
- an invalid phone number;
- a gift to the buyer's own number;
- the token not yet vended when "payment confirmed" fires (that message must not say "token ready").

**Security and privacy:**
- NDPA 2023: purpose limitation and data minimisation.
- Retention: delivery rows keep no content; destination HMACs are kept 12 months (configurable), then purged by a job.
- Audit operator views of delivery history.

**Tests (both stores):**
- the dedupe under 10 concurrent consumers;
- a lost send response gives `UNKNOWN` and no resend of a token SMS;
- opt-out at send time gives `SUPPRESSED`;
- gift SMS sent only when all three conditions hold;
- no token, phone or email in logs, DB columns or outbox (scan like the existing e2e test);
- a STOP webhook with a bad signature gets 401;
- templates render in English (add a structure for i18n; Yoruba, Igbo, Hausa and Pidgin come later).

**Definition of done:**
- Purchases on dev mocks produce correct delivery rows, and mock senders receive messages.
- The fake-provider adapters pass their contract tests.
- `FEATURE_GIFT_SMS` defaults to off outside dev.
- `HANDOFF.md` and `DECISIONS.md` are updated.

---

### M6: Production readiness

**Goal.** The service can be deployed, observed and integrated by partners.

1. **OpenAPI 3.1** at `api/openapi.yaml` describing every route, schema, error code (`{"error":{"code","message","request_id","details"}}`) and security scheme. Add a Go test that walks the mux's registered patterns and fails when one is missing from the spec, or when the spec lists one that doesn't exist. Adding `gopkg.in/yaml.v3` is acceptable; justify it in DECISIONS.
2. **Metrics** (Prometheus) on a **separate internal port** (`METRICS_ADDR`, default `:9090`):
   - `http_requests_total{route,code}` and `http_request_duration_seconds{route}`;
   - `provider_calls_total{provider,op,outcome=ok|ambiguous|definitive}` and `provider_call_duration_seconds`;
   - `transactions_by_status` (gauge, refreshed every minute);
   - `manual_review_open`, `resume_escalations_total`, `outbox_lag_seconds`;
   - `notification_deliveries_total{channel,status}`.
   - Never use a meter number, user ID or transaction ID as a label.
3. **Tracing:** OpenTelemetry. One span per request and per provider call, with the correlation ID as an attribute. Export over OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set; no-op otherwise.
4. **Containers:** multi-stage `Dockerfile`, distroless static, non-root, for `api`, `worker` and `migrate`; `docker-compose.yml` extended to run the full stack locally.
5. **Graceful shutdown:** drain HTTP, stop the worker loops, finish in-flight claims. This already exists partially; verify it with a test.

**Definition of done:**
- The route-coverage test passes.
- `/metrics` is served on the internal port only.
- The images build in CI.
- A compose-up smoke test completes a purchase on mocks.

---

### M7: F1, Pay-for-Me links ("request electricity from anyone")

**Why it's distinctive.** Students, tenants and parents back home often need someone *else* to pay: a sibling in the UK, a friend, an employer. Today they forward a meter number on WhatsApp and hope the payer types it right. With Pay-for-Me, the person at the meter creates a link; anyone can open it and pay with no account, and the electricity arrives at the right meter.

**User stories:**
- As a requester, I pick one of my meters, optionally fix an amount (or allow "₦2,000–₦20,000, payer chooses"), add a note ("Light don go, abeg 🙏"), and get a link `https://meterly.ng/p/<code>` plus a QR code to share on WhatsApp.
- As a payer (no account), I open the link and see the requester's display name, the masked meter (`****5678`), the DisCo, the amount and the note, but **never** the address or the customer name from validation. I enter my email, pay through Paystack, and get a receipt.
- As the requester, I'm notified when someone pays; the token (or remote delivery) is mine, exactly as if I had bought it.
- As a requester, I can cancel an open link. Links expire (default 72 hours, max 7 days) and are single-use by default (optionally "allow multiple payments up to N").

**Design:**
- New package `internal/payrequests`. Entity `PaymentRequest{ID, RequesterID, MeterID, AmountMode (FIXED|RANGE), AmountMinor | Min/Max, NoteEncrypted, Status (OPEN|PAID|EXPIRED|CANCELLED), MaxPayments, PaymentsCount, CodeHMAC, ExpiresAt}`.
- **The code** is 128 bits of randomness, base32-encoded. Store only its HMAC; the URL contains the code. Lookups are constant-time through the HMAC.
- **Paying creates an ordinary electricity transaction**, so the entire existing engine (verify, vend, deliver, refund, resume) is reused unchanged:
  - **customer** = the requester (they own the outcome and see the token);
  - **payer** = a guest, recorded on a new `transaction_payers` row (encrypted email, payer name). The payer gets the receipt (token-free) and status page access through an unguessable status code.
- Quote and transaction creation happen when the payer clicks "Pay". Use a public endpoint with an `Idempotency-Key` generated by the page, rate-limited per IP and per link.
- **Concurrency:** two payers on a single-use link. Lock the request row inside the create transaction; the second payer gets `409 LINK_ALREADY_PAID`. If the first payer abandons checkout, the link reopens when that transaction's payment fails or expires (hook it into the payment-failed path in the same `mutate`).
- The requester must still hold PURCHASE on the meter at pay time; re-check it under the meter lock, the same way normal creation already does.

**Data model (migration `0006_payment_requests.sql`):** `payment_requests`, `transaction_payers`, and an index on `code_hmac`.

**API:**
```
POST   /v1/payment-requests                 (auth)  → {id, url, qr_png_base64, expires_at}
GET    /v1/payment-requests                 (auth)  list mine
DELETE /v1/payment-requests/{id}            (auth)  cancel
GET    /v1/public/pay/{code}                (public, rate-limited) → display data (masked)
POST   /v1/public/pay/{code}/checkout       (public, Idempotency-Key) {amount_minor?, email, payer_name?} → {checkout_url, status_code}
GET    /v1/public/pay/status/{status_code}  (public) → payer-safe status + receipt (no token)
```

**Edge cases:**
- the link expires mid-checkout (honour the checkout already created; the expiry applies to *new* checkouts);
- the requester removes the meter after creating the link (the link becomes unusable, with a clear error);
- an amount outside the range;
- the payer is the requester;
- link enumeration attempts (rate limit plus a uniform 404).

**Security:**
- The note is encrypted and limited to 140 characters, with no URLs allowed (anti-phishing).
- Public pages carry `Cache-Control: no-store` and `Referrer-Policy: no-referrer`.
- Audit link creation and cancellation.

**Tests:**
- full flow on both stores;
- single-use race (two payers): one wins, the other gets 409;
- the link reopens after an abandoned payment expires;
- range validation;
- the payer never sees the token or the address;
- a removed grant blocks payment;
- logs are free of codes and emails.

**Definition of done:**
- End-to-end on mocks and on `paystacktest`.
- Behind `FEATURE_PAY_FOR_ME`.

---

### M8: F2, Split the Meter (built on a reusable Funding Pot)

**Why it's distinctive.** In Nigerian compounds and shared flats, 3–10 households often share **one prepaid meter**, and "who paid last?" is a constant fight. Split the Meter lets a group fund one purchase together. Electricity is bought only when the target is reached; if it isn't, everyone is refunded automatically.

> ⚠️ **Legal gate.** Collecting money from several people and holding it until a target is reached may count as holding customer funds. **Before production, the founder must confirm with Paystack and counsel** whether this is allowed as is, or needs a split-payment or escrow product. Build it behind `FEATURE_SPLIT_METER` (default off outside dev), with a short maximum hold (`POT_MAX_HOLD`, default 72h), and record the open question in DECISIONS.

**User stories:**
- As an organiser, I create a pot for a meter I hold PURCHASE on: target ₦20,000, deadline Friday 6pm, an optional "equal shares among 4" suggestion, and a share link.
- As a contributor (account optional, via the same public-link pattern as M7), I pay my share. I see progress ("₦15,000 of ₦20,000, 3 of 4 paid") with names shown as initials only.
- When the target is reached, Meterly buys the electricity automatically; the token goes to the organiser (and, optionally, by SMS to the contributors who opted in, gated by M5 consent).
- If the deadline passes unfunded, or the organiser cancels, **every contribution is refunded** to the original payment method, automatically and exactly once.
- Overfunding (two people pay the last share at once) refunds the excess to the last contributor(s), automatically.

**Design: the Funding Pot (a reusable engine, also used by M10):**
- `internal/pots`.
  - `Pot{ID, Kind (SPLIT_METER|LANDLORD_BULK), OwnerID, TargetMinor, Status, Deadline}`.
  - `Contribution{ID, PotID, Payer (user or guest), AmountMinor, PaymentProviderRef, Status (PENDING|PAID|REFUND_PENDING|REFUNDED|FAILED), RefundAmountMinor}`.
  - `Allocation{ID, PotID, TransactionID, AmountMinor}`.
- **Pot states:** `OPEN → FUNDED → ALLOCATING → SETTLED`, or `OPEN → EXPIRED|CANCELLED → REFUNDING → REFUNDED`.
- **Contributions** reuse the payment adapter: each contribution is a Paystack checkout verified server-side, with the same "amount must equal exactly" rule.
- **A new funding source for electricity transactions.** Add `FundingSource (PSP|POT)` to `transactions.Transaction`. A POT-funded transaction is created with payment already `CONFIRMED`, inside the same DB transaction that records the allocation. It has no PSP reference. Its vend-failure refund returns the money **to the pot**, and the pot then refunds contributors. **Every refund path in the orchestrator must branch on `FundingSource`, and tests must cover both.**
- **Ledger.** Write this table into DECISIONS before coding:
  - New liability account `POT_FUNDS_HELD`.
  - Contribution verified: Dr `PSP_SETTLEMENT_RECEIVABLE` / Cr `POT_FUNDS_HELD`.
  - Allocation to a transaction: Dr `POT_FUNDS_HELD` / Cr `CUSTOMER_FUNDS_CLEARING`.
  - The existing vend posting is unchanged.
  - A vend failure on a POT transaction: Dr `CUSTOMER_FUNDS_CLEARING` / Cr `POT_FUNDS_HELD`.
  - A contribution refund: Dr `POT_FUNDS_HELD` / Cr `REFUND_PAYABLE`, then the existing refund-paid posting.
  - The trial balance must stay zero, and `POT_FUNDS_HELD` must net to zero for every settled pot.
- **Fees:** the platform fee is charged once, on allocation, not per contribution. Each contributor pays exactly their amount, so the target must include the fee, and the UI shows "includes ₦100 fee".
- **The worker.** Add a pot sweep to `Resume`: expire overdue pots, retry refunds that are `REFUND_PENDING`, finish interrupted allocations. Claim each refund with the same `claimed` pattern, with idempotency key `refund:contribution:<id>`.

**API:**
```
POST /v1/pots                      (auth) {kind:"SPLIT_METER", meter_id, target_minor, deadline, suggested_shares?} → {id, url}
GET  /v1/pots/{id}                 (auth: owner) full detail
POST /v1/pots/{id}/cancel          (auth: owner)
GET  /v1/public/pots/{code}        (public) progress (initials only)
POST /v1/public/pots/{code}/contribute   (public, Idempotency-Key) {amount_minor, email, name?} → {checkout_url}
```

**Edge cases:**
- a contribution paid after the pot expired (verify, then refund immediately);
- the organiser loses PURCHASE on the meter (cancel and refund);
- a vend failure after funding (money back to the pot, then refunds);
- a vend that is UNKNOWN (wait; **never refund contributors while a vend may have happened**);
- a refund call that fails repeatedly (escalate to review);
- the deadline in the past;
- a target outside the min/max limits.

**Tests:**
- the full happy path;
- expiry with 3 contributions, giving exactly 3 refunds once each (with a crash injected mid-refund);
- an overfund race;
- a vend failure leading to contributor refunds;
- an UNKNOWN vend blocking refunds;
- ledger balance and `POT_FUNDS_HELD` netting to zero;
- both stores, with the race detector.

**Definition of done:** as M7, plus the legal note recorded and the flag defaulting to off.

---

### M9: F3, AutoPower (scheduled auto-recharge with spend guard)

**Why it's distinctive.** People forget to recharge until the lights go off at night. AutoPower recharges on a schedule ("₦10,000 every 1st and 15th at 7am"). Once remote meter readings exist (capability-gated), it can also recharge when credit runs low. Strict spending limits make it safe to leave on.

**User stories:**
- As a user, after one successful card payment, I can save that card for AutoPower (explicit consent checkbox; consent text and version stored).
- I create a rule: meter, amount, schedule (weekly or monthly days, time in Africa/Lagos), a monthly cap (for example ₦40,000), and an optional end date.
- I get a heads-up 12 hours before each charge ("We'll recharge ₦10,000 tomorrow 7am. Skip?"), with a one-tap skip.
- If a charge fails (card declined), I'm notified. After 3 consecutive failures the rule pauses itself.
- **Low-balance mode** is available only for meters where `remote_reading` is supported, authorised **and** confirmed: "keep at least 20 kWh; top up ₦5,000". Hide it otherwise. Since no AMI partner exists yet, build the reading interface plus a mock only.

**Design:**
- **Saved payment methods.** Paystack returns an `authorization` object on verify. When `reusable=true` and the user consented, store `authorization_code` **encrypted** (bound to the user ID), plus a display brand/last4/expiry.
- Extend `PaymentProvider` with `ChargeSaved(ctx, ChargeRequest{AuthorizationRef, Email, Amount, Reference}) (PaymentVerification, error)`, implemented with Paystack's documented `POST /transaction/charge_authorization`. The result is still treated as unverified until `VerifyPayment` confirms it (invariant 1). Charges can come back `pending` or `send_otp`; anything other than `success` leaves the payment pending, and the normal sweeper resolves it.
- **Rules and runs.** `autopower_rules` and `autopower_runs`, with a unique `(rule_id, scheduled_for)`; that is the idempotency, so one slot is charged at most once ever.
  - The scheduler loop (in the worker) materialises the next run and sends the heads-up.
  - At the scheduled time it creates a normal electricity transaction (quote + create, with idempotency key `autopower:<run_id>`) and charges the saved card.
  - From there, the existing engine takes over.
- **Spend guard.** Before charging, under a lock on the rule: the sum of this calendar month's successful plus in-flight runs, plus this amount, must not exceed the cap. Otherwise the run is `SKIPPED_CAP` and the user is notified. Also enforce a global per-user daily maximum (config).
- **Low-balance trigger.** Add a `MeterReadingProvider` interface: `LatestReading(ctx, meter) (kWh, at, err)`. A poller reads every N hours for eligible rules only, and triggers at most once per 24h per rule.
- **Time zones.** All schedules are evaluated in Africa/Lagos and stored as UTC instants.

**API:**
```
GET/DELETE /v1/me/payment-methods[/{id}]
POST   /v1/me/payment-methods/{transaction_id}/save   {consent:true}  (only after a successful card payment you made)
POST   /v1/autopower/rules        GET /v1/autopower/rules    PATCH/DELETE /v1/autopower/rules/{id}
POST   /v1/autopower/runs/{id}/skip
```

**Edge cases:**
- the worker is down at the scheduled time (catch up within a 6h window, otherwise mark the run `MISSED` and notify; **never** charge twice to catch up);
- the card expires;
- the meter is removed;
- the user's grant is revoked;
- the tariff changes;
- daylight saving (Nigeria has none, but code it properly anyway);
- duplicate scheduler replicas (the unique constraint plus `SKIP LOCKED` handle it).

**Tests:**
- the scheduler with the fake clock across month boundaries;
- cap enforcement under concurrent runs;
- a lost `charge_authorization` response, giving no double charge (verify by reference);
- 3 failures pausing the rule;
- low-balance mode refused for ineligible meters;
- `paystacktest` extended with `charge_authorization`.

**Definition of done:** behind `FEATURE_AUTOPOWER`; consent stored and audited; deleting a payment method stops every rule that uses it.

---

### M10: F4, Landlord Power Desk

**Why it's distinctive.** Landlords and estate managers juggle dozens of meters, top up for vacant units, and chase tenants for reimbursement. Power Desk gives them a property → unit → meter view, a one-payment bulk top-up across many units, and per-unit statements. Remote meters get credit without anyone visiting.

**User stories:**
- As a landlord, I create an organisation, invite a manager (role MANAGER) and an accountant (role VIEWER), and add properties and units.
- I attach meters to units. Because registration grants PURCHASE only, **attaching a meter for management requires verified ownership**, using one of two methods:
  - (a) the DisCo account name from validation matches the organisation's verified name (a manual operator review step in v1); or
  - (b) an operator approves uploaded evidence (out of scope for file storage in v1: record the evidence reference only).

  Until verified, the org can still *buy* for the meter, but can't see its history or delegate it.
- **Bulk top-up:** select 12 units, set an amount per unit (or a different amount each), pay **once**. Meterly buys 12 separate electricity transactions. Each one succeeds or fails independently and is shown independently. Any failure is refunded **to me** (through the pot) with a clear statement.
- Monthly statements per property and per unit: purchases, refunds, fees. Export as CSV.
- An optional approval rule: a MANAGER's bulk top-up over ₦X needs OWNER approval.

**Design:**
- Use the existing schema tables: `organisations`, `organisation_members`, `properties`, `buildings`, `units`. Add `unit_meters` (unit, meter, verified, verified_by, verified_at).
- Organisation-scoped RBAC: OWNER, MANAGER, VIEWER. Check it in a single helper (`authz.Require(ctx, orgID, role)`), and **test every endpoint for cross-organisation access** (no IDOR).
- **Bulk top-up = Funding Pot** with `Kind=LANDLORD_BULK`: one contribution (the landlord's single payment) and N allocations, one per unit. Allocation creates POT-funded transactions (M8). Unallocated or failed amounts are refunded to the landlord's original payment through the pot refund path. Partial success is normal and must be displayed honestly: "10 delivered, 1 failed and refunded, 1 pending".
- Statements are derived from **ledger postings and transactions**, never from UI arithmetic. Purchases are not consumption: label them "electricity purchased", never "used".

**API (sketch):**
```
POST /v1/orgs   GET /v1/orgs/{id}   POST /v1/orgs/{id}/members   PATCH/DELETE …/members/{uid}
POST /v1/orgs/{id}/properties   POST /v1/properties/{id}/units   POST /v1/units/{id}/meters
POST /v1/admin/unit-meters/{id}/verify   (operator, reason, audited)
POST /v1/orgs/{id}/bulk-topups   {items:[{unit_id, amount_minor}], approval_note?} → {pot_id, checkout_url | pending_approval}
POST /v1/orgs/{id}/bulk-topups/{pot_id}/approve   (OWNER)
GET  /v1/orgs/{id}/bulk-topups/{pot_id}           (per-item status)
GET  /v1/orgs/{id}/statements?property_id=&month=2026-11&format=csv
```

**Tests:**
- IDOR matrix: every endpoint called with another organisation's IDs returns 404;
- role matrix;
- a bulk top-up of 5 with 1 vend failure and 1 UNKNOWN: correct refunds, no refund while UNKNOWN, ledger balanced;
- the approval threshold;
- statement totals equal the ledger;
- both stores.

**Definition of done:** behind `FEATURE_POWER_DESK`; documented in the README; operator verification flow audited.

---

### M11: F5, Meterly Offline (USSD + WhatsApp purchase channels)

**Why it's distinctive.** Many Nigerians use feature phones or have poor data, and WhatsApp is where people already talk. Meterly Offline lets a user buy electricity by dialling a USSD code (`*xxx#`) or chatting on WhatsApp, without the app. Both channels drive the same purchase engine.

**User stories:**
- **USSD:** dial `*xxx#` → "1. Buy for my meter 2. Buy for saved meter 3. Last token 4. Help" → choose a meter → enter an amount → confirm → receive a **payment link by SMS** (v1), or a bank USSD code to dial (v2, using Paystack's documented USSD charge channel) → the token arrives by SMS once vended.
- **WhatsApp:** message "buy" → the bot lists saved meters (masked) → pick one → enter an amount → get a Paystack link → get the token in the chat (inside the 24h service window; otherwise as a template message).
- The phone number is the identity. First-time users link their phone to an account through an OTP sent by SMS. A verified phone from OIDC (M5) links automatically.
- "Last token" resends the most recent token for the caller's own purchases only, at most 3 times a day, and every resend is audited.

**Design:**
- `internal/conversation`: a channel-agnostic state machine (menus, steps, validation, timeouts) with per-session state in the DB (`conversation_sessions`, with a TTL; USSD sessions last about 180 seconds). Channel adapters translate between the engine and USSD text or WhatsApp messages.
- **USSD adapter:** build from the aggregator's official docs (Africa's Talking: the request carries `sessionId, serviceCode, phoneNumber, text`, and the reply starts with `CON …` to continue or `END …` to finish). Authenticate the aggregator by IP allowlist plus a shared secret. Responses must fit 182 characters.
- **WhatsApp Cloud API adapter:** verify webhooks with the `hub.verify_token` handshake and the `X-Hub-Signature-256` HMAC. Respect the 24-hour window and use templates outside it.
- **The phone number from the aggregator is trusted only after authentication.** Map it to `users` through the phone HMAC.
- Purchases call the **same orchestrator** (quote → create with `Idempotency-Key` = `ussd:<sessionId>:<step>` → checkout link). Nothing is bypassed.
- Rate limits per phone number; daily purchase caps for channel-initiated purchases until the phone is verified.

**Edge cases:**
- a USSD session times out mid-flow (no transaction created unless confirmed);
- a duplicate aggregator callback;
- a WhatsApp message arriving out of order;
- the user replies STOP (ties into M5 opt-outs);
- a SIM swap (require re-verification after N days of inactivity).

**Tests:**
- engine flows table-driven with the fake clock;
- adapter contract tests with fakes;
- duplicate callbacks are idempotent;
- signature failures get 401;
- the 182-character limit;
- token resend limits and audit.

**Definition of done:**
- Behind `FEATURE_USSD` and `FEATURE_WHATSAPP`.
- **Blocked for production on the founder:** a USSD shortcode (NCC and aggregator) and a WhatsApp Business Account with approved templates. Document both in HANDOFF.

---

## Part 5: Decisions the founder must make (ask; don't assume)

| Decision | Needed for | Default until decided |
|---|---|---|
| Vending partner/aggregator + API docs | any non-dev deployment | mocks only |
| AMI/HES partner (remote credit + readings) | remote delivery, AutoPower low-balance | mocks; low-balance hidden |
| Identity provider (Auth0, Clerk, Keycloak, Cognito) | live login | OIDC tested with an in-process JWKS |
| SMS / email / WhatsApp providers | M5 live sending | fakes |
| Is gift-recipient SMS lawful under NDPA, with STOP? | M5 gift SMS | `FEATURE_GIFT_SMS=false` |
| Can pooled contributions be held for up to 72h? | M8, M10 | flags off outside dev |
| USSD shortcode + aggregator | M11 | flag off |
| Fee policy (flat ₦100 today) for pots and AutoPower | M8–M10 | ₦100 per electricity transaction |
| Where PostgreSQL runs in production (Supabase?) and the migration process | deployment | never touch shared DBs from dev |

---

## Part 6: How to work, session by session

1. Read `docs/HANDOFF.md`, `docs/DECISIONS.md` and this prompt. Check the state against `git log` and the open PRs.
2. Start PostgreSQL locally (HANDOFF §5) and run the full suite with `DATABASE_URL` set. It must be green before you change anything.
3. Pick the next milestone. Write its ledger postings (if it moves money) and its state machine into DECISIONS **first**.
4. Build in small commits:
   1. domain types and transitions plus unit tests;
   2. store interfaces, then memstore, then pgstore, then a migration;
   3. orchestrator or service logic with the claim, mutate and audit patterns;
   4. HTTP handlers;
   5. end-to-end tests on both stores;
   6. config flag;
   7. docs.
5. Before finishing:
   - `gofmt`, `go vet ./...` and `go test -race -count=1 ./...` with `DATABASE_URL` set;
   - repeat the concurrency tests 3×;
   - check that no `meterly_*` test databases are left behind;
   - scan the logs in e2e tests for secrets.
6. Update HANDOFF (state table, gotchas, next steps, one session-log entry: what was done, how it was verified, **what's unverified**) and DECISIONS. Commit, push, and open or update one PR per milestone.

## Part 7: Definition of done for the whole plan

- All eight milestones merged, each behind its flag where marked.
- Every scenario in every milestone passes on **both** stores under the race detector, with crash-injection tests for every new external call (send, charge, refund, allocation).
- The seven invariants still hold. Every new money flow has a ledger table in DECISIONS and a test proving the trial balance is zero and every holding account nets to zero.
- No secrets, tokens, meter numbers, phone numbers or emails in logs, URLs, outbox, audit metadata or plaintext columns (enforced by tests).
- The OpenAPI spec covers every route (enforced by test). Metrics and tracing are wired.
- HANDOFF and DECISIONS are current. Every founder-blocked item is listed with its default.
