# Partner emails (copy-paste)

Fill the brackets once, then send **in this order**. Do not wait for Ikeja before opening VTpass and Paystack accounts — those two you can do today.

| # | Who | Why | Send to |
|---|---|---|---|
| 1 | Ikeja Electric | Who runs AMI / HES (the no-keypad pipe) | `info@ikejaelectric.com` |
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

## 1. Ikeja Electric — ask who runs the smart-meter head-end

**To:** info@ikejaelectric.com  
**Cc:** vendorregistration@ikejaelectric.com  
**Subject:** Partnership request — remote credit to IE prepaid smart meters (Meterly)

If mail bounces, print and take the same letter to **178 Obafemi Awolowo Way, Alausa, Ikeja** (switchboard 0201-7000-250). `vendorregistration@` is IE’s vendor desk; `customercare@` is for customers — do not use that as the only To.

```
Dear Ikeja Electric Partnership / AMI team,

I am [YOUR NAME], [ROLE] of [COMPANY]. We are building Meterly: customers pay for prepaid electricity on their phone and the units appear on the meter, without typing a token — the same way airtime lands on a SIM.

We are not a meter manufacturer. We do not need hardware. We will collect payment through a CBN-licensed PSP (Paystack) and vend through a licensed aggregator (VTpass). What we need from Ikeja Electric is the path that already talks to your prepaid smart meters.

Please could you tell us:

1. Who operates the head-end / AMI for IE prepaid smart meters today (your AMI team, or a named MAP)?
2. Can a licensed vendor push credit to those meters and receive confirmation that the meter accepted it?
3. Can we read remaining units (kWh) for auto-recharge when the customer is low?
4. Who is the right integrations contact, and what document or NDA do you need from us?

We would like to start with one meter cohort in the IE network, prove credit on a real meter, and only then talk about scale.

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
