# Partner emails (copy-paste)

Fill the brackets once, then send **in this order**. Do not wait for Ikeja before opening VTpass and Paystack accounts — those two you can do today.

| # | Who | Why | Send to |
|---|---|---|---|
| 1 | IE Energy Lagos Limited **and** Ikeja Electric Plc | Who runs AMI / HES (the no-keypad pipe) | `info@ikejaelectric.com` |
| 2 | The MAP Ikeja names | API docs for remote credit + meter reading | they will give you the address |
| 3 | VTpass | Meter verify + licensed vend | Register first, then email |
| 4 | Paystack | Collect money + later auto-recharge | Dashboard first, then email |

Replace:

```
[YOUR NAME]
[ROLE]                 e.g. Founder
[COMPANY]              legal name on CAC
[PHONE]
[EMAIL]
[WEBSITE]              https://meterly-iota.vercel.app until you have meterly.ng
```

---

## 1. IE Energy Lagos / Ikeja Electric — ask who runs the smart-meter head-end

**To:** info@ikejaelectric.com  
**Cc:** vendorregistration@ikejaelectric.com  
**Subject:** Partnership request — remote credit to prepaid smart meters (Meterly × IE Energy Lagos / Ikeja Electric)

October 2025: LASERC licensed **IE Energy Lagos Limited** as Ikeja Electric’s successor **in Lagos**. Ikeja Electric Plc may still cover **Ogun**. Same Alausa house until they publish a new domain. Address **both** legal names. Do **not** email random `ienergyltd.com` — different company.

If mail bounces, print and take the same letter to **178 Obafemi Awolowo Way, Alausa, Ikeja** (switchboard 0201-7000-250). `vendorregistration@` is procurement — not the AMI door; Cc only. Not `customercare@` as the only To.

```
Dear Partnership / AMI team at IE Energy Lagos Limited and Ikeja Electric Plc,

I am [YOUR NAME], [ROLE] of [COMPANY]. We are building Meterly: customers pay for prepaid electricity on their phone and the units appear on the meter, without typing a token — the same way airtime lands on a SIM.

We are not a meter manufacturer. We do not need hardware. We will collect payment through a CBN-licensed PSP (Paystack) and vend through a licensed aggregator (VTpass). What we need is the path that already talks to prepaid smart meters on your network (your public remote-load page today is Mojec-only and still asks the customer to paste a token).

Please could you tell us:

1. Who operates the head-end / AMI for prepaid smart meters today (your AMI team, or a named MAP — Mojec, CIG/Conlog, NHC, other)?
2. Can a licensed vendor push credit to those meters and receive confirmation that the meter accepted it — not only that the head-end queued it?
3. Can we read remaining units (kWh) for auto-recharge when the customer is low?
4. What happens when Disco supply is off at the premises (we will not call that delivered)?
5. Who is the right integrations contact, and what document or NDA do you need from us?
6. For Lagos vs Ogun, which legal entity signs?

We would like to start with one meter cohort (Mojec on the Ikeja / IE Energy Lagos network), prove credit on a real meter, and only then talk about scale.

I can meet in Alausa or on a call this week. Phone: [PHONE]. Email: [EMAIL]. Product: [WEBSITE].

Thank you.
[YOUR NAME]
[ROLE], [COMPANY]
```

---

## 2. MAP / HES operator — send after Ikeja names them

**To:** (the email Ikeja gives you)  
**Subject:** Integrations request — remote credit + confirmation + balance (Meterly × [MAP NAME] × Ikeja Electric)

