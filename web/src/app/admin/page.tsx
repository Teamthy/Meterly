"use client";

import React, { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useAuth } from "@/components/AuthContext";
import { ApiError, apiFetch, formatSTSToken } from "@/lib/api";
import {
  AdminConsole,
  AdminInvestigation,
  AdminTxnRow,
  ConsoleAction,
  ScreenLine,
} from "@/lib/types";
import { StatusPill, ThreeDimensionStatus } from "@/components/StatusBadges";
import {
  ShieldAlert,
  Search,
  CheckCircle2,
  RotateCcw,
  Sliders,
  Radio,
  Headphones,
} from "lucide-react";

function Lines({
  lines,
  empty,
}: {
  lines?: ScreenLine[];
  empty?: string;
}) {
  if (!lines || lines.length === 0) {
    return <p className="text-xs text-stone-400">{empty}</p>;
  }
  return (
    <dl className="space-y-1.5">
      {lines.map((line, i) => (
        <div key={`${line.label}-${i}`} className="flex justify-between gap-3 text-xs">
          <dt className="text-stone-500">{line.label}</dt>
          <dd
            className={
              line.emphasis
                ? "font-semibold text-stone-900 text-right"
                : "text-stone-800 text-right"
            }
          >
            {line.value}
          </dd>
        </div>
      ))}
    </dl>
  );
}

