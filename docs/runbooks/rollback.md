# Rollback

1. Render: deploy previous image (Git SHA). `preDeployCommand: /migrate` is **forward-only**. Do not write down-migrations.
2. If the bad release needed a new migration that already applied, rolling back the binary may be incompatible — stay forward, feature-flag off (`PILOT_MODE`, `FEATURE_*`), or pause.
3. Web: Vercel previous deployment. Keep `METERLY_API_ORIGIN` pointed at a compatible API.
4. Never `git push --force` to `main`.
