# Legal pack (draft — counsel must sign)

Not legal advice. Public copies: `/legal/terms`, `/legal/privacy`. Do not advertise “we sell electricity in Nigeria” until a lawyer has read this and the NDPA bits.

---

## What Meterly is (say this in every contract)

Meterly is software. We are **not** a Disco, not a MAP, not a CBN PSP, not NERC-licensed to distribute power.

- Money: **Paystack** (CBN-licensed PSP). We never hold PAN.
- Units: **VTpass** (or later a Disco vend API). Licensed vend.
- No-keypad: a **MAP/HES** we do not operate. We only call delivered on **meter confirm**.
- Auto-renew: **off** until `FEATURE_AUTOPOWER` and counsel say yes.

---

## Before public launch (checklist)

- [ ] CAC (BN or LTD) matching Paystack settlement account
- [ ] Terms + privacy on the site (drafts shipped; counsel edit)
- [ ] NDPA: lawful basis (contract + consent for cards), retention, DPO contact mailbox
- [ ] Written opinion: merchant vs NERC **Collection Service Provider**
- [ ] Paystack merchant T&Cs accepted
- [ ] VTpass merchant T&Cs accepted
- [ ] No “no token in Nigeria” in ads
- [ ] Insurance quote: cyber + professional indemnity (buy when live volume exists, not before first vend)

Auto-renew later: explicit mandate text, skip, cap, CBN/card-scheme recurring rules — already in the engine; still needs counsel.

---

## NDPA 2023 — what we already do in code

- Meter numbers encrypted; HMAC for lookup; masked in UI
- Tokens encrypted; purchaser-only read; not in logs/URLs
- Security events: action/outcome/IP HMAC, not raw IP
- Contacts encrypted when stored for notify
- Gift SMS off

Still needed from humans: privacy policy, cookie/localStorage notice (passkeys, auto-renew prefs, E2E chat keys), deletion request path (`/support` is enough at launch if you actually delete).

---

## Insurance (when, not now)

Not a launch blocker. Get quotes after 20 live vends: cyber, PI, maybe crime for wallet float. Do not buy a MAP licence to look serious.

---

## Counsel questions (paste)

1. Are we a NERC CSP if Paystack collects and VTpass vends?
2. VAT on a ₦100 convenience fee?
3. Can we auto-charge a saved Paystack authorization for electricity with the consent string we already store?
4. Lagos: contract with **IE Energy Lagos Limited** vs **Ikeja Electric Plc** vs both?
5. NDPA DPIA required at our volume?
