"use client";

import React, { useCallback, useEffect, useState } from "react";
import { ApiError, apiFetch } from "@/lib/api";
import { ConsoleAction, DevConsole } from "@/lib/types";
import { StatusPill } from "@/components/StatusBadges";
import { FlaskConical, Gauge, Radio, CreditCard, RotateCcw } from "lucide-react";

export default function DevConsolePage() {
  const [screen, setScreen] = useState<DevConsole | null>(null);
  const [missing, setMissing] = useState(false);
  const [kwh, setKwh] = useState("82");
  const [busy, setBusy] = useState(false);
  const [banner, setBanner] = useState<{ type: "ok" | "err"; text: string } | null>(
    null
  );

  const load = useCallback(async () => {
    try {
      const c = await apiFetch<DevConsole>("/v1/dev/console");
      setScreen(c);
      setMissing(false);
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) {
        setMissing(true);
        setScreen(null);
        return;
      }
      setBanner({
        type: "err",
        text: e instanceof Error ? e.message : "Failed to load sandbox console",
      });
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const run = async (
    action: ConsoleAction,
    body?: Record<string, unknown>
  ) => {
    if (!action.path || !action.enabled) return;
    setBusy(true);
    try {
      await apiFetch(action.path, {
        method: action.method || "POST",
        body,
      });
      await load();
      setBanner({ type: "ok", text: action.label + " done." });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : `${action.label} failed`,
      });
    } finally {
      setBusy(false);
    }
  };

  if (missing) {
    return (
      <div className="mx-auto max-w-xl rounded-2xl border border-amber-200 bg-white p-8 text-center space-y-3">
        <FlaskConical className="mx-auto h-10 w-10 text-amber-600" />
        <h1 className="text-xl font-bold text-stone-900">Sandbox is off</h1>
        <p className="text-sm text-stone-600">
          <code className="font-mono">GET /v1/dev/console</code> is only registered
          when the API runs with MODE=dev. It is never enabled in production.
        </p>
      </div>
    );
  }

  if (!screen) {
    return (
      <div className="rounded-2xl border border-stone-200 bg-white p-8 text-sm text-stone-500">
        Loading sandbox console…
      </div>
    );
  }

  const reconcile = screen.actions.find((a) => a.id === "reconcile");
  const resumeTmpl =
    screen.actions.find((a) => a.id === "resume")?.path_template || "";

  return (
    <div className="space-y-6">
      <div className="rounded-2xl bg-[#0B1F1A] p-6 text-white">
        <span className="text-xs font-semibold uppercase tracking-wider text-emerald-300">
          MODE=dev · never production
        </span>
        <h1 className="mt-1 text-2xl font-bold">{screen.title}</h1>
        <p className="mt-1 max-w-2xl text-sm text-stone-300">{screen.subtitle}</p>
        <p className="mt-2 max-w-2xl text-xs text-stone-400">{screen.copy}</p>
      </div>

      {banner && (
        <div
          className={`rounded-xl border px-4 py-3 text-sm ${
            banner.type === "ok"
              ? "border-emerald-200 bg-emerald-50 text-emerald-900"
              : "border-rose-200 bg-rose-50 text-rose-900"
          }`}
        >
          {banner.text}
        </div>
      )}

      <div className="flex flex-wrap gap-2">
        {reconcile && (
          <button
            type="button"
            disabled={!reconcile.enabled || busy}
            onClick={() => run(reconcile)}
            className="inline-flex items-center gap-1.5 rounded-xl bg-stone-900 px-4 py-2 text-xs font-semibold text-white disabled:opacity-50"
          >
            <RotateCcw className="h-3.5 w-3.5" />
            {reconcile.label}
          </button>
        )}
      </div>

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
        <section className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-3">
          <h2 className="text-sm font-bold text-stone-900 flex items-center gap-1.5">
            <Gauge className="h-4 w-4 text-emerald-600" />
            {screen.meters_title}
          </h2>
          {screen.meters.length === 0 ? (
            <p className="text-xs text-stone-400">{screen.meters_empty}</p>
          ) : (
            <ul className="space-y-2">
              {screen.meters.map((m) => (
                <li
                  key={m.meter_id}
                  className="rounded-xl border border-stone-200 bg-stone-50 p-3 space-y-2"
                >
                  <div className="flex items-center justify-between gap-2 text-xs">
                    <span className="font-mono font-semibold">{m.masked_meter_number}</span>
                    <span className="text-stone-500">{m.disco}</span>
                  </div>
                  <p className="text-[11px] text-stone-600">{m.delivery_hint}</p>
                  <p className="text-[11px] text-stone-500">{m.reading_label}</p>
                  <div className="flex gap-2">
                    <input
                      type="number"
                      step="0.1"
                      min="0"
                      value={kwh}
                      onChange={(e) => setKwh(e.target.value)}
                      className="w-24 rounded-lg border border-stone-300 px-2 py-1 text-xs"
                    />
                    <button
                      type="button"
                      disabled={!m.set_reading.enabled || busy}
                      onClick={() =>
                        run(m.set_reading, { kwh: Number(kwh) })
                      }
                      className="rounded-lg bg-stone-900 px-3 py-1 text-xs font-semibold text-white disabled:opacity-40"
                    >
                      {m.set_reading.label}
                    </button>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </section>

        <section className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-3">
          <h2 className="text-sm font-bold text-stone-900 flex items-center gap-1.5">
            <CreditCard className="h-4 w-4 text-emerald-600" />
            {screen.payments_title}
          </h2>
          {screen.payments.length === 0 ? (
            <p className="text-xs text-stone-400">{screen.payments_empty}</p>
          ) : (
            <ul className="space-y-2">
              {screen.payments.map((p) => (
                <li
                  key={p.reference}
                  className="rounded-xl border border-stone-200 bg-stone-50 p-3 space-y-2"
                >
                  <div className="flex items-center justify-between gap-2 text-xs">
                    <span className="font-semibold">{p.amount_label}</span>
                    <span className="text-stone-500">{p.status_label}</span>
                  </div>
                  <p className="font-mono text-[11px] text-stone-500">{p.reference}</p>
                  <div className="flex gap-2">
                    <button
                      type="button"
                      disabled={!p.complete.enabled || busy}
                      onClick={() => run(p.complete, { success: true })}
                      className="rounded-lg bg-emerald-800 px-3 py-1 text-xs font-semibold text-white disabled:opacity-40"
                    >
                      {p.complete.label}
                    </button>
                    <button
                      type="button"
                      disabled={!p.fail.enabled || busy}
                      onClick={() => run(p.fail, { success: false })}
                      className="rounded-lg border border-stone-300 px-3 py-1 text-xs font-semibold disabled:opacity-40"
                    >
                      {p.fail.label}
                    </button>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </section>

        <section className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-3">
          <h2 className="text-sm font-bold text-stone-900 flex items-center gap-1.5">
            <Radio className="h-4 w-4 text-emerald-600" />
            {screen.remote.title}
          </h2>
          <p className="text-xs text-stone-600">{screen.remote.copy}</p>
          <p className="text-sm font-semibold text-stone-900">
            {screen.remote.pending_label}
          </p>
          <button
            type="button"
            disabled={!screen.remote.settle.enabled || busy}
            onClick={() => run(screen.remote.settle)}
            className="rounded-xl bg-stone-900 px-4 py-2 text-xs font-semibold text-white disabled:opacity-40"
          >
            {screen.remote.settle.label}
          </button>
        </section>

        <section className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-3">
          <h2 className="text-sm font-bold text-stone-900">{screen.recent_title}</h2>
          {screen.recent.length === 0 ? (
            <p className="text-xs text-stone-400">{screen.recent_empty}</p>
          ) : (
            <ul className="space-y-2 max-h-96 overflow-y-auto">
              {screen.recent.map((t) => (
                <li
                  key={t.transaction_id}
                  className="rounded-xl border border-stone-200 bg-stone-50 p-3 space-y-2"
                >
                  <div className="flex items-center justify-between gap-2 text-xs">
                    <span className="font-semibold">{t.amount_label}</span>
                    <StatusPill status={t.status} />
                  </div>
                  <p className="text-[11px] text-stone-500">{t.summary}</p>
                  <p className="font-mono text-[11px] text-stone-400">
                    {t.when_label} · {t.method_label}
                  </p>
                  {resumeTmpl ? (
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() =>
                        run({
                          id: "resume",
                          label: "Resume",
                          method: "POST",
                          path: resumeTmpl.replace(
                            "{transaction_id}",
                            t.transaction_id
                          ),
                          enabled: true,
                        })
                      }
                      className="rounded-lg border border-stone-300 px-3 py-1 text-xs font-semibold"
                    >
                      Resume
                    </button>
                  ) : null}
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>
    </div>
  );
}
