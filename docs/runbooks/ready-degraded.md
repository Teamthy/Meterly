# ReadyDegraded

**Symptom:** `/readyz` JSON `status=degraded` (HTTP 200). Distinct from HTTP 503 (DB down / pending migrations).

**Branch**

- `vending=paused` → [vending-paused.md](vending-paused.md)
- `service=paused` → [service-paused.md](service-paused.md)
- HTTP 503 `unavailable` → database or pending migrations. Do not serve writes. See [outage.md](outage.md).
