"use client";

import React, { use, useCallback, useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { useAuth } from "@/components/AuthContext";
import { apiFetch, formatNaira, newIdempotencyKey } from "@/lib/api";
import { PublicPayLinkView } from "@/lib/types";
import { StatusPill } from "@/components/StatusBadges";
import { Zap, ShieldCheck } from "lucide-react";

export default function GuestPayLinkPage({
  params,
}: {
  params: Promise<{ code: string }>;
}) {
  const { code } = use(params);
  const router = useRouter();
  const { openCheckout } = useAuth();

  const [view, setView] = useState<PublicPayLinkView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [amountNaira, setAmountNaira] = useState("");
  const [payerEmail, setPayerEmail] = useState("sponsor@example.com");
  const [payerName, setPayerName] = useState("Uncle Chidi");
  const [submitting, setSubmitting] = useState(false);

  const loadLink = useCallback(async () => {
    try {
      const v = await apiFetch<PublicPayLinkView>(
        `/v1/public/pay/${encodeURIComponent(code)}`
      );
      setView(v);
      if (v.amount_mode === "FIXED" && v.amount_minor) {
        setAmountNaira(String(v.amount_minor / 100));
      } else if (v.min_amount_minor) {
        setAmountNaira(String(v.min_amount_minor / 100));
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Payment link not found");
    }
  }, [code]);

  useEffect(() => {
    loadLink();
  }, [loadLink]);

  const handleCheckout = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!view) return;
    setSubmitting(true);
    setError(null);
    try {
      const minor =
        view.amount_mode === "FIXED"
          ? view.amount_minor || 0
          : Math.round(Number(amountNaira) * 100);

      const res = await apiFetch<{
        transaction_id: string;
        total_minor: number;
        checkout_url: string;
        status_code: string;
      }>(`/v1/public/pay/${encodeURIComponent(code)}/checkout`, {
        method: "POST",
        idempotencyKey: newIdempotencyKey("guestpay"),
        body: {
          amount_minor: minor,
          email: payerEmail.trim(),
          payer_name: payerName.trim(),
        },
      });

      openCheckout({
        checkoutUrl: res.checkout_url,
        totalMinor: res.total_minor,
        title: `Pay-for-Me • ${view.masked_meter_number} (${view.disco})`,
        subtitle: `Requested by ${view.requester_display_name}`,
        onComplete: () => {
          router.push(`/p/status/${encodeURIComponent(res.status_code)}`);
        },
      });
    } catch (err) {
      setError(err instanceof Error ? err.message : "Guest checkout failed");
    } finally {
      setSubmitting(false);
    }
  };

  if (error && !view) {
    return (
      <div className="mx-auto max-w-lg rounded-2xl border border-rose-200 bg-white p-8 text-center space-y-2">
        <h1 className="text-lg font-bold text-stone-900">Link Unavailable</h1>
        <p className="text-sm text-rose-700">{error}</p>
      </div>
    );
  }

  if (!view) {
    return (
      <div className="mx-auto max-w-lg ml-card p-8 text-center text-sm text-stone-500">
        Loading electricity request…
      </div>
    );
  }

  const chosenMinor =
    view.amount_mode === "FIXED"
      ? view.amount_minor || 0
      : Math.round((Number(amountNaira) || 0) * 100);

  return (
    <div className="mx-auto max-w-lg space-y-5">
      <div className="ml-card overflow-hidden shadow-sm">
        <div className="bg-ink p-6 text-white space-y-2">
          <div className="flex items-center justify-between">
            <span className="inline-flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wider text-volt">
              <Zap className="h-3.5 w-3.5" /> Meterly Pay-for-Me Request
            </span>
            <StatusPill status={view.status} />
          </div>
          <h1 className="text-xl font-bold">
            Pay electricity for {view.requester_display_name}
          </h1>
          <p className="font-mono text-sm text-volt">
            Meter {view.masked_meter_number} • {view.disco}
          </p>
        </div>

        <div className="p-6 space-y-4">
          {view.note && (
            <blockquote className="rounded-xl border border-stone-200 bg-stone-50 px-4 py-3 text-sm italic text-stone-800">
              “{view.note}”
            </blockquote>
          )}

          {error && (
            <div className="rounded-xl border border-rose-200 bg-rose-50 px-3 py-2 text-xs text-rose-800">
              {error}
            </div>
          )}

          {view.status !== "OPEN" ? (
            <p className="rounded-xl bg-stone-100 p-4 text-center text-sm font-medium text-stone-700">
              This request is currently <span className="font-mono">{view.status}</span> and is no longer accepting new checkouts.
            </p>
          ) : (
            <form onSubmit={handleCheckout} className="space-y-4">
              {view.amount_mode === "FIXED" ? (
                <div className="rounded-xl bg-volt/15 border border-stone-200 p-4 flex items-baseline justify-between">
                  <span className="text-xs font-semibold uppercase text-ink">
                    Electricity Amount
                  </span>
                  <span className="font-mono text-2xl font-bold text-stone-900">
                    {formatNaira(view.amount_minor)}
                  </span>
                </div>
              ) : (
                <div>
                  <label className="block text-xs font-medium text-stone-700 mb-1">
                    Choose Electricity Amount ({formatNaira(view.min_amount_minor)} –{" "}
                    {formatNaira(view.max_amount_minor)})
                  </label>
                  <input
                    type="number"
                    min={(view.min_amount_minor || 10000) / 100}
                    max={(view.max_amount_minor || 50000000) / 100}
                    value={amountNaira}
                    onChange={(e) => setAmountNaira(e.target.value)}
                    required
                    className="w-full rounded-xl border border-stone-300 px-3 py-2.5 font-mono text-base"
                  />
                </div>
              )}

              <div>
                <label className="block text-xs font-medium text-stone-700 mb-1">
                  Your Email (for token-free payment receipt)
                </label>
                <input
                  type="email"
                  value={payerEmail}
                  onChange={(e) => setPayerEmail(e.target.value)}
                  required
                  className="w-full rounded-xl border border-stone-300 px-3 py-2 text-sm"
                />
              </div>

              <div>
                <label className="block text-xs font-medium text-stone-700 mb-1">
                  Your Name (optional, shown to {view.requester_display_name})
                </label>
                <input
                  type="text"
                  maxLength={80}
                  value={payerName}
                  onChange={(e) => setPayerName(e.target.value)}
                  className="w-full rounded-xl border border-stone-300 px-3 py-2 text-sm"
                />
              </div>

              <button
                type="submit"
                disabled={submitting}
                className="w-full rounded-full bg-ink px-4 py-3 text-sm font-semibold text-volt shadow-xs hover:bg-black disabled:opacity-50"
              >
                {submitting
                  ? "Reserving Slot..."
                  : `Pay ${formatNaira(chosenMinor + view.fee_minor)} (incl. ${formatNaira(
                      view.fee_minor
                    )} fee)`}
              </button>
            </form>
          )}

          <div className="flex items-start gap-2 pt-2 text-[11px] text-stone-500">
            <ShieldCheck className="h-4 w-4 text-ink shrink-0 mt-0.5" />
            <span>
              Privacy guarantee: Your receipt confirms payment & delivery status without exposing the requester&apos;s home address or STS keypad token.
            </span>
          </div>
        </div>
      </div>
    </div>
  );
}
