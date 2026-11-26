"use client";

import React, { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useAuth } from "@/components/AuthContext";
import { apiFetch, formatNaira, toRelativeSharePath } from "@/lib/api";
import { CreatedPot, Meter } from "@/lib/types";
import { StatusPill } from "@/components/StatusBadges";
import { Users, ExternalLink, XCircle, RefreshCw } from "lucide-react";

const STORAGE_KEY = "meterly_pot_ids";

export default function SplitPotsPage() {
  const { token } = useAuth();
  const [meters, setMeters] = useState<Meter[]>([]);
  const [pots, setPots] = useState<CreatedPot[]>([]);
  const [latestPot, setLatestPot] = useState<CreatedPot | null>(null);
  const [banner, setBanner] = useState<{ type: "ok" | "err"; text: string } | null>(
    null
  );

  const [meterId, setMeterId] = useState("");
  const [targetNaira, setTargetNaira] = useState("20100");
  const [shares, setShares] = useState("4");
  const [expiresHours, setExpiresHours] = useState("48");
  const [note, setNote] = useState("Flat 3B shared meter contribution");
  const [creating, setCreating] = useState(false);

  const loadData = useCallback(async () => {
    try {
      const mRes = await apiFetch<{ data: Meter[] }>("/v1/meters", { token });
      const mList = mRes.data || [];
      setMeters(mList);
      if (mList.length > 0) {
        setMeterId((prev) =>
          mList.some((m) => m.meter_id === prev) ? prev : mList[0].meter_id
        );
      }

      let savedIds: string[] = [];
      try {
        savedIds = JSON.parse(
          window.localStorage.getItem(`${STORAGE_KEY}:${token}`) || "[]"
        );
      } catch {
        savedIds = [];
      }
      const loaded: CreatedPot[] = [];
      for (const id of savedIds) {
        try {
          const p = await apiFetch<CreatedPot>(`/v1/pots/${id}`, { token });
          loaded.push(p);
        } catch {
          // ignore stale or other-owner ids
        }
      }
      setPots(loaded);
    } catch (e) {
      setBanner({
        type: "err",
        text: e instanceof Error ? e.message : "Failed to load pots",
      });
    }
  }, [token]);

  useEffect(() => {
    setLatestPot(null);
    loadData();
  }, [loadData]);

  const rememberPotId = (id: string) => {
    try {
      const key = `${STORAGE_KEY}:${token}`;
      const cur: string[] = JSON.parse(window.localStorage.getItem(key) || "[]");
      if (!cur.includes(id)) {
        window.localStorage.setItem(key, JSON.stringify([id, ...cur]));
      }
    } catch {
      // ignore
    }
  };

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!meterId) {
      setBanner({ type: "err", text: "Save a meter on Buy Power first." });
      return;
    }
    setCreating(true);
    setBanner(null);
    try {
      const created = await apiFetch<CreatedPot>("/v1/pots", {
        method: "POST",
        token,
        body: {
          meter_id: meterId,
          target_minor: Math.round(Number(targetNaira) * 100),
          suggested_shares: Number(shares) || 0,
          expires_in_hours: Number(expiresHours) || 48,
          note: note.trim(),
        },
      });
      rememberPotId(created.id);
      setLatestPot(created);
      await loadData();
      setBanner({
        type: "ok",
        text: "Split-the-Meter Funding Pot created! Share the contributor link below.",
      });
    } catch (err) {
      setBanner({
        type: "err",
        text:
          err instanceof Error
            ? err.message
            : "Could not create pot (check FEATURE_SPLIT_METER)",
      });
    } finally {
      setCreating(false);
    }
  };

  const handleCancel = async (id: string) => {
    try {
      await apiFetch(`/v1/pots/${id}/cancel`, { method: "POST", token });
      await loadData();
      setBanner({
        type: "ok",
        text: "Pot cancelled — all paid contributions are automatically queued for PSP refund.",
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Cancel failed",
      });
    }
  };

  const latestSharePath = latestPot
    ? toRelativeSharePath(latestPot.share_url, "/pots", latestPot.code)
    : "";

  return (
    <div className="space-y-6">
      <div className="rounded-2xl bg-[#0B1F1A] p-6 text-white">
        <span className="text-xs font-semibold uppercase tracking-wider text-emerald-300">
          F2 • Split the Meter (Funding Pots)
        </span>
        <h1 className="mt-1 text-2xl font-bold">
          Pool compound or flatmate shares — buy automatically when funded.
        </h1>
        <p className="mt-1 max-w-2xl text-sm text-stone-300">
          Contributors see initials only, pay zero per-share fees (single ₦100 fee included in target), and receive automatic refunds on expiry, cancellation, or overfunding.
        </p>
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

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-12">
        <form
          onSubmit={handleCreate}
          className="space-y-4 rounded-2xl border border-stone-200 bg-white p-5 shadow-xs lg:col-span-5"
        >
          <h2 className="text-base font-bold text-stone-900 flex items-center gap-2">
            <Users className="h-4 w-4 text-emerald-600" />
            Create Split-the-Meter Pot
          </h2>

          <div>
            <label className="block text-xs font-medium text-stone-700 mb-1">
              Shared Meter
            </label>
            <select
              value={meterId}
              onChange={(e) => setMeterId(e.target.value)}
              className="w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-sm"
            >
              {meters.length === 0 && (
                <option value="">Save a meter on Buy Power first</option>
              )}
              {meters.map((m) => (
                <option key={m.meter_id} value={m.meter_id}>
                  {m.label ? `${m.label} • ` : ""}
                  {m.masked_meter_number} ({m.disco})
                </option>
              ))}
            </select>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="block text-xs font-medium text-stone-700 mb-1">
                Target Total (₦, incl. ₦100 fee)
              </label>
              <input
                type="number"
                min={1100}
                value={targetNaira}
                onChange={(e) => setTargetNaira(e.target.value)}
                required
                className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
              />
            </div>
            <div>
              <label className="block text-xs font-medium text-stone-700 mb-1">
                Suggested Shares
              </label>
              <input
                type="number"
                min={2}
                max={20}
                value={shares}
                onChange={(e) => setShares(e.target.value)}
                className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
              />
            </div>
          </div>

          <p className="rounded-lg bg-stone-50 border border-stone-200 px-3 py-2 text-xs text-stone-600">
            Includes ₦100 fee • Electricity vended:{" "}
            <span className="font-mono font-semibold text-stone-900">
              {formatNaira(Math.max(0, Math.round(Number(targetNaira) * 100) - 10000))}
            </span>
          </p>

          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="block text-xs font-medium text-stone-700 mb-1">
                Hold Window (Hours, max 72h)
              </label>
              <input
                type="number"
                min={1}
                max={72}
                value={expiresHours}
                onChange={(e) => setExpiresHours(e.target.value)}
                className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
              />
            </div>
            <div>
              <label className="block text-xs font-medium text-stone-700 mb-1">
                Compound Note
              </label>
              <input
                type="text"
                maxLength={140}
                value={note}
                onChange={(e) => setNote(e.target.value)}
                className="w-full rounded-xl border border-stone-300 px-3 py-2 text-sm"
              />
            </div>
          </div>

          <button
            type="submit"
            disabled={creating || !meterId}
            className="w-full rounded-xl bg-emerald-600 px-4 py-3 text-sm font-semibold text-white hover:bg-emerald-700 disabled:opacity-50"
          >
            {creating ? "Creating Pot..." : "Create Funding Pot"}
          </button>
        </form>

        <div className="space-y-4 lg:col-span-7">
          {latestPot && (
            <div className="rounded-2xl border-2 border-emerald-500 bg-emerald-50/50 p-5 space-y-3">
              <div className="flex items-center justify-between">
                <span className="text-xs font-bold uppercase tracking-wider text-emerald-900">
                  Share Contributor Link
                </span>
                <StatusPill status={latestPot.status} />
              </div>
              <div className="flex flex-wrap items-center gap-4">
                {latestPot.qr_png_base64 && (
                  <img
                    src={`data:image/png;base64,${latestPot.qr_png_base64}`}
                    alt="Split Pot QR"
                    className="h-24 w-24 rounded-xl border border-stone-200 bg-white p-1"
                  />
                )}
                <div className="flex-1 space-y-2">
                  <code className="block rounded-lg bg-white px-3 py-2 font-mono text-xs border border-stone-200">
                    {latestSharePath}
                  </code>
                  <Link
                    href={latestSharePath}
                    className="inline-flex items-center gap-1.5 rounded-lg bg-[#0B1F1A] px-3.5 py-2 text-xs font-semibold text-white"
                  >
                    Open Public Contributor Page <ExternalLink className="h-3.5 w-3.5" />
                  </Link>
                </div>
              </div>
            </div>
          )}

          <div className="flex items-center justify-between">
            <h2 className="text-base font-bold text-stone-900">
              My Organised Pots ({pots.length})
            </h2>
            <button
              type="button"
              onClick={loadData}
              className="inline-flex items-center gap-1 text-xs font-semibold text-stone-600 hover:text-stone-900"
            >
              <RefreshCw className="h-3.5 w-3.5" /> Refresh
            </button>
          </div>

          {pots.length === 0 ? (
            <div className="rounded-2xl border border-dashed border-stone-300 bg-white p-8 text-center text-xs text-stone-500">
              No Split-the-Meter pots created in this session yet.
            </div>
          ) : (
            <div className="space-y-3">
              {pots.map((p) => {
                const pct = Math.min(
                  100,
                  Math.round((p.funded_minor / Math.max(1, p.target_minor)) * 100)
                );
                return (
                  <div
                    key={p.id}
                    className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-3"
                  >
                    <div className="flex items-center justify-between">
                      <div>
                        <span className="font-mono text-sm font-bold text-stone-900">
                          {formatNaira(p.funded_minor)} of {formatNaira(p.target_minor)}
                        </span>
                        <span className="ml-2 text-xs text-stone-500">
                          ({p.paid_count} contributor{p.paid_count === 1 ? "" : "s"})
                        </span>
                      </div>
                      <StatusPill status={p.status} />
                    </div>

                    <div className="h-2 w-full overflow-hidden rounded-full bg-stone-100">
                      <div
                        className="h-full bg-emerald-500 transition-all"
                        style={{ width: `${pct}%` }}
                      />
                    </div>

                    <div className="flex items-center justify-between text-xs text-stone-500">
                      <span>
                        {p.masked_meter_number} ({p.disco}) • Includes{" "}
                        {formatNaira(p.fee_minor)} fee
                      </span>
                      {p.status === "OPEN" && (
                        <button
                          type="button"
                          onClick={() => handleCancel(p.id)}
                          className="inline-flex items-center gap-1 rounded-lg border border-rose-200 bg-rose-50 px-2.5 py-1 font-semibold text-rose-800"
                        >
                          <XCircle className="h-3.5 w-3.5" /> Cancel & Refund All
                        </button>
                      )}
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
