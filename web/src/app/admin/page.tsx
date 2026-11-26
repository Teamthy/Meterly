"use client";

import React, { useCallback, useEffect, useState } from "react";
import { useAuth } from "@/components/AuthContext";
import { apiFetch, formatNaira, formatSTSToken } from "@/lib/api";
import { TransactionView } from "@/lib/types";
import { StatusPill, ThreeDimensionStatus } from "@/components/StatusBadges";
import {
  ShieldAlert,
  Search,
  CheckCircle2,
  Eye,
  RotateCcw,
  Sliders,
} from "lucide-react";

export default function AdminConsolePage() {
  const { token, setPersonaById } = useAuth();
  const isStaff =
    token.startsWith("operator:") || token.startsWith("admin:");

  const [reviewOnly, setReviewOnly] = useState(false);
  const [txns, setTxns] = useState<TransactionView[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [detail, setDetail] = useState<Record<string, unknown> | null>(null);
  const [providers, setProviders] = useState<
    Array<{ name: string; type?: string; enabled: boolean }>
  >([]);
  const [reason, setReason] = useState(
    "Operator investigation & reconciliation per runbook"
  );
  const [revealedToken, setRevealedToken] = useState<string | null>(null);

  // Unit meter verification form
  const [unitMeterId, setUnitMeterId] = useState("");
  const [verifyMethod, setVerifyMethod] = useState<
    "ACCOUNT_NAME_MATCH" | "EVIDENCE_APPROVED"
  >("EVIDENCE_APPROVED");
  const [verifyEvidence, setVerifyEvidence] = useState("DEED-LP1-2026-01");
  const [banner, setBanner] = useState<{ type: "ok" | "err"; text: string } | null>(
    null
  );

  const loadList = useCallback(async () => {
    if (!isStaff) return;
    try {
      const [tRes, pRes] = await Promise.all([
        apiFetch<{ data: TransactionView[] }>(
          `/v1/admin/transactions${reviewOnly ? "?review=true" : ""}`,
          { token }
        ),
        apiFetch<{
          data?: Array<{ name: string; type?: string; enabled: boolean }>;
          providers?: Array<{ name: string; type?: string; enabled: boolean }>;
        }>("/v1/admin/providers", { token }).catch(() => ({
          data: [],
          providers: [],
        })),
      ]);
      setTxns(tRes.data || []);
      setProviders(pRes.data || pRes.providers || []);
    } catch (e) {
      setBanner({
        type: "err",
        text: e instanceof Error ? e.message : "Failed to query admin API",
      });
    }
  }, [isStaff, reviewOnly, token]);

  useEffect(() => {
    loadList();
  }, [loadList]);

  const loadDetail = useCallback(
    async (id: string) => {
      setSelectedId(id);
      setRevealedToken(null);
      try {
        const d = await apiFetch<Record<string, unknown>>(
          `/v1/admin/transactions/${id}`,
          { token }
        );
        setDetail(d);
      } catch (e) {
        setBanner({
          type: "err",
          text: e instanceof Error ? e.message : "Failed to load detail",
        });
      }
    },
    [token]
  );

  const runAdminAction = async (
    actionPath: "reconcile" | "fallback-to-token" | "requeue" | "refund"
  ) => {
    if (!selectedId) return;
    if (reason.trim().length < 5) {
      setBanner({
        type: "err",
        text: "Audit reason (5–500 chars) is mandatory for every staff mutation.",
      });
      return;
    }
    try {
      await apiFetch(`/v1/admin/transactions/${selectedId}/${actionPath}`, {
        method: "POST",
        token,
        body: { reason: reason.trim() },
      });
      await loadList();
      await loadDetail(selectedId);
      setBanner({
        type: "ok",
        text: `Executed audited ${actionPath} on transaction ${selectedId.slice(
          0,
          8
        )}.`,
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : `${actionPath} failed`,
      });
    }
  };

  const handleAuditedReveal = async () => {
    if (!selectedId) return;
    try {
      const res = await apiFetch<{ token: string }>(
        `/v1/admin/transactions/${selectedId}/token-reveal`,
        {
          method: "POST",
          token,
          body: { reason: reason.trim() },
        }
      );
      setRevealedToken(res.token);
      await loadDetail(selectedId);
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Token reveal failed",
      });
    }
  };

  const handleToggleProvider = async (name: string, enable: boolean) => {
    try {
      await apiFetch(
        `/v1/admin/providers/${encodeURIComponent(name)}/${
          enable ? "enable" : "disable"
        }`,
        {
          method: "POST",
          token,
          body: { reason: reason.trim() },
        }
      );
      await loadList();
      setBanner({
        type: "ok",
        text: `Provider ${name} ${enable ? "enabled" : "paused"} (admin audited).`,
      });
    } catch (err) {
      setBanner({
        type: "err",
        text:
          err instanceof Error
            ? err.message
            : "Switch to Segun (Admin) to toggle providers",
      });
    }
  };

  const handleVerifyUnitMeter = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!unitMeterId.trim()) return;
    try {
      await apiFetch(
        `/v1/admin/unit-meters/${encodeURIComponent(unitMeterId.trim())}/verify`,
        {
          method: "POST",
          token,
          body: {
            method: verifyMethod,
            evidence_reference: verifyEvidence.trim(),
            reason: reason.trim(),
          },
        }
      );
      setBanner({
        type: "ok",
        text: `Unit meter ${unitMeterId.slice(
          0,
          8
        )}… verified via ${verifyMethod} (audited)!`,
      });
      setUnitMeterId("");
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Verification failed",
      });
    }
  };

  if (!isStaff) {
    return (
      <div className="mx-auto max-w-xl rounded-2xl border border-amber-200 bg-white p-8 text-center space-y-4">
        <ShieldAlert className="mx-auto h-10 w-10 text-amber-600" />
        <h1 className="text-xl font-bold text-stone-900">
          Staff Role Required (Operator / Admin + MFA)
        </h1>
        <p className="text-sm text-stone-600">
          Customer tokens receive <code className="font-mono">403 FORBIDDEN</code> on{" "}
          <code className="font-mono">/v1/admin/*</code>. Switch to a staff persona to inspect stuck transactions, ledger postings, and audited actions:
        </p>
        <div className="flex justify-center gap-3">
          <button
            type="button"
            onClick={() => setPersonaById("operator")}
            className="rounded-xl bg-stone-900 px-4 py-2.5 text-xs font-semibold text-white"
          >
            Switch to Kemi (Operator)
          </button>
          <button
            type="button"
            onClick={() => setPersonaById("admin")}
            className="rounded-xl bg-emerald-600 px-4 py-2.5 text-xs font-semibold text-white"
          >
            Switch to Segun (Admin)
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="rounded-2xl bg-[#0B1F1A] p-6 text-white">
        <span className="text-xs font-semibold uppercase tracking-wider text-emerald-300">
          M4 / M5 / M10 • Operator & Admin Investigation Console
        </span>
        <h1 className="mt-1 text-2xl font-bold">
          Reconciliation, double-entry ledger inspection & audited actions.
        </h1>
        <p className="mt-1 max-w-2xl text-sm text-stone-300">
          Every staff mutation requires a 5–500 character reason and commits an immutable <code className="font-mono">audit_events</code> row in the same database transaction.
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

      {/* Mandatory Audit Reason Bar */}
      <div className="rounded-2xl border border-stone-200 bg-white p-4 flex flex-wrap items-center gap-3">
        <label className="text-xs font-bold uppercase tracking-wider text-stone-600">
          Mandatory Audit Reason:
        </label>
        <input
          type="text"
          minLength={5}
          maxLength={500}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          className="flex-1 min-w-[240px] rounded-xl border border-stone-300 px-3 py-1.5 text-xs"
        />
      </div>

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-12">
        {/* Left Column: Search + Unit Meter Verify + Provider Switches */}
        <div className="space-y-5 lg:col-span-5">
          <section className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-3">
            <div className="flex items-center justify-between">
              <h2 className="text-sm font-bold text-stone-900 flex items-center gap-1.5">
                <Search className="h-4 w-4 text-emerald-600" />
                Transactions ({txns.length})
              </h2>
              <label className="flex items-center gap-1.5 text-xs text-stone-700 cursor-pointer">
                <input
                  type="checkbox"
                  checked={reviewOnly}
                  onChange={(e) => setReviewOnly(e.target.checked)}
                />
                Manual Review Only
              </label>
            </div>

            <div className="max-h-80 overflow-y-auto space-y-2 pr-1">
              {txns.map((t) => (
                <button
                  key={t.transaction_id}
                  type="button"
                  onClick={() => loadDetail(t.transaction_id)}
                  className={`w-full rounded-xl border p-3 text-left text-xs transition ${
                    selectedId === t.transaction_id
                      ? "border-emerald-600 bg-emerald-50/60"
                      : "border-stone-200 bg-stone-50 hover:bg-stone-100"
                  }`}
                >
                  <div className="flex items-center justify-between">
                    <span className="font-mono font-bold text-stone-900">
                      {formatNaira(t.total_minor)}
                    </span>
                    <StatusPill status={t.status} />
                  </div>
                  <p className="mt-1 font-mono text-[11px] text-stone-500">
                    {t.transaction_id.slice(0, 12)}… • Pay:{t.payment_status} •
                    Vend:{t.vending_status}
                  </p>
                </button>
              ))}
            </div>
          </section>

          {/* Power Desk Unit-Meter Verification */}
          <form
            onSubmit={handleVerifyUnitMeter}
            className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-2.5"
          >
            <h3 className="text-sm font-bold text-stone-900 flex items-center gap-1.5">
              <CheckCircle2 className="h-4 w-4 text-emerald-600" />
              Verify Power Desk Unit Meter
            </h3>
            <input
              type="text"
              value={unitMeterId}
              onChange={(e) => setUnitMeterId(e.target.value)}
              placeholder="Paste UnitMeter UUID from Power Desk"
              className="w-full rounded-xl border border-stone-300 px-3 py-1.5 font-mono text-xs"
            />
            <div className="grid grid-cols-2 gap-2">
              <select
                value={verifyMethod}
                onChange={(e) =>
                  setVerifyMethod(
                    e.target.value as "ACCOUNT_NAME_MATCH" | "EVIDENCE_APPROVED"
                  )
                }
                className="rounded-xl border border-stone-300 bg-white px-2.5 py-1.5 text-xs"
              >
                <option value="EVIDENCE_APPROVED">EVIDENCE_APPROVED</option>
                <option value="ACCOUNT_NAME_MATCH">ACCOUNT_NAME_MATCH</option>
              </select>
              <input
                type="text"
                value={verifyEvidence}
                onChange={(e) => setVerifyEvidence(e.target.value)}
                placeholder="Deed / utility bill ref"
                className="rounded-xl border border-stone-300 px-2.5 py-1.5 text-xs"
              />
            </div>
            <button
              type="submit"
              className="w-full rounded-xl bg-stone-900 py-2 text-xs font-semibold text-white"
            >
              Approve & Audit Unit Meter Ownership
            </button>
          </form>

          {/* Provider Switches */}
          {providers.length > 0 && (
            <section className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-2">
              <h3 className="text-sm font-bold text-stone-900 flex items-center gap-1.5">
                <Sliders className="h-4 w-4 text-emerald-600" />
                Provider Circuit Switches (Admin)
              </h3>
              {providers.map((p) => (
                <div
                  key={p.name}
                  className="flex items-center justify-between rounded-xl bg-stone-50 px-3 py-2 text-xs"
                >
                  <span className="font-mono font-semibold">{p.name}</span>
                  <button
                    type="button"
                    onClick={() => handleToggleProvider(p.name, !p.enabled)}
                    className={`rounded-lg px-2.5 py-1 font-semibold ${
                      p.enabled
                        ? "bg-emerald-100 text-emerald-900"
                        : "bg-rose-100 text-rose-900"
                    }`}
                  >
                    {p.enabled ? "Enabled (Pause)" : "Paused (Enable)"}
                  </button>
                </div>
              ))}
            </section>
          )}
        </div>

        {/* Right Column: Full Transaction Detail */}
        <div className="lg:col-span-7">
          {!detail ? (
            <div className="rounded-2xl border border-dashed border-stone-300 bg-white p-10 text-center text-xs text-stone-500">
              Select a transaction on the left to inspect its 3-dimension state transitions, balanced ledger postings, M5 redacted notification deliveries, and audit trail.
            </div>
          ) : (
            <div className="rounded-2xl border border-stone-200 bg-white p-5 shadow-xs space-y-4">
              {(() => {
                const txObj = (detail.transaction || detail) as TransactionView;
                const ledger = (detail.ledger || []) as Array<
                  Record<string, unknown>
                >;
                const notifs = (detail.notifications || []) as Array<
                  Record<string, unknown>
                >;
                const audits = (detail.audit || []) as Array<
                  Record<string, unknown>
                >;

                return (
                  <>
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <div>
                        <h3 className="font-mono text-sm font-bold text-stone-900">
                          {txObj.transaction_id}
                        </h3>
                        <p className="text-xs text-stone-500">
                          Total {formatNaira(txObj.total_minor)} • Method:{" "}
                          {txObj.delivery_method}
                        </p>
                      </div>
                      <StatusPill status={txObj.status} />
                    </div>

                    <ThreeDimensionStatus
                      payment={txObj.payment_status}
                      vending={txObj.vending_status}
                      delivery={txObj.delivery_status}
                      method={txObj.delivery_method}
                    />

                    {/* Operator Action Buttons */}
                    <div className="flex flex-wrap gap-2 pt-1">
                      <button
                        type="button"
                        onClick={() => runAdminAction("reconcile")}
                        className="inline-flex items-center gap-1 rounded-lg bg-stone-900 px-3 py-1.5 text-xs font-semibold text-white"
                      >
                        <RotateCcw className="h-3.5 w-3.5" /> Reconcile
                      </button>
                      <button
                        type="button"
                        onClick={() => runAdminAction("fallback-to-token")}
                        className="rounded-lg border border-stone-300 px-3 py-1.5 text-xs font-semibold text-stone-800 hover:bg-stone-50"
                      >
                        Fallback to Token
                      </button>
                      <button
                        type="button"
                        onClick={handleAuditedReveal}
                        className="inline-flex items-center gap-1 rounded-lg border border-stone-300 px-3 py-1.5 text-xs font-semibold text-stone-800 hover:bg-stone-50"
                      >
                        <Eye className="h-3.5 w-3.5" /> Audited Token Reveal
                      </button>
                      <button
                        type="button"
                        onClick={() => runAdminAction("requeue")}
                        className="rounded-lg border border-amber-300 bg-amber-50 px-3 py-1.5 text-xs font-semibold text-amber-900"
                      >
                        Requeue (Admin)
                      </button>
                      <button
                        type="button"
                        onClick={() => runAdminAction("refund")}
                        className="rounded-lg border border-rose-300 bg-rose-50 px-3 py-1.5 text-xs font-semibold text-rose-900"
                      >
                        Operator Refund (Admin)
                      </button>
                    </div>

                    {revealedToken && (
                      <div className="rounded-xl border border-amber-300 bg-amber-50 p-3 font-mono text-sm font-bold text-stone-950">
                        Audited Reveal: {formatSTSToken(revealedToken)}
                      </div>
                    )}

                    {/* Double-Entry Ledger */}
                    <div className="space-y-1.5 pt-2 border-t border-stone-100">
                      <p className="text-xs font-bold uppercase tracking-wider text-stone-500">
                        Balanced Double-Entry Ledger ({ledger.length})
                      </p>
                      {ledger.length === 0 ? (
                        <p className="text-xs text-stone-400">
                          No ledger entries posted yet.
                        </p>
                      ) : (
                        <pre className="max-h-40 overflow-auto rounded-xl bg-stone-50 p-3 font-mono text-[11px] text-stone-700 border border-stone-200">
                          {JSON.stringify(ledger, null, 2)}
                        </pre>
                      )}
                    </div>

                    {/* Redacted Notification Deliveries */}
                    <div className="space-y-1.5 pt-2 border-t border-stone-100">
                      <p className="text-xs font-bold uppercase tracking-wider text-stone-500">
                        Redacted Notification Deliveries ({notifs.length})
                      </p>
                      {notifs.length === 0 ? (
                        <p className="text-xs text-stone-400">
                          No notification rows recorded.
                        </p>
                      ) : (
                        <pre className="max-h-36 overflow-auto rounded-xl bg-stone-50 p-3 font-mono text-[11px] text-stone-700 border border-stone-200">
                          {JSON.stringify(notifs, null, 2)}
                        </pre>
                      )}
                    </div>

                    {/* Audit Trail */}
                    <div className="space-y-1.5 pt-2 border-t border-stone-100">
                      <p className="text-xs font-bold uppercase tracking-wider text-stone-500">
                        Immutable Audit Trail ({audits.length})
                      </p>
                      <pre className="max-h-40 overflow-auto rounded-xl bg-stone-50 p-3 font-mono text-[11px] text-stone-700 border border-stone-200">
                        {JSON.stringify(audits, null, 2)}
                      </pre>
                    </div>
                  </>
                );
              })()}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
