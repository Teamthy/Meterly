"use client";

import React, { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useAuth } from "@/components/AuthContext";
import { apiFetch, formatNaira, toRelativeSharePath } from "@/lib/api";
import { Meter, PayRequestView } from "@/lib/types";
import { StatusPill } from "@/components/StatusBadges";
import { Share2, QrCode, ExternalLink, Copy, Check, XCircle } from "lucide-react";

export default function PayForMePage() {
  const { token } = useAuth();
  const [meters, setMeters] = useState<Meter[]>([]);
  const [links, setLinks] = useState<PayRequestView[]>([]);
  const [latestCreated, setLatestCreated] = useState<PayRequestView | null>(null);
  const [banner, setBanner] = useState<{ type: "ok" | "err"; text: string } | null>(
    null
  );

  const [meterId, setMeterId] = useState("");
  const [mode, setMode] = useState<"FIXED" | "RANGE">("FIXED");
  const [fixedNaira, setFixedNaira] = useState("5000");
  const [minNaira, setMinNaira] = useState("2000");
  const [maxNaira, setMaxNaira] = useState("20000");
  const [note, setNote] = useState("Light don go, abeg help top up");
  const [expiresHours, setExpiresHours] = useState("72");
  const [maxPayments, setMaxPayments] = useState("1");
  const [creating, setCreating] = useState(false);
  const [copied, setCopied] = useState(false);

  const loadData = useCallback(async () => {
    try {
      const [mRes, lRes] = await Promise.all([
        apiFetch<{ data: Meter[] }>("/v1/meters", { token }),
        apiFetch<{ data: PayRequestView[] }>("/v1/payment-requests", { token }),
      ]);
      const mList = mRes.data || [];
      setMeters(mList);
      setLinks(lRes.data || []);
      if (mList.length > 0) {
        setMeterId((prev) =>
          mList.some((m) => m.meter_id === prev) ? prev : mList[0].meter_id
        );
      }
    } catch (e) {
      setBanner({
        type: "err",
        text: e instanceof Error ? e.message : "Failed to load Pay-for-Me links",
      });
    }
  }, [token]);

  useEffect(() => {
    setLatestCreated(null);
    loadData();
  }, [loadData]);

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!meterId) {
      setBanner({
        type: "err",
        text: "Save a meter on the Buy Power tab first.",
      });
      return;
    }
    setCreating(true);
    setBanner(null);
    try {
      const created = await apiFetch<PayRequestView>("/v1/payment-requests", {
        method: "POST",
        token,
        body: {
          meter_id: meterId,
          amount_mode: mode,
          amount_minor:
            mode === "FIXED" ? Math.round(Number(fixedNaira) * 100) : 0,
          min_amount_minor:
            mode === "RANGE" ? Math.round(Number(minNaira) * 100) : 0,
          max_amount_minor:
            mode === "RANGE" ? Math.round(Number(maxNaira) * 100) : 0,
          note: note.trim(),
          expires_in_hours: Number(expiresHours) || 72,
          max_payments: Number(maxPayments) || 1,
        },
      });
      setLatestCreated(created);
      await loadData();
      setBanner({
        type: "ok",
        text: "Shareable Pay-for-Me link & QR code generated! Copy or open the guest link below.",
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Could not create link",
      });
    } finally {
      setCreating(false);
    }
  };

  const handleCancel = async (id: string) => {
    try {
      await apiFetch(`/v1/payment-requests/${id}`, { method: "DELETE", token });
      await loadData();
      setBanner({ type: "ok", text: "Pay-for-Me link cancelled." });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Cancel failed",
      });
    }
  };

  const latestPath = latestCreated
    ? toRelativeSharePath(latestCreated.share_url, "/p", latestCreated.code)
    : "";

  return (
    <div className="space-y-6">
      <div className="rounded-2xl bg-[#0B1F1A] p-6 text-white">
        <span className="text-xs font-semibold uppercase tracking-wider text-emerald-300">
          F1 • Pay-for-Me Shareable Links
        </span>
        <h1 className="mt-1 text-2xl font-bold">
          Request electricity from anyone — zero account needed to pay.
        </h1>
        <p className="mt-1 max-w-2xl text-sm text-stone-300">
          Generate an unguessable 128-bit link & QR code. Your sponsor sees only your masked meter and DisCo (never your street address), gets a token-free receipt, and the electricity lands on your meter.
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
            <Share2 className="h-4 w-4 text-emerald-600" />
            Create Payment Request Link
          </h2>

          <div>
            <label className="block text-xs font-medium text-stone-700 mb-1">
              Meter
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

          <div className="grid grid-cols-2 gap-2">
            {(["FIXED", "RANGE"] as const).map((m) => (
              <button
                key={m}
                type="button"
                onClick={() => setMode(m)}
                className={`rounded-xl border py-2 text-xs font-semibold ${
                  mode === m
                    ? "border-emerald-600 bg-emerald-50 text-emerald-900"
                    : "border-stone-200 bg-stone-50 text-stone-600"
                }`}
              >
                {m === "FIXED" ? "Fixed Amount" : "Payer Chooses Range"}
              </button>
            ))}
          </div>

          {mode === "FIXED" ? (
            <div>
              <label className="block text-xs font-medium text-stone-700 mb-1">
                Amount (₦ Naira)
              </label>
              <input
                type="number"
                min={100}
                value={fixedNaira}
                onChange={(e) => setFixedNaira(e.target.value)}
                required
                className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
              />
            </div>
          ) : (
            <div className="grid grid-cols-2 gap-3">
              <div>
                <label className="block text-xs font-medium text-stone-700 mb-1">
                  Min (₦)
                </label>
                <input
                  type="number"
                  min={100}
                  value={minNaira}
                  onChange={(e) => setMinNaira(e.target.value)}
                  required
                  className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
                />
              </div>
              <div>
                <label className="block text-xs font-medium text-stone-700 mb-1">
                  Max (₦)
                </label>
                <input
                  type="number"
                  min={100}
                  value={maxNaira}
                  onChange={(e) => setMaxNaira(e.target.value)}
                  required
                  className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
                />
              </div>
            </div>
          )}

          <div>
            <label className="block text-xs font-medium text-stone-700 mb-1">
              Short Note (max 140 chars, URLs blocked for anti-phishing)
            </label>
            <input
              type="text"
              maxLength={140}
              value={note}
              onChange={(e) => setNote(e.target.value)}
              className="w-full rounded-xl border border-stone-300 px-3 py-2 text-sm"
            />
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="block text-xs font-medium text-stone-700 mb-1">
                Expires In (Hours, max 168)
              </label>
              <input
                type="number"
                min={1}
                max={168}
                value={expiresHours}
                onChange={(e) => setExpiresHours(e.target.value)}
                className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
              />
            </div>
            <div>
              <label className="block text-xs font-medium text-stone-700 mb-1">
                Max Payments
              </label>
              <input
                type="number"
                min={1}
                max={10}
                value={maxPayments}
                onChange={(e) => setMaxPayments(e.target.value)}
                className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
              />
            </div>
          </div>

          <button
            type="submit"
            disabled={creating || !meterId}
            className="w-full rounded-xl bg-emerald-600 px-4 py-3 text-sm font-semibold text-white hover:bg-emerald-700 disabled:opacity-50"
          >
            {creating ? "Generating Link & QR..." : "Generate Shareable Link"}
          </button>
        </form>

        <div className="space-y-4 lg:col-span-7">
          {latestCreated && (
            <div className="rounded-2xl border-2 border-emerald-500 bg-emerald-50/50 p-5 space-y-3">
              <div className="flex items-center justify-between">
                <span className="inline-flex items-center gap-1.5 text-xs font-bold uppercase tracking-wider text-emerald-900">
                  <QrCode className="h-4 w-4" /> Newly Created Guest Link (Shown Once — Stored as HMAC)
                </span>
                <StatusPill status={latestCreated.status} />
              </div>

              <div className="flex flex-wrap items-center gap-4">
                {latestCreated.qr_png_base64 && (
                  <img
                    src={`data:image/png;base64,${latestCreated.qr_png_base64}`}
                    alt="Pay-for-Me QR Code"
                    className="h-28 w-28 rounded-xl border border-stone-200 bg-white p-1.5"
                  />
                )}
                <div className="flex-1 space-y-2 min-w-[220px]">
                  <p className="text-xs text-stone-600">
                    Open this guest link directly to experience payer checkout with zero login:
                  </p>
                  <div className="flex items-center gap-2">
                    <code className="flex-1 truncate rounded-lg bg-white px-3 py-2 font-mono text-xs border border-stone-200">
                      {latestPath}
                    </code>
                    <button
                      type="button"
                      onClick={() => {
                        const full = `${window.location.origin}${latestPath}`;
                        navigator.clipboard.writeText(full).catch(() => null);
                        setCopied(true);
                        setTimeout(() => setCopied(false), 2000);
                      }}
                      className="inline-flex items-center gap-1 rounded-lg border border-stone-300 bg-white px-3 py-2 text-xs font-semibold"
                    >
                      {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
                      Copy
                    </button>
                    <Link
                      href={latestPath}
                      className="inline-flex items-center gap-1 rounded-lg bg-[#0B1F1A] px-3 py-2 text-xs font-semibold text-white hover:bg-emerald-950"
                    >
                      Open Guest Page <ExternalLink className="h-3.5 w-3.5" />
                    </Link>
                  </div>
                </div>
              </div>
            </div>
          )}

          <h2 className="text-base font-bold text-stone-900">
            My Payment Requests ({links.length})
          </h2>

          {links.length === 0 ? (
            <div className="rounded-2xl border border-dashed border-stone-300 bg-white p-8 text-center text-xs text-stone-500">
              No Pay-for-Me links created yet.
            </div>
          ) : (
            <div className="space-y-3">
              {links.map((l) => (
                <div
                  key={l.id}
                  className="flex flex-wrap items-center justify-between gap-3 rounded-2xl border border-stone-200 bg-white p-4 shadow-xs"
                >
                  <div className="space-y-1">
                    <div className="flex items-center gap-2">
                      <span className="font-mono text-sm font-bold text-stone-900">
                        {l.amount_mode === "FIXED"
                          ? formatNaira(l.amount_minor)
                          : `${formatNaira(l.min_amount_minor)} – ${formatNaira(
                              l.max_amount_minor
                            )}`}
                      </span>
                      <StatusPill status={l.status} />
                      <span className="text-xs text-stone-500 font-mono">
                        {l.payments_count}/{l.max_payments} paid
                      </span>
                    </div>
                    <p className="text-xs text-stone-600">
                      {l.masked_meter_number} ({l.disco})
                      {l.note ? ` • “${l.note}”` : ""}
                    </p>
                    <p className="text-[11px] text-stone-400 font-mono">
                      Expires {new Date(l.expires_at).toLocaleString()}
                    </p>
                  </div>

                  {l.status === "OPEN" && (
                    <button
                      type="button"
                      onClick={() => handleCancel(l.id)}
                      className="inline-flex items-center gap-1 rounded-lg border border-rose-200 bg-rose-50 px-3 py-1.5 text-xs font-semibold text-rose-800 hover:bg-rose-100"
                    >
                      <XCircle className="h-3.5 w-3.5" /> Cancel
                    </button>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
