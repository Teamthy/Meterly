# Partner sandbox demo (B6)

End-to-end against a **local** Meterly API: create a sandbox partner, two-person float top-up, register a webhook, validate/quote/purchase, verify HMAC signatures, reconcile float.

```bash
# terminal 1 — MODE=dev enables FEATURE_PARTNER_API, FEATURE_PARTNER_FLOAT,
# in-process worker, mock vending/payments, and loopback webhook URLs.
MODE=dev go run ./cmd/api

# terminal 2
go run ./scripts/partner-demo
```

`API_URL` defaults to `http://127.0.0.1:8080`. Dev operators default to `operator:ada` / `operator:bola`.

Deterministic mock meters (see `internal/providers/mock`):

| Meter number   | Behaviour |
|---|---|
| `45012345678`  | Success (token vend) |
| suffix `0000`  | Vend fails; float refunded |
| suffix `8888`  | Vend UNKNOWN (no refund) |
| suffix `9999`  | Vend succeeded, response lost (UNKNOWN) |
| prefix `0`     | Validation fails |
