# Key rotation

1. Generate 32-byte `TOKEN_ENC_KEY_NEW` (64 hex). Keep `METER_HMAC_KEY` (hashes are not rotated).
2. Dry-run on a restore-drill database: [restore.md](restore.md).
3. `TOKEN_ENC_KEY=<old> TOKEN_ENC_KEY_NEW=<new> KEY_VERSION=v2 DATABASE_URL=... go run ./cmd/rekey` (or the distroless `rekey` binary).
4. Point the API at `TOKEN_ENC_KEY=<new>` and `KEY_VERSION=v2`. Bounce. Old key is no longer needed for reads.
5. Audit: rekey report counts only, no plaintext. If decrypt fails mid-batch, stop — remaining rows still on old version.
