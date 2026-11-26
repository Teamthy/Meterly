# 100 likely edge cases — and how we build against them

Meterly fails in the dark, with naira, on other people’s radios. The **build** is not 100 extra screens. It is: **invariants in the orchestrator**, **honest UI**, **runbook**, **do not invent the HES**.

Legend: **IN** = already in code · **OPS** = `docs/RUNBOOK.md` · **GAP** = still to build or partner · **OFF** = frozen until AMI/legal

---

## How we build (the pattern)

| Class of failure | Weapon |
|---|---|
| Crash / timeout / double click | `mutate` + claim flag + idempotency key. Timeout = **UNKNOWN**, never FAILED |
| “Did it vend?” | Requery by **stored** `request_id`. Re-pay only on provider **NOT_FOUND** |
| “Is it on the meter?” | ACCEPTED ≠ CONFIRMED. Hide STS while REMOTE |
| “Give me my money” | Refund only after **definitive** vend failure |
| Human in the dark | Runbook 2h/1h. Never refund a maybe |
| Partner lies | Do not code around missing docs |

If a row says GAP, the build is the **smallest** thing in the last column — not a rewrite.

---

## Payment (1–15)

| # | Edge | Failure | Against it |
|---|---|---|---|
| 1 | Double tap Pay | Two checkouts, two vends | **IN** `Idempotency-Key` on create; same key returns same txn |
| 2 | Paystack webhook twice | Double vend | **IN** `OnPaymentEvent` is progress, not a second pay; verify server-side |
| 3 | Webhook before our commit | Orphan Paystack ref | **IN** new ref per init; only committed ref is shown |
| 4 | Client tampers amount | Underpay, still vend | **IN** verify amount == quote else **MANUAL_REVIEW** |
| 5 | Pay after `PAYMENT_EXPIRY` | Money in, we dropped txn | **IN** late pay → review, never silent drop |
| 6 | Abandoned checkout | Dangling PayPending | **IN** expiry sweeper marks unpaid FAILED only if PSP says unpaid |
| 7 | Paystack timeout after debit | We think unpaid, they charged | **IN** ambiguous → UNKNOWN; verify by ref; never new ref for same attempt |
| 8 | `send_otp` / 3DS on saved card | AutoPower hangs | **IN** ChargeSaved pending → VerifyPayment; **OFF** live AutoPower |
| 9 | Chargeback after token typed | We lost naira, they have light | **OPS** evidence pack; do not re-vend; **GAP** Paystack `charge.dispute` → pause user |
| 10 | Card-testing (₦500 × 20 meters) | Stolen card | **IN** write rate limit; **GAP** velocity cap: N new meters / day / card |
| 11 | Bank transfer T+1, user retries | Two payments | **IN** one txn one pay record; second init is new txn only with new key |
| 12 | Kobo vs naira | 100× overcharge | **IN** money.Minor; VTpass amount is naira at adapter edge |
| 13 | Refund while vend UNKNOWN | Double: refund + token | **IN** refund only if VendFailed definitive; review otherwise |
| 14 | Email missing | Paystack init 400, half txn | **IN** API requires email before create |
| 15 | Settlement weekend, wallet empty | Monday vends fail | **OPS** Friday float ≥ 2 days GMV; **GAP** alert when VTpass balance < threshold |

---

## Vending (16–32)

| # | Edge | Failure | Against it |
|---|---|---|---|
| 16 | HTTP 500 after VTpass applied | Retry = second token | **IN** `request_id` stored at **claim**; requery; never `NewVendKey` on Resume |
| 17 | Clock wrong, `request_id` date | VTpass reject / other day’s txn | **IN** Lagos formatter; **GAP** NTP on Render |
| 18 | Code `000` but status pending | We show success, no token | **IN** inspect `content.transactions.status`; `000` alone ≠ success |
| 19 | Code `014` (id exists) | Naïve retry double-vend | **IN** requery, not second pay |
| 20 | Code `015` requery NOT_FOUND | Stuck forever | **IN** safe to retry pay with **new** key only then |
| 21 | `099` for hours | Customer in dark | **IN** backoff → MANUAL_REVIEW; **OPS** tell them wait, don’t pay twice |
| 22 | Delivered, empty `purchased_code` | No token, we mark success | **IN** prepaid delivered without token stays PENDING |
| 23 | VTpass wallet 0 | All vends fail | **OPS** E; **GAP** `/readyz` degrade + banner “vending paused” |
| 24 | Unknown Disco / Benin | Invented serviceID | **IN** local map only; reject ACME |
| 25 | `Can_Vend=no` | Pay then fail | **GAP** plumb verify `Can_Vend` into quote 422 before pay |
| 26 | Min purchase (e.g. ₦500/₦1000) | Pay ₦400, vend reject | **GAP** use verify `Min_Purchase_Amount` on quote |
| 27 | Arrears taken from amount | User thought ₦5k units, got debt | **GAP** show arrears on confirm (VTpass field exists) |
| 28 | Prepaid vs postpaid mix | Wrong variation_code | **IN** verify tries both; persist `MeterType` |
| 29 | Meter blocked / 016 | Definitive fail after pay | **IN** VendFailed + refund path |
| 30 | Requery storm | Rate-limit / ban | **IN** Resume backoff 30s–30m, max 12 then review |
| 31 | VTpass status `initiated` forever | Same as 21 | **IN** PENDING; **OPS** |
| 32 | Disco rename (IE Energy Lagos) | serviceID 404 | **OPS** outreach; **GAP** config map, not a code guess |

