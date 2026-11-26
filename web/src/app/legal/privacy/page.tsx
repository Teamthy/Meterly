export default function PrivacyPage() {
  return (
    <article className="mx-auto max-w-2xl space-y-6 rounded-[24px] border border-stone-200/80 bg-paper p-6 text-sm leading-relaxed text-stone-700 shadow-[0_20px_60px_rgba(0,0,0,0.12)] sm:p-8">
      <p className="text-[11px] font-semibold uppercase tracking-[0.16em] text-stone-500">
        Draft — NDPA 2023 · counsel reviews before you rely on it
      </p>
      <h1 className="font-display text-3xl font-semibold text-ink">Privacy</h1>
      <p>
        We process what we need to sell you units and keep the account safe.
        Lawful basis: the contract to vend, and consent where we say so (saved
        cards, optional notify).
      </p>
      <h2 className="font-display text-xl font-semibold text-ink">What we keep</h2>
      <ul className="list-disc space-y-1 pl-5">
        <li>Account and passkeys.</li>
        <li>Meter numbers encrypted; lookup by HMAC; masked on screen.</li>
        <li>Tokens encrypted. Only the purchaser can reveal them.</li>
        <li>Payment references — not your full card number. Paystack holds PAN.</li>
        <li>Security events: action, outcome, IP HMAC. Not raw IP, not meters.</li>
      </ul>
      <h2 className="font-display text-xl font-semibold text-ink">What we do not do</h2>
      <p>
        We do not put tokens or meter numbers in logs or URLs. We do not sell
        your list. Gift SMS to a third party is off until legal says yes.
      </p>
      <h2 className="font-display text-xl font-semibold text-ink">On this device</h2>
      <p>
        Passkeys, optional auto-renew prefs, and (if you use person-to-person
        Help) chat keys live in the browser. Clearing the browser loses those
        keys. We cannot decrypt old human Help messages.
      </p>
      <h2 className="font-display text-xl font-semibold text-ink">Your rights</h2>
      <p>
        Access, correction, deletion, objection — NDPA. Use Help in the app.
        We will need to keep ledger rows that the law requires.
      </p>
      <p className="text-xs text-stone-500">Nigeria. Ask Help to reach the operator mailbox.</p>
    </article>
  );
}
