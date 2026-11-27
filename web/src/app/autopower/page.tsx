"use client";

import React, { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useAuth } from "@/components/AuthContext";
import { apiFetch, formatNaira } from "@/lib/api";
import { Meter } from "@/lib/types";

type Mode = "AMOUNT" | "THRESHOLD";

type Intent = {
  meter_id: string;
  mode: Mode;
  amount_naira: number;
  threshold_kwh: number;
  saved_at: string;
};

function storageKey(token: string) {
  return `meterly:autorenew:${token}`;
}

export default function AutoPowerPage() {
  const { token } = useAuth();
  const [meters, setMeters] = useState<Meter[]>([]);
  const [meterId, setMeterId] = useState("");
  const [mode, setMode] = useState<Mode>("THRESHOLD");
  const [amountNaira, setAmountNaira] = useState("5000");
  const [thresholdKwh, setThresholdKwh] = useState("10");
  const [intent, setIntent] = useState<Intent | null>(null);
  const [banner, setBanner] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const mRes = await apiFetch<{ data: Meter[] }>("/v1/meters", { token });
      const list = mRes.data || [];
      setMeters(list);
      const raw = localStorage.getItem(storageKey(token));
      const saved: Intent | null = raw ? JSON.parse(raw) : null;
      if (saved) {
        setIntent(saved);
        setMeterId(saved.meter_id);
        setMode(saved.mode);
        setAmountNaira(String(saved.amount_naira));
        setThresholdKwh(String(saved.threshold_kwh));
      } else if (list[0]) {
        setMeterId(list[0].meter_id);
      }
    } catch {
      const raw = localStorage.getItem(storageKey(token));
      if (raw) setIntent(JSON.parse(raw));
    }
  }, [token]);

  useEffect(() => {
    load();
  }, [load]);

  const selected = meters.find((m) => m.meter_id === meterId);

  const save = (e: React.FormEvent) => {
    e.preventDefault();
    const next: Intent = {
      meter_id: meterId,
      mode,
      amount_naira: Math.max(500, Number(amountNaira) || 0),
      threshold_kwh: Math.max(1, Number(thresholdKwh) || 0),
      saved_at: new Date().toISOString(),
    };
    localStorage.setItem(storageKey(token), JSON.stringify(next));
    setIntent(next);
    setBanner("Saved. We will not charge your card until Auto-renew launches.");
  };

  const clear = () => {
    localStorage.removeItem(storageKey(token));
    setIntent(null);
    setBanner(null);
  };

  return (
    <div className="space-y-6">
      <section className="overflow-hidden rounded-[28px] border border-white/10 bg-[#111318] p-6 text-paper sm:p-8">
        <span className="ml-chip">Coming soon</span>
        <h1 className="font-display mt-4 max-w-xl text-4xl font-semibold leading-[1.05] text-paper sm:text-5xl">
          Auto-renew, before the lights go.
        </h1>
        <p className="mt-3 max-w-lg text-sm leading-relaxed text-white/55">
          Tell us how you want power topped up. We&apos;ll start charging only
          when Auto-renew ships — Lagos-time cap, skip once, credit on a
          compatible smart meter. Low-balance needs a confirmed remote reading.
          If Disco supply is off, a charge still cannot put light in the house.
        </p>
      </section>

      {banner && (
        <div className="rounded-2xl border border-volt/30 bg-volt/10 px-4 py-3 text-sm text-volt-ink">
          <span className="text-[#d8ff3e]">{banner}</span>
        </div>
      )}

      <div className="grid gap-6 lg:grid-cols-12">
        <form
          onSubmit={save}
          className="space-y-5 rounded-[24px] border border-stone-200/80 bg-paper p-5 shadow-[0_20px_60px_rgba(0,0,0,0.18)] lg:col-span-7"
        >
          <div>
            <p className="text-[11px] font-semibold uppercase tracking-[0.16em] text-stone-500">
              How should it fire
            </p>
            <div className="mt-3 grid grid-cols-2 gap-2">
              <button
                type="button"
                onClick={() => setMode("THRESHOLD")}
                className={`rounded-2xl border px-4 py-4 text-left transition ${
                  mode === "THRESHOLD"
                    ? "border-ink bg-ink text-paper"
                    : "border-stone-200 bg-white text-stone-700 hover:border-stone-400"
                }`}
              >
                <p className="font-display text-lg font-semibold">When units drop</p>
                <p className={`mt-1 text-xs ${mode === "THRESHOLD" ? "text-white/60" : "text-stone-500"}`}>
                  Top up as soon as remaining kWh hits your floor.
                </p>
              </button>
              <button
                type="button"
                onClick={() => setMode("AMOUNT")}
                className={`rounded-2xl border px-4 py-4 text-left transition ${
                  mode === "AMOUNT"
                    ? "border-ink bg-ink text-paper"
                    : "border-stone-200 bg-white text-stone-700 hover:border-stone-400"
                }`}
              >
                <p className="font-display text-lg font-semibold">Fixed amount</p>
                <p className={`mt-1 text-xs ${mode === "AMOUNT" ? "text-white/60" : "text-stone-500"}`}>
                  Always buy the same pack when Auto-renew runs.
                </p>
              </button>
            </div>
          </div>

          <label className="block">
            <span className="text-[11px] font-semibold uppercase tracking-[0.16em] text-stone-500">
              Meter
            </span>
            {meters.length === 0 ? (
              <p className="mt-2 rounded-xl border border-dashed border-stone-300 px-3 py-3 text-sm text-stone-500">
                Save a meter on{" "}
                <Link href="/" className="font-semibold text-ink underline">
                  Power
                </Link>{" "}
                first.
              </p>
            ) : (
              <select
                value={meterId}
                onChange={(e) => setMeterId(e.target.value)}
                className="mt-2 w-full rounded-xl border border-stone-300 bg-white px-3 py-2.5 text-sm"
              >
                {meters.map((m) => (
                  <option key={m.meter_id} value={m.meter_id}>
                    {m.label ? `${m.label} · ` : ""}
                    {m.masked_meter_number} ({m.disco})
                  </option>
                ))}
              </select>
            )}
          </label>

          <div className="grid gap-4 sm:grid-cols-2">
            <label className="block">
              <span className="text-[11px] font-semibold uppercase tracking-[0.16em] text-stone-500">
                Amount to buy
              </span>
              <div className="mt-2 flex items-center rounded-xl border border-stone-300 bg-white px-3">
                <span className="text-sm text-stone-400">₦</span>
                <input
                  type="number"
                  min={500}
                  step={100}
                  value={amountNaira}
                  onChange={(e) => setAmountNaira(e.target.value)}
                  className="w-full bg-transparent px-2 py-2.5 font-display text-2xl font-semibold tabular-nums focus:outline-none"
                />
              </div>
            </label>
            <label className="block">
              <span className="text-[11px] font-semibold uppercase tracking-[0.16em] text-stone-500">
                Unit floor
              </span>
              <div className="mt-2 flex items-center rounded-xl border border-stone-300 bg-white px-3">
                <input
                  type="number"
                  min={1}
                  step={1}
                  value={thresholdKwh}
                  onChange={(e) => setThresholdKwh(e.target.value)}
                  disabled={mode !== "THRESHOLD"}
                  className="w-full bg-transparent px-2 py-2.5 font-display text-2xl font-semibold tabular-nums focus:outline-none disabled:opacity-40"
                />
                <span className="text-sm text-stone-400">kWh</span>
              </div>
            </label>
          </div>

          <p className="text-xs leading-relaxed text-stone-500">
            {mode === "THRESHOLD"
              ? `When ${selected?.masked_meter_number || "this meter"} falls to ${thresholdKwh} kWh, Meterly will buy ₦${Number(amountNaira).toLocaleString()} — after launch.`
              : `Meterly will buy ₦${Number(amountNaira).toLocaleString()} on a schedule you confirm at launch. No charge today.`}
          </p>

          <div className="flex flex-wrap gap-2">
            <button
              type="submit"
              disabled={!meterId}
              className="rounded-full bg-ink px-5 py-2.5 text-sm font-semibold text-volt disabled:opacity-40"
            >
              Save my Auto-renew
            </button>
            {intent && (
              <button
                type="button"
                onClick={clear}
                className="rounded-full border border-stone-300 px-4 py-2.5 text-sm font-medium text-stone-600"
              >
                Clear
              </button>
            )}
          </div>
        </form>

        <aside className="space-y-4 lg:col-span-5">
          <div className="rounded-[24px] border border-white/10 bg-[#111318] p-5 text-paper">
            <p className="text-[11px] font-semibold uppercase tracking-[0.16em] text-white/40">
              Your plan
            </p>
            {intent ? (
              <div className="mt-4 space-y-3">
                <p className="font-display text-4xl font-semibold tabular-nums text-volt">
                  {formatNaira(intent.amount_naira * 100)}
                </p>
                <p className="text-sm text-white/70">
                  {intent.mode === "THRESHOLD"
                    ? `Fires when units hit ${intent.threshold_kwh} kWh`
                    : "Fixed amount, schedule at launch"}
                </p>
                <p className="text-xs text-white/40">
                  Saved {new Date(intent.saved_at).toLocaleString("en-NG")} · not charging yet
                </p>
              </div>
            ) : (
              <p className="mt-4 text-sm text-white/50">
                Nothing saved. Pick a mode — we keep it on this device until Auto-renew is live.
              </p>
            )}
          </div>
          <div className="rounded-[24px] border border-stone-200/80 bg-paper p-5 text-sm text-stone-600">
            <p className="font-display text-lg font-semibold text-ink">What launches with it</p>
            <ul className="mt-3 space-y-2 text-xs leading-relaxed">
              <li>Africa/Lagos time, monthly spend cap, skip once.</li>
              <li>Low-balance only on meters with confirmed remote reading.</li>
              <li>Card mandate from a purchase you already consented to.</li>
              <li>Credit on compatible smart meters. Token only if the meter cannot take it.</li>
              <li>No charge while this page says coming soon. Prefs stay on this device.</li>
            </ul>
          </div>
        </aside>
      </div>
    </div>
  );
}
