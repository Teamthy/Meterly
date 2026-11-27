# Decisions

Defaults chosen where the spec left room, or where a decision depends on a commercial or legal call. Each one can be revisited; most are configuration.

## Support chat (agent + E2E human)

- **Two modes, labelled honestly.** Agent messages are processed by Meterly so the agent can look up the caller's own purchases (never anyone else's) and never include STS tokens. Human messages are end-to-end encrypted: P-256 ECDH + HKDF-SHA256 + AES-GCM in the browser; the API stores ciphertext, nonce and wrapped conversation keys only. Private keys never leave the device (`localStorage`).
- **No key escrow.** Operators decrypt only if the customer wrapped the conversation key to their published staff public key. Switching browsers loses old human transcripts — that is the E2E tradeoff, and the UI says so.
- **SYSTEM notices are server-sealed, not E2E** (“you're now talking to a person”) so both sides can read the handshake without a key.
- **`FEATURE_SUPPORT` defaults to true.** This is not a money-pooling feature.
- **Hosted API may stay `MODE=dev` on Render** until VTpass + Paystack keys are pasted; config still refuses mocks outside dev. Set `VENDING_PROVIDER=vtpass` and `PAYMENT_PROVIDER=paystack` (with `REMOTE_PROVIDER=none`) to run `MODE=staging`.

## Persistence (M1)

- **pgx v5, hand-written SQL; no sqlc yet.** The repositories are small and the SQL is easier to review inline. Adopt sqlc if the query count grows.
- **Unit of work = `store.InTx`.** Each state transition commits together with its history rows, ledger posting and outbox events. Provider calls never run inside a DB transaction. Rows being changed are locked with `SELECT … FOR UPDATE`; `Save` also checks a `version` column, so an unlocked writer can't overwrite newer state.
- **Claims are explicit.** Only the worker whose commit moved a step to `PENDING` (vend, remote send, refund) or re-claimed it (reconcile re-send) may call the provider. A worker that lost the race sees `PENDING` too, but must not call. Running the suite on PostgreSQL found a bug here: inferring the claim from the resulting state let 10 concurrent webhooks call the vend provider 10 times (the provider deduplicated them).
- **Migration runner is our own** (`internal/platform/migrate`, about 150 lines) rather than golang-migrate or goose. It embeds the SQL, runs each file in its own transaction with its `schema_migrations` row, takes an advisory lock, and refuses to run if an applied file's checksum has changed. That is all we need, with no extra dependency.
- **`0001_init.sql` was edited once**, before it had been applied anywhere: its `BEGIN`/`COMMIT` were removed, because the runner provides the transaction and an inner `COMMIT` would have ended it early. From now on applied migrations are immutable; the runner enforces this.
- **Meter capabilities are stored as a JSONB snapshot on `meters`.** The per-provider `meter_capabilities` table stays reserved until more than one vending or AMI provider can serve the same meter.
- **One payment, vend, delivery and refund record per transaction** (unique indexes in `0002`). Multiple payment attempts per transaction would need a new design and migration.
- **Users are created on first sight** from the identity-provider subject (`users.external_id`), so foreign keys hold from day one. M4 replaces dev auth with OIDC; the mapping stays the same.
- **Provider and DisCo reference rows are upserted outside the caller's transaction** and cached, so a rollback can't leave a cached ID pointing at nothing.

## NDPA data map, consent and retention (B1)

- **Data map** lives in `docs/PRIVACY_ENGINEERING.md` and `internal/privacy.Columns()`. A test walks the migration chain and fails if a new column named like `email`/`phone`/`name`/`address`/`ip`/`msisdn` is not registered.
- **Transactional vs marketing.** `notification_preferences` remain the current transactional flags (SMS/email on, WhatsApp off by default). Marketing is opt-in (default withdrawn). `consents` is append-only history. Withdrawal of transactional consent also writes `opt_outs` for the user's destination HMAC so the next send is suppressed immediately.
- **Retention periods are engineering defaults pending legal sign-off** (HANDOFF §9). Financial rows are `ANONYMISE` with a 7-year default; the B1 job never deletes them. Operational DELETE: OTPs/challenges 7 days, sessions 90 days after expiry, completed idempotency keys 30 days, risk observations 90 days, closed support threads 2 years, notification destination HMACs 365 days. All overridable via `RETENTION_*` / `NOTIFICATION_HMAC_RETENTION`.
- **Legal holds** (`legal_holds`) skip that user's rows in every DELETE policy. Open disputes should be recorded as a hold by operators (no automatic hold yet).
- **Audit:** one `retention_runs` row and one `audit_events` row per job, counts only, no PII.
- **B4 partners** uses `0027_*.sql`; B1 took `0025_privacy.sql`; B2 took `0026_data_export.sql`.

## NDPA subject-access export (B2)

- **One job per user per UTC day.** A second `POST /v1/me/data-export` while PENDING/READY/DOWNLOADED/EXPIRED returns the existing job. FAILED jobs may be retried the same day.
- **Async.** POST returns PENDING; the worker (`data-export` loop) builds and encrypts the JSON. Tests call `dataexport.Process` directly.
- **Download is single-use and expires (24h).** Token is stored encrypted + HMAC; `Cache-Control: no-store`. After download or expiry the ciphertext is cleared.
- **Archive contents** are the data subject's: decrypted email/phone, masked meters, token-free receipts, consents, preferences, own households (name + own role + masked meters; no other members), own payment requests **without** guest payer ciphertext, own support threads (server-sealed AGENT/SYSTEM plaintext; HUMAN E2E is flagged, not decrypted — the server never holds that plaintext), security events (no IP HMAC).
- **Isolation:** another user's email, meter number, token, support body, or user id must not appear.
- **Step-up:** `security.ActionDataExport` is a gated action. Dev/OIDC bearers without a passkey session still work (fail-open, same as TOKEN_READ).

## Chart of accounts (P2 settlement)

Illustrative CoA from spec §9.5, extended for daily Paystack settlement and VTpass wallet recon.

**Pending founder/finance decision:** how to post Paystack fees, VTpass commission, and VAT on the convenience fee. Until signed off, use the default table below (`PSP_FEES_EXPENSE` only when a settlement batch balances; `VENDOR_COMMISSION_INCOME` never posted implicitly). Flagged in HANDOFF §9. The engine never invents a split.

| Account | Type | Role | Default posting |
|---|---|---|---|
| `PSP_SETTLEMENT_RECEIVABLE` | ASSET | Money Paystack still owes us after a successful charge | Debited on payment confirm (existing purchase path). **Credited** when a settlement batch is recognised. |
| `CUSTOMER_FUNDS_CLEARING` | LIABILITY | Customer money in flight | Unchanged (purchase / refund paths). |
| `PROVIDER_PAYABLE` | LIABILITY | Amount owed to the vending provider | Debited when a confirmed vend is recognised against the prefunded wallet. |
| `REFUND_PAYABLE` | LIABILITY | Refunds accepted but not yet settled | Unchanged. `REFUNDED` still means Paystack accepted the refund; final `refund.processed` is a recon report, not an implicit ledger post. |
| `PLATFORM_FEE_REVENUE` | INCOME | Our fee | Unchanged; P2 does not accrue it from settlement. |
| `POT_FUNDS_HELD` | LIABILITY | Funding-pot hold | Unchanged (M8). |
| `CASH_AT_BANK` | ASSET | Bank / Paystack settlement destination | **Debited** for settlement `effective_amount`. **Credited** when we top up the VTpass wallet from bank. |
| `PSP_FEES_EXPENSE` | EXPENSE | Paystack fees | **Debited** for settlement `total_fees` when `effective_amount + total_fees == total_amount`. Otherwise the batch is an exception (`SETTLEMENT_UNBALANCED`) and **nothing is posted**. |
| `VENDOR_WALLET_PREFUND` | ASSET | VTpass prefunded wallet | **Debited** on an audited wallet top-up. **Credited** when a confirmed vend is recognised (`vend-wallet:{txn}`). |
| `VENDOR_COMMISSION_INCOME` | INCOME | Vendor commission | **Unused until finance set the rate.** Never posted implicitly. |

- **Daily job** is idempotent per `(provider, Africa/Lagos business date)`. Completed days are no-ops. Provider HTTP is outside `InTx`; exceptions, settlement/wallet postings and the run row commit together.
- **Paystack** matches `GET /transaction` + `GET /refund` + `GET /settlement` against our transactions for that WAT day. **VTpass** requeries by the claim-time `request_id` (`VendIdempotencyKey`).
- **Corrections never post themselves.** An operator proposes an adjusting entry; a *different* admin approves it (`recon-adj:{id}`). Same person cannot approve. Resolve/write-off without an adjustment is notes-only (false positive), not a ledger change.
- Settlement reference `psp-settle:{id}` and vend-wallet reference `vend-wallet:{txn}` are ledger-idempotent, so a retried failed run does not double-post.

## Risk engine and chargebacks (P3)

`FEATURE_RISK_ENGINE` defaults **on** in `MODE=dev` and `MODE=staging` (pilot), **off** in production until explicitly enabled. Evaluation is skipped when the flag is off; dispute ingestion still runs.

- **Surfaces:** transaction create, Pay-for-Me public checkout, pot contribution, save-card, AutoPower rule create, third-party token send.
- **Never deny after payment** on purchase / Pay-for-Me / pot: a post-capture DENY is coerced to REVIEW (`risk:<rule>`) so money is not stranded. Save-card / AutoPower / token-send may DENY (no new charge).
- **Velocity** counts `checkout` observations under the user-row lock (or the public-link row lock). Concurrent checkouts cannot slip past the cap.
- **Public-link card testing:** too many distinct `authorization.signature` values, or too many failed payments, pauses the link (`risk_link_pauses`) and DENYs further checkouts.
- **Denylist** is admin-managed: card signature, email HMAC, phone HMAC, IP CIDR. Raw IPs are matched in memory and never stored.
- **Decisions** (`risk_decisions`) store HMACs and metadata only. They appear on admin transaction detail.
- **Dispute ledger** (finance default; revisit against the merchant contract):

| Event | Debit | Credit | Ref |
|---|---|---|---|
| Open | `DISPUTE_LOSS` | `DISPUTE_RESERVE` | `dispute-open:{psp_id}` |
| Win | `DISPUTE_RESERVE` | `DISPUTE_LOSS` | `dispute-win:{psp_id}` |
| Loss / merchant-accepted | `DISPUTE_RESERVE` | `PSP_SETTLEMENT_RECEIVABLE` | `dispute-loss:{psp_id}` |

  Open is a conservative P&L provision. Win reverses it. Loss keeps the expense and reduces the PSP receivable (Paystack will not settle the charge). Posts are ledger-idempotent. Evidence packs have receipt/vend/delivery timestamps and `token_issued` — never the STS token.
- **Paystack APIs** used as documented: [Disputes API](https://paystack.com/docs/api/dispute/), [manage disputes](https://paystack.com/docs/payments/manage-disputes/), webhooks `charge.dispute.create|remind|resolve`.
- Two lost/accepted disputes on one user feed the email/card denylist.

## Resume and reconciliation (M2)

| Setting | Default | Why |
|---|---|---|
| `PAYMENT_EXPIRY` | 2h | An unpaid checkout older than this is marked FAILED, but only when the provider says it is unpaid. If money arrives later, the transaction goes to MANUAL_REVIEW, never silently dropped. |
| `RESUME_STALE_AFTER` | 2m | The worker only touches transactions idle this long. Must exceed `PROVIDER_TIMEOUT` (enforced), so it never races an in-flight request. |
| `RESUME_BASE_BACKOFF` / `RESUME_MAX_BACKOFF` | 30s / 30m | Exponential backoff per transaction. |
| `RESUME_MAX_ATTEMPTS` | 12 | Checks without progress before escalating to MANUAL_REVIEW (about 2.5 hours at the defaults). Unpaid transactions don't escalate; `PAYMENT_EXPIRY` bounds them. |
| `RESUME_LEASE` | 2m | A claimed transaction is hidden from other workers for this long. |

- **MANUAL_REVIEW is a `review_reason` on the transaction**, not another payment state. Setting it stops all automation. Current reasons: payment amount mismatch, payment arriving after expiry, the HES losing an accepted command, reconciliation exhausted, and `risk:<rule>` (P3). M4 adds the operator API to resolve them.
- **The HES losing a credit command it had accepted goes to review, not a re-send.** Re-sending could double-credit if the original is still queued at the HES.
- **`cmd/worker` refuses to run with mock providers.** Mocks keep their state in the process that called them, so a separate worker would see none of the API's simulated payments. In dev, the worker runs inside `cmd/api` (`RUN_WORKER=true`, the default). `cmd/worker` becomes usable once real adapters exist (M3).
- **Outbox delivery is at-least-once.** Events are marked published in the same transaction that claimed them, after the sink accepts them. The sink currently logs metadata only; M5 notification consumers replace it and must dedupe on the event ID.

## Paystack (M3)

- **A new Paystack reference for each initialisation attempt** (`<transaction id>.<random>`). Paystack rejects a reused reference, and a retry after a crash or lost response can't recover the first attempt's checkout URL. Only the attempt we commit is ever shown to the customer, so an orphaned attempt has no link anyone can pay. We verify by the committed reference.
- **Refunds are made idempotent on our side.** Paystack has no idempotency key for refunds, so the adapter lists existing refunds on the transaction first and returns a live one instead of creating another. Only the worker that claimed `REFUND_PENDING` calls it, and the worker only retries stale transactions.
- **`REFUNDED` means Paystack accepted the refund** (its status `pending`/`processing`), not that the money has landed. Tracking final settlement through `refund.processed` webhooks belongs with the reconciliation reports.
- **Status mapping is conservative.** Only `success` counts as paid; `failed` and `reversed` count as failed; everything else (`abandoned`, `ongoing`, `processing`, `queued`, unknown values) counts as pending, which `PAYMENT_EXPIRY` bounds.
- **Error classes:** network errors, timeouts, 5xx and unparseable 2xx bodies are *ambiguous* (the operation may have happened). 4xx and `status:false` are definitive.
- **Webhooks:** HMAC-SHA512 over the raw body, compared in constant time. Only `charge.success` triggers processing; other events are acknowledged and ignored. The body is never trusted: we always call verify. Paystack's IP allowlist is not enforced in the app; it belongs at the edge (WAF or load balancer).
- **Key safety:** config only accepts `sk_test_` keys outside `MODE=production` and `sk_live_` keys in production, and `PROVIDER_ENVIRONMENT` must match.
- **Email is required to create a transaction.** Paystack needs it, and checking it in the API before anything is created avoids leaving a half-created transaction the client can't retry with the same Idempotency-Key.

## VTpass electricity vending

Public docs only: [authentication](https://www.vtpass.com/documentation/authentication/), [request_id](https://www.vtpass.com/documentation/how-to-generate-request-id/), [response codes](https://www.vtpass.com/documentation/response-codes/), per-DisCo electricity pages (IKEDC, EKEDC, AEDC, IBEDC, EEDC, PHED, KEDCO, KAEDCO, JED, YEDC).

- **Auth:** POST `api-key` + `secret-key` headers. Sandbox base `https://sandbox.vtpass.com/api`; live `https://vtpass.com/api`.
- **request_id** is generated at vend-claim (`NewVendKey`) as Africa/Lagos `YYYYMMDDHHII` + alphanumeric suffix of `vend:{txnUUID}`, persisted on `vending_records.idempotency_key`, and reused for pay + requery. Never re-dated on retry (that would double-vend).
- **code 000 is not success.** Inspect `content.transactions.status`: `delivered` → succeeded (prepaid token from `purchased_code` / `token` / extras); `pending`/`initiated`/unknown → PENDING; `099`/`089` → PENDING. Prepaid `delivered` without a token stays PENDING for requery.
- **code 014** (request id exists) → requery, not a second pay. **015** on requery → `NOT_FOUND` (safe to retry). Documented failure codes (`016`, `091`, `085`, `087`, …) return `VendFailed` with **nil error** so the orchestrator does not treat them as UNKNOWN.
- **5xx, timeout, empty or unparseable body** → `ErrAmbiguous`. The stateful fake applies the pay then returns 500 so Resume can requery the token.
- **Amount** is whole naira (`minor/100`). **Phone** is `VTPASS_DEFAULT_PHONE` (required); VTpass pay needs an MSISDN.
- **DisCo codes** map only to documented `serviceID`s. Unknown codes are rejected locally.
- **AMI/HES is still not a vendor client.** No Hexing/Conlog/Mojec API is invented. `REMOTE_PROVIDER=http` speaks Meterly's signed HES gateway contract (`internal/providers/heshttp`). `REMOTE_PROVIDER=none` in staging/production until a partner implements that contract or we write a documented translator.

## Credit on the meter (STS-over-AMI)

- **Licensed vend still happens first.** VTpass (or mock) mints an STS token; the HES injects that token into the meter. The purchaser does not see a keypad token while `delivery_method=REMOTE`. Operator `FallbackToToken` after a **confirmed** remote failure releases the same token.
- **ACCEPTED ≠ delivered.** Only meter confirmation (`CONFIRMED`) sets `credit_on_meter`. HES webhooks are a hint: we always `GetCreditStatus` by `credit:<txn-uuid>`.
- **Capability truth:** `supported` comes from the vending adapter **or** an operator-attested `remote_credit_cohorts` row (DisCo + digit prefix + evidence). `authorised` requires a live RemoteCreditProvider and `CheckEligibility`. Application code must not hard-code `7…` as production AMI. Mock vending may still report prefix 7 as supported.
- **Lost accepted command:** if the HES later returns NOT_FOUND for an ACCEPTED credit, MANUAL_REVIEW — never auto re-send (double-credit).

## Customer security engine

- **Not a rewrite of the purchase engine.** Payment, vend and delivery stay in `orchestrator`. This package only decides whether a *caller* may perform a sensitive action, and records that decision.
- **Dev/OIDC bearers are not stepped-up.** There is no passkey session to bind. Token reads and saved-card consent stay available in sandbox and staff impersonation. Passkey sessions (`mls_…`) must confirm the device (WebAuthn assertion, purpose `step-up`) before `TOKEN_READ`, `SAVE_CARD`, `DELETE_PASSKEY` and `REVOKE_OTHERS` if the last step-up is older than 15 minutes.
- **Last passkey cannot be deleted.** An account that has exactly one credential is denied `DELETE_PASSKEY` even without a session.
- **Events are metadata.** `security_events` stores action, outcome, reason, session id and IP HMAC. Never tokens, meter numbers, contacts or raw IPs.
- **`FEATURE` flag is not used.** This is account security, not a money-pooling product. Console is `GET /v1/security/console`.

## Auth, admin and audit (M4)

- **OIDC verification uses only the Go standard library** (`internal/auth`), not a JWT library. The supported surface is deliberately small:
  - RS256 (RSA ≥ 2048 bits) and ES256 only. `none` and HS* are rejected, which closes the alg-confusion attacks.
  - `iss` must match exactly; `aud` must contain the configured audience.
  - `exp` is required. `nbf` and `iat` are checked. Clock skew is at most 60s.
  - The JWKS is cached for 10 minutes. An unknown `kid` triggers a refresh for key rotation, but at most once per 30s so junk tokens can't make us hammer the identity provider.
  - If `OIDC_JWKS_URL` is empty, the JWKS URL is discovered from the issuer.
- **Users are keyed by `sub`** (`users.external_id`). This assumes a single issuer; supporting several would need `iss`-qualified IDs.
- **Roles:** `customer` (everyone), `operator`, `admin` (implies operator). They are read from `OIDC_ROLES_CLAIM` (default `roles`; dotted paths like `realm_access.roles` work), and unknown roles are ignored.
- **Staff roles need MFA:** they count only if the token's `amr` contains one of `OIDC_MFA_AMR` (default `mfa,otp,hwk,swk`). Otherwise the caller is treated as a plain customer. This can't be disabled outside dev.
- **Dev auth** is allowed only in `MODE=dev`. Subjects prefixed `operator:` or `admin:` get those roles, for local testing.
- **Who can do what:**

| Action | Role |
|---|---|
| Search, view detail (ledger, events, audit) | operator |
| Reconcile one transaction, fallback to token, audited token reveal | operator |
| Requeue from review, refund from review | admin |
| Enable or disable a provider | admin |

- **Every operator action needs a reason** (5–500 characters) and writes an `audit_events` row in the same DB transaction as the change. Customer token reads (`token.read`) and operator reveals (`token.reveal`) are audited before the token is returned: no audit row, no token. Audit metadata never holds tokens, meter numbers or contact details.
- **Operator refunds** are allowed only from MANUAL_REVIEW, and only when vending is `NOT_STARTED` or `FAILED`, so electricity was provably not delivered. They refund **the amount the PSP reports as paid**, which covers underpayments and late payments, and record its receipt in the ledger first. New payment transitions `PENDING→REFUND_PENDING` and `FAILED→REFUND_PENDING` exist only for this path.
- **Requeue** clears the review flag, resets the attempt budget and runs Resume. If the cause persists, automation flags the transaction again. There is no "close without action" yet; add one if operations need it.
- **Provider switches** pause new outbound work: checkouts, vends, credit sends and their re-sends. Status queries still run. A paused step doesn't count as a failed attempt, so an outage the operator is managing never escalates.
- **Rate limits** are token buckets. In-process by default; `REDIS_URL` (`redis://` or `rediss://`) shares the same budgets across API replicas (`internal/ratelimit` RESP client, no extra module):
  - writes (meter validation and registration, quotes, purchases, payment checks): per user, default 30/min with a burst of 10
  - failed authentication: per client IP, default 20/min
  - webhooks: per IP, default 600/min
  - the response is 429 with `Retry-After`
  - `CLIENT_IP_HEADER` takes the client IP from the right-most value of a header the trusted proxy appended
  - if Redis is configured but later unreachable, Allow **fails open** (request proceeds) so a cache outage cannot freeze vending
- **Admin search pagination** is keyset on `created_at` (`before=` / `next_before`). Transactions created in the same microsecond across a page boundary could be skipped; add an ID tiebreaker if that ever matters.


## Receipts (M5, initial slice)

- A receipt is a purchaser-only transaction record, available while pending as well as after completion. It reports payment, vending, delivery and overall states independently, and does not claim meter delivery from provider acceptance. It is not a tax invoice or settlement statement.
- JSON is the default; `?format=html` provides a printable, escaped HTML rendering. Both use `Cache-Control: no-store` and omit tokens, checkout details, recipient contact and provider references. Sharing and notification delivery are not yet implemented.


## Notification foundations (M5, partial)

- Templates use trusted event type and transaction delivery method, never arbitrary outbox payload values. Command acceptance does not produce a delivery confirmation message. Token-availability messages direct the purchaser to the authenticated app and explain meter binding; no raw token is inserted.
- Sender interfaces and a concurrency-safe in-process mock are available, but **not wired to the outbox**. The mock keeps only keyed digests and loses dedupe state on restart. It is not suitable for real delivery.
- Before enabling any sender, implement durable per-event/per-recipient/channel intent and outcome records, consent and encrypted contacts, secure authorised token access for gifting, and provider-specific idempotency or reconciliation for ambiguous sends. A database dedupe row alone cannot guarantee exactly-once external delivery after a timeout or crash. Never send within the outbox claim transaction.


## Token copy metadata (M5, partial)

- Only a purchaser may report a `COPY` action for a confirmed vend whose token has been released via the token path. Recording the action is not evidence that a clipboard operation or recipient delivery occurred. Unsupported channels are rejected; no SMS, email or WhatsApp is sent.
- Each copy record contains only actor, vending record, channel and time. It is committed alongside a redacted audit event, an outbox metadata event and an idempotency reservation, with the transaction row locked. Retried requests return the original record within the configured idempotency window; requests with the same key for a different transaction conflict. No token or recipient destination is stored in this record or event. Token reads remain separately authorised and audited.
- The history endpoint returns up to 200 most recent records. External notification and destination handling require separate consent, encrypted contact storage, and delivery reconciliation.

## Token send (FR-007)

- `POST /v1/electricity/transactions/{id}/token/shares` with `channel` `SMS`, `EMAIL` or `WHATSAPP` enqueues a token-bearing `notification_deliveries` row (`recipient_kind=TOKEN_SHARE`). The HTTP handler never decrypts or transmits the STS token.
- Empty `destination` sends to the purchaser's verified OIDC contact for that channel. A different destination is a third-party share and requires `FEATURE_GIFT_SMS` (same NDPA gate as gift-recipient SMS), re-checked at send time.
- Destination ciphertext is AES-GCM bound to `token-share-dest:<share UUID>`; only HMAC and a masked form are listed. Opt-outs are honoured at enqueue and send. Token-bearing UNKNOWN is never auto-resent. Cap: 5 sends per purchaser per Africa/Lagos day.
- WhatsApp uses in-window free-form text when `HasActiveWhatsAppWindow` is true, otherwise the approved `meterly_offline_token_v1` template. Recording a send is not proof of receipt or meter credit.


## Gift contact storage (M5, partial)

- Gift purchases may include a phone in E.164 format with an explicit `notify_recipient` request. This records only the purchaser's request, **not** the recipient's consent; no message is sent and contact is not returned from APIs or placed in outbox payloads.
- Contact encryption uses AES-GCM with transaction-specific associated data (`gift-contact:<transaction UUID>`) and stores the existing encryption key version. Contact-bearing idempotency request hashes use a domain-separated HMAC rather than a plain hash. Keep the HMAC key stable through the idempotency retention window; a future keyring is needed for rotation and old-key decryptions.
- Creation of gift contact, transaction, idempotency and outbox event is atomic. Migration 0004 adds key-version and request fields; its constraint is `NOT VALID` to avoid failing on unknown legacy data while still enforcing new writes. Audit and validate legacy rows before running `VALIDATE CONSTRAINT` in a later migration.
- Recipient opt-in/consent, retention/deletion controls, delivery intents, destination routing and provider-specific ambiguous-send reconciliation are still required before any notification can use the contact. Do not interpret purchaser request as permission to send.


## Held notification candidates (M5, partial)

- The independent candidate consumer scans an explicit event-type allowlist irrespective of `published_at`, using locked, skip-locked rows and a unique outbox-event key. Candidate insertion commits in the same transaction as the claim; rollback or competing workers do not lose events or duplicate records. It never marks events published and does not interfere with the existing metadata logging publisher.
- Candidates contain only event ID, aggregate ID, event type and creation time: no payload, contact, consent, channel, token or content. They are **held records**, not sendable delivery intents, and do not authorize delivery. The worker runs this consumer only when `NOTIFICATION_CANDIDATES_SINCE` is explicitly set.
- Migration 0005 requires outbox retention while candidates reference events. A future rollout must define retention, index/volume operations and a backfill policy before enabling consumption. Historical candidates must not automatically send messages. Future delivery requires recipient consent, safe destination resolution, durable per-recipient intents, revocation checks and provider idempotency/ambiguous-send reconciliation. None is implemented here.


## Meter registration is not ownership (M5, partial)

- Provider validation and knowledge of a meter number do not prove ownership or authority to manage a household. Self-service meter registration and beneficiary saving now grant at most `PURCHASE`; they cannot mint `MANAGE`. Existing higher grants are deliberately not silently downgraded. Older versions created `MANAGE` via self-registration, so **legacy MANAGE grants are unverified**: audit/remediate them before any future delegation feature relies on them. There is no household-management endpoint in this slice.
- `DELETE /v1/meters/{id}` revokes only the authenticated caller's own active grant, recording a redacted audit event atomically. Meter-row locks serialize removal with self-registration. Removed users may register the meter again for purchases; this is a saved-list action, not a household ban or permanent revocation. Purchases already in flight are not cancelled. An unused quote is reauthorized at transaction creation and fails while the grant is absent.
- Permission comparisons fail closed for unrecognized held or required permission values. Verified ownership, household membership/acceptance, delegation scope, revocation semantics and race-safe authorization for delegated actions need separate design and tests before enabling household permissions.


## Purchase authorization and self-removal ordering

- Transaction creation locks the meter row in the same transaction before rechecking the purchase grant, and holds that lock until creation commits. Self-removal and registration take the same meter lock. Therefore removal that commits first blocks a new transaction from an unused quote; a transaction committed first remains in flight and is not cancelled by removal. Idempotent replays of already-created transactions remain available to their purchaser.
- Any future grant mutation or delegated-access operation must acquire the meter lock before checking or modifying access, and must observe lock ordering. Repository methods alone do not enforce that higher-level contract. The quote itself can still be created concurrently with removal; transaction creation reauthorizes under the lock.


## Opt-in held-candidate worker

- Collection is off unless `NOTIFICATION_CANDIDATES_SINCE` is set to a nonzero RFC3339 timestamp with timezone; invalid values fail startup. Each worker records at most 100 candidates per outbox tick; no senders are invoked. Worker shutdown waits for all loops before the store is closed.
- The cutoff is inclusive on outbox `created_at`, not commit time. Events created earlier but committed later are intentionally excluded; eligible late commits are eventually collected because there is no high-water cursor. Define a safe rollout window, clock discipline and backfill policy before using this in production.
- All replicas must use the same stable cutoff. This is an operational requirement, not enforced in the database: moving it forward can skip uncollected events; moving it backward or running a replica with an older value can add historical candidates. Held records are not deliverable. Assess event volumes, query plans and retention before enabling; the outbox FK prevents deletion of referenced events.


## Notification delivery, consent and provider adapters (M5)

### Contact capture and encryption

- **Verified OIDC claims only.** Under `AUTH_MODE=oidc`, `users` contact columns are populated exclusively when `email_verified === true` (strict boolean) or `phone_number_verified === true` (strict boolean, E.164). Unverified OIDC claims and checkout emails passed on `POST /v1/electricity/transactions` are never treated as verified notification destinations.
- **Dev simulation.** Under `AUTH_MODE=dev` (`MODE=dev` only), callers may simulate verified OIDC claims via `X-Dev-Verified-Email`, `X-Dev-Verified-Phone` and `X-Dev-Display-Name`, and `POST /v1/electricity/transactions` seeds the user's encrypted email if none is stored yet so local mock purchases produce notification deliveries out of the box.
- **No plaintext contacts at rest.** Migration `0006_notifications.sql` drops the unused plaintext `users.email` and `users.phone` columns and adds AES-GCM bound ciphertexts (`user-email:<user_id>`, `user-phone:<user_id>`), domain-separated HMACs (`destination:<normalised>`) for constant-time lookup and opt-out matching, and masked display strings (`a***@example.com`, `**********5678`).

### Preferences, consent and global opt-outs

- **Account-holder defaults:** transactional notifications default to `sms: true`, `email: true`, `whatsapp: false`. WhatsApp defaults to `false` because Meta's WhatsApp Business Policy requires explicit user opt-in before sending business-initiated templates. Preferences are stored per `(user_id, channel)` in `notification_preferences` and updated atomically with an audit event via `PUT /v1/me/notification-preferences`.
- **Global `opt_outs` keyed by `destination_hmac`:** inbound `STOP` (and `STOPALL` / `UNSUBSCRIBE`, case-insensitive) on SMS, WhatsApp or Resend email complaints/inbound inserts `destination_hmac` into `opt_outs`. Because the key is the destination HMAC rather than a `user_id`, non-users such as gift recipients can opt out permanently.
- **Gift-recipient SMS gate:** a `GIFT_RECIPIENT` SMS is created and sent only on `token.available` when all three hold: (1) `FEATURE_GIFT_SMS=true` (defaults to `true` in `MODE=dev`, `false` outside dev until NDPA legal sign-off), (2) `gift_transactions.notification_requested=true`, and (3) the recipient's `destination_hmac` is not in `opt_outs`. All three conditions are checked at job creation and re-verified under lock at send time.
- **Gift to the buyer's own phone:** if a gift purchase specifies the purchaser's own verified phone number and gift SMS is eligible, the redundant generic `PURCHASER` `SMS` job for `token.available` is suppressed (`DUPLICATE_DESTINATION`) so the phone receives only the single token-bearing gift SMS (and `MockSender`'s `(event, channel, destination)` idempotency digest never sees two conflicting bodies).

### Delivery state machine (`notification_deliveries`)

Deduplication is enforced in PostgreSQL and `memstore` by `UNIQUE (event_id, recipient_kind, channel)`. Rendered bodies, tokens and plaintext destinations are never stored in `notification_deliveries`; destinations and tokens are decrypted in worker memory at send time and never logged.

| From | To | Trigger & rules |
|---|---|---|
| *(new)* | `PENDING` | Outbox dispatcher consumes an allowlisted event (`payment.confirmed`, `token.available`, `electricity.delivery.confirmed`, `electricity.delivery.failed`, `electricity.delivery.unknown`, `electricity.refund.completed`) for an eligible channel. |
| *(new)* / `PENDING` | `SUPPRESSED` | Destination is in `opt_outs`, channel preference is disabled, `FEATURE_GIFT_SMS` was turned off, verified contact was removed, or gift-to-self duplicate SMS. Checked again at send time before any provider call. |
| `PENDING` | `FAILED` | Destination fails format validation before send (`INVALID_DESTINATION`). |
| `PENDING` | `SENDING` | Explicit worker claim in `InTx` (`SELECT … FOR UPDATE`), incrementing `attempts` and setting a lease on `next_attempt_at`. Only the worker whose commit set `claimed=true` calls the provider. |
| `SENDING` | `SENT` | Provider accepted the message; records `provider_ref` and `sent_at`. |
| `SENDING` | `PENDING` | Retryable provider failure while `attempts < 5`; scheduled with backoff `30s`, `2m`, `10m`, `1h`. |
| `SENDING` | `FAILED` | Permanent 4xx rejection from the provider, or retryable failure on attempt 5. |
| `SENDING` | `UNKNOWN` | Timeout / network error / 5xx (`providers.ErrAmbiguous`), or recovery of a stale `SENDING` claim after a worker crash. |
| `UNKNOWN` | `SENT` / `FAILED` | Provider status API (`StatusChecker`) confirms final delivery or failure. |
| `UNKNOWN` | `SENDING` | Non-token messages only, when the provider has no status record (or no status API) and `attempts == 1` (at most one retry after backoff). **Token-bearing messages (`gift_token_sms`) are never auto-resent from `UNKNOWN`** unless a status API confirms the provider has no record of the send. |

### Outbox dispatcher

- `outbox.Dispatcher` fans each claimed outbox event out to registered `Sink`s (the notification job consumer plus `LogSink`) inside a per-event transaction carrying `store.Repos` in context. Each event's `notification_deliveries` inserts and `MarkPublished` commit atomically without nested-transaction deadlocks, while standalone/replayed calls to `ConsumeEvent` remain idempotent via `UNIQUE (event_id, recipient_kind, channel)`. `notification_candidates` remains strictly held metadata and is never read by the sender.

### Provider adapters and inbound webhooks

- **Africa's Talking SMS (`internal/notifications/africastalking`):** implements `POST /version1/messaging` (`apiKey` header, form-encoded) per `https://developers.africastalking.com/docs/sms/sending/bulk`. Inbound `STOP` callbacks (`POST /v1/notifications/inbound/africastalking`) require an HMAC-SHA256 signature in `X-AfricasTalking-Signature` keyed with `AT_WEBHOOK_SECRET` (supplied by the edge/API gateway proxy when connecting live Africa's Talking, since AT's raw form callback carries no built-in HMAC). Africa's Talking has no outbound lookup-by-client-key API, so lost responses stay `UNKNOWN` for token SMS.
- **Resend Email (`internal/notifications/resend`):** implements `POST /emails` with `Idempotency-Key: notif:<delivery_id>` and `GET /emails/{id}` status queries per `https://resend.com/docs/api-reference/emails/send-email`. Inbound/suppression webhooks (`POST /v1/notifications/inbound/resend`) verify Svix HMAC-SHA256 headers (`svix-id`, `svix-timestamp`, `svix-signature` with ±5m replay tolerance) per `https://docs.svix.com/receiving/verifying-payloads/how-manual`.
- **WhatsApp Cloud API (`internal/notifications/whatsapp`):** implements `POST /{version}/{phone_number_id}/messages` per `https://developers.facebook.com/docs/whatsapp/cloud-api/reference/messages`. Outside an active 24-hour customer-service window (tracked per `destination_hmac` from verified inbound messages), only pre-approved `"type": "template"` payloads may be sent; free-form text is restricted to active 24-hour windows. Inbound webhooks verify `X-Hub-Signature-256` (`sha256=<hex>`) with `WHATSAPP_APP_SECRET` and support the `GET` `hub.verify_token` challenge handshake.

### Privacy, retention and operator audit (NDPA 2023)

- **Retention:** `notification_deliveries` stores no message body or plaintext destination; a periodic worker sweep clears `destination_hmac` (`SET destination_hmac = ''`) on settled rows older than `NOTIFICATION_HMAC_RETENTION` (default `8760h` / 365 days).
- **Operator audit:** `GET /v1/admin/transactions/{id}` includes redacted delivery attempt metadata (`notifications` array: channel, recipient kind, template ID/version, status, attempts, error code, timestamps—never content or destination) and writes an `audit_events` record (`action: "notification_deliveries.view"`, `resource_type: "notification_deliveries"`, `resource_id: <transaction_id>`) before returning.

---

## M6 — Production readiness (OpenAPI 3.1, metrics, tracing, containers, graceful shutdown)

### `gopkg.in/yaml.v3` for OpenAPI verification

We add `gopkg.in/yaml.v3` (`v3.0.1`) solely to parse `api/openapi.yaml` in the automated route-coverage and schema-completeness test (`internal/httpapi/openapi_test.go`). It is a pure-Go, zero-runtime-dependency library used only in tests so partner-facing OpenAPI 3.1 YAML stays human-editable while CI proves bidirectional parity against the `http.ServeMux` patterns registered by `httpapi.Server.Routes()`.

### Stdlib Prometheus exposition and OpenTelemetry OTLP/HTTP JSON

Following the same minimal-dependency principle used for OIDC/JWKS in M4 (`internal/auth/oidc.go`), we implement Prometheus text exposition (`text/plain; version=0.0.4`) and OpenTelemetry W3C TraceContext (`traceparent`) + OTLP/HTTP JSON trace export (`POST <OTEL_EXPORTER_OTLP_ENDPOINT>/v1/traces`, `ExportTraceServiceRequest`) using the Go standard library (`internal/observability`):

1. **Zero supply-chain surface:** avoids pulling 25+ transitive gRPC/protobuf/SDK modules into the money-handling binary.
2. **Compile-time label safety:** `metrics.Registry` exposes typed methods for the exact approved metric set (`http_requests_total{route,code}`, `http_request_duration_seconds{route}`, `provider_calls_total{provider,op,outcome}`, `provider_call_duration_seconds{provider,op}`, `transactions_by_status{status}`, `manual_review_open`, `resume_escalations_total`, `outbox_lag_seconds`, `notification_deliveries_total{channel,status}`). Unmatched HTTP paths map to `route="UNMATCHED"` so attacker-chosen URLs can neither explode cardinality nor leak path values, and no API accepts arbitrary label keys (preventing meter numbers, user IDs, or transaction IDs from ever reaching `/metrics`).
3. **Internal metrics port:** `/metrics` is served exclusively on a dedicated listener (`METRICS_ADDR`, default `:9090`, disableable via `METRICS_ADDR=off` in tests) and is never mounted on the public API mux.
4. **OTLP tracing:** when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset, tracing is a zero-allocation no-op. When set, one server span per HTTP request and one client span per external provider call are exported asynchronously in bounded batches with `meterly.correlation_id` and low-cardinality attributes only (never bodies, tokens, meter numbers, phones, or emails), and flushed during graceful shutdown.

### Graceful shutdown ordering

`app.Serve` coordinates shutdown on `SIGTERM`/`SIGINT` (or context cancellation) in strict order:
1. Stop scheduling new background worker ticks while granting any already-running tick a bounded `ShutdownTimeout` context so in-flight claimed provider calls finish and commit their resulting state transitions before database connections close.
2. Drain the public HTTP server (`http.Server.Shutdown`), completing in-flight requests while rejecting new ones.
3. Wait on the worker `sync.WaitGroup` until all loops exit cleanly.
4. Shut down the internal metrics HTTP server and flush buffered OpenTelemetry spans before `App.Close()` closes the store.

### Container packaging

A single multi-stage `Dockerfile` builds static (`CGO_ENABLED=0`) binaries for three targets (`api`, `worker`, `migrate`) on `gcr.io/distroless/static-debian12:nonroot` running as `nonroot:nonroot` (UID 65532), with `docker-compose.yml` running `postgres` → `migrate` → `api` out of the box and CI verifying both image builds and an end-to-end purchase smoke test.

## M7: Pay-for-Me links (`internal/payrequests`, `migrations/0007_payment_requests.sql`)

### Secret codes, AEAD-bound metadata, and idempotent replay

1. **128-bit base32 link and status codes:** Both the shareable link code (`/p/{code}`) and the guest payer's status code (`/v1/public/pay/status/{status_code}`) are generated from 16 bytes of `crypto/rand` encoded with unpadded RFC 4648 base32 (26 uppercase characters). Database lookups use domain-separated keyed HMAC-SHA256 (`payrequest-code:<code>` and `payer-status-code:<code>`) with `UNIQUE` indexes and constant-time comparison; plaintext link codes are never stored.
2. **Encrypted note, guest PII, and replay status code:** Request notes (`<= 140` runes, URLs forbidden to prevent phishing) are encrypted with `secure.Box.EncryptBound` bound to `payrequest-note:<request_id>`. On `transaction_payers`, the guest payer's email, optional payer name, and status code are encrypted bound to `payer-email:<txn_id>`, `payer-name:<txn_id>`, and `payer-status-code:<txn_id>` respectively so an idempotent retry of `POST /v1/public/pay/{code}/checkout` (for example after a transient PSP timeout) returns the exact same `status_code` without storing it in plaintext.
3. **Slot reservation and automatic reopening:** `POST /v1/public/pay/{code}/checkout` locks `payment_requests` (`FOR UPDATE`) and the meter (`AuthorizeForUpdateIn` for `PURCHASE`), increments `payments_count`, and transitions `status` from `OPEN` to `PAID` when `payments_count == max_payments` (concurrent payers past capacity receive `409 LINK_ALREADY_PAID`). Every `SetPayment(PayFailed)` path in the orchestrator (`Resume` abandoned `CREATED` expiry, `checkPayment` `PENDING` expiry, and `checkPayment` definitive PSP failure) atomically marks `transaction_payers.slot_released = true`, decrements `payments_count`, and reopens a `PAID` link to `OPEN` (or `EXPIRED` if past `expires_at`, never overriding `CANCELLED`).
4. **Pure-stdlib QR PNG generation:** To keep zero external dependencies in the production binary, `internal/payrequests/qr.go` implements ISO/IEC 18004 QR Version 1–6 Byte-mode encoding (GF(256) Reed-Solomon ECC + penalty-minimized mask selection) rendered via `image/png`.
5. **Feature flag:** Gated behind `FEATURE_PAY_FOR_ME` (`true` by default in `MODE=dev`, `false` outside dev until rollout).

---

## M8: Split the Meter & Reusable Funding Pot (`internal/pots`, `migrations/0009_funding_pots.sql`)

### Legal gate (open founder / counsel question)

Collecting money from several contributors and holding it up to `POT_MAX_HOLD` (default `72h`) until a purchase target is reached may constitute holding customer funds under CBN/Paystack rules. Before enabling in production, the founder must confirm with Paystack and legal counsel whether pooled holds up to 72 hours are permitted under Meterly's merchant agreement as-is, or require a Paystack split-payment / licensed escrow arrangement. Split the Meter is therefore gated behind `FEATURE_SPLIT_METER` (defaults to `true` in `MODE=dev` and **`false`** outside `dev`) with a bounded `POT_MAX_HOLD` (default `72h`, maximum `168h`).

### Double-entry ledger table (`POT_FUNDS_HELD`)

We introduce the liability account `POT_FUNDS_HELD` (`LIABILITY`, `NGN`). Every contribution, overfund refund, allocation, vend, POT-transaction vend-failure return, and contributor refund posts balanced, idempotent entries:

| Event | Debit | Credit | Idempotent Reference |
|---|---|---|---|
| Contribution verified at PSP (`paid_minor`) | `PSP_SETTLEMENT_RECEIVABLE` | `POT_FUNDS_HELD` | `pot-contrib:<contrib_id>` |
| Overfund / late-payment refund owed (`excess_minor`) | `POT_FUNDS_HELD` | `REFUND_PAYABLE` | `pot-refund-due:<contrib_id>:1` |
| Allocation to POT-funded electricity transaction (`alloc_minor`) | `POT_FUNDS_HELD` | `CUSTOMER_FUNDS_CLEARING` | `pot-alloc:<alloc_id>` |
| Electricity vended (`Total = Amount + Fee`) | `CUSTOMER_FUNDS_CLEARING` (`Total`) | `PROVIDER_PAYABLE` (`Amount`) + `PLATFORM_FEE_REVENUE` (`Fee`) | `vend:<txn_id>` (unchanged) |
| Vend failure (or operator refund) on POT-funded transaction (`Total`) | `CUSTOMER_FUNDS_CLEARING` | `POT_FUNDS_HELD` | `refund-pot:<txn_id>` |
| Expiry / cancel / vend-failure contributor refund owed (`retained_minor`) | `POT_FUNDS_HELD` | `REFUND_PAYABLE` | `pot-refund-due:<contrib_id>:<seq>` |
| Contributor refund executed at PSP (`refund_step_minor`) | `REFUND_PAYABLE` | `PSP_SETTLEMENT_RECEIVABLE` | `pot-refund-paid:<contrib_id>:<seq>` |

Invariants enforced and tested:
1. Global trial balance remains strictly `0` at every step.
2. `POT_FUNDS_HELD` nets to `0` for every `SETTLED` or `REFUNDED` pot.
3. The platform fee (`FeeMinor`, default ₦100) is charged once on allocation (never per contribution): `target_minor` includes `FeeMinor`, so contributors pay exact shares without per-share fee multiplication.

### Pot, contribution and `FundingSource` state machines

1. **Pot (`Kind`: `SPLIT_METER` | `LANDLORD_BULK`):**
   - Funding & purchase path: `OPEN → FUNDED → ALLOCATING → SETTLED`.
   - Unfunded expiry / organiser cancellation path: `OPEN → EXPIRED|CANCELLED → REFUNDING → REFUNDED` (transitions directly from `REFUNDING` to `REFUNDED` inside the same commit when zero paid contributions exist).
   - Post-funding failure path: if the organiser lost `PURCHASE` before allocation or a POT-funded electricity transaction definitively fails vend (`VendFailed`), funds return to `POT_FUNDS_HELD` and the pot moves `FUNDED|ALLOCATING → REFUNDING → REFUNDED`. While an allocated transaction's vending is `UNKNOWN`, the pot stays in `ALLOCATING` and **never** refunds contributors.
2. **Contribution (`Status`: `PENDING` | `PAID` | `REFUND_PENDING` | `REFUNDED` | `FAILED`):**
   - Tracks `amount_minor` (PSP charge), `retained_minor` (applied toward `target_minor`), `refund_amount_minor` (total owed back to contributor), and `refunded_minor` (confirmed refunded by PSP), plus an explicit `refund_claimed` lease so concurrent workers claim each PSP refund call (`refund:contribution:<id>` for single-step refunds; `refund:contribution:<id>:<seq>` if a second principal refund follows an earlier partial overfund refund) at most once. Repeated refund failures escalate the pot to `review_reason` after `ResumeMaxAttempts`.
3. **Transaction `FundingSource` (`PSP` | `POT`):**
   - `transactions.Transaction` gains `FundingSource` (default `PSP`). A `POT`-funded transaction is created inside the allocation's `InTx` with `Payment = PayConfirmed` and empty `PaymentProviderRef`. Both `Orchestrator.refund` and `Orchestrator.OperatorRefund` branch on `FundingSource`: `PSP` calls `payments.RefundPayment`, whereas `POT` posts `Dr CUSTOMER_FUNDS_CLEARING / Cr POT_FUNDS_HELD`, marks the allocation returned, queues contributor refunds on the pot, and drives the pot refund sweep.

---

## M9: AutoPower Scheduled & Low-Balance Auto-Recharge (`internal/autopower`, `migrations/0011_autopower.sql`)

### Feature gate, consent and bound AEAD card authorization storage

1. **Feature gate:** AutoPower and saved payment methods are gated behind `FEATURE_AUTOPOWER`, which defaults to **`false`** in every `MODE` (`dev`, `staging`, `production`) until explicitly enabled.
2. **Consent-gated card saving:** `VerifyPayment` extracts reusable card metadata (`authorization_code`, `reusable`, `card_brand`, `last4`, `exp_month`, `exp_year`, `channel`, `signature`, `customer.email`), but `authorization_code` is **never** stored automatically during ordinary checkouts. Only an explicit `POST /v1/me/payment-methods/{transaction_id}/save` with `{"consent": true}` on the caller's own settled `PSP` card transaction encrypts `authorization_code` and charge email via `secure.Box.EncryptBound` bound to `autopower-auth:<user_id>:<method_id>` and `autopower-email:<user_id>:<method_id>`, persisting `consent_version`, `consent_text`, `consented_at`, and an audit event (`payment_method.saved`).
3. **Atomic rule stop on card deletion or meter removal:** `DELETE /v1/me/payment-methods/{id}` soft-deletes the payment method, pauses every non-cancelled `autopower_rule` referencing it (`PAYMENT_METHOD_DELETED`), cancels all unstarted `SCHEDULED` runs, and writes audit events inside one database transaction. Likewise, `Registry.RemoveSavedMeter` atomically pauses active AutoPower rules for `(user_id, meter_id)` (`METER_ACCESS_REVOKED`) and cancels scheduled runs, with an execution-time `AuthorizeForUpdateIn` check as defense in depth.

### Zero-double-charge `ChargeSaved` protocol and spend guards

1. **Slot idempotency (`UNIQUE (rule_id, scheduled_for)`) and pre-committed deterministic reference:** Before calling `PaymentProvider.ChargeSaved` (`POST /transaction/charge_authorization`), `Orchestrator.executeRun` commits inside `InTx` the consumed `Quote`, `Transaction` (`IdempotencyKey = "autopower:<run_id>"`, `Payment = PayPending`, `PaymentProviderRef = "apchg:<run_id>"`), and `autopower_runs` transition (`SCHEDULED → CHARGING`, `charge_dispatched_at = now`). Because `apchg:<run_id>` is deterministic and persisted before the outbound network call, a dropped HTTP response (`FailAfterApply`), timeout, or `pending`/`send_otp` challenge is always reconciled via `VerifyPayment("apchg:<run_id>")` (Invariant 1) without ever calling `ChargeSaved` a second time for that run.
2. **Canonical lock ordering and `Africa/Lagos` spend caps:** Spend checks run inside `InTx` under canonical lock order `User → Meter → PaymentMethod → Rule → Run`. Both `SumRuleSpendMinor` (rule `monthly_cap_minor` over the current `Africa/Lagos` calendar month) and `SumUserSpendMinor` (`AUTOPOWER_DAILY_MAX_MINOR` over the current `Africa/Lagos` calendar day) sum runs in `status IN ('CHARGING', 'SUCCEEDED')`. Over-cap runs transition to `SKIPPED_CAP` and emit `autopower.run.skipped_cap` without touching the PSP.
3. **Heads-up, catch-up window, 3-strike auto-pause and capability-gated low-balance mode:**
   - All schedules evaluate in `Africa/Lagos` (`time/tzdata` embedded, DST-safe noon-anchored iteration + short-month clamping) and store UTC instants.
   - `TickAutoPower` dispatches a 12-hour pre-charge heads-up (`autopower.run.heads_up`, one-tap skippable via `POST /v1/autopower/runs/{id}/skip`), executes due slots within a 6-hour catch-up window, and marks slots overdue past 6 hours `MISSED` (`autopower.run.missed`).
   - Failed/declined charges emit `autopower.run.failed` and increment `consecutive_failures`; on the 3rd consecutive failure (`CONSECUTIVE_FAILURES`) or on card expiry (`CARD_EXPIRED`), the rule pauses automatically and emits `autopower.rule.paused`.
   - `LOW_BALANCE` / `BOTH` triggers require `MeterReadingProvider` (`LatestReading(ctx, meter) (kWh, at, err)`) and `Capabilities.RemoteReading` having `Supported && Authorised && Confirmed` (`422 METER_NOT_ELIGIBLE` otherwise), polling every `AUTOPOWER_POLL_INTERVAL` and triggering at most once per 24 hours per rule.

---

## M10: Landlord Power Desk (`internal/powerdesk`, `internal/authz`, `migrations/0012_power_desk.sql`)

### Feature gate, organisation-scoped RBAC and zero-IDOR enforcement

1. **Feature gate:** Landlord Power Desk endpoints are gated behind `FEATURE_POWER_DESK`, defaulting to **`false`** in every `MODE` (`dev`, `staging`, `production`) until explicitly enabled.
2. **Centralised `authz.Require(ctx, orgID, minRole)`:** Every organisation, property, unit, unit-meter, bulk-top-up, and statement operation authenticates membership through a single helper `authz.Require(ctx, orgID, minRole)` backed by `organisation_members` (`OWNER > MANAGER > VIEWER`).
   - Callers who are **not** active members of `orgID` (or pass a `property_id`, `unit_id`, `unit_meter_id`, or `pot_id` belonging to another organisation) receive **`404 NOT_FOUND`**, never `403`, eliminating cross-organisation existence leaks (IDOR).
   - Active members with insufficient rank (for example, `VIEWER` creating properties/bulk top-ups, or `MANAGER` managing members or approving over-threshold bulk top-ups) receive **`403 FORBIDDEN`**.
   - Demoting or removing the last active `OWNER` of an organisation is rejected (`409 LAST_OWNER`) so an organisation can never become ownerless.

### Unit-meter attachment and operator ownership verification

1. **Unverified attachment (`PURCHASE`-only):** Reusing `organisations`, `roles`, `organisation_members`, `properties`, `buildings`, and `units` from `0001_init.sql`, `unit_meters` links an active meter to a unit with `verified = false`. While unverified, the organisation can include the unit in bulk electricity top-ups (`PURCHASE`), but **cannot** view meter-wide history (`GET /v1/units/{id}/meters/{unit_meter_id}/history` -> `403 UNIT_METER_UNVERIFIED`) or delegate access to tenants (`POST /v1/units/{id}/meters/{unit_meter_id}/delegate` -> `403 UNIT_METER_UNVERIFIED`).
2. **Audited operator verification (`POST /v1/admin/unit-meters/{id}/verify`):** Requires an MFA-authenticated operator/admin and a mandatory 5–500 character `reason`, recording an immutable `unit_meter.verified` audit event under one of two methods:
   - `ACCOUNT_NAME_MATCH`: verifies that the DisCo validation `customer_name` matches the organisation's `legal_name` / `name` (`422 VERIFICATION_MISMATCH` otherwise).
   - `EVIDENCE_APPROVED`: records an operator-reviewed external evidence reference (v1 stores the reference string only; document storage is out of scope).

### Multi-allocation `LANDLORD_BULK` Funding Pot, partial refunds and approval rule

1. **Reusable Funding Pot (`Kind = LANDLORD_BULK`):** Bulk top-ups reuse the M8 Funding Pot engine (`pots`, `pot_contributions`, `pot_allocations`) with **1 contribution** (the landlord's single Paystack checkout for `Σ (amount_i + fee_i)`, where `fee_i = ₦100` per electricity transaction) and **N POT-funded allocations** (one `transactions` row per unit, locked in canonical ascending `meter_id` order). Public `/v1/public/pots/*` routes reject `LANDLORD_BULK` pots and use domain-separated HMACs (`pot-bulk:<code>`).
2. **Optional `MANAGER` approval state machine (`PENDING_APPROVAL → OPEN`):** When an organisation configures `manager_approval_threshold_minor` (`₦X`), a `MANAGER` bulk top-up with `total_minor > threshold` is created in `PENDING_APPROVAL` with **no** PSP checkout initiated. Only an `OWNER` calling `POST /v1/orgs/{id}/bulk-topups/{pot_id}/approve` transitions the pot `PENDING_APPROVAL → OPEN`, writes `bulk_topup.approved` to the audit log, and initializes the single Paystack checkout.
3. **Double-entry ledger postings and partial-failure settlement:**

| Event | Debit | Credit | Idempotent Reference |
|---|---|---|---|
| Landlord single payment verified (`Total = Σ Total_i`) | `PSP_SETTLEMENT_RECEIVABLE` | `POT_FUNDS_HELD` | `pot-contrib:<contrib_id>` |
| Per-unit allocation `i` (`Total_i = Amount_i + Fee_i`) | `POT_FUNDS_HELD` | `CUSTOMER_FUNDS_CLEARING` | `pot-alloc:<alloc_i>` |
| Unit `i` electricity vended | `CUSTOMER_FUNDS_CLEARING` (`Total_i`) | `PROVIDER_PAYABLE` (`Amount_i`) + `PLATFORM_FEE_REVENUE` (`Fee_i`) | `vend:<txn_i>` |
| Unit `i` definitive vend failure (`VendFailed`) return | `CUSTOMER_FUNDS_CLEARING` (`Total_i`) | `POT_FUNDS_HELD` (`Total_i`) | `refund-pot:<txn_i>` |
| Unit `i` partial refund queued on landlord contribution | `POT_FUNDS_HELD` (`Total_i`) | `REFUND_PAYABLE` (`Total_i`) | `pot-item-refund-due:<alloc_i>:<contrib_id>` |
| Unallocated unit `i` refund queued (meter detached pre-alloc) | `POT_FUNDS_HELD` (`Total_i`) | `REFUND_PAYABLE` (`Total_i`) | `pot-unalloc-refund-due:<item_i>:<contrib_id>` |
| Partial / full contributor refund paid at PSP (`step`) | `REFUND_PAYABLE` | `PSP_SETTLEMENT_RECEIVABLE` | `pot-refund-paid:<contrib_id>:<step>` |

   - **Never refund on `UNKNOWN`:** An allocated unit whose vending is `VendUnknown` keeps its `Total_i` in `CUSTOMER_FUNDS_CLEARING` while sibling failed units (`VendFailed`) immediately refund their `Total_i` back to the landlord's contribution (`ContribPaid → ContribRefundPending → ContribPaid` while `retained_minor > 0`).
   - **Honest partial-success settlement:** Status views report exact per-item counts (`"<D> delivered, <F> failed and refunded, <P> pending"`). Once all N items are terminal (`DelConfirmed` or `AllocReturned`) and all queued contributor refunds have settled, the pot transitions `ALLOCATING → SETTLED` when `D >= 1` or `ALLOCATING → REFUNDING → REFUNDED` when `D == 0`, leaving `POT_FUNDS_HELD = 0`, `CUSTOMER_FUNDS_CLEARING = 0`, `REFUND_PAYABLE = 0`, and global trial balance `= 0`.

### Ledger-derived monthly statements and CSV export

`GET /v1/orgs/{id}/statements?property_id=&month=YYYY-MM&format=csv` evaluates `[monthStart, nextMonthStart)` in `Africa/Lagos` and derives per-unit, per-property, and organisation totals directly from double-entry `ledger` postings (`vend:<txn_id>`, `refund-pot:<txn_id>`, `pot-unalloc-refund-due:<item_id>:<contrib_id>`), never from request payloads or UI arithmetic. All JSON fields and CSV headers label principal `"electricity purchased"` (never `"used"` or `"consumed"`), and CSV cells sanitize spreadsheet-formula prefixes (`=`, `+`, `-`, `@`, `\t`, `\r`).

---

## M11 — Meterly Offline (USSD & WhatsApp Purchase Channels)

### Feature gates and external founder blockers

1. **Independent channel gates:** USSD and WhatsApp purchase channels are gated independently behind `FEATURE_USSD` (default `false`) and `FEATURE_WHATSAPP` (default `false`). When disabled, inbound channel routes return `404 FEATURE_DISABLED`.
2. **No new ledger accounts or bypasses:** Both channels drive the existing `Orchestrator` purchase pipeline (`CreateQuote` → `CreateTransaction` with deterministic `Idempotency-Key = <ussd|whatsapp>:<sessionId>:<step>` → `EnsurePaymentInitialised` → standard M1–M3 PSP webhook verification, vending, delivery, and refund ledger postings). No quote, payment verification, or state-machine guard is bypassed.

### Conversation state machine (`internal/conversation`)

Per-session state is persisted in `conversation_sessions` with channel-specific TTLs (`180s` for USSD, `30m` for WhatsApp). If a USSD session expires (`now >= expires_at`) before explicit confirmation, the expired session is discarded and **zero** transactions are created.

```
IDLE / NEW SESSION
  ├─(unverified or inactive > OFFLINE_REVERIFY_AFTER and OTP flow chosen / required for Last Token)
  │    └─► AWAIT_OTP ──(valid 6-digit SMS OTP)──► MENU (phone VERIFIED)
  └─► MENU ("1. Buy for my meter  2. Buy for saved meter  3. Last token  4. Help  [5. Verify phone]")
        ├─ "1" (1 saved meter) ──────────────────────► AWAIT_AMOUNT
        ├─ "1" (>1 saved meters) / "2" (has saved) ──► AWAIT_METER_CHOICE ──► AWAIT_AMOUNT
        ├─ "1"/"2" (0 saved meters or "NEW") ────────► AWAIT_DISCO ──► AWAIT_METER_NUMBER ──► AWAIT_AMOUNT
        ├─ "3" ("Last token", requires VERIFIED) ────► COMPLETED (resends caller's own latest token, ≤3/day, audited)
        └─ "4" ("Help") ─────────────────────────────► COMPLETED
AWAIT_AMOUNT ──(valid ₦ amount within cap)──► AWAIT_CONFIRM ("1. Confirm  2. Cancel")
  ├─ "1" ──► Orchestrator Quote + CreateTransaction + Checkout link ──► COMPLETED
  └─ "2" ──► CANCELLED (zero transaction created)
```

### Phone identity, SMS OTP linking, daily caps and SIM-swap guard

1. **Authenticated caller phone:** The caller's phone number from Africa's Talking or Meta WhatsApp Cloud API is trusted **only after** transport authentication (`USSD` IP allowlist + HMAC-SHA256 `X-USSD-Signature` / `X-AfricasTalking-Signature` -> `401` on failure; `WhatsApp` `hub.verify_token` challenge + `X-Hub-Signature-256` -> `401` on failure) and mapped via `notifications.DestinationHMAC(box, normalizedE164)` (zero plaintext phone numbers stored or logged).
2. **Automatic OIDC link vs SMS OTP:**
   - If `users` already holds an OIDC-verified `phone_hmac` (M5), the phone links automatically as `VERIFIED` unless inactive beyond `OFFLINE_REVERIFY_AFTER`.
   - First-time callers link their phone via a 6-digit SMS OTP (`channel_otps`, HMAC-bound `otp:<id>:<code>`, 10-minute TTL, max 5 attempts). Authenticated app users can also link a phone via `POST /v1/channels/phone/link/start` and `POST /v1/channels/phone/link/verify`.
   - **Daily caps by verification tier (`Africa/Lagos` day):** Unverified / pending-reverification phones can still make small emergency purchases up to `OFFLINE_UNVERIFIED_DAILY_MAX_MINOR` (default `₦5,000`/day), while OTP/OIDC-verified phones allow up to `OFFLINE_VERIFIED_DAILY_MAX_MINOR` (default `₦200,000`/day).
   - **SIM-swap inactivity guard:** If a verified phone has had no channel activity for more than `OFFLINE_REVERIFY_AFTER` (default `30d`), its channel link enters `REVERIFY_REQUIRED`, blocking `Last token` and capping spend at the unverified tier until a fresh SMS OTP is verified.

### Transport constraints, idempotency, STOP opt-outs and audited token resends

1. **USSD 182-character hard limit & callback idempotency:** Every USSD response (`CON …` or `END …`) is bounded to `≤ 182` characters. Each session caches `(last_raw_input, last_reply)` so duplicate Africa's Talking callbacks return the identical reply without re-executing steps.
2. **WhatsApp 24-hour window, templates, dedupe & out-of-order safety:** Inbound `wamid` IDs are recorded atomically in `processed_channel_messages` (duplicate webhooks are no-ops), and messages older than `session.last_inbound_msg_ts` are ignored so out-of-order delivery never corrupts active prompts. When `token.available` fires for a WhatsApp purchase, `notificationservice` sends free-form text in-chat when `HasActiveWhatsAppWindow` is true, and falls back to pre-approved template `meterly_offline_token_v1` outside the 24-hour customer-service window. Replying `STOP` records an M5 `opt_outs` entry immediately and closes any active session.
3. **Audited `Last token` cap:** `"Last token"` inspects only transactions where `customer_id == callerUserID`, enforces at most **3** resends per `(user_id, Africa/Lagos day)` in `offline_token_resends`, and records an `offline.token.resent` audit event (with redacted metadata only) in the same database transaction.

## Review fixes (2026-11-27)

A review of everything merged after `68acdab` (PRs #5–#23) found these defects. Each fix has a regression test that fails without it.

- **Saved cards must belong to the person saving them.** `SavePaymentMethod` refuses any transaction with a Pay-for-Me payer record. Such a transaction belongs to the requester but was paid with a guest's card, so saving it would let AutoPower charge a stranger.
- **Sessions are bound to one network.** `TouchSession` records the client IP HMAC only when none is stored. Only a successful passkey step-up (`MarkSessionStepUp`) re-binds it. Previously every request overwrote the stored IP before the step-up gate compared against it, so the "moved network" check never fired.
- **Passkey assertions require user verification.** Login and step-up need both UP (0x01) and UV (0x04), matching `userVerification: "required"` in the options.
- **Adding a passkey to an existing account needs a fresh step-up** (`ADD_PASSKEY`), the same as deleting one. Otherwise a stolen session could enrol its own key and pass every later step-up.
- **At registration, the attested key is authoritative.** When the authenticator data carries an attested credential, its key is stored; a client-supplied key may only repeat it.
- **No prepaid vend is confirmed without a token.** `applyVendResult` treats a "succeeded" result with an empty token as still pending, unless the meter is POSTPAID (bill payment, no token). VTpass requery can report `delivered` before the token exists, and the requery path doesn't know the meter type.
- **VTpass `083` (SYSTEM ERROR) is PENDING, not FAILED.** The pay may have gone through; a definitive failure would trigger an automatic refund (invariants 2 and 4).
- **Paystack references only use `[A-Za-z0-9-.=]`.** The AutoPower charge prefix changed from `apchg:` to `apchg-`, and `paystacktest` now rejects invalid references as Paystack does. AutoPower had never been live, so no stored references needed migrating.
- **Self-service meter ownership by display-name match is off by default** (`FEATURE_OWNERSHIP_NAME_MATCH`, on only in `MODE=dev`). Display names are user-editable and `/v1/meters/validate` reveals the DisCo customer name, so the match proves nothing. Outside dev, users get `OWNERSHIP_REVIEW_REQUIRED`, and operators verify through the audited `POST /v1/admin/meters/{id}/ownership`. Re-enable it only with a non-editable, identity-verified name source.
- **Not fixed (accepted):** the Redis limiter re-runs EVAL after a lost reply, which can occasionally count one request twice. That only errs toward stricter limiting.

## Sandbox certification (P1)

One command proves each adapter against that provider's real sandbox, and only when keys are present.

- Package `internal/certify`, command `cmd/certify`. Live tests sit behind `go test -tags certify ./internal/certify/...` and are skipped without env keys, so they never run in normal CI.
- `cmd/certify -o certify-report.json` records provider, scenario, pass/fail/skipped, latency and provider request/reference IDs. No secrets, tokens or meter numbers (free-text fields are sanitised).
- Certify refuses `sk_live_` Paystack keys and non-sandbox VTpass / Africa's Talking URLs unless `CERTIFY_ALLOW_LIVE=true`.
- Paystack test-card payment uses the documented Charge API (`POST /charge`, PIN/OTP submit) with card `4084084084084081` (https://paystack.com/docs/payments/test-payments). Production never sends raw PANs; this path is certify-only. `paystacktest` now implements `/charge` so the harness can run against the fake.
- VTpass scenarios use the documented sandbox meters `1111111111111` (prepaid) and `1010101010101` (postpaid). `vtpasstest` now rejects a request_id whose first 12 digits are not a real `YYYYMMDDHHII` (code `085`).
- OIDC without a founder IdP is certified only against an in-process JWKS (discovery, keys, staff token with `amr` + roles). A live staff token is `CERTIFY_OIDC_STAFF_TOKEN`.
- Optional captured Paystack webhook: `CERTIFY_PAYSTACK_WEBHOOK_BODY` + `CERTIFY_PAYSTACK_WEBHOOK_SIGNATURE`.

### Sandbox findings

None yet — no founder sandbox keys in this environment. Every provider is **not certified**. Differences found later against a real sandbox must change the fake, add a regression test, and land here.

## PR #24 review fixes (2026-11-27)

Reviewing PR #24 (P0–P4) found these, now fixed with tests on both stores:

- **CI was red.** The pilot tests passed `"admin:test"` as an operator's user ID. Production passes the internal UUID; PostgreSQL rejects non-UUIDs in `audit_events.actor_user_id`, and memstore didn't. The tests now use a real user.
- **Dispute outcomes are never guessed.** An unrecognised or empty Paystack `resolution` previously became LOST, which posted a write-off and counted toward the repeat-chargeback deny-list. It now stays OPEN/REMINDED for an operator, with Paystack's resolution recorded. Only `merchant-accepted` (lost) and `declined` (won) are mapped.
- **Dispute outcomes are final.** A dispute already WON/LOST/ACCEPTED never changes status. A conflicting later event is logged and ignored, because flipping would post both the win and the loss entries and release `DISPUTE_RESERVE` twice.
- **Wallet top-ups are idempotent.** `POST /v1/admin/reconciliation/wallet-topup` requires an `Idempotency-Key`. The ledger reference is derived from it, so a retry or double click books the money once, and the same key with a different amount returns 409.
- **Risk rule changes are audited atomically.** The rule update and its audit row (with before/after values) commit in one transaction, and the update fails if the audit can't be written. Previously the audit was written separately and its error ignored.
- **`internal/reconcile` tests leaked template databases** (no `TestMain` calling `pgtest.DropTemplate()`). Fixed.