function TxnList({
  title,
  empty,
  rows,
  selectedId,
  onSelect,
}: {
  title: string;
  empty: string;
  rows: AdminTxnRow[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  return (
    <section className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-3">
      <h2 className="text-sm font-bold text-stone-900 flex items-center gap-1.5">
        <Search className="h-4 w-4 text-emerald-600" />
        {title}
      </h2>
      <div className="max-h-80 overflow-y-auto space-y-2 pr-1">
        {rows.length === 0 ? (
          <p className="text-xs text-stone-400">{empty}</p>
        ) : (
          rows.map((t) => (
            <button
              key={t.transaction_id}
              type="button"
              onClick={() => onSelect(t.transaction_id)}
              className={`w-full rounded-xl border p-3 text-left text-xs transition ${
                selectedId === t.transaction_id
                  ? "border-emerald-600 bg-emerald-50/60"
                  : "border-stone-200 bg-stone-50 hover:bg-stone-100"
              }`}
            >
              <div className="flex items-center justify-between gap-2">
                <span className="font-semibold text-stone-900">{t.amount_label}</span>
                <StatusPill status={t.status} />
              </div>
              <p className="mt-1 text-[11px] text-stone-500">{t.summary}</p>
              <p className="mt-0.5 font-mono text-[11px] text-stone-400">
                {t.when_label} · {t.method_label}
              </p>
            </button>
          ))
        )}
      </div>
    </section>
  );
}

export default function AdminConsolePage() {
  const { token, setPersonaById } = useAuth();
  const [console, setConsole] = useState<AdminConsole | null>(null);
  const [forbidden, setForbidden] = useState(false);
  const [ready, setReady] = useState(false);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [investigation, setInvestigation] = useState<AdminInvestigation | null>(
    null
  );
  const [reason, setReason] = useState("");
  const [revealedToken, setRevealedToken] = useState<string | null>(null);
  const [unitMeterId, setUnitMeterId] = useState("");
  const [verifyMethod, setVerifyMethod] = useState("");
  const [verifyEvidence, setVerifyEvidence] = useState("DEED-LP1-2026-01");
  const [cohortDisco, setCohortDisco] = useState("");
  const [cohortPrefix, setCohortPrefix] = useState("62");
  const [cohortMfr, setCohortMfr] = useState("");
  const [cohortEvidence, setCohortEvidence] = useState(
    "MAP letter March 2026 Hexing AMI — demo attestation"
  );
  const [busy, setBusy] = useState(false);
  const [banner, setBanner] = useState<{ type: "ok" | "err"; text: string } | null>(
    null
  );

  const loadConsole = useCallback(async () => {
    try {
      const c = await apiFetch<AdminConsole>("/v1/admin/console", { token });
      setConsole(c);
      setForbidden(false);
      setReason((prev) => prev || c.reason.placeholder);
      setVerifyMethod((prev) => prev || c.unit_meter.methods[0]?.id || "");
      setCohortDisco((prev) => prev || c.discos[0]?.code || "");
    } catch (e) {
      if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
        setForbidden(true);
        setConsole(null);
        return;
      }
      setBanner({
        type: "err",
        text: e instanceof Error ? e.message : "Failed to load ops console",
      });
    } finally {
      setReady(true);
    }
  }, [token]);

  const loadDetail = useCallback(
    async (id: string) => {
      setSelectedId(id);
      setRevealedToken(null);
      try {
        const d = await apiFetch<{ investigation: AdminInvestigation }>(
          `/v1/admin/transactions/${id}`,
          { token }
        );
        setInvestigation(d.investigation);
      } catch (e) {
        setBanner({
          type: "err",
          text: e instanceof Error ? e.message : "Failed to load investigation",
        });
      }
    },
    [token]
  );

  useEffect(() => {
    loadConsole();
  }, [loadConsole]);

  const runAction = async (action: ConsoleAction, body?: Record<string, unknown>) => {
    if (!action.path || !action.enabled) return;
    const min = console?.reason.min_length ?? 5;
    if (action.needs_reason && reason.trim().length < min) {
      setBanner({
        type: "err",
        text: console?.reason.hint || "Audit reason is required.",
      });
      return;
    }
    setBusy(true);
    try {
      const payload = {
        ...(action.needs_reason ? { reason: reason.trim() } : {}),
        ...(body || {}),
      };
      const res = await apiFetch<Record<string, unknown>>(action.path, {
        method: action.method || "POST",
        token,
        body: Object.keys(payload).length ? payload : undefined,
      });
      if (action.id === "token-reveal" && typeof res.token === "string") {
        setRevealedToken(res.token);
      }
      await loadConsole();
      if (selectedId) await loadDetail(selectedId);
      setBanner({ type: "ok", text: action.label + " recorded." });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : `${action.label} failed`,
      });
    } finally {
      setBusy(false);
    }
  };

  const handleVerifyUnitMeter = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!console) return;
    const tmpl = console.unit_meter.action.path_template || "";
    const path = tmpl.replace("{id}", encodeURIComponent(unitMeterId.trim()));
    if (!unitMeterId.trim() || !path) return;
    await runAction(
      { ...console.unit_meter.action, path, enabled: true },
      {
        method: verifyMethod,
        evidence_reference: verifyEvidence.trim(),
        reason: reason.trim(),
      }
    );
    setUnitMeterId("");
  };

  const handleCreateCohort = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!console?.cohort_attest.enabled || !console.cohort_attest.path) return;
    setBusy(true);
    try {
      await apiFetch(console.cohort_attest.path, {
        method: console.cohort_attest.method || "POST",
        token,
        body: {
          disco: cohortDisco,
          number_prefix: cohortPrefix.trim(),
          manufacturer: cohortMfr.trim() || undefined,
          evidence: cohortEvidence.trim(),
        },
      });
      await loadConsole();
      setBanner({ type: "ok", text: console.cohort_attest.label + " recorded." });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Cohort attest failed",
      });
    } finally {
      setBusy(false);
    }
  };

  if (!ready) {
    return (
      <div className="rounded-2xl border border-stone-200 bg-white p-8 text-sm text-stone-500">
        Loading ops console…
      </div>
    );
  }

  if (forbidden) {
    return (
      <div className="mx-auto max-w-xl rounded-2xl border border-amber-200 bg-white p-8 text-center space-y-4">
        <ShieldAlert className="mx-auto h-10 w-10 text-amber-600" />
        <h1 className="text-xl font-bold text-stone-900">
          Staff role required
        </h1>
        <p className="text-sm text-stone-600">
          The ops console is served by{" "}
          <code className="font-mono">GET /v1/admin/console</code>. Customer
          tokens receive 403. Switch to a staff persona:
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

  if (!console) {
    return (
      <div className="rounded-2xl border border-rose-200 bg-rose-50 p-8 text-sm text-rose-900">
        Could not load the ops console from the API.
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="rounded-2xl bg-[#0B1F1A] p-6 text-white">
        <span className="text-xs font-semibold uppercase tracking-wider text-emerald-300">
          {console.role} · ops
        </span>
        <h1 className="mt-1 text-2xl font-bold">{console.title}</h1>
        <p className="mt-1 max-w-2xl text-sm text-stone-300">{console.subtitle}</p>
        <p className="mt-2 max-w-2xl text-xs text-stone-400">{console.copy}</p>
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

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {console.summary.map((line) => (
          <div
            key={line.label}
            className="rounded-2xl border border-stone-200 bg-white p-4"
          >
            <p className="text-[11px] font-semibold uppercase tracking-wider text-stone-500">
              {line.label}
            </p>
            <p
              className={`mt-1 text-2xl font-bold ${
                line.emphasis ? "text-amber-700" : "text-stone-900"
              }`}
            >
              {line.value}
            </p>
          </div>
        ))}
      </div>

      <div className="rounded-2xl border border-stone-200 bg-white p-4 flex flex-wrap items-center gap-3">
        <label className="text-xs font-bold uppercase tracking-wider text-stone-600">
          {console.reason.label}
        </label>
        <input
          type="text"
          minLength={console.reason.min_length}
          maxLength={console.reason.max_length}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          placeholder={console.reason.placeholder}
          className="flex-1 min-w-[240px] rounded-xl border border-stone-300 px-3 py-1.5 text-xs"
        />
        <p className="w-full text-[11px] text-stone-500">{console.reason.hint}</p>
      </div>

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-12">
        <div className="space-y-5 lg:col-span-5">
          <TxnList
            title={console.review_title}
            empty={console.review_empty}
            rows={console.review}
            selectedId={selectedId}
            onSelect={loadDetail}
          />
          <TxnList
            title={console.recent_title}
            empty={console.recent_empty}
            rows={console.recent}
            selectedId={selectedId}
            onSelect={loadDetail}
          />

          <section className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-2">
            <h3 className="text-sm font-bold text-stone-900 flex items-center gap-1.5">
              <Headphones className="h-4 w-4 text-emerald-600" />
              {console.support.title}
            </h3>
            <p className="text-xs text-stone-600">{console.support.summary}</p>
            <Link
              href={console.support.inbox_path}
              className="inline-flex text-xs font-semibold text-emerald-800 hover:underline"
            >
              Open inbox
            </Link>
          </section>

          <form
            onSubmit={handleVerifyUnitMeter}
            className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-2.5"
          >
            <h3 className="text-sm font-bold text-stone-900 flex items-center gap-1.5">
              <CheckCircle2 className="h-4 w-4 text-emerald-600" />
              {console.unit_meter.title}
            </h3>
            <p className="text-[11px] text-stone-500">{console.unit_meter.hint}</p>
            <input
              type="text"
              value={unitMeterId}
              onChange={(e) => setUnitMeterId(e.target.value)}
              placeholder="UnitMeter UUID"
              className="w-full rounded-xl border border-stone-300 px-3 py-1.5 font-mono text-xs"
            />
            <div className="grid grid-cols-2 gap-2">
              <select
                value={verifyMethod}
                onChange={(e) => setVerifyMethod(e.target.value)}
                className="rounded-xl border border-stone-300 bg-white px-2.5 py-1.5 text-xs"
              >
                {console.unit_meter.methods.map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.label}
                  </option>
                ))}
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
              disabled={busy}
              className="w-full rounded-xl bg-stone-900 py-2 text-xs font-semibold text-white disabled:opacity-50"
            >
              {console.unit_meter.action.label}
            </button>
          </form>

          <section className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-2.5">
            <h3 className="text-sm font-bold text-stone-900 flex items-center gap-1.5">
              <Radio className="h-4 w-4 text-emerald-600" />
              {console.cohorts_title}
            </h3>
            <p className="text-[11px] text-stone-500">{console.cohorts_copy}</p>
            {console.cohort_attest.enabled ? (
              <form onSubmit={handleCreateCohort} className="space-y-2">
                <div className="grid grid-cols-2 gap-2">
                  <select
                    value={cohortDisco}
                    onChange={(e) => setCohortDisco(e.target.value)}
                    className="rounded-xl border border-stone-300 bg-white px-2.5 py-1.5 text-xs"
                  >
                    {console.discos.map((d) => (
                      <option key={d.code} value={d.code}>
                        {d.code}
                      </option>
                    ))}
                  </select>
                  <input
                    value={cohortPrefix}
                    onChange={(e) => setCohortPrefix(e.target.value)}
                    placeholder="Digit prefix"
                    className="rounded-xl border border-stone-300 px-2.5 py-1.5 font-mono text-xs"
                    inputMode="numeric"
                  />
                </div>
                <input
                  value={cohortMfr}
                  onChange={(e) => setCohortMfr(e.target.value)}
                  placeholder="Manufacturer label (optional)"
                  className="w-full rounded-xl border border-stone-300 px-2.5 py-1.5 text-xs"
                />
                <input
                  value={cohortEvidence}
                  onChange={(e) => setCohortEvidence(e.target.value)}
                  placeholder="Evidence 5–500 chars"
                  className="w-full rounded-xl border border-stone-300 px-2.5 py-1.5 text-xs"
                />
                <button
                  type="submit"
                  disabled={busy}
                  className="w-full rounded-xl bg-emerald-800 py-2 text-xs font-semibold text-white disabled:opacity-50"
                >
                  {console.cohort_attest.label}
                </button>
              </form>
            ) : (
              <p className="text-[11px] text-amber-800 bg-amber-50 border border-amber-200 rounded-lg px-2.5 py-1.5">
                {console.cohort_attest.hint || "Admin role required to attest."}
              </p>
            )}
            <ul className="space-y-1.5 max-h-48 overflow-y-auto">
              {console.cohorts.length === 0 ? (
                <li className="text-[11px] text-stone-400">No cohorts yet.</li>
              ) : (
                console.cohorts.map((c) => (
                  <li
                    key={c.id}
                    className="rounded-lg border border-stone-200 bg-stone-50 px-2.5 py-1.5 text-[11px]"
                  >
                    <div className="flex items-start justify-between gap-2">
                      <div className="font-mono">{c.summary}</div>
                      {c.action?.enabled && (
                        <button
                          type="button"
                          disabled={busy}
                          onClick={() => runAction(c.action!)}
                          className="text-rose-800 font-semibold hover:underline disabled:opacity-50"
                        >
                          {c.action.label}
                        </button>
                      )}
                    </div>
                    <p className="text-stone-500 truncate" title={c.evidence}>
                      {c.evidence}
                    </p>
                  </li>
                ))
              )}
            </ul>
          </section>

          {console.providers.length > 0 && (
            <section className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-2">
              <h3 className="text-sm font-bold text-stone-900 flex items-center gap-1.5">
                <Sliders className="h-4 w-4 text-emerald-600" />
                Providers
              </h3>
              {console.providers.map((p) => (
                <div
                  key={p.name}
                  className="flex items-center justify-between rounded-xl bg-stone-50 px-3 py-2 text-xs"
                >
                  <div>
                    <span className="font-mono font-semibold">{p.name}</span>
                    <span className="ml-2 text-stone-500">{p.status_label}</span>
                  </div>
                  <button
                    type="button"
                    disabled={!p.action.enabled || busy}
                    onClick={() => runAction(p.action)}
                    className={`rounded-lg px-2.5 py-1 font-semibold disabled:opacity-40 ${
                      p.enabled
                        ? "bg-emerald-100 text-emerald-900"
                        : "bg-rose-100 text-rose-900"
                    }`}
                  >
                    {p.action.label}
                  </button>
                </div>
              ))}
            </section>
          )}

          <section className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-2">
            <h3 className="text-sm font-bold text-stone-900">{console.audit_title}</h3>
            <Lines lines={console.audit} empty={console.audit_empty} />
          </section>
        </div>

        <div className="lg:col-span-7">
          {!investigation ? (
            <div className="rounded-2xl border border-dashed border-stone-300 bg-white p-10 text-center text-xs text-stone-500">
              Select a transaction to inspect the server-composed investigation
              pane: three-dimension state, ledger, notifications and audit.
            </div>
          ) : (
            <div className="rounded-2xl border border-stone-200 bg-white p-5 shadow-xs space-y-4">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div>
                  <h3 className="text-sm font-bold text-stone-900">
                    {investigation.title}
                  </h3>
                  <p className="font-mono text-[11px] text-stone-500">
                    {investigation.subtitle}
                  </p>
                </div>
                <StatusPill status={investigation.status} />
              </div>

              <ThreeDimensionStatus
                payment={investigation.payment_status}
                vending={investigation.vending_status}
                delivery={investigation.delivery_status}
                method={investigation.delivery_method}
              />

              <Lines lines={investigation.lines} />

              <div className="flex flex-wrap gap-2 pt-1">
                {investigation.actions.map((action) => (
                  <button
                    key={action.id}
                    type="button"
                    disabled={!action.enabled || busy}
                    title={action.hint}
                    onClick={() => runAction(action)}
                    className="inline-flex items-center gap-1 rounded-lg border border-stone-300 px-3 py-1.5 text-xs font-semibold text-stone-800 hover:bg-stone-50 disabled:opacity-40 disabled:cursor-not-allowed"
                  >
                    {action.id === "reconcile" ? (
                      <RotateCcw className="h-3.5 w-3.5" />
                    ) : null}
                    {action.label}
                  </button>
                ))}
              </div>

              {revealedToken && (
                <div className="rounded-xl border border-amber-300 bg-amber-50 p-3 font-mono text-sm font-bold text-stone-950">
                  Audited reveal: {formatSTSToken(revealedToken)}
                </div>
              )}

              <div className="space-y-1.5 pt-2 border-t border-stone-100">
                <p className="text-xs font-bold uppercase tracking-wider text-stone-500">
                  Ledger
                </p>
                <Lines lines={investigation.ledger} empty={investigation.ledger_empty} />
              </div>
              <div className="space-y-1.5 pt-2 border-t border-stone-100">
                <p className="text-xs font-bold uppercase tracking-wider text-stone-500">
                  Notifications
                </p>
                <Lines
                  lines={investigation.notifications}
                  empty={investigation.notifications_empty}
                />
              </div>
              <div className="space-y-1.5 pt-2 border-t border-stone-100">
                <p className="text-xs font-bold uppercase tracking-wider text-stone-500">
                  History
                </p>
                <Lines lines={investigation.history} empty="No state transitions yet." />
              </div>
              <div className="space-y-1.5 pt-2 border-t border-stone-100">
                <p className="text-xs font-bold uppercase tracking-wider text-stone-500">
                  Audit
                </p>
                <Lines lines={investigation.audit} empty={investigation.audit_empty} />
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
