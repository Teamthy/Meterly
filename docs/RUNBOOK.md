# Runbook — 2am, UNKNOWN, wrong meter, money

Phone on until there is a hire. Use `/admin` (search txn) and `/admin/support`. Never log raw tokens or meter numbers.

**Invariant:** timeout = UNKNOWN, never FAILED. Refund only after a **definitive** vend failure. Remote ACCEPTED ≠ delivered. Do not re-send a vend unless the provider has **no record**.

---

## A. Payment succeeded, customer has no light

1. Open txn in `/admin`. Read **three dimensions**: payment, vending, delivery.
2. Branch:

| You see | You do | You tell the customer |
|---|---|---|
| Payment PENDING | Wait for webhook / `payment-check`. Do not vend by hand. | “Payment not confirmed yet.” |
| Payment SUCCESS, vending PENDING/UNKNOWN | Wait for Resume (worker). If stale > 15 min, admin **reconcile**. Do **not** click pay again. | “We’re confirming with the Disco. Do not pay twice.” |
| Vending CONFIRMED, delivery TOKEN | Reveal token (step-up if passkey). SMS/email if they ask (own contact only). | 20 digits + how to type on **their** meter brand |
| Vending CONFIRMED, delivery REMOTE ACCEPTED | Do not release token yet. Ask: is Disco supply **on**? UIU in wall socket? | “Units are at the meter network. They land when supply is on. If still dark after supply returns, we retry or release a token.” |
| REMOTE CONFIRMED / credit_on_meter | Done | “Meter confirmed. No token.” |
| Vending FAILED (definitive) | Refund path in admin. Ledger already expects this. | “Vend failed. Refund started. 1–3 working days via Paystack.” |
| Amount mismatch / late pay | Leave MANUAL_REVIEW | “With an operator. We will not guess.” |

3. **Never** paste a token into WhatsApp as the company. Customer reveals in-app or we send through the share endpoint.
4. If they paid twice: second txn — if vend never claimed, expire/fail unpaid rules; if both vended, do not refund blindly (two credits may be on the meter).

**Reply window:** 2 hours night, 1 hour day, even if the answer is “still UNKNOWN.”

---

## B. Feeder dead / catch-22

Customer: “I paid, still dark, NEPA is off.”

- Remote cannot confirm. That is expected.
- Do not mark delivered.
- When supply returns: Resume / HES status. If CONFIRMED, done. If HES never took it and vend has a token, operator **FallbackToToken** only after GetCreditStatus is NOT credited.
- If they have a keypad and supply is off, a token still does nothing until supply is on. Say that.

---

## C. Wrong meter / “I meant mum”

1. Name on verify must have been shown at checkout. If they ignored it, we still help but we may not refund.
2. If **our** vend has no token delivered and provider NOT_FOUND → safe to refund.
3. If token was typed or remote CONFIRMED on the wrong meter → **cannot pull units back.** Offer: we help the other household; we do not steal from Disco.
4. New meter + first buy: watch for card-testing (many small pays). Admin can disable via ordinary Paystack dispute flow; pause saved cards.

Chargeback: do not vend again. If already vended, fight with Paystack evidence (verify + meter name + time). Three-strike AutoPower pause already exists when live.

---

## D. Stolen account / passkey

- Customer: `/security` revoke other sessions, step-up.
- Last passkey cannot be deleted (engine).
- Staff: do not impersonate with `AUTH_MODE=dev` in staging/prod (`AUTH_MODE=dev` is refused outside `MODE=dev`).

---

## E. Weekly money close (Friday)

1. Paystack dashboard: successful charges vs refunds.
2. VTpass wallet: opening, vends, commission, closing.
3. Meterly admin: txn count by payment/vend/delivery.
4. They must **tie**. Difference = UNKNOWN pile + fees (~₦100/txn if no convenience fee).
5. Top up VTpass so balance ≥ 2× yesterday’s GMV before Friday night.

---

## F. Who to ping

| Failure | Ping |
|---|---|
| Paystack verify/webhook | Paystack support + our logs (`X-Correlation-ID`) |
| VTpass 099/requery loop | support@vtpass.com + request_id (not the token) |
| HES / remote | MAP named by IE — not Twitter |
| Customer abuse | Document, do not argue units off a meter |

Founder owns all of these until a hire. No “the intern will watch Slack.”

Likely failures (100) and what is already IN vs GAP: `docs/EDGE_CASES.md`.