---

## Remote / no-keypad (33–50)

| # | Edge | Failure | Against it |
|---|---|---|---|
| 33 | ACCEPTED, never CONFIRMED | We celebrate, meter empty | **IN** credit_on_meter only on CONFIRMED; UI says so |
| 34 | Feeder dead | Pay, still dark | **IN** not delivered; **OPS** B; copy on buy/receipt |
| 35 | UIU on generator | Remote load fails | **OPS** + copy (wall socket, Disco supply) |
| 36 | HES timeout after accept | Unknown if queued | **IN** GetCreditStatus by `credit:<txn>`; UNKNOWN |
| 37 | HES later NOT_FOUND for ACCEPTED | Lost command | **IN** MANUAL_REVIEW, **never auto re-send** (double-credit) |
| 38 | Webhook “confirmed” for other txn | Wrong meter lit | **IN** webhook is hint; always GetCreditStatus by our key |
| 39 | Webhook replay | Double complete | **IN** mutate guards; already CONFIRMED → skip |
| 40 | Token typed **and** remote push | Double credit | **IN** hide STS while REMOTE; FallbackToToken only after confirmed **remote failure** |
| 41 | Operator fallback too early | Token + silent credit | **IN** admin FallbackToToken checks; **OPS** GetCreditStatus first |
| 42 | Non-Mojec claimed as AMI | Push into void | **IN** cohort attest in `/admin`; mock `7…` is not production |
| 43 | Cohort prefix too wide | Keypad meters marked remote | **OPS** attest with evidence; **GAP** require MAP list not just digits |
| 44 | Radio down, supply on | ACCEPTED stuck | **IN** Resume; then fallback after failure confirmed |
| 45 | Confirm 6 hours later | User already refunded | **IN** we do not refund ACCEPTED; **OPS** never refund maybe |
| 46 | Customer pays twice because dark | Two credits when feeder back | **OPS** “do not pay twice”; **GAP** block second buy on same meter while REMOTE pending |
| 47 | Mock HES in staging | False “no keypad” ads | **IN** `REMOTE_PROVIDER=none` outside docs; **OPS** launch rule |
| 48 | Partner idempotency weaker than ours | Double push | **IN** our key `credit:<uuid>`; **GAP** translator must send it |
| 49 | Meter firmware rejects inject | FAILED remote | **IN** FAILED + operator token fallback |
| 50 | LASERC entity vs Ikeja Plc | Contract with wrong name | **OPS** `OUTREACH.md` both names |

---

## Meter identity (51–62)

| # | Edge | Failure | Against it |
|---|---|---|---|
| 51 | Typo one digit | Neighbour gets units | **IN** verify shows Disco name; **OPS** cannot pull back |
| 52 | Right number, wrong Disco | Verify fail or wrong biller | **IN** serviceID from Disco code; invalid meter |
| 53 | 11 vs 13 digits / leading zero | Hash mismatch, duplicate meters | **GAP** normalise digits-only before hash |
| 54 | Meter replaced, old number still vends | Money on dead box | **GAP** re-verify before each pay (stale ValidatedAt) |
| 55 | Postpaid account vs prepaid meter | Wrong product | **IN** type loop; persist type |
| 56 | Tenant buys landlord meter | Allowed (product) / abuse | **IN** PURCHASE grant ≠ ownership; households |
| 57 | Household member without role | 403 or IDOR | **IN** grants; never mint MANAGE |
| 58 | Self-register MANAGE | Fake owner | **IN** self ≤ PURCHASE |
| 59 | Same meter two users | Fight over token reveal | **IN** token = purchaser only |
| 60 | Sandbox meter `1111111111111` in prod | Fake success | **GAP** reject known sandbox numbers when `PROVIDER_ENVIRONMENT=LIVE` |
| 61 | Verify prepaid, later postpaid | Wrong variation | **GAP** re-verify if ValidatedAt > 7d |
| 62 | Name “TEST METER” ignored | Fat-finger | **IN** confirm UI; **GAP** require tap “this is my name” |

---

## Token / dark house (63–72)

