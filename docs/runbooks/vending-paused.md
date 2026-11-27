# VendingPaused

**Symptom:** `/readyz` `vending=paused`, purchases return 503 `VENDING_PAUSED`, metric `vending_paused=1`. UI: “Electricity vending is paused.”

**Cause:** VTpass wallet ≤ 0, below `VTPASS_WALLET_MIN_MINOR` (default ₦50,000 when VTpass), or below the 3-day average daily confirmed spend. Lookup failures **fail open** unless the last known balance was already paused and younger than 5× cache TTL (30s).

**Do**

1. Do **not** tell customers a naira figure. `/readyz` never includes one.
2. Check `vending_wallet_minor` on the **metrics** port. Top up the VTpass float if low.
3. Confirm sandbox vs live (`PROVIDER_ENVIRONMENT`). Do not send production money to sandbox.
4. After top-up, wait 30s for cache or bounce the API. `/readyz` should return `vending=ok`.
5. If wallet is healthy but still paused, check 3-day spend (large test buys in staging inflate the average).

**Do not** take payment while paused. Do not disable the pause by setting min to 0 in production.
