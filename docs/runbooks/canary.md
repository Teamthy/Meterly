# Staging canary

Hourly GitHub workflow `canary.yml` hits `STAGING_API_URL` `/healthz` + `/readyz`. If sandbox keys are present it runs `go run ./cmd/certify` (P1 live-sandbox). Failure emails GitHub; page if production-bound.

Local: `STAGING_API_URL=https://... ./scripts/canary.sh`

Skip (exit 0) when `STAGING_API_URL` is unset so CI without secrets stays green.
