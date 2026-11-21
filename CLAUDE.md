# Meterly: instructions for Claude sessions

## Session protocol (mandatory)

1. **Start of session:** read `docs/HANDOFF.md` before doing anything else. It records the current state, what's left, open PRs and branches, and gotchas from earlier sessions. Then check it against reality (`git log`, open PRs). The handoff can be stale.
2. **End of session, or before your final report:** update `docs/HANDOFF.md`. Commit it with the session's work and push. A session isn't finished until the handoff is current. Update it in place (state, next steps, gotchas) and add one entry at the top of the session log. Keep it factual: what was done, how it was verified, what's unverified, what's blocked and why.
3. Record design decisions in `docs/DECISIONS.md`, not only in the handoff.

## Project rules

- Read `docs/HANDOFF.md` → "Invariants" before touching the orchestrator, store or providers. Every change must keep them, and tests must prove it.
- Run tests with the race detector, against **both** stores: `go test -race ./...` with `DATABASE_URL` set (see HANDOFF → "Running PostgreSQL locally"). The PostgreSQL tests skip silently without it.
- Never put secrets, real tokens or real meter numbers in the repo, tests or logs.
- Don't invent external partner APIs (vending, AMI/HES). Without documentation, leave an interface plus a TODO and ask.
- Don't include model identifiers in commits, PRs or code.
