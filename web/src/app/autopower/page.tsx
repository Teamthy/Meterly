"use client";

import React, { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useAuth } from "@/components/AuthContext";
import { apiFetch, formatNaira } from "@/lib/api";
import { AutoPowerRule, Meter, SavedPaymentMethod } from "@/lib/types";
import { StatusPill } from "@/components/StatusBadges";
import {
  CalendarClock,
  CreditCard,
  Trash2,
  SkipForward,
  Pause,
  Play,
} from "lucide-react";

export default function AutoPowerPage() {
  const { token } = useAuth();
  const [meters, setMeters] = useState<Meter[]>([]);
  const [cards, setCards] = useState<SavedPaymentMethod[]>([]);
  const [rules, setRules] = useState<AutoPowerRule[]>([]);
  const [banner, setBanner] = useState<{ type: "ok" | "err"; text: string } | null>(
    null
  );

  const [meterId, setMeterId] = useState("");
  const [pmId, setPmId] = useState("");
  const [triggerType, setTriggerType] = useState<"SCHEDULED" | "LOW_BALANCE">(
    "SCHEDULED"
  );
  const [amountNaira, setAmountNaira] = useState("10000");
  const [monthlyCapNaira, setMonthlyCapNaira] = useState("40000");
  const [frequency, setFrequency] = useState<"MONTHLY" | "WEEKLY">("MONTHLY");
  const [daysInput, setDaysInput] = useState("1,15");
  const [timeOfDay, setTimeOfDay] = useState("07:00");
  const [thresholdKwh, setThresholdKwh] = useState("20");
  const [creating, setCreating] = useState(false);

  const loadAll = useCallback(async () => {
    try {
      const [mRes, pmRes, rRes] = await Promise.all([
        apiFetch<{ data: Meter[] }>("/v1/meters", { token }),
        apiFetch<{ payment_methods: SavedPaymentMethod[] }>(
          "/v1/me/payment-methods",
          { token }
        ),
        apiFetch<{ rules?: AutoPowerRule[]; data?: AutoPowerRule[] }>(
          "/v1/autopower/rules",
          { token }
        ),
      ]);
      const mList = mRes.data || [];
      const pmList = pmRes.payment_methods || [];
      const rList = rRes.rules || rRes.data || [];
      setMeters(mList);
      setCards(pmList);
      setRules(rList);
      if (mList.length > 0) {
        setMeterId((prev) =>
          mList.some((m) => m.meter_id === prev) ? prev : mList[0].meter_id
        );
      }
      if (pmList.length > 0) {
        setPmId((prev) =>
          pmList.some((c) => c.id === prev) ? prev : pmList[0].id
        );
      }
    } catch (e) {
      setBanner({
        type: "err",
        text: e instanceof Error ? e.message : "Failed to load AutoPower state",
      });
    }
  }, [token]);

  useEffect(() => {
    loadAll();
  }, [loadAll]);

  const selectedMeter = meters.find((m) => m.meter_id === meterId);
  // Spec M9: Hide Low-Balance mode unless remote_reading is supported, authorised, confirmed & not unreachable!
  const readingEligible = Boolean(
    selectedMeter?.capabilities?.remote_reading?.supported &&
      selectedMeter?.capabilities?.remote_reading?.authorised &&
      selectedMeter?.capabilities?.remote_reading?.confirmed &&
      selectedMeter?.capabilities?.remote_reading?.reachable !== false
  );

  useEffect(() => {
    if (!readingEligible && triggerType === "LOW_BALANCE") {
      setTriggerType("SCHEDULED");
    }
  }, [readingEligible, triggerType]);

  const handleCreateRule = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!meterId || !pmId) {
      setBanner({
        type: "err",
        text: "Both a saved meter and a saved card mandate are required.",
      });
      return;
    }
    setCreating(true);
    setBanner(null);
    try {
      const parsedDays = daysInput
        .split(",")
        .map((s) => Number(s.trim()))
        .filter((n) => Number.isInteger(n) && n >= 0);

      const payload: Record<string, unknown> = {
        meter_id: meterId,
        payment_method_id: pmId,
        trigger_type: triggerType,
        amount_minor: Math.round(Number(amountNaira) * 100),
        monthly_cap_minor: Math.round(Number(monthlyCapNaira) * 100),
        delivery_preference: "TOKEN",
        timezone: "Africa/Lagos",
      };
      if (triggerType === "SCHEDULED") {
        payload.frequency = frequency;
        payload.time_of_day = timeOfDay;
        if (frequency === "MONTHLY") {
          payload.days_of_month = parsedDays;
        } else {
          payload.days_of_week = parsedDays;
        }
      } else {
        payload.low_balance_threshold_kwh = Number(thresholdKwh);
      }

      await apiFetch("/v1/autopower/rules", {
        method: "POST",
        token,
        body: payload,
      });
      await loadAll();
      setBanner({
        type: "ok",
        text: "AutoPower rule active! Evaluated in Africa/Lagos with 12h heads-up notifications.",
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Could not create AutoPower rule",
      });
    } finally {
      setCreating(false);
    }
  };

  const handleDeleteCard = async (id: string) => {
    try {
      await apiFetch(`/v1/me/payment-methods/${id}`, {
        method: "DELETE",
        token,
      });
      await loadAll();
      setBanner({
        type: "ok",
        text: "Saved card deleted — all AutoPower rules referencing it were atomically stopped.",
      });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Failed to delete card",
      });
    }
  };

  const handleToggleRuleStatus = async (rule: AutoPowerRule) => {
    const nextStatus = rule.status === "ACTIVE" ? "PAUSED" : "ACTIVE";
    try {
      await apiFetch(`/v1/autopower/rules/${rule.rule_id}`, {
        method: "PATCH",
        token,
        body: { status: nextStatus },
      });
      await loadAll();
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Failed to update rule",
      });
    }
  };

  const handleDeleteRule = async (id: string) => {
    try {
      await apiFetch(`/v1/autopower/rules/${id}`, { method: "DELETE", token });
      await loadAll();
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Failed to delete rule",
      });
    }
  };

  const handleSkipRun = async (runId: string) => {
    try {
      await apiFetch(`/v1/autopower/runs/${runId}/skip`, {
        method: "POST",
        token,
        body: { reason: "Skipped from customer dashboard" },
      });
      await loadAll();
      setBanner({ type: "ok", text: "Upcoming scheduled recharge skipped." });
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Could not skip run",
      });
    }
  };

  return (
    <div className="space-y-6">
      <div className="rounded-2xl bg-[#0B1F1A] p-6 text-white">
        <span className="text-xs font-semibold uppercase tracking-wider text-emerald-300">
          F3 • AutoPower Scheduled Recharge & Spend Guards
        </span>
        <h1 className="mt-1 text-2xl font-bold">
          Never lose power at night — automated recharges with hard monthly caps.
        </h1>
        <p className="mt-1 max-w-2xl text-sm text-stone-300">
          Evaluated in <code className="font-mono">Africa/Lagos</code> with 12-hour heads-up alerts, one-tap skip, server-side charge verification, and automatic pause after 3 consecutive declines.
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
        <div className="space-y-5 lg:col-span-5">
          {/* Saved Cards */}
          <section className="rounded-2xl border border-stone-200 bg-white p-5 shadow-xs space-y-3">
            <h2 className="text-base font-bold text-stone-900 flex items-center gap-2">
              <CreditCard className="h-4 w-4 text-emerald-600" />
              Saved Card Mandates ({cards.length})
            </h2>

            {cards.length === 0 ? (
              <p className="rounded-xl border border-dashed border-stone-300 p-3.5 text-xs text-stone-600">
                No saved cards yet. Complete one card purchase on{" "}
                <Link href="/" className="font-semibold text-emerald-700 underline">
                  Buy Power
                </Link>{" "}
                and click <strong>Save Card for AutoPower</strong> with explicit consent.
              </p>
            ) : (
              <div className="space-y-2">
                {cards.map((c) => (
                  <div
                    key={c.id}
                    className="flex items-center justify-between rounded-xl border border-stone-200 bg-stone-50 px-3.5 py-2.5 text-xs"
                  >
                    <div>
                      <p className="font-semibold text-stone-900 uppercase">
                        {c.brand} •••• {c.last4}{" "}
                        <span className="font-mono text-stone-500">
                          ({String(c.exp_month).padStart(2, "0")}/{c.exp_year})
                        </span>
                      </p>
                      <p className="text-[11px] text-stone-500">
                        Consent {c.consent_version} • AEAD-bound
                      </p>
                    </div>
                    <button
                      type="button"
                      onClick={() => handleDeleteCard(c.id)}
                      className="rounded-lg p-1.5 text-stone-400 hover:bg-rose-50 hover:text-rose-700"
                      title="Delete card & stop referencing rules"
                    >
                      <Trash2 className="h-4 w-4" />
                    </button>
                  </div>
                ))}
              </div>
            )}
          </section>

          {/* Create Rule */}
          <form
            onSubmit={handleCreateRule}
            className="space-y-3.5 rounded-2xl border border-stone-200 bg-white p-5 shadow-xs"
          >
            <h2 className="text-base font-bold text-stone-900 flex items-center gap-2">
              <CalendarClock className="h-4 w-4 text-emerald-600" />
              Create AutoPower Rule
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
                {meters.map((m) => (
                  <option key={m.meter_id} value={m.meter_id}>
                    {m.label ? `${m.label} • ` : ""}
                    {m.masked_meter_number} ({m.disco})
                  </option>
                ))}
              </select>
            </div>

            <div>
              <label className="block text-xs font-medium text-stone-700 mb-1">
                Saved Payment Method
              </label>
              <select
                value={pmId}
                onChange={(e) => setPmId(e.target.value)}
                className="w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-sm"
              >
                {cards.length === 0 && (
                  <option value="">Save a card from Buy Power first</option>
                )}
                {cards.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.brand.toUpperCase()} •••• {c.last4}
                  </option>
                ))}
              </select>
            </div>

            {/* Capability-gated trigger selector */}
            {readingEligible && (
              <div className="grid grid-cols-2 gap-2">
                <button
                  type="button"
                  onClick={() => setTriggerType("SCHEDULED")}
                  className={`rounded-xl border py-2 text-xs font-semibold ${
                    triggerType === "SCHEDULED"
                      ? "border-emerald-600 bg-emerald-50 text-emerald-900"
                      : "border-stone-200 bg-stone-50"
                  }`}
                >
                  Calendar Schedule
                </button>
                <button
                  type="button"
                  onClick={() => setTriggerType("LOW_BALANCE")}
                  className={`rounded-xl border py-2 text-xs font-semibold ${
                    triggerType === "LOW_BALANCE"
                      ? "border-emerald-600 bg-emerald-50 text-emerald-900"
                      : "border-stone-200 bg-stone-50"
                  }`}
                >
                  Low kWh Balance
                </button>
              </div>
            )}

            <div className="grid grid-cols-2 gap-3">
              <div>
                <label className="block text-xs font-medium text-stone-700 mb-1">
                  Recharge Amount (₦)
                </label>
                <input
                  type="number"
                  min={1000}
                  value={amountNaira}
                  onChange={(e) => setAmountNaira(e.target.value)}
                  required
                  className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
                />
              </div>
              <div>
                <label className="block text-xs font-medium text-stone-700 mb-1">
                  Monthly Spend Cap (₦)
                </label>
                <input
                  type="number"
                  min={1000}
                  value={monthlyCapNaira}
                  onChange={(e) => setMonthlyCapNaira(e.target.value)}
                  required
                  className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
                />
              </div>
            </div>

            {triggerType === "SCHEDULED" ? (
              <div className="grid grid-cols-3 gap-2.5">
                <div>
                  <label className="block text-xs font-medium text-stone-700 mb-1">
                    Cadence
                  </label>
                  <select
                    value={frequency}
                    onChange={(e) => {
                      const f = e.target.value as "MONTHLY" | "WEEKLY";
                      setFrequency(f);
                      setDaysInput(f === "MONTHLY" ? "1,15" : "1");
                    }}
                    className="w-full rounded-xl border border-stone-300 bg-white px-2.5 py-2 text-xs"
                  >
                    <option value="MONTHLY">Monthly</option>
                    <option value="WEEKLY">Weekly</option>
                  </select>
                </div>
                <div>
                  <label className="block text-xs font-medium text-stone-700 mb-1">
                    {frequency === "MONTHLY" ? "Days (1–28)" : "Weekdays (0=Sun)"}
                  </label>
                  <input
                    type="text"
                    value={daysInput}
                    onChange={(e) => setDaysInput(e.target.value)}
                    className="w-full rounded-xl border border-stone-300 px-2.5 py-2 font-mono text-xs"
                  />
                </div>
                <div>
                  <label className="block text-xs font-medium text-stone-700 mb-1">
                    Time (WAT)
                  </label>
                  <input
                    type="time"
                    value={timeOfDay}
                    onChange={(e) => setTimeOfDay(e.target.value)}
                    className="w-full rounded-xl border border-stone-300 px-2.5 py-2 font-mono text-xs"
                  />
                </div>
              </div>
            ) : (
              <div>
                <label className="block text-xs font-medium text-stone-700 mb-1">
                  Minimum Balance Threshold (kWh)
                </label>
                <input
                  type="number"
                  min={1}
                  step="0.5"
                  value={thresholdKwh}
                  onChange={(e) => setThresholdKwh(e.target.value)}
                  className="w-full rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
                />
              </div>
            )}

            <button
              type="submit"
              disabled={creating || !meterId || !pmId}
              className="w-full rounded-xl bg-emerald-600 px-4 py-3 text-sm font-semibold text-white hover:bg-emerald-700 disabled:opacity-50"
            >
              {creating ? "Activating Rule..." : "Enable AutoPower Rule"}
            </button>
          </form>
        </div>

        {/* Active Rules */}
        <div className="space-y-4 lg:col-span-7">
          <h2 className="text-base font-bold text-stone-900">
            Active & Paused Rules ({rules.length})
          </h2>

          {rules.length === 0 ? (
            <div className="rounded-2xl border border-dashed border-stone-300 bg-white p-8 text-center text-xs text-stone-500">
              No AutoPower rules configured yet.
            </div>
          ) : (
            <div className="space-y-3">
              {rules.map((r) => (
                <div
                  key={r.rule_id}
                  className="rounded-2xl border border-stone-200 bg-white p-4 shadow-xs space-y-3"
                >
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div>
                      <div className="flex items-center gap-2">
                        <span className="font-mono text-base font-bold text-stone-900">
                          {formatNaira(r.amount_minor)}
                        </span>
                        <span className="text-xs text-stone-500">
                          (Cap {formatNaira(r.monthly_cap_minor)}/mo)
                        </span>
                        <StatusPill status={r.status} />
                      </div>
                      <p className="text-xs text-stone-600 mt-0.5 font-mono">
                        {r.masked_meter_number || r.meter_id.slice(0, 8)} •{" "}
                        {r.trigger_type === "SCHEDULED"
                          ? `${r.frequency} @ ${r.time_of_day} ${r.timezone}`
                          : `Low Balance < ${r.low_balance_threshold_kwh} kWh`}
                      </p>
                    </div>

                    <div className="flex items-center gap-1.5">
                      {r.status !== "STOPPED" && (
                        <button
                          type="button"
                          onClick={() => handleToggleRuleStatus(r)}
                          className="inline-flex items-center gap-1 rounded-lg border border-stone-300 px-2.5 py-1 text-xs font-semibold text-stone-700 hover:bg-stone-50"
                        >
                          {r.status === "ACTIVE" ? (
                            <>
                              <Pause className="h-3.5 w-3.5" /> Pause
                            </>
                          ) : (
                            <>
                              <Play className="h-3.5 w-3.5" /> Resume
                            </>
                          )}
                        </button>
                      )}
                      <button
                        type="button"
                        onClick={() => handleDeleteRule(r.rule_id)}
                        className="rounded-lg border border-rose-200 bg-rose-50 p-1.5 text-rose-700 hover:bg-rose-100"
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                      </button>
                    </div>
                  </div>

                  {r.next_scheduled_at && (
                    <p className="text-xs text-stone-500">
                      Next scheduled charge:{" "}
                      <span className="font-mono text-stone-800">
                        {new Date(r.next_scheduled_at).toLocaleString()}
                      </span>
                    </p>
                  )}

                  {r.upcoming_run &&
                    (r.upcoming_run.status === "SCHEDULED" ||
                      r.upcoming_run.status === "NOTIFIED") && (
                      <div className="flex items-center justify-between rounded-xl bg-amber-50 border border-amber-200 px-3 py-2 text-xs">
                        <span className="text-amber-950">
                          Upcoming run:{" "}
                          <strong className="font-mono">
                            {new Date(
                              r.upcoming_run.scheduled_for
                            ).toLocaleString()}
                          </strong>
                        </span>
                        <button
                          type="button"
                          onClick={() => handleSkipRun(r.upcoming_run!.id)}
                          className="inline-flex items-center gap-1 rounded-lg bg-amber-700 px-2.5 py-1 font-semibold text-white"
                        >
                          <SkipForward className="h-3.5 w-3.5" /> Skip Once
                        </button>
                      </div>
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
