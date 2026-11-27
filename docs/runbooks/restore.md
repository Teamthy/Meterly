# Restore / PITR

**RPO** (pilot): 5 minutes — Supabase PITR / WAL. **RTO** (pilot): 60 minutes to a passing `cmd/integrity`.

1. Create a **scratch** database (Supabase PITR restore to a new project, or `createdb`). Never restore over production in place on the first try.
2. `RESTORE_DATABASE_URL=... DUMP=optional.pgdump ./scripts/restore-drill.sh`
3. Script runs `cmd/migrate` then `cmd/integrity` (trial balance 0, snapshot, 24h window). Integrity prints counts, never tokens.
4. Point a staging Render service at the scratch URL; run `scripts/canary.sh`.
5. Cut over `DATABASE_URL` only after integrity OK. See `docs/DEPLOY.md` PITR.
