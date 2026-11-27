# Deploy Meterly (Vercel + Render + Supabase)

This is the “for now” hosting layout. VTpass vending exists in-repo; Render still defaults to `MODE=dev` until you paste VTpass + Paystack keys and drop mocks.

| Piece | Where | Notes |
|---|---|---|
| Web UI | Vercel project `meterly`, Root Directory `web` | Already on production (`meterly-iota.vercel.app`) |
| Go API + in-process worker | Render Blueprint `render.yaml` | `MODE=dev` because mock providers are refused outside dev |
| Postgres | Existing Supabase project `Meterly` (`wiqfbscyppgsolvmereh`, `eu-west-1`) | Apply repo migrations (or let Render `preDeployCommand: /migrate`) |

## 1. Supabase

1. Open [Supabase](https://supabase.com/dashboard/project/wiqfbscyppgsolvmereh/settings/database).
2. Copy the **URI** for the **session pooler** (port **5432**, not transaction-mode 6543 — pgx uses prepared statements).
3. Append `sslmode=require` if it is missing.
4. That value is `DATABASE_URL` for Render.

Do not use the transaction pooler (6543). Do not commit the password.

## 2. Render

Render is not an Arena-connected app. Deploy the Blueprint from GitHub:

1. Push this branch / `main` so `render.yaml` and `Dockerfile` are on GitHub.
2. Open https://render.com/deploy?repo=https://github.com/Teamthy/Meterly
3. Paste:

   | Key | Value |
   |---|---|
   | `DATABASE_URL` | Supabase session-pooler URI |
   | `TOKEN_ENC_KEY` | 64 hex characters (32 bytes) |
   | `METER_HMAC_KEY` | 64 hex characters (32 bytes) |

4. Health check is `GET /healthz`. `/migrate` runs as `preDeployCommand`.

The public API URL will look like `https://meterly-api.onrender.com`.

`MODE=dev` is the Blueprint default because mocks are refused outside dev. After pasting VTpass + Paystack keys, switch to `MODE=staging` with:

```
PAYMENT_PROVIDER=paystack
PAYSTACK_SECRET_KEY=sk_test_...
VENDING_PROVIDER=vtpass
VTPASS_API_KEY=...
VTPASS_SECRET_KEY=...
VTPASS_DEFAULT_PHONE=2348012345678
REMOTE_PROVIDER=none
METER_READING_PROVIDER=none
SMS_PROVIDER=none
EMAIL_PROVIDER=none
WHATSAPP_PROVIDER=none
PROVIDER_ENVIRONMENT=SANDBOX
```

## 3. Point Vercel at Render

On Vercel project `meterly` (Production):

```
METERLY_API_ORIGIN=https://meterly-api.onrender.com
```

`web/next.config.mjs` rewrites `/v1/*`, `/healthz` and `/readyz` to that origin. Redeploy the frontend after setting it.

## 4. Support chat

- Customer: `/support` — Meterly Agent, or **Talk to a person** (E2E).
- Operator: `/admin/support` — claim / encrypted reply / resolve.
- Agent messages are processed by Meterly (not E2E) so the agent can look up the caller’s own purchases. Human messages are ciphertext; private keys never leave the browser.
- Device keys live in `localStorage`. Switching browsers means you cannot decrypt old person-to-person messages.

## 5. What this hosted stack cannot do yet

- Live Paystack / VTpass / Africa’s Talking / WhatsApp / OIDC — still fakes in `MODE=dev` until keys are pasted.
- Remote meter credit — `REMOTE_PROVIDER=none` until a partner implements Meterly's `heshttp` gateway (or we write a documented translator). Cohorts and confirmation webhooks are in code.
- Production feature flags (`FEATURE_SPLIT_METER`, gift SMS, USSD, …) pending legal sign-off.
- More than one API replica: set `REDIS_URL` (`redis://` or `rediss://`) so write/auth/webhook budgets are shared. One instance can keep the in-memory limiter.

## 6. Backups, PITR, restore drill

Pilot RPO **5 minutes** (Supabase PITR / WAL on Pro). Pilot RTO **60 minutes** to a passing `cmd/integrity`.

1. Enable PITR on the Supabase project. Do not use the transaction pooler (6543) for restore either.
2. Practice on a **scratch** database, never in-place first:
   `RESTORE_DATABASE_URL=postgres://... ./scripts/restore-drill.sh`
   Optional `DUMP=` is a `pg_dump` custom file restored with `pg_restore`.
3. The script applies `cmd/migrate` then `cmd/integrity` (trial balance must be 0). See `docs/runbooks/restore.md`.
4. Key rotation: `docs/runbooks/key-rotation.md` (`cmd/rekey`).
5. Alerts: `deployments/monitoring/alerts.yml`. Worker heartbeat needs `METRICS_ADDR` not `off` on at least one process that runs `RUN_WORKER=true`.

## 7. Pilot gate

`PILOT_MODE=true` allow-lists users, meters, optional DisCos (`POST /v1/admin/pilot/allowlist`). Daily caps: `PILOT_DAILY_USER_CAP_MINOR`, `PILOT_DAILY_GLOBAL_CAP_MINOR`. Kill switch: `SERVICE_PAUSED=true` or admin pause. Non-allow-listed callers see 503 `SERVICE_PAUSED` (no invitation leak).
