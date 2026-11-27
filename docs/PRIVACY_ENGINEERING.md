# Privacy engineering (NDPA 2023)

Data map for counsel’s DPIA. Machine-readable copy: `internal/privacy` (`Columns()`, `Policies()`). A test fails if migrations add a column named like `email`, `phone`, `name`, `address`, `ip` or `msisdn` without a registry entry.

**Legal sign-off on retention periods is pending** (HANDOFF §9). Defaults below are engineering defaults, not counsel’s.

Lawful bases used here: **contract** (performing a purchase), **consent** (marketing, saved cards, gift/pot guest contacts), **legitimate interest** (abuse control, security audit), **legal obligation** (financial books).

Storage: **encrypted** (AEAD, bound to user id where noted), **HMAC** (lookup only), **plaintext** (minimised), **unused** (do not populate).

---

## Retention job (B1)

`privacy.Run` on the worker (`retention` loop). Batched, idempotent, dry-run via `RETENTION_DRY_RUN`. One `audit_events` row and one `retention_runs` row per execution (counts only, no PII). Active `legal_holds` skip that user’s rows. Financial tables are **ANONYMISE** and the B1 job **does not delete them** (B3 erasure).

| Table | Action | Default duration | Env |
|---|---|---|---|
| `channel_otps` | DELETE | 7 days (`created_at`) | `RETENTION_OTP` |
| `auth_challenges` | DELETE | 7 days after `expires_at` | `RETENTION_CHALLENGE` |
| `auth_sessions` | DELETE | 90 days after expiry/revoke | `RETENTION_SESSION` |
| `idempotency_keys` | DELETE | 30 days after completion | `RETENTION_IDEMPOTENCY` |
| `risk_observations` | DELETE | 90 days | `RETENTION_RISK_OBSERVATION` |
| `support_threads` (CLOSED/RESOLVED, cascade messages) | DELETE | 2 years | `RETENTION_SUPPORT` |
| `notification_deliveries.destination_hmac` | ANONYMISE (clear HMAC) | 365 days | `NOTIFICATION_HMAC_RETENTION` |
| `electricity_transactions`, `payments`, `ledger_entries`, `refunds`, `disputes` | ANONYMISE (time-based job still a no-op; B3 account erasure re-points user FKs) | 7 years | `RETENTION_FINANCIAL` |

---

## Account erasure (B3)