```
Dear [MAP NAME] integrations team,

Ikeja Electric pointed us to you as the operator of the prepaid smart-meter head-end on their network.

Meterly is a software vendor. Customers pay (Paystack); we obtain a licensed vend (VTpass); we need you to inject that credit so the customer never uses the keypad. We only treat a sale as delivered when the meter confirms — “accepted by the head-end” is not enough.

Please send, under NDA if required:

1. How we authenticate (mTLS, HMAC, OAuth, or IP allowlist).
2. How to push credit / inject an STS token for a meter, including the idempotency key.
3. How to query status (queued vs accepted vs meter-confirmed).
4. How the meter confirms (webhook we can host, or poll).
5. Error list: timeout, duplicate, unreachable, already credited.
6. Sandbox meters and a date for a witnessed test on one real IE meter.

We already have a signed HTTP gateway on our side. We will write a translator to your API. We will not invent endpoints.

Contact: [YOUR NAME], [PHONE], [EMAIL].

Thank you.
[YOUR NAME]
[ROLE], [COMPANY]
```

---

## 3. VTpass — meter verify + electricity vend

Do this today, even before Ikeja replies.

1. Register: https://vtpass.com/register  
2. Log in → **My Earning** → activate **API**  
3. Email below so they enable **electricity** and send sandbox + live keys  

**To:** support@vtpass.com  
**Cc:** ibukun.f@broadshift.com  
**Subject:** API merchant — electricity verify + vend (Meterly)

VTpass documents API support at support@vtpass.com and Skype `vtpass.techsupport`. Phone 09087482377 / 08161675187. Broadshift is the operating company.

```
Hello VTpass API team,

I am [YOUR NAME], [ROLE] of [COMPANY]. We have registered on vtpass.com ([EMAIL]).

Please provision this account as an API merchant for electricity, sandbox first, then live.

We will call only the documented endpoints:

- POST /api/merchant-verify  (meter number → customer name, address, prepaid/postpaid)
- POST /api/pay
- POST /api/requery

DisCos we need: ikeja-electric, eko-electric, abuja-electric, ibadan-electric, enugu-electric, portharcourt-electric, kano-electric, kaduna-electric, jos-electric, yola-electric.

Please send:

1. Confirmation that electricity is enabled on our merchant.
2. Sandbox api-key and secret-key.
3. Written confirmation we are authorised to vend those serviceIDs.
4. Any extra KYC you need (CAC, directors, settlement account).

We collect customer payment ourselves via Paystack. We will not use VTpass as a wallet for cards.

Phone: [PHONE]. Email: [EMAIL].

Thank you.
[YOUR NAME]
[ROLE], [COMPANY]
```

---

## 4. Paystack — money now, saved card later

Do this today.

1. Create a business: https://dashboard.paystack.com/#/signup  
2. Complete KYC (CAC, BVN, bank) so you get `sk_test_` immediately; live keys after activation  
3. In Settings → API Keys & Webhooks, set the webhook to `https://<your-api>/v1/payments/webhooks/paystack` once Render is up  
4. Send the email only if you want a named integrations person for **recurring electricity** (saved-card auto-recharge)

**To:** support@paystack.com  
**Subject:** Merchant for prepaid electricity — test keys + later authorization charges (Meterly)

```
Hello Paystack,

I am [YOUR NAME], [ROLE] of [COMPANY]. Our dashboard login is [EMAIL].

Meterly sells prepaid electricity. Customers pay us; we vend to the meter. We need:

Now
- Test secret key (sk_test_) and webhook secret
- Confirmation we may collect card and bank transfer for electricity units in Nigeria

Next (auto-recharge, not this week)
- Charge a saved authorization (POST /transaction/charge_authorization) when the customer has opted in
- Each charge creates its own Paystack reference; we always Verify before we vend
- Caps, 12-hour heads-up, and one-tap skip sit on our side

We never store PAN. Authorization codes are encrypted at rest, only after explicit consent.

Please confirm the account is in good standing for energy / bill payments and flag anything extra compliance needs.

Phone: [PHONE]. Product: [WEBSITE].

Thank you.
[YOUR NAME]
[ROLE], [COMPANY]
```

---

## After you send

- Ikeja may take weeks. VTpass + Paystack keys should arrive in days. Paste those on Render (`docs/DEPLOY.md`) so the app can verify meters and take sandbox payment even while AMI is pending.
- When Ikeja names a MAP, send email 2 the same day. Forward their API PDF to engineering — do not implement from memory.
- Do not tell customers “no token” until one real IE smart meter has confirmed credit.

