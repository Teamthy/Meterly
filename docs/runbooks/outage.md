# Outage

1. `/healthz` dead → process. `/readyz` 503 → DB or pending migrations. Do not apply `0022`/`0023`/`0024` from a laptop against hosted if HANDOFF says blocked — use Render `preDeployCommand: /migrate`.
2. Pause: `POST /v1/admin/pilot/pause` with a reason, or `SERVICE_PAUSED=true` and bounce. UI shows “Service is paused.”
3. Status page / WhatsApp: “Do not pay. We will not take money we cannot vend.”
4. Restore: [restore.md](restore.md). Rollback: [rollback.md](rollback.md).
5. When back, canary (`scripts/canary.sh`) then unpause.
