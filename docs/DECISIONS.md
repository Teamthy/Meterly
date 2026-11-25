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





