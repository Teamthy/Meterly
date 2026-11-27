"use client";

import React, { use, useCallback, useEffect, useState } from "react";
import { useAuth } from "@/components/AuthContext";
import { apiFetch, formatNaira, newIdempotencyKey } from "@/lib/api";
import { PublicPotView } from "@/lib/types";
import { StatusPill } from "@/components/StatusBadges";
import { Users, RefreshCw } from "lucide-react";

export default function PublicPotContributePage({
  params,
}: {
  params: Promise<{ code: string }>;
}) {
  const { code } = use(params);
  const { openCheckout } = useAuth();

  const [pot, setPot] = useState<PublicPotView | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [amountNaira, setAmountNaira] = useState("5000");
  const [email, setEmail] = useState("flatmate@example.com");
  const [name, setName] = useState("Tolu Adeyemi");
  const [phone, setPhone] = useState("");
  const [optInSms, setOptInSms] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  const loadPot = useCallback(async () => {
    try {
      const v = await apiFetch<PublicPotView>(
        `/v1/public/pots/${encodeURIComponent(code)}`
      );
      setPot(v);
      if (v.suggested_share_minor && v.remaining_minor > 0) {
        const share = Math.min(v.suggested_share_minor, v.remaining_minor);
        setAmountNaira(String(Math.ceil(share / 100)));
      } else if (v.remaining_minor > 0) {
        setAmountNaira(String(Math.ceil(v.remaining_minor / 100)));
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : "Funding pot not found");
    }
  }, [code]);

  useEffect(() => {
    loadPot();
  }, [loadPot]);

  const handleContribute = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!pot) return;
    setSubmitting(true);
    setErr(null);
    try {
      const minor = Math.round(Number(amountNaira) * 100);
      const res = await apiFetch<{
        contribution_id: string;
        amount_minor: number;
        checkout_url: string;
      }>(`/v1/public/pots/${encodeURIComponent(code)}/contribute`, {
        method: "POST",
        idempotencyKey: newIdempotencyKey("potcontrib"),
        body: {
          amount_minor: minor,
          email: email.trim(),
          name: name.trim(),
          phone: optInSms ? phone.trim() : "",
          opt_in_sms: optInSms,
        },
      });

      openCheckout({
        checkoutUrl: res.checkout_url,
        totalMinor: res.amount_minor,
        title: `Split Pot Contribution • ${pot.masked_meter_number}`,
        subtitle: "Zero per-contributor fee (target includes single ₦100 fee)",
        onComplete: async () => {
          await loadPot();
        },
      });
    } catch (error) {
      setErr(error instanceof Error ? error.message : "Contribution failed");
    } finally {
      setSubmitting(false);
    }
  };

  if (err && !pot) {
    return (
      <div className="mx-auto max-w-lg rounded-2xl border border-rose-200 bg-white p-8 text-center text-sm text-rose-700">
        {err}
      </div>
    );
  }

  if (!pot) {
    return (
      <div className="mx-auto max-w-lg ml-card p-8 text-center text-sm text-stone-500">
        Loading Split-the-Meter pot…
      </div>
    );
  }

  const pct = Math.min(
    100,
    Math.round((pot.funded_minor / Math.max(1, pot.target_minor)) * 100)
  );

  return (
    <div className="mx-auto max-w-lg space-y-5">
      <div className="ml-card overflow-hidden shadow-sm">
        <div className="bg-ink p-6 text-white space-y-2">
          <div className="flex items-center justify-between">
            <span className="inline-flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wider text-volt">
              <Users className="h-3.5 w-3.5" /> Split the Meter Pot
            </span>
            <StatusPill status={pot.status} />
          </div>
          <h1 className="text-xl font-bold">
            Organised by {pot.organiser_display_name}
          </h1>
          <p className="font-mono text-sm text-volt">
            Meter {pot.masked_meter_number} ({pot.disco}) • Includes{" "}
            {formatNaira(pot.fee_minor)} fee
          </p>
        </div>

        <div className="p-6 space-y-4">
          {pot.note && (
            <p className="rounded-xl bg-stone-50 border border-stone-200 px-3.5 py-2.5 text-xs italic text-stone-700">
              “{pot.note}”
            </p>
          )}

          {/* Progress */}
          <div className="space-y-1.5">
            <div className="flex justify-between text-xs">
              <span className="font-mono font-bold text-stone-900">
                {formatNaira(pot.funded_minor)} of {formatNaira(pot.target_minor)}
              </span>
              <span className="text-stone-500">
                {pot.paid_count} paid
                {pot.suggested_shares ? ` of ${pot.suggested_shares}` : ""}
              </span>
            </div>
            <div className="h-2.5 w-full overflow-hidden rounded-full bg-stone-100">
              <div
                className="h-full bg-volt transition-all"
                style={{ width: `${pct}%` }}
              />
            </div>
            <p className="text-[11px] text-stone-500">
              Remaining: <span className="font-mono">{formatNaira(pot.remaining_minor)}</span> • Deadline{" "}
              {new Date(pot.deadline).toLocaleString()}
            </p>
          </div>

          {/* Contributors (Initials Only) */}
          {pot.contributors && pot.contributors.length > 0 && (
            <div className="space-y-1.5">
              <p className="text-[11px] font-semibold uppercase tracking-wider text-stone-500">
                Contributors (Initials Only)
              </p>
              <div className="flex flex-wrap gap-1.5">
                {pot.contributors.map((c, idx) => (
                  <span
                    key={idx}
                    className="inline-flex items-center gap-1.5 rounded-lg bg-stone-100 px-2.5 py-1 text-xs font-medium text-stone-800"
                  >
                    <span className="font-bold">{c.initials}</span>
                    <span className="font-mono text-stone-600">
                      {formatNaira(c.retained_minor || c.amount_minor)}
                    </span>
                  </span>
                ))}
              </div>
            </div>
          )}

          {err && (
            <div className="rounded-xl border border-rose-200 bg-rose-50 px-3 py-2 text-xs text-rose-800">
              {err}
            </div>
          )}

          {pot.status === "OPEN" ? (
            <form onSubmit={handleContribute} className="space-y-3.5 pt-1">
              <div>
                <label className="block text-xs font-medium text-stone-700 mb-1">
                  Your Share (₦ Naira)
                </label>
                <input
                  type="number"
                  min={100}
                  value={amountNaira}
                  onChange={(e) => setAmountNaira(e.target.value)}
                  required
                  className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
                />
              </div>

              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <label className="block text-xs font-medium text-stone-700 mb-1">
                    Receipt Email
                  </label>
                  <input
                    type="email"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    required
                    className="w-full rounded-xl border border-stone-300 px-3 py-2 text-xs"
                  />
                </div>
                <div>
                  <label className="block text-xs font-medium text-stone-700 mb-1">
                    Your Name (shown as initials)
                  </label>
                  <input
                    type="text"
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    className="w-full rounded-xl border border-stone-300 px-3 py-2 text-xs"
                  />
                </div>
              </div>

              <div className="space-y-2 rounded-xl bg-stone-50 p-3 border border-stone-200">
                <label className="flex items-center gap-2 text-xs text-stone-700 cursor-pointer">
                  <input
                    type="checkbox"
                    checked={optInSms}
                    onChange={(e) => setOptInSms(e.target.checked)}
                  />
                  Send me a settlement alert by SMS when the pot buys electricity
                </label>
                {optInSms && (
                  <input
                    type="tel"
                    value={phone}
                    onChange={(e) => setPhone(e.target.value)}
                    placeholder="+2348031234567"
                    className="w-full rounded-lg border border-stone-300 bg-white px-2.5 py-1.5 font-mono text-xs"
                  />
                )}
              </div>

              <button
                type="submit"
                disabled={submitting}
                className="w-full rounded-full bg-ink px-4 py-3 text-sm font-semibold text-volt hover:bg-black disabled:opacity-50"
              >
                {submitting
                  ? "Opening Checkout..."
                  : `Pay My Share (${formatNaira(
                      Math.round((Number(amountNaira) || 0) * 100)
                    )})`}
              </button>
            </form>
          ) : (
            <div className="rounded-xl bg-volt/20 border border-stone-200 p-4 text-xs text-ink flex items-center justify-between">
              <span>
                Pot status is <strong className="font-mono">{pot.status}</strong>. Electricity is automatically vended when funded, or contributions are refunded if expired/cancelled.
              </span>
              <button
                type="button"
                onClick={loadPot}
                className="inline-flex items-center gap-1 rounded-lg bg-white px-2.5 py-1 font-semibold border border-stone-200"
              >
                <RefreshCw className="h-3 w-3" /> Refresh
              </button>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
