"use client";

import React, { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useAuth } from "@/components/AuthContext";
import { ApiError, apiFetch } from "@/lib/api";
import { Household, HouseholdView } from "@/lib/types";
import { StatusPill } from "@/components/StatusBadges";
import { Home, Plus, Check, X } from "lucide-react";

export default function HouseholdsPage() {
  const { token, activePersona } = useAuth();
  const [views, setViews] = useState<HouseholdView[]>([]);
  const [name, setName] = useState("Ada house");
  const [busy, setBusy] = useState(false);
  const [banner, setBanner] = useState<{ type: "ok" | "err"; text: string } | null>(
    null
  );

  const load = useCallback(async () => {
    try {
      const res = await apiFetch<{ data: HouseholdView[] }>("/v1/households", {
        token,
      });
      setViews(res.data || []);
    } catch (e) {
      const msg =
        e instanceof ApiError && e.code === "FEATURE_DISABLED"
          ? "Households are off on this API (FEATURE_HOUSEHOLDS)."
          : e instanceof Error
            ? e.message
            : "Failed to load households";
      setBanner({ type: "err", text: msg });
    }
  }, [token]);

  useEffect(() => {
    setBanner(null);
    load();
  }, [load]);

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setBanner(null);
    try {
      const h = await apiFetch<Household>("/v1/households", {
        method: "POST",
        token,
        body: { name: name.trim() },
      });
      setBanner({ type: "ok", text: `Created “${h.name}”. Verify ownership, then add the meter.` });
      await load();
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Could not create household",
      });
    } finally {
      setBusy(false);
    }
  };

  const respond = async (id: string, accept: boolean) => {
    setBusy(true);
    setBanner(null);
    try {
      await apiFetch(
        `/v1/households/${id}/members/me/${accept ? "accept" : "decline"}`,
        { method: "POST", token, body: {} }
      );
      setBanner({
        type: "ok",
        text: accept ? "You’re in. You can buy for this household’s meters." : "Invite declined.",
      });
      await load();
    } catch (err) {
      setBanner({
        type: "err",
        text: err instanceof Error ? err.message : "Could not update invite",
      });
    } finally {
      setBusy(false);
    }
  };

  const pending = views.filter((v) => v.my_status === "INVITED");
  const active = views.filter((v) => v.my_status === "ACTIVE");

  return (
    <div className="space-y-6">
      <div>
        <h1 className="font-display flex items-center gap-2 text-3xl font-semibold text-paper">
          <Home className="h-5 w-5 text-volt" />
          Household
        </h1>
        <p className="mt-2 max-w-2xl text-sm text-white/55">
          Your meter, who may buy, someone else can pay. Verified ownership first —
          knowing a meter number is not enough. Invite a flatmate as{" "}
          <span className="font-mono">bola</span> then switch persona to accept.
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

      <form
        onSubmit={create}
        className="ml-card space-y-3 p-5"
      >
        <h2 className="text-sm font-bold text-stone-900 flex items-center gap-2">
          <Plus className="h-4 w-4 text-ink" />
          New household
        </h2>
        <p className="text-xs text-stone-500">
          Signed in as {activePersona?.displayName || "you"}. You become OWNER.
          Adding a meter still requires a name match with the DisCo account.
        </p>
        <div className="flex flex-wrap gap-2">
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
            minLength={2}
            className="flex-1 min-w-[12rem] rounded-xl border border-stone-300 px-3 py-2 text-sm"
            placeholder="e.g. Lekki Flat 3B"
          />
          <button
            type="submit"
            disabled={busy}
            className="rounded-full bg-ink px-4 py-2 text-sm font-semibold text-volt hover:bg-black disabled:opacity-50"
          >
            Create
          </button>
        </div>
      </form>

      {pending.length > 0 && (
        <section className="space-y-2">
          <h2 className="text-sm font-semibold text-white/70">Pending invites</h2>
          {pending.map((v) => (
            <div
              key={v.household.household_id}
              className="flex flex-wrap items-center justify-between gap-3 rounded-2xl border border-amber-200 bg-amber-50/70 p-4"
            >
              <div>
                <p className="font-semibold text-stone-900">{v.household.name}</p>
                <p className="text-xs text-stone-600">
                  Invited as {v.my_role} · switch persona if this isn’t your account
                </p>
              </div>
              <div className="flex gap-2">
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => respond(v.household.household_id, true)}
                  className="inline-flex items-center gap-1 rounded-full bg-ink px-3 py-1.5 text-xs font-semibold text-volt"
                >
                  <Check className="h-3.5 w-3.5" /> Accept
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => respond(v.household.household_id, false)}
                  className="inline-flex items-center gap-1 rounded-lg border border-stone-300 bg-white px-3 py-1.5 text-xs font-medium text-stone-700"
                >
                  <X className="h-3.5 w-3.5" /> Decline
                </button>
              </div>
            </div>
          ))}
        </section>
      )}

      <section className="space-y-2">
        <h2 className="text-sm font-semibold text-white/70">Your households</h2>
        {active.length === 0 ? (
          <div className="rounded-[24px] border border-dashed border-white/15 bg-white/5 p-8 text-center text-sm text-white/50">
            None yet. Create one, or accept an invite from Ada / a landlord.
          </div>
        ) : (
          <div className="grid gap-3 sm:grid-cols-2">
            {active.map((v) => (
              <Link
                key={v.household.household_id}
                href={`/households/${v.household.household_id}`}
                className="ml-card p-4 hover:border-ink/20 transition"
              >
                <div className="flex items-start justify-between gap-2">
                  <p className="font-semibold text-stone-900">{v.household.name}</p>
                  <StatusPill status={v.my_role || ""} />
                </div>
                <p className="mt-2 text-xs text-stone-500">
                  {(v.members || []).filter((m) => m.status === "ACTIVE").length} active
                  member{(v.members || []).filter((m) => m.status === "ACTIVE").length === 1 ? "" : "s"}
                  {" · "}
                  {(v.meters || []).length} meter{(v.meters || []).length === 1 ? "" : "s"}
                </p>
              </Link>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}
