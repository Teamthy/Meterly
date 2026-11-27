export default function TermsPage() {
  return (
    <article className="mx-auto max-w-2xl space-y-6 rounded-[24px] border border-stone-200/80 bg-paper p-6 text-sm leading-relaxed text-stone-700 shadow-[0_20px_60px_rgba(0,0,0,0.12)] sm:p-8">
      <p className="text-[11px] font-semibold uppercase tracking-[0.16em] text-stone-500">
        Draft — counsel reviews before you rely on it
      </p>
      <h1 className="font-display text-3xl font-semibold text-ink">Terms</h1>
      <p>
        Meterly is software. We are not your Disco, not a meter company, and not
        a bank. You buy prepaid units. A CBN-licensed PSP (Paystack) takes the
        money. A licensed aggregator (VTpass, unless we name another) vends.
        On a <strong>compatible smart meter</strong> we ask the head-end to put
        credit on the meter. We only call it delivered when the meter confirms.
        Network “accepted” is not delivered.
      </p>
      <h2 className="font-display text-xl font-semibold text-ink">Keypad meters</h2>
      <p>
        If the meter has no remote path, you get a 20-digit STS token. You type
        it. That is fallback, not a failure of the product — most Nigerian
        meters still work this way.
      </p>
      <h2 className="font-display text-xl font-semibold text-ink">No light after pay</h2>
      <p>
        Remote load needs Disco supply at the premises (and the UIU on a wall
        socket on that supply). If NEPA is off, units cannot land and a token
        cannot run the house. We will not mark that as delivered. When supply
        returns we retry or, if the head-end never credited, an operator may
        release the token. Do not pay twice.
      </p>
      <h2 className="font-display text-xl font-semibold text-ink">Wrong meter</h2>
      <p>
        We show the Disco name on the meter before you pay. If units land on a
        meter you did not mean, we cannot pull them back. Refunds only when the
        vend definitely failed.
      </p>
      <h2 className="font-display text-xl font-semibold text-ink">Auto-renew</h2>
      <p>
        Auto-renew is coming soon. Saving a plan on this device does not charge
        your card. When it ships, it will need a separate consent, a cap, and a
        way to skip.
      </p>
      <h2 className="font-display text-xl font-semibold text-ink">You</h2>
      <p>
        You must be allowed to buy for that meter. You keep your passkey safe.
        You do not share tokens in public channels.
      </p>
      <p className="text-xs text-stone-500">
        Questions: use Help in the app. Nigerian law. Lagos.
      </p>
    </article>
  );
}
