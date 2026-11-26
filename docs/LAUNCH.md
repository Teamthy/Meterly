# Launch operating system

Founder (2026-11-26): **we are not backing out.** No kill date. Token is fallback. No-keypad + auto-recharge is the product.

This file attends to everything that was still open. Details live in `RUNBOOK.md`, `LEGAL.md`, `OUTREACH.md`, `BUDGET.md`, `DEPLOY.md`.

---

## 1. Catch-22 (remote load needs power)

Ikeja’s own remote load says: **Disco supply must be on at the premises** or the push does not land. The person in the dark is the one who needs us.

**Policy (do not water down):**

1. We still **pay → vend → push**. Licensed units exist even if the radio cannot hear.
2. If the meter does not **confirm**, we do **not** call it delivered. Status stays ACCEPTED / PENDING / UNKNOWN.
3. If the feeder is dead, the customer gets an honest line: *units are waiting; they land when Disco supply is back. If you have a keypad, here is the token as fallback after an operator confirms the HES did not already credit.*
4. We **never** promise light while NEPA is off. Generators and inverters are not our meter.
5. UIU must be on the wall socket on Disco supply (not an extension, not a gen) — same as Ikeja’s own Mojec notes.
6. Auto-retry is Resume in the worker, not a human typing. Humans only step in on the runbook.

Copy is on Buy Power and the receipt. Do not “fix” this with a fake delivered state.

---

## 2. Who launch is for (first 200)

Not “Nigeria.” Not Meta.

| Order | Who | Why |
|---|---|---|
| 1 | Households in **Ikeja Electric / IE Energy Lagos** with a **Mojec** (or later attested AMI) prepaid meter | Only cohort IE already remotely loads |
| 2 | Same person buying for **mum / shop** on that network | Households, not a national wallet |
| 3 | Keypad STS on the same DisCos | Fallback so we can take Paystack+VTpass money while AMI is pending |

**How you find them:** meter face / UIU says Mojec (or Holley/Conlog/NHC — do not claim remote until `/admin` attests the cohort). Friends in Akowonjo, Ikeja, Ikorodu, Oshodi, Shomolu, Abule Egba. Not a 10k ad buy first.

**First 20 live vends** (any meter, even token) before ads. **First 1 remote confirm** before the words “no keypad” on an ad.

---

## 3. We do not stop

No “if no AMI in 9 months, shut down.” We keep:

- VTpass + Paystack live
- Outreach to **IE Energy Lagos Limited** and **Ikeja Electric Plc** (see §8)
- Honest token fallback
- Auto-renew as coming-soon until AMI reading + legal

We **do** refuse to lie in marketing if AMI is slow.

---

## 4. Who is awake at 2am

Until 10k **payers**: **the founder**. Phone on. `/admin` + `/admin/support` + `docs/RUNBOOK.md`.

After that: one support hire (budgeted in `BUDGET.md`). No 24/7 agency until GMV pays for it.

SLA we will say publicly:

- Payment taken, token/credit **confirmed**: seconds–minutes (provider)
- Payment taken, vend **UNKNOWN**: we reply within **2 hours** night, **1 hour** day, with wait / token / refund **only if vend definitely failed**

---

## 5. Wrong meter / stolen card

See `RUNBOOK.md` § fraud.

Product already: verify name from VTpass before pay; purchaser-only token; step-up to read token.

**Rules:** first purchase to a new meter capped (quote min is fine); no gift SMS in prod (`FEATURE_GIFT_SMS=false`); chargebacks pause AutoPower (engine already pauses on card delete/fail). Do not auto-refund a vend that may have succeeded.

---

## 6. Money after the first sale

| Rule | Default |
|---|---|
| Convenience fee | **₦0** until 1,000 paying customers, then consider ₦100–150 (VAT-inclusive if counsel says VAT applies) |
| Eat the ~₦100 Paystack leak | Yes, until then |
| VTpass wallet | Keep ≥ **2 days** of trailing GMV, extra before weekends |
| Books | Paystack settlement vs VTpass wallet vs “units sold” — weekly reconcile (`RUNBOOK.md`) |
| Auto-renew | Not charging. Prefs on device only |

---

## 7. Legal

Drafts in `docs/LEGAL.md` and public `/legal/terms`, `/legal/privacy`. **Counsel reviews before public “we sell electricity.”** We are not the Disco. Paystack is the PSP. VTpass is the vend. NDPA applies.

---

## 8. Lagos Disco name

October 2025: **LASERC** licensed **IE Energy Lagos Limited** as the Lagos successor to Ikeja Electric, and **Excel Distribution Company Limited** as successor to Eko. Ikeja Electric Plc may still matter for **Ogun**. VTpass `serviceID` is still `ikeja-electric` until they document a change.

**Send the partnership letter to both names** at the Alausa address / `info@ikejaelectric.com`. Do **not** use random `ienergyltd.com` — different company.

Cc later: LASERC only for a collection-licence question, not for AMI API.

---

## 9. Team (three jobs, one person until hired)

| Job | Who now | Hire when |
|---|---|---|
| Alausa / MAP sitting | Founder | Advisor (power) if meetings stall 60 days |
| 2am UNKNOWN / refunds | Founder | Support at ~1k weekly tickets or 10k payers |
| Product / git | This repo | Not a second engineer until AMI docs exist |

---

## 10. Brand honesty

Say: **compatible smart meters.** Never: “all meters, no token.”

Footer, metadata, buy card, and legal pages already hold this line. Ads must.

---

## 11. Frozen until AMI + legal

Do not ship as live product:

- Live AutoPower charges (`FEATURE_AUTOPOWER` stays false)
- Gift SMS, USSD, WhatsApp, Split the Meter, Power Desk, Pay-for-Me in production flags
- Native apps
- Invented HES/Benin Electric `serviceID`

Code may stay in the repo. UI: Auto is “coming soon”; More items that are frozen say **later**.

---

## 12. Hosting (only remaining engineering)

`docs/DEPLOY.md`. Human: Render `DATABASE_URL` **:5432**, `TOKEN_ENC_KEY`, `METER_HMAC_KEY`, migrate through **0021**, Vercel `METERLY_API_ORIGIN`. Then paste VTpass + Paystack sandbox keys, `MODE=staging`.

Without this, live keys have nowhere to go.