`POST /v1/me/delete-account` starts a configurable 7-day cooling-off (`ACCOUNT_ERASURE_COOLING`). After that, `erasure.Process` runs a resumable job: financial FKs → tombstone user `00000000-0000-0000-0000-00000000e5ed`; identity, memberships, support, destinations, cards and exports are deleted. Personal columns and their erasure action (`DELETE` / `ANONYMISE` / `SHARED` / `KEEP`) live in `internal/privacy.ErasureActions()`. Shared meters keep Disco name/address while another household still has an active grant. Pay-for-Me guest payer ciphertext is kept (other people's data). Open disputes, unresolved txns, pots with funds, and unpaid obligations block the job. Paystack card revoke is TODO until documented.

Batch size `RETENTION_BATCH_SIZE` (500). Interval `RETENTION_INTERVAL` (1h).

---

## Consent ledger

Existing `notification_preferences` remain the **current transactional** flags (SMS/email on, WhatsApp off by default). `consents` is append-only history. Marketing defaults to **withdrawn** (opt-in).

- `GET /v1/me/consents` — current × purpose × channel, plus history
- `POST /v1/me/consents/withdraw` `{purpose, channel}` — writes history, turns off transactional prefs, adds `opt_outs` for the user’s destination HMAC so the **next send is blocked**

STOP keywords still write `opt_outs` (already shipped).

---

## Column map

| Table.column | Personal | What | Purpose | Basis | Retention | Storage |
|---|---|---|---|---|---|---|
| `users.email_ciphertext` | yes | verified email | notify / login | contract | until erasure | encrypted |
| `users.email_hmac` | yes | lookup / STOP | lookup | contract | until erasure | HMAC |
| `users.email_masked` | yes | UI mask | profile | contract | until erasure | plaintext |
| `users.phone_ciphertext` | yes | verified E.164 | notify / USSD | contract | until erasure | encrypted |
| `users.phone_hmac` | yes | lookup / STOP | lookup | contract | until erasure | HMAC |
| `users.phone_masked` | yes | UI mask | profile | contract | until erasure | plaintext |
| `users.display_name` | yes | chosen name | profile / receipts | contract | until erasure | plaintext |
| `users.first_name` / `last_name` | yes | leftover from `0001`; never written | unused | unused | delete on erasure | unused |
| `users.email_key_version` / `phone_key_version` | no | crypto version | crypto | unused | with ciphertext | plaintext |
| `meters.customer_name` | yes | Disco account name | meter identity | contract | while linked; anonymise on erasure | plaintext |
| `meters.address` | yes | service address | show the right meter | contract | while linked; anonymise on erasure | plaintext |
| `beneficiaries.display_name` | yes | saved-beneficiary label | UX | contract | until removed / erasure | plaintext |
| `gift_transactions.recipient_name` | yes | gift label | gift UX | consent | 7 years then anonymise | plaintext |
| `households.name` | yes | household name | sharing | contract | until deleted / erasure | plaintext |
| `transaction_payers.email_ciphertext` | yes | Pay-for-Me guest email | receipt | contract | 7 years then anonymise | encrypted |
| `transaction_payers.payer_name_ciphertext` | yes | guest payer name | receipt | contract | 7 years then anonymise | encrypted |
| `pot_contributions.email_ciphertext` | yes | guest contributor email | settlement notify | consent | 7 years then anonymise | encrypted |
| `pot_contributions.phone_ciphertext` | yes | guest contributor phone | optional SMS | consent | 7 years then anonymise | encrypted |
| `pot_contributions.name_ciphertext` | yes | contributor name | initials on public page | consent | 7 years then anonymise | encrypted |
| `saved_payment_methods.email_ciphertext` | yes | card-network email | AutoPower | consent | until method deleted | encrypted |
| `auth_sessions.ip_hmac` | yes | session IP bind | step-up | contract | 90 days after expiry | HMAC |
| `security_events.ip_hmac` | yes | security audit IP | security | legitimate interest | with the event | HMAC |
| `webauthn_credentials.name` | yes | passkey nickname | security UI | contract | until credential deleted | plaintext |
| `channel_otps.phone_hmac` | yes | OTP destination | link verify | contract | 7 days | HMAC |
| `phone_channel_links.phone_hmac` | yes | phone↔user bind | offline login | contract | until unlinked / erasure | HMAC |
| `conversation_sessions.phone_hmac` | yes | USSD/WA session | offline | contract | session TTL | HMAC |
| `offline_purchases.phone_hmac` | yes | channel purchase | daily caps | contract | 7 years then anonymise | HMAC |
| `offline_token_resends.phone_hmac` | yes | token resend | caps | contract | 7 years then anonymise | HMAC |
| `processed_channel_messages.phone_hmac` | yes | inbound webhook | idempotency | legitimate interest | 30 days | HMAC |
| `risk_observations.email_hmac` / `phone_hmac` / `ip_hmac` | yes | velocity | abuse control | legitimate interest | 90 days | HMAC |
| `risk_decisions.email_hmac` / `ip_hmac` | yes | decision metadata | abuse control | legitimate interest | with the txn | HMAC |
| `organisations.name` / `legal_name` | yes | landlord org | Power Desk | contract | while org exists | plaintext |
| `properties.name` / `address` | yes | landlord property | Power Desk | contract | while property exists | plaintext |
| `buildings.name` | yes | building label | Power Desk | contract | while building exists | plaintext |
| `units.unit_name` | yes | unit / apartment label | Power Desk | contract | while unit exists | plaintext |
| `unit_meters.disco_account_name` | yes | Disco name on a unit meter | ownership match | contract | while attached | plaintext |
| `roles.name`, `providers.name`, `provider_integrations.integration_name`, `distribution_companies.name`, `meter_manufacturers.name`, `meter_models.model_name`, `ledger_accounts.account_name`, `notification_deliveries.provider_name`, `pot_contributions.payment_provider_name` | no | reference / ops labels | catalog / ops | unused | indefinite | plaintext |

Key-version columns (`*_key_version`) are not personal; they ride with the ciphertext.

Meter **numbers** and STS **tokens** are encrypted / HMAC elsewhere (`meters.number_ciphertext`, vending token ciphertext). They do not match the name heuristic; they remain forbidden in logs and URLs (invariants).

---

## Not in this document (later batches)

- B3 account erasure (anonymise financial rows, delete contacts)
- Partner API personal data (B4+)

## Subject-access export (B2)

`POST /v1/me/data-export` (passkey step-up, rate-limited, audited) creates one job per user per UTC day. A worker writes an encrypted JSON archive (`data_exports.archive_ciphertext`). `GET …/download` is single-use and expires in 24 hours (`DATA_EXPORT_TTL`); ciphertext is then deleted. The archive includes the subject's decrypted email/phone, masked meters, token-free receipts, consents, preferences, own household membership (not other members' contacts), own support threads (agent/system decrypted; human E2E plaintext is not on the server), and security events. It never includes STS tokens, raw meter numbers, or another user's PII.

Retention: `data_exports` DELETE after the TTL (`RETENTION` uses `DATA_EXPORT_TTL`). Legal holds skip the user's rows.
