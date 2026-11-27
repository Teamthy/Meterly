# Breach (suspected)

1. `SERVICE_PAUSED=true` / admin pause. Rotate `TOKEN_ENC_KEY` ([key-rotation.md](key-rotation.md)), Paystack/VTpass secrets, OIDC client secret, Render/Vercel env.
2. Revoke sessions (`/security`). Do not dump the DB to Slack.
3. NDPA clock starts when personal data may have left our control — legal (`docs/LEGAL.md`). Engineering does not notify customers until legal says so.
4. Preserve logs. Do not wipe outbox or audit_events.
