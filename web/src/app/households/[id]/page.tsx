"use client";

import React, { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useParams } from "next/navigation";
import { useAuth } from "@/components/AuthContext";
import { ApiError, apiFetch } from "@/lib/api";
import {
  HouseholdMember,
  HouseholdRole,
  HouseholdView,
  Meter,
  MeterOwnership,
} from "@/lib/types";
import { StatusPill } from "@/components/StatusBadges";
import { ArrowLeft, ShieldCheck, UserPlus, Zap } from "lucide-react";

const INVITE_ROLES: HouseholdRole[] = [
  "MEMBER",
  "ADMINISTRATOR",
  "VIEWER",
  "RECIPIENT",
];

function canAdmin(role?: string) {
  return role === "OWNER" || role === "ADMINISTRATOR";
}

export default function HouseholdDetailPage() {
  const { id } = useParams<{ id: string }>();
  const { token } = useAuth();
  const [view, setView] = useState<HouseholdView | null>(null);
  const [meters, setMeters] = useState<Meter[]>([]);
  const [ownership, setOwnership] = useState<Record<string, MeterOwnership[]>>({});
  const [banner, setBanner] = useState<{ type: "ok" | "err"; text: string } | null>(
    null
  );
  const [busy, setBusy] = useState(false);

  const [inviteExternal, setInviteExternal] = useState("bola");
  const [inviteRole, setInviteRole] = useState<HouseholdRole>("MEMBER");
  const [meterToAdd, setMeterToAdd] = useState("");
  const [verifyMeterId, setVerifyMeterId] = useState("");
  const [billName, setBillName] = useState("");

  const load = useCallback(async () => {
    try {
      const [hh, mRes] = await Promise.all([
        apiFetch<HouseholdView>(`/v1/households/${id}`, { token }),
        apiFetch<{ data: Meter[] }>("/v1/meters", { token }),
      ]);
      setView(hh);
      const list = mRes.data || [];
      setMeters(list);
      setMeterToAdd((prev) =>
        prev && list.some((m) => m.meter_id === prev) ? prev : list[0]?.meter_id || ""
      );
      setVerifyMeterId((prev) => {
        const next =
          prev && list.some((m) => m.meter_id === prev) ? prev : list[0]?.meter_id || "";
        const m = list.find((x) => x.meter_id === next);
        if (m) setBillName((bn) => bn || m.customer_name || "");
        return next;
      });
      const own: Record<string, MeterOwnership[]> = {};
      await Promise.all(
        list.map(async (m) => {
          try {
            const r = await apiFetch<{ data: MeterOwnership[] }>(
              `/v1/meters/${m.meter_id}/ownership`,
              { token }
            );
            own[m.meter_id] = r.data || [];
          } catch {
            own[m.meter_id] = [];
          }
        })
      );
      setOwnership(own);
    } catch (e) {
      setBanner({
        type: "err",
        text: e instanceof Error ? e.message : "Failed to load household",
      });
    }
  }, [id, token]);

  useEffect(() => {
    load();
  }, [load]);

  const run = async (fn: () => Promise<void>, ok: string) => {
    setBusy(true);
    setBanner(null);
    try {
      await fn();
      setBanner({ type: "ok", text: ok });
      await load();
    } catch (e) {
      const msg =
        e instanceof ApiError
          ? e.message
          : e instanceof Error
            ? e.message
            : "Request failed";
      setBanner({ type: "err", text: msg });
    } finally {
      setBusy(false);
    }
  };

  const verifyOwnership = (e: React.FormEvent) => {
    e.preventDefault();
    if (!verifyMeterId) return;
    return run(async () => {
      await apiFetch(`/v1/meters/${verifyMeterId}/ownership/verify`, {
        method: "POST",
        token,
        headers: billName.trim()
          ? { "X-Dev-Display-Name": billName.trim() }
          : undefined,
        body: {},
      });
    }, "Ownership verified against the DisCo account name.");
  };

  const addMeter = (e: React.FormEvent) => {
    e.preventDefault();
    return run(async () => {
      await apiFetch(`/v1/households/${id}/meters`, {
        method: "POST",
        token,
        body: { meter_id: meterToAdd },
      });
    }, "Meter linked. Members with purchase roles can buy for it.");
  };

  const invite = (e: React.FormEvent) => {
    e.preventDefault();
    return run(async () => {
      await apiFetch<HouseholdMember>(`/v1/households/${id}/members`, {
        method: "POST",
        token,
        body: { external_id: inviteExternal.trim(), role: inviteRole },
      });
    }, `Invited ${inviteExternal.trim()} as ${inviteRole}. They must accept.`);
  };

  const revoke = (uid: string) =>
    run(async () => {
      await apiFetch(`/v1/households/${id}/members/${uid}`, {
        method: "DELETE",
        token,
      });
    }, "Member revoked. They can no longer buy for this household.");

  if (!view) {
    return (
      <p className="text-sm text-stone-500">
        {banner?.text || "Loading household…"}
      </p>
    );
  }

  const admin = canAdmin(view.my_role);
  const linkedIds = new Set((view.meters || []).map((l) => l.meter_id));
  const verifyMeter = meters.find((m) => m.meter_id === verifyMeterId);

  return (
    <div className="space-y-6">
      <div>
        <Link
          href="/households"
          className="inline-flex items-center gap-1 text-xs font-medium text-volt hover:underline"
        >
          <ArrowLeft className="h-3.5 w-3.5" /> All households
        </Link>
        <h1 className="font-display mt-2 text-3xl font-semibold text-paper">
          {view.household.name}
        </h1>
        <p className="mt-1 text-xs text-white/50">
          Your role <StatusPill status={view.my_role || ""} /> · status{" "}
          <StatusPill status={view.my_status || ""} />
        </p>
      </div>

      {banner && (
        <div
          className={`rounded-xl border px-4 py-2.5 text-sm ${
            banner.type === "ok"
              ? "border-stone-200 bg-volt/20 text-ink"
              : "border-rose-200 bg-rose-50 text-rose-900"
          }`}
        >
          {banner.text}
        </div>
      )}

      <div className="grid gap-4 lg:grid-cols-2">
        <form
          onSubmit={verifyOwnership}
          className="ml-card space-y-3 p-5"
        >
          <h2 className="text-sm font-bold flex items-center gap-2">
            <ShieldCheck className="h-4 w-4 text-ink" />
            Verify ownership
          </h2>
          <p className="text-xs text-stone-500">
            The name must match the DisCo customer name exactly (normalized).
            Sandbox meters use <span className="font-mono">TEST CUSTOMER ####</span>.
          </p>
          <select
            value={verifyMeterId}
            onChange={(e) => {
              setVerifyMeterId(e.target.value);
              const m = meters.find((x) => x.meter_id === e.target.value);
              setBillName(m?.customer_name || "");
            }}
            className="w-full rounded-xl border border-stone-300 px-3 py-2 text-sm"
          >
            {meters.map((m) => (
              <option key={m.meter_id} value={m.meter_id}>
                {m.masked_meter_number} · {m.disco} · {m.customer_name}
              </option>
            ))}
          </select>
          <input
            value={billName}
            onChange={(e) => setBillName(e.target.value)}
            className="w-full rounded-xl border border-stone-300 px-3 py-2 text-sm"
            placeholder="Name on the electricity bill"
            required
          />
          {verifyMeter && (
            <p className="text-[11px] text-stone-500">
              DisCo currently reports:{" "}
              <span className="font-mono">{verifyMeter.customer_name}</span>
              {(ownership[verifyMeter.meter_id] || []).length > 0
                ? " · already verified on this account"
                : ""}
            </p>
          )}
          <button
            type="submit"
            disabled={busy || !verifyMeterId}
            className="rounded-full bg-ink px-4 py-2 text-sm font-semibold text-volt disabled:opacity-50"
          >
            Match name & verify
          </button>
        </form>

        {admin && (
          <form
            onSubmit={addMeter}
            className="ml-card space-y-3 p-5"
          >
            <h2 className="text-sm font-bold flex items-center gap-2">
              <Zap className="h-4 w-4 text-ink" />
              Add a verified meter
            </h2>
            <p className="text-xs text-stone-500">
              Only a verified owner can attach a meter. Roles never grant MANAGE.
            </p>
            <select
              value={meterToAdd}
              onChange={(e) => setMeterToAdd(e.target.value)}
              className="w-full rounded-xl border border-stone-300 px-3 py-2 text-sm"
            >
              {meters.map((m) => (
                <option key={m.meter_id} value={m.meter_id}>
                  {m.masked_meter_number} {linkedIds.has(m.meter_id) ? "(already linked)" : ""}
                </option>
              ))}
            </select>
            <button
              type="submit"
              disabled={busy || !meterToAdd}
              className="rounded-full bg-ink px-4 py-2 text-sm font-semibold text-volt disabled:opacity-50"
            >
              Link meter
            </button>
          </form>
        )}

        {admin && (
          <form
            onSubmit={invite}
            className="ml-card space-y-3 p-5"
          >
            <h2 className="text-sm font-bold flex items-center gap-2">
              <UserPlus className="h-4 w-4 text-ink" />
              Invite who may buy
            </h2>
            <p className="text-xs text-stone-500">
              Dev login is the bearer token. Invite <span className="font-mono">bola</span>,
              then switch the persona to Bola and accept.
            </p>
            <div className="grid grid-cols-2 gap-2">
              <input
                value={inviteExternal}
                onChange={(e) => setInviteExternal(e.target.value)}
                className="rounded-xl border border-stone-300 px-3 py-2 font-mono text-sm"
                placeholder="external_id"
                required
              />
              <select
                value={inviteRole}
                onChange={(e) => setInviteRole(e.target.value as HouseholdRole)}
                className="rounded-xl border border-stone-300 px-3 py-2 text-sm"
              >
                {INVITE_ROLES.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
            </div>
            <button
              type="submit"
              disabled={busy}
              className="rounded-full bg-ink px-4 py-2 text-sm font-semibold text-volt disabled:opacity-50"
            >
              Send invite
            </button>
          </form>
        )}
      </div>

      <section className="ml-card p-5">
        <h2 className="text-sm font-bold mb-3">Meters in this household</h2>
        {(view.meters || []).length === 0 ? (
          <p className="text-sm text-stone-500">None linked yet.</p>
        ) : (
          <ul className="space-y-2 text-sm">
            {(view.meters || []).map((link) => {
              const m = meters.find((x) => x.meter_id === link.meter_id);
              return (
                <li
                  key={link.meter_id}
                  className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-stone-100 px-3 py-2"
                >
                  <span className="font-mono">
                    {m?.masked_meter_number || link.meter_id.slice(0, 8)}
                  </span>
                  <span className="text-xs text-stone-500">
                    {m?.disco} · {m?.customer_name}
                  </span>
                </li>
              );
            })}
          </ul>
        )}
      </section>

      <section className="ml-card p-5">
        <h2 className="text-sm font-bold mb-3">Members</h2>
        <ul className="space-y-2">
          {(view.members || []).map((mem) => (
            <li
              key={mem.member_id}
              className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-stone-100 px-3 py-2 text-sm"
            >
              <div>
                <span className="font-mono text-xs text-stone-600">
                  {mem.user_id.slice(0, 8)}…
                </span>
                <span className="ml-2">
                  <StatusPill status={mem.role} />{" "}
                  <StatusPill status={mem.status} />
                </span>
              </div>
              {admin && mem.role !== "OWNER" && mem.status !== "REVOKED" && (
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => revoke(mem.user_id)}
                  className="text-xs font-medium text-rose-700 hover:underline"
                >
                  Revoke
                </button>
              )}
            </li>
          ))}
        </ul>
      </section>
    </div>
  );
}
