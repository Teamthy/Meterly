# HTTP5xx

**Symptom:** `http_requests_total{code=~"5.."}` elevated.

1. Filter 503 `VENDING_PAUSED` / `SERVICE_PAUSED` — those are controlled pauses, not crashes.
2. Correlation ID from the customer or Render logs. Never grep tokens.
3. Provider 5xx → [provider-errors.md](provider-errors.md). Resume will requery UNKNOWN; do not re-vend.
4. If deploy-related, [rollback.md](rollback.md).
