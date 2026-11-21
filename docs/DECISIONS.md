# Decisions

Defaults chosen where the spec left room, or where a decision depends on a commercial or legal call. Each one can be revisited; most are configuration.

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

## Resume and reconciliation (M2)

| Setting | Default | Why |
|---|---|---|
| `PAYMENT_EXPIRY` | 2h | An unpaid checkout older than this is marked FAILED, but only when the provider says it is unpaid. If money arrives later, the transaction goes to MANUAL_REVIEW, never silently dropped. |
| `RESUME_STALE_AFTER` | 2m | The worker only touches transactions idle this long. Must exceed `PROVIDER_TIMEOUT` (enforced), so it never races an in-flight request. |
| `RESUME_BASE_BACKOFF` / `RESUME_MAX_BACKOFF` | 30s / 30m | Exponential backoff per transaction. |
| `RESUME_MAX_ATTEMPTS` | 12 | Checks without progress before escalating to MANUAL_REVIEW (about 2.5 hours at the defaults). Unpaid transactions don't escalate; `PAYMENT_EXPIRY` bounds them. |
| `RESUME_LEASE` | 2m | A claimed transaction is hidden from other workers for this long. |

- **MANUAL_REVIEW is a `review_reason` on the transaction**, not another payment state. Setting it stops all automation. Current reasons: payment amount mismatch, payment arriving after expiry, the HES losing an accepted command, and reconciliation exhausted. M4 adds the operator API to resolve them.
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
- **Rate limits** are in-memory token buckets, one set per API replica:
  - writes (meter validation and registration, quotes, purchases, payment checks): per user, default 30/min with a burst of 10
  - failed authentication: per client IP, default 20/min
  - webhooks: per IP, default 600/min
  - the response is 429 with `Retry-After`
  - `CLIENT_IP_HEADER` takes the client IP from the right-most value of a header the trusted proxy appended
  - move to a shared store (Redis) when one replica's view stops being enough
- **Admin search pagination** is keyset on `created_at` (`before=` / `next_before`). Transactions created in the same microsecond across a page boundary could be skipped; add an ID tiebreaker if that ever matters.
