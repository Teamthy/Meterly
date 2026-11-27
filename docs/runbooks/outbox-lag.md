# OutboxLag

Oldest unpublished outbox event > 2 minutes.

1. Is the worker heartbeating? [worker-heartbeat.md](worker-heartbeat.md)
2. `RUN_WORKER=true` on the API Blueprint, or a dedicated worker process.
3. Poison event: worker logs the error, skip identifiers. Fix consumer, do not truncate `outbox`.
4. After recover, lag gauge should fall on the next metrics refresh.
