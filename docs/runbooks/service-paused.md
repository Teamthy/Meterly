# SERVICE_PAUSED

Purchases return 503 `SERVICE_PAUSED`. `/readyz` `service=paused`. UI: “Service is paused.”

- Env `SERVICE_PAUSED=true` (all instances) or `POST /v1/admin/pilot/pause` with a reason (process-local — Blueprint is one API replica).
- Resume: `POST /v1/admin/pilot/resume` or unset env and bounce.
- `PILOT_MODE=true` users/meters/DisCos **not** on the allow-list also see SERVICE_PAUSED (no “you are not invited”). Caps return 429 `PILOT_CAP`.
- Audited. Reason 5–500 chars.
