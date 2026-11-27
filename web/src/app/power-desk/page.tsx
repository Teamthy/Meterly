"use client";

import React, { useCallback, useEffect, useState } from "react";
import { useAuth } from "@/components/AuthContext";
import {
  apiFetch,
  DISCOS,
  formatNaira,
  newIdempotencyKey,
} from "@/lib/api";
import {
  BulkTopupDetailView,
  MonthlyStatement,
  OrgDetailView,
} from "@/lib/types";
import { StatusPill } from "@/components/StatusBadges";
import {
  Building2,
  Plus,
  Layers,
  Download,
  CheckCircle2,
  Copy,
  RefreshCw,
} from "lucide-react";

const ORG_STORAGE = "meterly_org_ids";
const BULK_STORAGE = "meterly_bulk_pots";

export default function PowerDeskPage() {
  const { token, email, openCheckout } = useAuth();

  const [orgs, setOrgs] = useState<OrgDetailView[]>([]);
  const [selectedOrgId, setSelectedOrgId] = useState<string>("");
  const [banner, setBanner] = useState<{ type: "ok" | "err"; text: string } | null>(
    null
  );

  // Create Org form
  const [orgName, setOrgName] = useState("TEST CUSTOMER 2222");
  const [orgType, setOrgType] = useState<"LANDLORD" | "ESTATE" | "PROPERTY_MANAGER">(
    "ESTATE"
  );
  const [thresholdNaira, setThresholdNaira] = useState("50000");

  // Add Property & Unit & Meter forms
  const [propName, setPropName] = useState("Lekki Phase 1 Court");
  const [propCity, setPropCity] = useState("Lagos");
  const [unitPropId, setUnitPropId] = useState("");
  const [unitName, setUnitName] = useState("Flat 1A");
  const [attachUnitId, setAttachUnitId] = useState("");
  const [attachMeterNo, setAttachMeterNo] = useState("45011112222");
  const [attachDisco, setAttachDisco] = useState("EKEDC");
  const [evidenceRef, setEvidenceRef] = useState("DEED-LP1-2026-01");

  // Bulk Top-Up state
  const [unitAmounts, setUnitAmounts] = useState<Record<string, string>>({});
  const [bulkHistory, setBulkHistory] = useState<BulkTopupDetailView[]>([]);

  // Monthly Statement state
  const [statementMonth, setStatementMonth] = useState(() => {
    const now = new Date();
    return `${now.getUTCFullYear()}-${String(now.getUTCMonth() + 1).padStart(
      2,
      "0"
    )}`;
  });
  const [statement, setStatement] = useState<MonthlyStatement | null>(null);

  const loadOrgs = useCallback(async () => {
    let ids: string[] = [];
    try {
      ids = JSON.parse(window.localStorage.getItem(ORG_STORAGE) || "[]");
    } catch {
      ids = [];
    }
    const loaded: OrgDetailView[] = [];
    for (const id of ids) {
      try {
        const o = await apiFetch<OrgDetailView>(`/v1/orgs/${id}`, { token });
        loaded.push(o);
      } catch {
        // caller not a member -> 404 (zero IDOR)
      }
    }
    setOrgs(loaded);
    if (loaded.length > 0) {
      setSelectedOrgId((prev) =>
        loaded.some((o) => o.id === prev) ? prev : loaded[0].id
      );
    } else {
      setSelectedOrgId("");
    }
  }, [token]);

  useEffect(() => {
    loadOrgs();
  }, [loadOrgs]);

  const activeOrg = orgs.find((o) => o.id === selectedOrgId) || null;

  useEffect(() => {
    if (!activeOrg) return;
    if (activeOrg.properties.length > 0) {
      setUnitPropId((prev) =>
        activeOrg.properties.some((p) => p.id === prev)
          ? prev
          : activeOrg.properties[0].id
      );
    }
    const meteredless = activeOrg.units.find((u) => !u.unit_meter);
    if (meteredless) {
      setAttachUnitId(meteredless.id);
    } else if (activeOrg.units.length > 0) {
      setAttachUnitId(activeOrg.units[0].id);
    }
    const defaults: Record<string, string> = {};
    for (const u of activeOrg.units) {
      if (u.unit_meter) {
        defaults[u.id] = unitAmounts[u.id] || "5000";
      }
    }
    setUnitAmounts(defaults);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeOrg?.id, activeOrg?.properties.length, activeOrg?.units.length]);

  const loadBulksAndStatement = useCallback(async () => {
    if (!selectedOrgId) {
      setBulkHistory([]);
      setStatement(null);
      return;
    }
    try {
      const st = await apiFetch<MonthlyStatement>(
        `/v1/orgs/${selectedOrgId}/statements?month=${encodeURIComponent(
          statementMonth
        )}`,
        { token }
      );
      setStatement(st);
    } catch {
      setStatement(null);
    }

    let potIds: string[] = [];
    try {
      potIds = JSON.parse(
        window.localStorage.getItem(`${BULK_STORAGE}:${selectedOrgId}`) || "[]"
      );
    } catch {
      potIds = [];
    }
    const loadedBulks: BulkTopupDetailView[] = [];
    for (const pid of potIds) {
      try {
        const b = await apiFetch<BulkTopupDetailView>(
          `/v1/orgs/${selectedOrgId}/bulk-topups/${pid}`,
          { token }
        );
        loadedBulks.push(b);
      } catch {
        // ignore
      }
    }
    setBulkHistory(loadedBulks);
  }, [selectedOrgId, statementMonth, token]);

  useEffect(() => {
    loadBulksAndStatement();
  }, [loadBulksAndStatement]);

  const rememberId = (key: string, id: string) => {
    try {
      const cur: string[] = JSON.parse(window.localStorage.getItem(key) || "[]");
      if (!cur.includes(id)) {
        window.localStorage.setItem(key, JSON.stringify([id, ...cur]));
      }
    } catch {
      // ignore
    }
  };

  const handleCreateOrg = async (e: React.FormEvent) => {
    e.preventDefault();
    setBanner(null);
    try {
      const thresh = Number(thresholdNaira);
      const created = await apiFetch<OrgDetailView>("/v1/orgs", {
        method: "POST",
        token,
        body: {
          name: orgName.trim(),
          legal_name: orgName.trim(),
          organisation_type: orgType,
          manager_approval_threshold_minor:
            thresh > 0 ? Math.round(thresh * 100) : undefined,
        },
      });
      rememberId(ORG_STORAGE, created.id);
      await loadOrgs();
      setSelectedOrgId(created.id);
      setBanner({
        type: "ok",
        text: `Organisation “${created.name}” created with caller as OWNER.`,
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Could not create organisation",
      });
    }
  };

  const handleCreateProperty = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!activeOrg) return;
    try {
      await apiFetch(`/v1/orgs/${activeOrg.id}/properties`, {
        method: "POST",
        token,
        body: {
          name: propName.trim(),
          city: propCity.trim(),
          state: "Lagos",
          country: "NG",
        },
      });
      await loadOrgs();
      setBanner({ type: "ok", text: "Property added." });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Property creation failed",
      });
    }
  };

  const handleCreateUnit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!unitPropId) return;
    try {
      await apiFetch(`/v1/properties/${unitPropId}/units`, {
        method: "POST",
        token,
        body: {
          building_name: "Main Block",
          unit_name: unitName.trim(),
          occupancy_status: "OCCUPIED",
        },
      });
      await loadOrgs();
      setBanner({ type: "ok", text: `Unit ${unitName} created.` });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Unit creation failed",
      });
    }
  };

  const handleAttachMeter = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!attachUnitId) return;
    try {
      await apiFetch(`/v1/units/${attachUnitId}/meters`, {
        method: "POST",
        token,
        body: {
          meter_number: attachMeterNo.trim(),
          disco_code: attachDisco,
          label: "Unit Prepaid",
          evidence_reference: evidenceRef.trim(),
        },
      });
      await loadOrgs();
      setBanner({
        type: "ok",
        text: "Meter attached to unit! Bulk top-ups work immediately; history & tenant delegation unlock once verified by an operator.",
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Attach meter failed",
      });
    }
  };

  const handleBulkTopup = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!activeOrg) return;
    const meteredUnits = activeOrg.units.filter((u) => u.unit_meter);
    const items = meteredUnits
      .map((u) => ({
        unit_id: u.id,
        amount_minor: Math.round((Number(unitAmounts[u.id]) || 0) * 100),
      }))
      .filter((it) => it.amount_minor > 0);

    if (items.length === 0) {
      setBanner({
        type: "err",
        text: "Attach at least one unit meter and set a positive Naira amount.",
      });
      return;
    }

    try {
      const view = await apiFetch<BulkTopupDetailView>(
        `/v1/orgs/${activeOrg.id}/bulk-topups`,
        {
          method: "POST",
          token,
          idempotencyKey: newIdempotencyKey("bulk"),
          body: { email, items },
        }
      );
      rememberId(`${BULK_STORAGE}:${activeOrg.id}`, view.pot_id);
      await loadBulksAndStatement();

      if (view.approval_status === "PENDING_APPROVAL") {
        setBanner({
          type: "ok",
          text: "Bulk top-up exceeds Manager threshold and is awaiting OWNER approval.",
        });
      } else if (view.checkout_url) {
        openCheckout({
          checkoutUrl: view.checkout_url,
          totalMinor: view.total_minor,
          title: `Compound Bulk Top-Up (${items.length} unit${
            items.length === 1 ? "" : "s"
          })`,
          subtitle: `${formatNaira(view.electricity_total_minor)} electricity + ${formatNaira(
            view.fee_total_minor
          )} fees`,
          onComplete: async () => {
            await loadBulksAndStatement();
            setBanner({
              type: "ok",
              text: "Single landlord payment verified — POT-funded unit allocations dispatched!",
            });
          },
        });
      }
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Bulk top-up failed",
      });
    }
  };

  const handleApproveBulk = async (potId: string) => {
    if (!activeOrg) return;
    try {
      const view = await apiFetch<BulkTopupDetailView>(
        `/v1/orgs/${activeOrg.id}/bulk-topups/${potId}/approve`,
        {
          method: "POST",
          token,
          body: { email },
        }
      );
      await loadBulksAndStatement();
      if (view.checkout_url) {
        openCheckout({
          checkoutUrl: view.checkout_url,
          totalMinor: view.total_minor,
          title: "Approved Bulk Top-Up Checkout",
          onComplete: loadBulksAndStatement,
        });
      }
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Approval failed",
      });
    }
  };

  const handleDownloadCSV = async () => {
    if (!activeOrg) return;
    try {
      const csv = await apiFetch<string>(
        `/v1/orgs/${activeOrg.id}/statements?month=${encodeURIComponent(
          statementMonth
        )}&format=csv`,
        { token }
      );
      const blob = new Blob([csv], { type: "text/csv;charset=utf-8" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `meterly-statement-${activeOrg.id.slice(0, 8)}-${statementMonth}.csv`;
      a.click();
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "CSV export failed",
      });
    }
  };

  return (
    <div className="space-y-6">
      <div className="ml-panel p-6 sm:p-8">
        <span className="text-xs font-semibold uppercase tracking-wider text-volt">
          F4 • Landlord Power Desk & Compound Bulk Top-Ups
        </span>
        <h1 className="font-display mt-3 text-3xl font-semibold leading-tight">
          One payment across dozens of units — ledger-derived monthly statements.
        </h1>
        <p className="mt-3 max-w-2xl text-sm leading-relaxed text-white/55">
          Organisation RBAC (<code className="font-mono">OWNER</code>,{" "}
          <code className="font-mono">MANAGER</code>,{" "}
          <code className="font-mono">VIEWER</code>), verified unit-meter ownership, honest partial-success reporting with automatic unit-failure refunds, and formula-sanitized CSV exports.
        </p>
      </div>

      {banner && (
        <div
          className={`rounded-xl border px-4 py-3 text-sm ${
            banner.type === "ok"
              ? "border-stone-200 bg-volt/20 text-ink"
              : "border-rose-200 bg-rose-50 text-rose-900"
          }`}
        >
          {banner.text}
        </div>
      )}

      {/* Top Row: Create or Select Organisation */}
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-12">
        <form
          onSubmit={handleCreateOrg}
          className="space-y-3 ml-card p-5 lg:col-span-5"
        >
          <h2 className="text-base font-bold text-stone-900 flex items-center gap-2">
            <Building2 className="h-4 w-4 text-ink" />
            Create Landlord / Estate Organisation
          </h2>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="block text-xs font-medium text-stone-700 mb-1">
                Organisation Name
              </label>
              <input
                type="text"
                value={orgName}
                onChange={(e) => setOrgName(e.target.value)}
                required
                className="w-full rounded-xl border border-stone-300 px-3 py-2 text-xs"
              />
            </div>
            <div>
              <label className="block text-xs font-medium text-stone-700 mb-1">
                Type
              </label>
              <select
                value={orgType}
                onChange={(e) =>
                  setOrgType(
                    e.target.value as "LANDLORD" | "ESTATE" | "PROPERTY_MANAGER"
                  )
                }
                className="w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-xs"
              >
                <option value="ESTATE">Estate</option>
                <option value="LANDLORD">Landlord</option>
                <option value="PROPERTY_MANAGER">Property Manager</option>
              </select>
            </div>
          </div>
          <div>
            <label className="block text-xs font-medium text-stone-700 mb-1">
              Manager Approval Threshold (₦ — optional)
            </label>
            <input
              type="number"
              min={0}
              value={thresholdNaira}
              onChange={(e) => setThresholdNaira(e.target.value)}
              className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-xs"
            />
          </div>
          <button
            type="submit"
            className="w-full rounded-full bg-ink px-4 py-2.5 text-xs font-semibold text-volt hover:bg-black"
          >
            Create Organisation
          </button>
        </form>

        <div className="ml-card p-5 lg:col-span-7 space-y-3">
          <div className="flex items-center justify-between">
            <h2 className="text-base font-bold text-stone-900">
              Active Organisation Workspace
            </h2>
            <button
              type="button"
              onClick={() => {
                loadOrgs();
                loadBulksAndStatement();
              }}
              className="inline-flex items-center gap-1 text-xs font-semibold text-stone-600"
            >
              <RefreshCw className="h-3.5 w-3.5" /> Refresh
            </button>
          </div>

          {orgs.length === 0 ? (
            <p className="text-xs text-stone-500">
              Create an organisation on the left (tip: switch persona to{" "}
              <strong>Chief Tunde (Landlord)</strong> in the top bar!).
            </p>
          ) : (
            <div className="space-y-3">
              <select
                value={selectedOrgId}
                onChange={(e) => setSelectedOrgId(e.target.value)}
                className="w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-sm font-semibold"
              >
                {orgs.map((o) => (
                  <option key={o.id} value={o.id}>
                    {o.name} ({o.organisation_type}) — Role: {o.caller_role}
                  </option>
                ))}
              </select>

              {activeOrg && (
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-3 pt-1">
                  {/* Add Property */}
                  <form
                    onSubmit={handleCreateProperty}
                    className="rounded-xl border border-stone-200 bg-stone-50 p-3 space-y-2"
                  >
                    <p className="text-xs font-bold text-stone-800">
                      1. Add Property
                    </p>
                    <input
                      type="text"
                      value={propName}
                      onChange={(e) => setPropName(e.target.value)}
                      placeholder="Property name"
                      className="w-full rounded-lg border border-stone-300 bg-white px-2.5 py-1.5 text-xs"
                    />
                    <input
                      type="text"
                      value={propCity}
                      onChange={(e) => setPropCity(e.target.value)}
                      placeholder="City"
                      className="w-full rounded-lg border border-stone-300 bg-white px-2.5 py-1.5 text-xs"
                    />
                    <button
                      type="submit"
                      className="w-full rounded-lg bg-stone-900 py-1.5 text-xs font-semibold text-white"
                    >
                      <Plus className="inline h-3 w-3" /> Add Property
                    </button>
                  </form>

                  {/* Add Unit */}
                  <form
                    onSubmit={handleCreateUnit}
                    className="rounded-xl border border-stone-200 bg-stone-50 p-3 space-y-2"
                  >
                    <p className="text-xs font-bold text-stone-800">2. Add Unit</p>
                    <select
                      value={unitPropId}
                      onChange={(e) => setUnitPropId(e.target.value)}
                      className="w-full rounded-lg border border-stone-300 bg-white px-2 py-1.5 text-xs"
                    >
                      {activeOrg.properties.length === 0 && (
                        <option value="">Add property first</option>
                      )}
                      {activeOrg.properties.map((p) => (
                        <option key={p.id} value={p.id}>
                          {p.name}
                        </option>
                      ))}
                    </select>
                    <input
                      type="text"
                      value={unitName}
                      onChange={(e) => setUnitName(e.target.value)}
                      placeholder="e.g. Flat 2B"
                      className="w-full rounded-lg border border-stone-300 bg-white px-2.5 py-1.5 text-xs"
                    />
                    <button
                      type="submit"
                      disabled={!unitPropId}
                      className="w-full rounded-lg bg-stone-900 py-1.5 text-xs font-semibold text-white disabled:opacity-40"
                    >
                      <Plus className="inline h-3 w-3" /> Add Unit
                    </button>
                  </form>

                  {/* Attach Meter */}
                  <form
                    onSubmit={handleAttachMeter}
                    className="rounded-xl border border-stone-200 bg-stone-50 p-3 space-y-2"
                  >
                    <p className="text-xs font-bold text-stone-800">
                      3. Attach Meter
                    </p>
                    <select
                      value={attachUnitId}
                      onChange={(e) => setAttachUnitId(e.target.value)}
                      className="w-full rounded-lg border border-stone-300 bg-white px-2 py-1.5 text-xs"
                    >
                      {activeOrg.units.length === 0 && (
                        <option value="">Add unit first</option>
                      )}
                      {activeOrg.units.map((u) => (
                        <option key={u.id} value={u.id}>
                          {u.unit_name}{" "}
                          {u.masked_meter_number
                            ? `(${u.masked_meter_number})`
                            : "(no meter)"}
                        </option>
                      ))}
                    </select>
                    <div className="flex gap-1">
                      <select
                        value={attachDisco}
                        onChange={(e) => setAttachDisco(e.target.value)}
                        className="w-24 rounded-lg border border-stone-300 bg-white px-1.5 py-1.5 text-xs"
                      >
                        {DISCOS.map((d) => (
                          <option key={d.code} value={d.code}>
                            {d.code}
                          </option>
                        ))}
                      </select>
                      <input
                        type="text"
                        value={attachMeterNo}
                        onChange={(e) => setAttachMeterNo(e.target.value)}
                        placeholder="45011112222"
                        className="flex-1 min-w-0 rounded-lg border border-stone-300 bg-white px-2 py-1.5 font-mono text-xs"
                      />
                    </div>
                    <input
                      type="text"
                      value={evidenceRef}
                      onChange={(e) => setEvidenceRef(e.target.value)}
                      placeholder="Deed ref"
                      className="w-full rounded-lg border border-stone-300 bg-white px-2 py-1 text-[11px]"
                    />
                    <button
                      type="submit"
                      disabled={!attachUnitId}
                      className="w-full rounded-full bg-ink py-1.5 text-xs font-semibold text-volt disabled:opacity-40"
                    >
                      Attach Meter
                    </button>
                  </form>
                </div>
              )}
            </div>
          )}
        </div>
      </div>

      {/* Bulk Top-Up & Statements */}
      {activeOrg && (
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-12">
          {/* Compound Bulk Top-Up */}
          <div className="space-y-4 ml-card p-5 lg:col-span-6">
            <h3 className="text-base font-bold text-stone-900 flex items-center gap-2">
              <Layers className="h-4 w-4 text-ink" />
              Compound Bulk Top-Up (1 Payment → N Units)
            </h3>

            {activeOrg.units.length === 0 ? (
              <p className="text-xs text-stone-500">
                Add properties, units, and meters above to run a bulk top-up.
              </p>
            ) : (
              <form onSubmit={handleBulkTopup} className="space-y-3">
                <div className="space-y-2">
                  {activeOrg.units.map((u) => (
                    <div
                      key={u.id}
                      className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-stone-200 bg-stone-50 px-3 py-2 text-xs"
                    >
                      <div>
                        <span className="font-bold text-stone-900">
                          {u.unit_name}
                        </span>
                        {u.unit_meter ? (
                          <span className="ml-2 font-mono text-stone-600">
                            {u.masked_meter_number} ({u.disco})
                          </span>
                        ) : (
                          <span className="ml-2 text-rose-600">No meter</span>
                        )}
                        {u.unit_meter && (
                          <span className="ml-2 inline-flex items-center gap-1">
                            {u.unit_meter.verified ? (
                              <span className="text-ink font-semibold">
                                <CheckCircle2 className="inline h-3 w-3" /> Verified
                              </span>
                            ) : (
                              <button
                                type="button"
                                onClick={() => {
                                  navigator.clipboard
                                    .writeText(u.unit_meter!.id)
                                    .catch(() => null);
                                  setBanner({
                                    type: "ok",
                                    text: `Copied UnitMeter ID ${u.unit_meter!.id} — verify it in Ops Console (/admin)!`,
                                  });
                                }}
                                className="inline-flex items-center gap-1 text-amber-800 underline"
                                title="Copy UnitMeter ID for Operator verification"
                              >
                                Unverified <Copy className="h-2.5 w-2.5" />
                              </button>
                            )}
                          </span>
                        )}
                      </div>
                      {u.unit_meter && (
                        <div className="flex items-center gap-1">
                          <span className="text-stone-500">₦</span>
                          <input
                            type="number"
                            min={0}
                            step={500}
                            value={unitAmounts[u.id] ?? "5000"}
                            onChange={(e) =>
                              setUnitAmounts((prev) => ({
                                ...prev,
                                [u.id]: e.target.value,
                              }))
                            }
                            className="w-24 rounded-lg border border-stone-300 bg-white px-2 py-1 font-mono text-xs"
                          />
                        </div>
                      )}
                    </div>
                  ))}
                </div>

                <button
                  type="submit"
                  className="w-full rounded-full bg-ink px-4 py-2.5 text-xs font-semibold text-volt hover:bg-black"
                >
                  Pay Once & Dispatch Bulk Top-Up (+₦100 fee/unit)
                </button>
              </form>
            )}

            {bulkHistory.length > 0 && (
              <div className="space-y-2.5 pt-2 border-t border-stone-100">
                <p className="text-xs font-bold uppercase tracking-wider text-stone-500">
                  Recent Bulk Top-Up Batches
                </p>
                {bulkHistory.map((b) => (
                  <div
                    key={b.pot_id}
                    className="rounded-xl border border-stone-200 p-3 text-xs space-y-2"
                  >
                    <div className="flex items-center justify-between">
                      <span className="font-mono font-bold">
                        {formatNaira(b.total_minor)}
                      </span>
                      <StatusPill status={b.pot_status} />
                    </div>
                    <p className="font-medium text-stone-800">{b.summary}</p>
                    {b.approval_status === "PENDING_APPROVAL" && (
                      <button
                        type="button"
                        onClick={() => handleApproveBulk(b.pot_id)}
                        className="rounded-lg bg-amber-600 px-3 py-1 font-semibold text-white"
                      >
                        Approve as OWNER & Open Checkout
                      </button>
                    )}
                    <div className="space-y-1">
                      {b.items.map((it) => (
                        <div
                          key={it.id}
                          className="flex items-center justify-between text-[11px] text-stone-600"
                        >
                          <span>
                            {it.unit_name} ({it.masked_meter_number})
                          </span>
                          <span className="font-mono">
                            {formatNaira(it.amount_minor)} • {it.status}
                          </span>
                        </div>
                      ))}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>

          {/* Ledger-Derived Monthly Statements */}
          <div className="space-y-4 ml-card p-5 lg:col-span-6">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div>
                <h3 className="text-base font-bold text-stone-900">
                  Monthly Statement (Africa/Lagos)
                </h3>
                <p className="text-[11px] text-stone-500">
                  Derived strictly from double-entry ledger postings — labelled “electricity purchased”, never “used”.
                </p>
              </div>
              <div className="flex items-center gap-2">
                <input
                  type="month"
                  value={statementMonth}
                  onChange={(e) => setStatementMonth(e.target.value)}
                  className="rounded-lg border border-stone-300 px-2.5 py-1 font-mono text-xs"
                />
                <button
                  type="button"
                  onClick={handleDownloadCSV}
                  className="inline-flex items-center gap-1 rounded-full bg-ink px-3 py-1.5 text-xs font-semibold text-volt"
                >
                  <Download className="h-3.5 w-3.5" /> CSV
                </button>
              </div>
            </div>

            {statement ? (
              <div className="space-y-3">
                <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-4">
                  <div className="rounded-xl bg-stone-50 p-3 border border-stone-200">
                    <p className="text-[10px] uppercase text-stone-500">
                      Electricity Purchased
                    </p>
                    <p className="font-mono text-sm font-bold text-stone-900">
                      {formatNaira(statement.electricity_purchased_minor)}
                    </p>
                  </div>
                  <div className="rounded-xl bg-stone-50 p-3 border border-stone-200">
                    <p className="text-[10px] uppercase text-stone-500">Fees</p>
                    <p className="font-mono text-sm font-bold text-stone-900">
                      {formatNaira(statement.fees_minor)}
                    </p>
                  </div>
                  <div className="rounded-xl bg-stone-50 p-3 border border-stone-200">
                    <p className="text-[10px] uppercase text-stone-500">Refunds</p>
                    <p className="font-mono text-sm font-bold text-purple-800">
                      {formatNaira(statement.refunds_minor)}
                    </p>
                  </div>
                  <div className="rounded-xl bg-volt/20 p-3 border border-stone-200">
                    <p className="text-[10px] uppercase text-ink">
                      Net Charged
                    </p>
                    <p className="font-mono text-sm font-bold text-ink">
                      {formatNaira(statement.net_charged_minor)}
                    </p>
                  </div>
                </div>

                {statement.properties.map((p) => (
                  <div
                    key={p.property_id}
                    className="rounded-xl border border-stone-200 p-3 space-y-2 text-xs"
                  >
                    <div className="flex justify-between font-bold text-stone-900">
                      <span>{p.property_name}</span>
                      <span className="font-mono">
                        {formatNaira(p.electricity_purchased_minor)} purchased
                      </span>
                    </div>
                    {p.units.map((u) => (
                      <div
                        key={u.unit_id}
                        className="flex justify-between text-stone-600 border-t border-stone-100 pt-1.5"
                      >
                        <span>
                          {u.unit_name} ({u.masked_meter_number})
                        </span>
                        <span className="font-mono">
                          {formatNaira(u.electricity_purchased_minor)} ({u.vended_count}{" "}
                          vended)
                        </span>
                      </div>
                    ))}
                  </div>
                ))}
              </div>
            ) : (
              <p className="text-xs text-stone-500">
                No ledger postings recorded for {statementMonth} yet.
              </p>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
