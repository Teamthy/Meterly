# ProviderErrors

`provider_calls_total{outcome="definitive"}` climbing.

| Provider | Do | Do not |
|---|---|---|
| Paystack | Verify by reference. Chargebacks: P3 disputes, do not re-vend. | Refund blindly; retry charges |
| VTpass | Requery `request_id`. 099 = UNKNOWN. | Invent a new request_id for the same txn |
| SMS/email | Check `notification_deliveries_total` | Put tokens in a manual SMS |

Invariant: timeout = UNKNOWN, never FAILED. Refund only after a **definitive** vend failure.