| # | Edge | Failure | Against it |
|---|---|---|---|
| 63 | Wrong 20 digits (CHECK) | Blame us | **OPS** brand-specific load notes; not a refund |
| 64 | Token already used | Second type | **OPS** “already loaded”; don’t vend again |
| 65 | Token in WhatsApp group | Theft | **IN** no token in logs; **OPS** don’t paste as company |
| 66 | Resend spam | Cost + leak | **IN** 5 sends/day; audited ≤3 last-token on USSD |
| 67 | Step-up expired at reveal | 403 STEP_UP | **IN** security engine 15m + IP bind |
| 68 | SMS to old SIM | Stranger has token | **IN** verified contact; gift SMS **OFF** |
| 69 | Feature phone, no app | Can’t buy | Frozen USSD; **OFF** |
| 70 | Illiterate / 20 digits | Can’t type | Product is no-keypad; token is fallback — don’t “simplify” STS |
| 71 | Screenshot in support chat | Agent sees token | **IN** agent never includes STS; E2E for humans |
| 72 | Copy metadata vs raw token | We stored plaintext | **IN** COPY is metadata only |

---

## Auto-renew (73–82) — **OFF** until AMI + legal

| # | Edge | Failure | Against it |
|---|---|---|---|
| 73 | Low kWh glitch fires twice | Double charge | **IN** 24h cooldown; unique (rule, scheduled_for) |
| 74 | Tick race two workers | Double ChargeSaved | **IN** pre-commit `apchg:<run_id>` then verify, never second charge |
| 75 | THRESHOLD + schedule same night | Two buys | **IN** rule mode; **GAP** one in-flight txn per meter |
| 76 | 3× card fail | Angry customer | **IN** pause 3-strike; heads-up 12h + skip |
| 77 | Skip after charge dispatched | Too late | **IN** skip only unstarted SCHEDULED |
| 78 | Cap hit | Surprise skip | **IN** SKIPPED_CAP; notify |
| 79 | Reading 48h stale | Buy with 80 kWh left | **IN** confirmed remote reading required; **GAP** max age on reading |
| 80 | Auto while feeder dead | Charged, still dark | **OFF** + catch-22 policy; don’t fire if last remote unread |
| 81 | Meter access revoked | Still charging | **IN** pause on RemoveSavedMeter / card delete |
| 82 | Consent text changed | Legal | **IN** consent_version stored; new save required |

---

## Auth / abuse (83–90)

| # | Edge | Failure | Against it |
|---|---|---|---|
| 83 | Stolen `mls_` cookie | Read tokens | **IN** step-up + IP HMAC bind + revoke others |
| 84 | Delete last passkey | Lockout | **IN** DENY last passkey |
| 85 | `AUTH_MODE=dev` in prod | Anyone is ada | **IN** refused outside MODE=dev |
| 86 | Redis down, 2 replicas | Rate limit fail-open | **IN** documented; **GAP** ready-degraded metric |
| 87 | WebAuthn origin mismatch | Step-up fail | **IN** origin allowlist |
| 88 | Step-up with 0 passkeys | Empty challenge | **IN** ErrNoPasskeys, no PutChallenge |
| 89 | Staff JWT without MFA | Admin refunds | **IN** MFA AMR gate on admin (OIDC) |
| 90 | IDOR other user’s txn | Token leak | **IN** purchaser-only token; 404 not 403 on pots/orgs |

---

## Platform (91–96)

| # | Edge | Failure | Against it |
|---|---|---|---|
| 91 | API crash mid-vend | Double or drop | **IN** claim then call; Resume |
| 92 | Render spin-down misses webhook | Pay stuck | **GAP** always-on instance + Paystack retry; payment-check button exists |
| 93 | `DATABASE_URL` port 6543 | prepared statement errors | **OPS** `DEPLOY.md` session **5432** |
| 94 | Edit applied migration | Checksum refuse | **IN** migrate runner |
| 95 | Log meter number | NDPA | **IN** no bodies in logs; correlation id |
| 96 | Worker with mocks | Split brain | **IN** cmd/worker refuses mocks |

---

## Business / human (97–100)

| # | Edge | Failure | Against it |
|---|---|---|---|
| 97 | “I paid still dark” in court | We marked success | **IN** three dimensions; ACCEPTED ≠ delivered; **OPS** |
| 98 | Convenience fee + VAT | Illegal invoice | **OPS** ₦0 fee until 1k payers; `LEGAL.md` counsel |
| 99 | VTpass outage + Twitter | Mob refunds | **OPS** UNKNOWN script; **GAP** status.meterly banner |
| 100 | Success theatre (fake AMI) | Brand death | **LAUNCH.md** no “no keypad” ad until 1 real confirm |

---

## Build order (gaps only)

Do **not** start 100 tickets. This order:

1. **Quote gates** — `Can_Vend`, `Min_Purchase_Amount`, arrears on confirm (25–27). Stops paying for a vend that cannot happen.
2. **One in-flight buy per meter** (46, 75). Stops panic double-pay in the dark.
3. **Normalise meter digits** + reject live sandbox numbers (53, 60).
4. **Re-verify stale meters** (54, 61).
5. **VTpass wallet / Paystack dispute hooks** (15, 9, 23).
6. **Confirm-name tap** (62).
7. AutoPower gaps only when the flag turns on (79–80).

Everything else is **already IN** or **OPS**. Building more product instead of (1)–(3) is how we fail the first live night.
