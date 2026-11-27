# WorkerHeartbeatStale

`worker_heartbeat_unix` is 0 or older than 120s. Resume, outbox, notifications, recon, and AutoPower are not running.

1. Render: API with `RUN_WORKER=true`, or worker service. Metrics port must be scraped (`METRICS_ADDR`, not `off`).
2. If the process is up but loops panicked, logs `worker loop panic`.
3. Purchases can still be created; they will stall at PENDING/UNKNOWN until Resume is back. Do not click pay again.