---

## What they will ask for, and what it costs

Published numbers below. Anything marked **quote** is not public — they will name a price after KYC.

### Paystack (money) — public price

No setup, no monthly. [Local Nigeria pricing](https://support.paystack.com/en/articles/2130306): **1.5% + ₦100**, ₦100 waived under ₦2,500, **capped at ₦2,000**. International cards 3.9% + ₦100. Settlement typically next working day.

They will ask: CAC (BN or LTD), BVN, director ID (NIN/passport), Nigerian bank account in the business name, address. Test keys are free before KYC. You do **not** need your own CBN licence.

### VTpass (verify + vend) — wallet + commission

No published API join fee. You **pre-fund a wallet**; every vend debit that wallet. Commission comes back (IKEDC API historically ~**1.0–1.5%** — confirm on your dashboard; it varies by Disco and meter type).

They will ask: account on vtpass.com, KYC (name, phone, ID), then API activation, then electricity enabled. Live keys after they like the account. Sandbox is free.

**Working capital is the real cost.** Paystack pays you T+1. VTpass takes naira **now**. If you vend ₦2m in a day you need ~₦2m sitting in VTpass.

### Unit economics (example, not a quote)

Customer buys **₦5,000** of Ikeja units:

| | ₦ |
|---|---|
| Paystack fee (1.5% + 100) | −175 |
| VTpass commission (~1.5%) | +75 |
| **Net before any service fee** | **−100** |

Electricity resale is thin. Either a small convenience fee, or you eat it for growth. Do not assume ₦5,000 in / ₦5,000 profit.

### Ikeja Electric (AMI intro) — do not pay the wrong fee

Their **vendor registration** (Dun & Bradstreet, Zenith `IE Vendor Operations`, conflict-of-interest form) is for companies that **sell cables/meters/services to IE**. CAC, profile, previous jobs, bank reference, proof of address, tax clearance for higher categories. That is **procurement**, not “let us push credit to meters.” Do not pay that fee unless they explicitly say the AMI partnership requires it.

For AMI they are likely to ask:

- Who you are (CAC, directors, TIN)
- That payment is via a CBN-licensed PSP
- That vending is via a licensed aggregator (or their own collection partner)
- NDA
- Cybersecurity / data-protection note (NDPA)
- A pilot on **one meter brand** (IE’s own site: remote loading today is **Mojec only**, and the customer still pastes a token at [ikejaelectric.com/remote-token-loading](https://www.ikejaelectric.com/remote-token-loading/))

Cost: **quote**. Could be ₦0 for a pilot that brings them prepaid revenue. Could be a commercial contract. Unknown until they reply.

### MAP / HES (Mojec, CIG/Conlog, NHC, …) — quote

Typical ask: NDA, CAC, insurance, SLA, mTLS or IP allowlist, sandbox then one witnessed meter, maybe a revenue share or per-push fee. Integration fee **₦0 to large** — they will not publish it. Budget time (weeks–months), not a SKU.

### NERC / legal (not a partner email)

You are **not** applying for a Disco, MAP, or generation licence. Counsel must still say whether Meterly is only a **software merchant** (Paystack + VTpass) or a **Collection Service Provider** (NERC 2025: CBN-licensed CSPs, Disco agreement, **₦100,000** registration in press reports). Do not spend millions on a trading licence because someone used the word “aggregator.”

### Cheap this month vs expensive later

| This month (known) | Later (unknown) |
|---|---|
| CAC if you do not have it | Ikeja/MAP commercial |
| Paystack 0 setup | NERC CSP if counsel says you are one |
| VTpass 0 setup + wallet float | NDPA DPIA / lawyer |
| Render + Vercel hosting | |

Float + Paystack fees are the costs that start the day the first live vend happens. AMI is the cost that starts when Ikeja answers.
